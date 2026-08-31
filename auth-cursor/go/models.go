package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// discoveredCatalog remembers the last successful per-credential discovery so that static
// registration, which runs without a credential, can still describe the provider.
var discoveredCatalog atomic.Value

// staticModels advertises the provider before any credential is consulted. The host binds a
// plugin executor to its provider through this list, so returning nothing would leave the
// provider unroutable. Configured ids bootstrap the first run; afterwards the last successful
// discovery keeps the list accurate without hard-coding Cursor's catalog.
func staticModels() ([]byte, error) {
	models := modelsFromIDs(loadedConfig().Models)
	if len(models) == 0 {
		if cached, ok := discoveredCatalog.Load().([]pluginapi.ModelInfo); ok {
			models = cached
		}
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
	cfg := loadedConfig()
	apiKey, errKey := requireAPIKey(req.StorageJSON)
	if errKey != nil {
		return okEnvelope(pluginapi.ModelResponse{
			Provider: providerIdentifier,
			Models:   modelsFromIDs(cfg.Models),
		})
	}

	models, errDiscover := discoverModels(apiKey)
	if errDiscover != nil || len(models) == 0 {
		// Discovery is best effort: a transient catalog failure should not drop the
		// provider's models out of the registry entirely.
		reason := "empty catalog"
		if errDiscover != nil {
			reason = errDiscover.Error()
		}
		models = modelsFromIDs(cfg.Models)
		hostLog("warn", "cursor model discovery failed, using configured fallback models", map[string]any{
			"auth_id":  req.AuthID,
			"reason":   reason,
			"fallback": len(models),
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

func discoverModels(apiKey string) ([]pluginapi.ModelInfo, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, release, errCall := callSidecar(ctx, sidecarRequest{Op: "models", APIKey: apiKey})
	if errCall != nil {
		return nil, errCall
	}
	defer release()

	for event := range events {
		switch event.Event {
		case "models":
			models := make([]pluginapi.ModelInfo, 0, len(event.Models))
			for _, model := range event.Models {
				if info, ok := modelInfoFromCatalog(model); ok {
					models = append(models, info)
				}
			}
			return models, nil
		case "error":
			return nil, errSidecar(event.errorText())
		}
	}
	return nil, errSidecar("cursor sidecar closed the stream before returning models")
}

func modelInfoFromCatalog(model sidecarModel) (pluginapi.ModelInfo, bool) {
	id := strings.TrimSpace(model.ID)
	if id == "" {
		return pluginapi.ModelInfo{}, false
	}
	displayName := strings.TrimSpace(model.DisplayName)
	if displayName == "" {
		displayName = id
	}
	return pluginapi.ModelInfo{
		ID:                         id,
		Object:                     "model",
		OwnedBy:                    providerIdentifier,
		Type:                       "chat",
		DisplayName:                displayName,
		Name:                       id,
		Description:                strings.TrimSpace(model.Description),
		SupportedGenerationMethods: []string{"chat"},
		SupportedInputModalities:   []string{"text", "image"},
		SupportedOutputModalities:  []string{"text"},
		SupportedParameters:        model.Parameters,
	}, true
}

func modelsFromIDs(ids []string) []pluginapi.ModelInfo {
	models := make([]pluginapi.ModelInfo, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		models = append(models, pluginapi.ModelInfo{
			ID:                         id,
			Object:                     "model",
			OwnedBy:                    providerIdentifier,
			Type:                       "chat",
			DisplayName:                id,
			Name:                       id,
			SupportedGenerationMethods: []string{"chat"},
			SupportedInputModalities:   []string{"text", "image"},
			SupportedOutputModalities:  []string{"text"},
			UserDefined:                true,
		})
	}
	return models
}

type sidecarError string

func (e sidecarError) Error() string { return string(e) }

func errSidecar(message string) error { return sidecarError(message) }
