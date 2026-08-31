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

	models, errDiscover := discoverModels(apiKey)
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

type sidecarError string

func (e sidecarError) Error() string { return string(e) }

func errSidecar(message string) error { return sidecarError(message) }
