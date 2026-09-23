package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

const (
	apiKeyRefreshInterval = 365 * 24 * time.Hour
	newKeyHint            = "run `--" + loginFlagName + "` to import a new Command Code API key"
	weightAttribute       = "weight"
)

func parseAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if !gjson.ValidBytes(req.RawJSON) {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	authType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(req.RawJSON, "type").String()))
	if authType != providerIdentifier {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	apiKey := apiKeyFromStorage(req.RawJSON)
	if apiKey == "" {
		return errorEnvelope("invalid_auth", "commandcode auth file requires a non-empty api_key"), nil
	}

	data := pluginapi.AuthData{
		Provider:         providerIdentifier,
		FileName:         strings.TrimSpace(req.FileName),
		Label:            authLabel(req.RawJSON, req.FileName),
		Prefix:           strings.TrimSpace(gjson.GetBytes(req.RawJSON, "prefix").String()),
		ProxyURL:         strings.TrimSpace(gjson.GetBytes(req.RawJSON, "proxy_url").String()),
		Disabled:         gjson.GetBytes(req.RawJSON, "disabled").Bool(),
		StorageJSON:      req.RawJSON,
		Metadata:         map[string]any{"type": providerIdentifier},
		NextRefreshAfter: time.Now().UTC().Add(apiKeyRefreshInterval),
	}
	applyConfiguredWeight(&data, req.RawJSON, req.FileName)
	return okEnvelope(pluginapi.AuthParseResponse{Handled: true, Auth: data})
}

func refreshAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if apiKeyFromStorage(req.StorageJSON) == "" {
		return errorEnvelope("invalid_auth", "commandcode auth no longer contains an api_key"), nil
	}
	next := time.Now().UTC().Add(apiKeyRefreshInterval)
	data := pluginapi.AuthData{
		Provider:         providerIdentifier,
		ID:               req.AuthID,
		StorageJSON:      req.StorageJSON,
		Metadata:         req.Metadata,
		Attributes:       attributesWithoutWeight(req.Attributes),
		NextRefreshAfter: next,
	}
	applyConfiguredWeight(&data, req.StorageJSON, req.AuthID)
	return okEnvelope(pluginapi.AuthRefreshResponse{Auth: data, NextRefreshAfter: next})
}

func applyConfiguredWeight(data *pluginapi.AuthData, storage []byte, fileName string) {
	if data == nil {
		return
	}
	weight, configured := configuredWeight(storage, fileName)
	if !configured {
		return
	}
	if data.Attributes == nil {
		data.Attributes = make(map[string]string)
	}
	data.Attributes[weightAttribute] = strconv.Itoa(weight)
}

func configuredWeight(storage []byte, fileName string) (int, bool) {
	weights := loadedConfig().Weights
	if len(weights) == 0 {
		return 0, false
	}
	for _, key := range weightLookupKeys(storage, fileName) {
		if weight, ok := weights[key]; ok {
			return int(weight), true
		}
	}
	return 0, false
}

func weightLookupKeys(storage []byte, fileName string) []string {
	keys := make([]string, 0, 4)
	add := func(value string) {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if normalized == "" {
			return
		}
		for _, existing := range keys {
			if existing == normalized {
				return
			}
		}
		keys = append(keys, normalized)
	}
	if len(storage) > 0 && gjson.ValidBytes(storage) {
		add(gjson.GetBytes(storage, "label").String())
		add(gjson.GetBytes(storage, "email").String())
	}
	name := strings.ToLower(strings.TrimSpace(fileName))
	add(name)
	add(strings.TrimSuffix(name, ".json"))
	return keys
}

func attributesWithoutWeight(attributes map[string]string) map[string]string {
	if len(attributes) == 0 {
		return nil
	}
	out := make(map[string]string, len(attributes))
	for key, value := range attributes {
		if key == weightAttribute {
			continue
		}
		out[key] = value
	}
	return out
}

func loginStart() ([]byte, error) {
	return errorEnvelope("unsupported", "commandcode has no interactive login; "+
		newKeyHint+" on the host, or save an API key auth file: "+
		`{"type":"commandcode","api_key":"<key>"}`), nil
}

func loginPoll() ([]byte, error) {
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusError,
		Message: "commandcode has no interactive login; " + newKeyHint + " on the host instead",
	})
}

func resolveProxyURL(storage []byte) string {
	if len(storage) > 0 && gjson.ValidBytes(storage) {
		if value := strings.TrimSpace(gjson.GetBytes(storage, "proxy_url").String()); value != "" {
			return value
		}
	}
	return loadedConfig().ProxyURL
}

func apiKeyFromStorage(storage []byte) string {
	if len(storage) == 0 || !gjson.ValidBytes(storage) {
		return ""
	}
	for _, field := range []string{"api_key", "apiKey"} {
		if value := strings.TrimSpace(gjson.GetBytes(storage, field).String()); value != "" {
			return value
		}
	}
	return ""
}

func authLabel(storage []byte, fileName string) string {
	for _, field := range []string{"label", "email"} {
		if value := strings.TrimSpace(gjson.GetBytes(storage, field).String()); value != "" {
			return value
		}
	}
	if trimmed := strings.TrimSpace(fileName); trimmed != "" {
		return strings.TrimSuffix(trimmed, ".json")
	}
	return providerIdentifier
}

func requireAPIKey(storage []byte) (string, error) {
	apiKey := apiKeyFromStorage(storage)
	if apiKey == "" {
		return "", fmt.Errorf("commandcode auth does not contain an api_key")
	}
	return apiKey, nil
}
