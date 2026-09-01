package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"

	sdkv1 "github.com/UNICKCHENG/cliproxyapi-plugins/auth-cursor/go/internal/sdk/v1"
)

// discoveredCatalog remembers the last successful per-credential discovery so that static
// registration, which runs without a credential, can still describe the provider.
var discoveredCatalog atomic.Value

// staticModels advertises the provider before any credential is consulted. The executor is
// bound to its provider through the executor identifier rather than this list, so an empty
// response only leaves the provider without registered models until the first per-credential
// discovery succeeds. Cursor's catalog is never hard-coded here.
func staticModels(raw []byte) ([]byte, error) {
	var req pluginapi.StaticModelRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}
	var models []pluginapi.ModelInfo
	if cached, ok := discoveredCatalog.Load().([]pluginapi.ModelInfo); ok {
		models = cached
	}
	return okEnvelope(pluginapi.ModelResponse{
		Provider: providerIdentifier,
		Models:   applyExcludedModels(models, excludedModelsForRequest(req.Host, nil, nil)),
	})
}

// modelsForAuth discovers the catalog visible to one credential. Which models a key can
// reach depends on the account plan and team policy, so this must be asked per auth rather
// than hard-coded.
func modelsForAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	apiKey, errKey := requireAPIKey(req.StorageJSON)
	if errKey != nil {
		return okEnvelope(pluginapi.ModelResponse{Provider: providerIdentifier})
	}

	models, errDiscover := discoverModels(apiKey, resolveProxyURL(req.StorageJSON))
	if errDiscover != nil || len(models) == 0 {
		// The credential keeps no models until a later discovery succeeds, which is what the
		// host reads as "this credential currently serves nothing".
		reason := "empty catalog"
		if errDiscover != nil {
			reason = errDiscover.Error()
		}
		hostLog("warn", "cursor model discovery failed", map[string]any{
			"auth_id": req.AuthID,
			"reason":  reason,
		})
	} else {
		discoveredCatalog.Store(models)
		hostLog("debug", "cursor model discovery succeeded", map[string]any{
			"auth_id": req.AuthID,
			"models":  len(models),
		})
	}
	return okEnvelope(pluginapi.ModelResponse{
		Provider: providerIdentifier,
		Models:   applyExcludedModels(models, excludedModelsForRequest(req.Host, req.Attributes, req.StorageJSON)),
	})
}

// discoverModels asks Cursor which models one credential can reach. Discovery forces a catalog
// refresh: this is the path whose whole purpose is to report the current list, and the cache it
// fills is what keeps model resolution off the hot path for subsequent requests.
func discoverModels(apiKey, proxyURL string) ([]pluginapi.ModelInfo, error) {
	process, errProcess := acquireBridge(proxyURL)
	if errProcess != nil {
		return nil, errProcess
	}
	catalog, errCatalog := catalogFor(context.Background(), process, apiKey, true)
	if errCatalog != nil {
		return nil, errCatalog
	}
	models := make([]pluginapi.ModelInfo, 0, len(catalog))
	for _, model := range catalog {
		if info, ok := modelInfoFromCatalog(model); ok {
			models = append(models, info)
		}
	}
	return models, nil
}

func modelInfoFromCatalog(model *sdkv1.SdkModel) (pluginapi.ModelInfo, bool) {
	id := strings.TrimSpace(model.GetId())
	if id == "" {
		return pluginapi.ModelInfo{}, false
	}
	displayName := strings.TrimSpace(model.GetDisplayName())
	if displayName == "" {
		displayName = id
	}
	parameters := make([]string, 0, len(model.GetParameters()))
	for _, parameter := range model.GetParameters() {
		if parameterID := strings.TrimSpace(parameter.GetId()); parameterID != "" {
			parameters = append(parameters, parameterID)
		}
	}
	return pluginapi.ModelInfo{
		ID:                         id,
		Object:                     "model",
		OwnedBy:                    providerIdentifier,
		Type:                       "chat",
		DisplayName:                displayName,
		Name:                       id,
		Description:                strings.TrimSpace(model.GetDescription()),
		SupportedGenerationMethods: []string{"chat"},
		SupportedInputModalities:   []string{"text", "image"},
		SupportedOutputModalities:  []string{"text"},
		SupportedParameters:        parameters,
	}, true
}

// excludedModelsForRequest collects hide-patterns the host already merged into attributes,
// otherwise the global oauth-excluded-models list plus any per-account excluded-models in
// the auth file. The catalog is filtered here because /v1/models is this response.
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

// matchExcludedModel is the host's oauth-excluded-models matcher: case-insensitive,
// with '*' matching any substring.
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
