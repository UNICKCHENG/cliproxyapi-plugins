package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	sdkv1 "github.com/UNICKCHENG/cliproxyapi-plugins/auth-cursor/go/internal/sdk/v1"
)

// discoveredCatalog remembers the last successful per-credential discovery so that static
// registration, which runs without a credential, can still describe the provider.
var discoveredCatalog atomic.Value

// staticModels advertises the provider before any credential is consulted. The executor is
// bound to its provider through the executor identifier rather than this list, so an empty
// response only leaves the provider without registered models until the first per-credential
// discovery succeeds. Cursor's catalog is never hard-coded here.
func staticModels() ([]byte, error) {
	var models []pluginapi.ModelInfo
	if cached, ok := discoveredCatalog.Load().([]pluginapi.ModelInfo); ok {
		models = cached
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: providerIdentifier, Models: models})
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
	return okEnvelope(pluginapi.ModelResponse{Provider: providerIdentifier, Models: models})
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
