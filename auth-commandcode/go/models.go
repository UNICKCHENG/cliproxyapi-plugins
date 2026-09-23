package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

const (
	catalogTTL     = 5 * time.Minute
	catalogBackoff = 30 * time.Second
)

var modelCatalogs = struct {
	mu       sync.Mutex
	entries  map[string]catalogEntry
	inflight map[string]*catalogWait
}{
	entries:  make(map[string]catalogEntry),
	inflight: make(map[string]*catalogWait),
}

type catalogEntry struct {
	models       []pluginapi.ModelInfo
	fetched      time.Time
	backoffUntil time.Time
}

type catalogWait struct {
	done   chan struct{}
	models []pluginapi.ModelInfo
	err    error
}

func staticModels(raw []byte) ([]byte, error) {
	var req pluginapi.StaticModelRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}
	return okEnvelope(pluginapi.ModelResponse{
		Provider: providerIdentifier,
		Models:   applyExcludedModels(nil, excludedModelsForRequest(req.Host, nil, nil)),
	})
}

func modelsForAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	apiKey, errKey := requireAPIKey(req.StorageJSON)
	if errKey != nil {
		return okEnvelope(pluginapi.ModelResponse{Provider: providerIdentifier})
	}

	models, errDiscover := catalogFor(context.Background(), apiKey, resolveProxyURL(req.StorageJSON), true)
	if errDiscover != nil || len(models) == 0 {
		reason := "empty catalog"
		if errDiscover != nil {
			reason = failureFrom(errDiscover).Message
		}
		hostLog("warn", "commandcode model discovery failed", map[string]any{
			"auth_id": req.AuthID,
			"reason":  reason,
		})
	} else {
		hostLog("debug", "commandcode model discovery succeeded", map[string]any{
			"auth_id": req.AuthID,
			"models":  len(models),
		})
	}
	return okEnvelope(pluginapi.ModelResponse{
		Provider: providerIdentifier,
		Models:   applyExcludedModels(models, excludedModelsForRequest(req.Host, req.Attributes, req.StorageJSON)),
	})
}

func catalogCacheKey(apiKey string) string {
	digest := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(digest[:])
}

func catalogFor(ctx context.Context, apiKey, proxyURL string, refresh bool) ([]pluginapi.ModelInfo, error) {
	key := catalogCacheKey(apiKey)
	now := time.Now()

	modelCatalogs.mu.Lock()
	if entry, ok := modelCatalogs.entries[key]; ok {
		if !refresh && now.Sub(entry.fetched) < catalogTTL && len(entry.models) > 0 {
			models := entry.models
			modelCatalogs.mu.Unlock()
			return models, nil
		}
		if refresh && now.Before(entry.backoffUntil) && len(entry.models) > 0 {
			models := entry.models
			modelCatalogs.mu.Unlock()
			return models, nil
		}
	}
	if wait, busy := modelCatalogs.inflight[key]; busy {
		modelCatalogs.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-wait.done:
			return wait.models, wait.err
		}
	}
	wait := &catalogWait{done: make(chan struct{})}
	modelCatalogs.inflight[key] = wait
	modelCatalogs.mu.Unlock()

	models, errFetch := fetchModels(ctx, apiKey, proxyURL)
	modelCatalogs.mu.Lock()
	delete(modelCatalogs.inflight, key)
	if errFetch != nil {
		if entry, ok := modelCatalogs.entries[key]; ok && len(entry.models) > 0 {
			entry.backoffUntil = time.Now().Add(catalogBackoff)
			modelCatalogs.entries[key] = entry
			wait.models = entry.models
			wait.err = nil
			close(wait.done)
			modelCatalogs.mu.Unlock()
			return entry.models, nil
		}
		wait.err = errFetch
		close(wait.done)
		modelCatalogs.mu.Unlock()
		return nil, errFetch
	}
	modelCatalogs.entries[key] = catalogEntry{models: models, fetched: time.Now()}
	wait.models = models
	close(wait.done)
	modelCatalogs.mu.Unlock()
	return models, nil
}

func fetchModels(ctx context.Context, apiKey, proxyURL string) ([]pluginapi.ModelInfo, error) {
	endpoint, errURL := providerEndpoint(loadedConfig().APIBase, "models")
	if errURL != nil {
		return nil, errURL
	}
	if ctx == nil {
		ctx = context.Background()
	}
	status, _, body, errDo := doJSON(ctx, upstreamCall{
		Method:   http.MethodGet,
		URL:      endpoint,
		APIKey:   apiKey,
		ProxyURL: proxyURL,
		Timeout:  catalogTimeout,
		MaxBytes: 2 << 20,
	})
	if errDo != nil {
		return nil, errDo
	}
	if status < 200 || status > 299 {
		failure := classifyHTTPStatus(status, body)
		return nil, &upstreamError{failure: failure}
	}
	models, errParse := parseModelsResponse(body)
	if errParse != nil {
		return nil, errParse
	}
	return models, nil
}

func parseModelsResponse(body []byte) ([]pluginapi.ModelInfo, error) {
	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("models response is not valid JSON")
	}
	data := gjson.GetBytes(body, "data")
	if !data.IsArray() {
		return nil, fmt.Errorf("models response data is not an array")
	}
	seen := make(map[string]struct{})
	models := make([]pluginapi.ModelInfo, 0, len(data.Array()))
	for _, item := range data.Array() {
		id := strings.TrimSpace(item.Get("id").String())
		if id == "" {
			continue
		}
		if _, duplicated := seen[id]; duplicated {
			continue
		}
		seen[id] = struct{}{}
		name := strings.TrimSpace(item.Get("name").String())
		if name == "" {
			name = id
		}
		info := pluginapi.ModelInfo{
			ID:                         id,
			Object:                     "model",
			OwnedBy:                    providerIdentifier,
			Type:                       "chat",
			DisplayName:                name,
			Name:                       id,
			SupportedGenerationMethods: []string{"chat"},
			SupportedInputModalities:   []string{"text"},
			SupportedOutputModalities:  []string{"text"},
		}
		if contextLength := item.Get("context_length").Int(); contextLength > 0 {
			info.ContextLength = contextLength
			info.InputTokenLimit = contextLength
		}
		if usesAnthropicMessages(id, nil) {
			info.Description = "Anthropic Messages endpoint"
		}
		models = append(models, info)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("models response contained no usable model ids")
	}
	return models, nil
}

func usesAnthropicMessages(modelID string, catalog []pluginapi.ModelInfo) bool {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return false
	}
	for _, model := range catalog {
		if model.ID != modelID {
			continue
		}
		if strings.Contains(strings.ToLower(model.Description), "anthropic") {
			return true
		}
	}
	return strings.HasPrefix(strings.ToLower(modelID), "claude-")
}

func lookupCatalog(apiKey string) []pluginapi.ModelInfo {
	key := catalogCacheKey(apiKey)
	modelCatalogs.mu.Lock()
	defer modelCatalogs.mu.Unlock()
	if entry, ok := modelCatalogs.entries[key]; ok {
		return entry.models
	}
	return nil
}

func excludedModelsForRequest(host pluginapi.HostConfigSummary, attributes map[string]string, storage []byte) []string {
	if attributes != nil {
		if combined := strings.TrimSpace(attributes["excluded_models"]); combined != "" {
			return strings.Split(combined, ",")
		}
	}
	out := append([]string(nil), hostExcludedModels(host)...)
	return append(out, excludedModelsFromStorage(storage)...)
}

func hostExcludedModels(host pluginapi.HostConfigSummary) []string {
	if len(host.ExcludedModels) == 0 {
		return nil
	}
	if models, ok := host.ExcludedModels[providerIdentifier]; ok {
		return models
	}
	for key, models := range host.ExcludedModels {
		if strings.EqualFold(key, providerIdentifier) {
			return models
		}
	}
	return nil
}

func excludedModelsFromStorage(storage []byte) []string {
	raw := gjson.GetBytes(storage, "excluded_models")
	if !raw.Exists() {
		raw = gjson.GetBytes(storage, "excluded-models")
	}
	if !raw.Exists() || !raw.IsArray() {
		return nil
	}
	out := make([]string, 0, len(raw.Array()))
	for _, item := range raw.Array() {
		if trimmed := strings.TrimSpace(item.String()); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func applyExcludedModels(models []pluginapi.ModelInfo, excluded []string) []pluginapi.ModelInfo {
	if len(models) == 0 || len(excluded) == 0 {
		return models
	}
	patterns := make([]string, 0, len(excluded))
	for _, item := range excluded {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			patterns = append(patterns, strings.ToLower(trimmed))
		}
	}
	if len(patterns) == 0 {
		return models
	}
	filtered := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		id := strings.ToLower(strings.TrimSpace(model.ID))
		blocked := false
		for _, pattern := range patterns {
			if matchExcludedModel(pattern, id) {
				blocked = true
				break
			}
		}
		if !blocked {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

func matchExcludedModel(pattern, value string) bool {
	if pattern == "" {
		return false
	}
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}
	parts := strings.Split(pattern, "*")
	if prefix := parts[0]; prefix != "" {
		if !strings.HasPrefix(value, prefix) {
			return false
		}
		value = value[len(prefix):]
	}
	if suffix := parts[len(parts)-1]; suffix != "" {
		if !strings.HasSuffix(value, suffix) {
			return false
		}
		value = value[:len(value)-len(suffix)]
	}
	for i := 1; i < len(parts)-1; i++ {
		segment := parts[i]
		if segment == "" {
			continue
		}
		idx := strings.Index(value, segment)
		if idx < 0 {
			return false
		}
		value = value[idx+len(segment):]
	}
	return true
}
