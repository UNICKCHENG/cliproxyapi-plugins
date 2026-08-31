package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

// apiKeyRefreshInterval keeps the host from re-checking a static API key in a tight loop.
// Dashboard keys carry no expiry the plugin can read, and none of them can be rotated
// programmatically: renewal always means the operator supplying a new key.
const apiKeyRefreshInterval = 365 * 24 * time.Hour

// reloginHint is appended wherever an expired or missing credential needs operator action.
const reloginHint = "run `--" + loginFlagName + "` to mint a new key"

// parseAuth claims auth files that declare the cursor provider and carry an API key.
// Files belonging to any other provider are declined so the host keeps looking.
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
		return errorEnvelope("invalid_auth", "cursor auth file requires a non-empty api_key"), nil
	}
	expiry := expiryFromStorage(req.RawJSON)
	if !expiry.IsZero() && !time.Now().UTC().Before(expiry) {
		return errorEnvelope("invalid_auth", fmt.Sprintf("cursor api key expired at %s; %s",
			expiry.Format(time.RFC3339), reloginHint)), nil
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
		NextRefreshAfter: refreshDeadline(expiry),
	}
	return okEnvelope(pluginapi.AuthParseResponse{Handled: true, Auth: data})
}

// refreshAuth re-validates the stored key without rotating it. Cursor has no refresh token:
// keys minted by the login flow expire (90 days by default) and can only be replaced by
// logging in again, so refresh reports expiry rather than attempting a renewal.
func refreshAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if apiKeyFromStorage(req.StorageJSON) == "" {
		return errorEnvelope("invalid_auth", "cursor auth no longer contains an api_key"), nil
	}
	expiry := expiryFromStorage(req.StorageJSON)
	if !expiry.IsZero() && !time.Now().UTC().Before(expiry) {
		return errorEnvelope("invalid_auth", fmt.Sprintf("cursor api key expired at %s; %s",
			expiry.Format(time.RFC3339), reloginHint)), nil
	}
	next := refreshDeadline(expiry)
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth: pluginapi.AuthData{
			Provider:         providerIdentifier,
			ID:               req.AuthID,
			StorageJSON:      req.StorageJSON,
			Metadata:         req.Metadata,
			Attributes:       req.Attributes,
			NextRefreshAfter: next,
		},
		NextRefreshAfter: next,
	})
}

// loginStart reports that the management/TUI login flow is not wired up. Cursor's sign-in
// completes in a browser on the operator's own machine and mints a key there, so the login
// runs as the `--cursor-login` command rather than as a server-hosted OAuth round trip.
func loginStart() ([]byte, error) {
	return errorEnvelope("unsupported", "cursor login is not available through the management API; "+
		reloginHint+" on the host, or save an API key auth file: "+
		`{"type":"cursor","api_key":"<key>"}`), nil
}

func loginPoll() ([]byte, error) {
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusError,
		Message: "cursor login is not available through the management API; " + reloginHint + " on the host instead",
	})
}

// apiKeyFromStorage reads the credential from the persisted auth JSON, tolerating both the
// documented snake_case field and the camelCase spelling used by the Cursor SDK.
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

// expiryFromStorage reads the expiry recorded by the login flow. Hand-written dashboard keys
// omit it, in which case the plugin cannot know when the key lapses.
func expiryFromStorage(storage []byte) time.Time {
	if len(storage) == 0 || !gjson.ValidBytes(storage) {
		return time.Time{}
	}
	for _, field := range []string{"expires_at", "expiresAt"} {
		value := strings.TrimSpace(gjson.GetBytes(storage, field).String())
		if value == "" {
			continue
		}
		if parsed, errParse := time.Parse(time.RFC3339, value); errParse == nil {
			return parsed.UTC()
		}
	}
	for _, field := range []string{"expires_at_ms", "apiKeyExpiresAtMs"} {
		if millis := gjson.GetBytes(storage, field).Int(); millis > 0 {
			return time.UnixMilli(millis).UTC()
		}
	}
	return time.Time{}
}

// refreshDeadline tells the host when to look at this credential again: at expiry for keys
// that report one, otherwise far in the future because there is nothing to re-check.
func refreshDeadline(expiry time.Time) time.Time {
	if expiry.IsZero() {
		return time.Now().UTC().Add(apiKeyRefreshInterval)
	}
	return expiry
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
		return "", fmt.Errorf("cursor auth does not contain an api_key")
	}
	return apiKey, nil
}
