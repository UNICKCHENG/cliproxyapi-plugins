package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

// loginFlagName is the host-facing flag that triggers the Cursor browser login.
const loginFlagName = "cursor-login"

// loginDeadline bounds the wait for the operator to finish the browser sign-in. Timeouts are
// permitted here because this is credential acquisition, not an established upstream call.
const loginDeadline = 10 * time.Minute

// commandLineRegister publishes the plugin-owned flags so they appear in -help and can be
// parsed by the host. Declaring the flag here keeps it tied to the plugin's lifetime: remove
// the plugin and the flag disappears with it.
func commandLineRegister() ([]byte, error) {
	return okEnvelope(pluginapi.CommandLineRegistrationResponse{
		Flags: []pluginapi.CommandLineFlag{{
			Name:  loginFlagName,
			Usage: "Login to Cursor in a browser and save the minted API key as an auth file",
			Type:  "bool",
		}},
	})
}

// commandLineExecute runs the browser login and hands the resulting credential to the host,
// which persists it into the configured auth directory.
func commandLineExecute(raw []byte) ([]byte, error) {
	var req pluginapi.CommandLineExecutionRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if !req.TriggeredFlags[loginFlagName].Set {
		return okEnvelope(pluginapi.CommandLineExecutionResponse{})
	}

	// -no-browser is a host flag, so read it from the full flag set rather than declaring a
	// duplicate. Headless hosts still complete the login through the printed URL.
	noBrowser := strings.EqualFold(strings.TrimSpace(req.Flags["no-browser"].Value), "true")

	credential, errLogin := runLogin(noBrowser)
	if errLogin != nil {
		return okEnvelope(pluginapi.CommandLineExecutionResponse{
			Stderr:   []byte(fmt.Sprintf("cursor login failed: %v\n", errLogin)),
			ExitCode: 1,
		})
	}

	auth, errAuth := credential.authData(req.Host.AuthDir)
	if errAuth != nil {
		return okEnvelope(pluginapi.CommandLineExecutionResponse{
			Stderr:   []byte(fmt.Sprintf("cursor login succeeded but the credential could not be prepared: %v\n", errAuth)),
			ExitCode: 1,
		})
	}
	return okEnvelope(pluginapi.CommandLineExecutionResponse{
		Stdout: []byte(credential.summary()),
		Auths:  []pluginapi.AuthData{auth},
	})
}

// loginCredential is the outcome of a completed browser login.
type loginCredential struct {
	apiKey    string
	email     string
	expiresAt time.Time
}

// runLogin drives the sidecar login flow, printing the sign-in URL as soon as the SDK
// reports it so the operator can complete the flow.
func runLogin(noBrowser bool) (loginCredential, error) {
	ctx, cancel := context.WithTimeout(context.Background(), loginDeadline)
	defer cancel()

	events, release, errCall := callSidecar(ctx, sidecarRequest{
		Op:         "login",
		NoBrowser:  noBrowser,
		APIKeyName: "CLIProxyAPI",
	})
	if errCall != nil {
		return loginCredential{}, errCall
	}
	defer release()

	for event := range events {
		switch event.Event {
		case "login_url":
			// The host only prints the response streams after the command returns, so the
			// URL has to reach the terminal directly to be usable.
			fmt.Fprintf(os.Stderr, "\nComplete the Cursor login in your browser:\n\n  %s\n\nWaiting for the login to finish...\n", event.URL)
		case "login":
			if strings.TrimSpace(event.APIKey) == "" {
				return loginCredential{}, fmt.Errorf("cursor login returned an empty api key")
			}
			credential := loginCredential{
				apiKey: strings.TrimSpace(event.APIKey),
				email:  strings.TrimSpace(event.Email),
			}
			if event.ExpiresAt > 0 {
				// Truncated to the second so the scheduled refresh matches the RFC3339
				// value persisted in the auth file, which is re-read on the next startup.
				credential.expiresAt = time.UnixMilli(event.ExpiresAt).UTC().Truncate(time.Second)
			}
			return credential, nil
		case "error":
			return loginCredential{}, fmt.Errorf("%s", event.errorText())
		}
	}
	if ctx.Err() != nil {
		return loginCredential{}, fmt.Errorf("cursor login was not completed within %s", loginDeadline)
	}
	return loginCredential{}, fmt.Errorf("cursor sidecar closed the stream before the login completed")
}

// credentialFields are the fields a login owns. Every spelling is cleared before the new
// values are written, so a stale key or expiry left in the previous file can never outrank
// the credential that was just minted.
var credentialFields = []string{
	"type", "email",
	"api_key", "apiKey",
	"expires_at", "expiresAt", "expires_at_ms", "apiKeyExpiresAtMs",
}

// authData renders the credential in the same shape parseAuth already accepts, so a
// logged-in key and a hand-written key are indistinguishable to the rest of the plugin.
// The host replaces the auth file wholesale, so operator-owned settings already recorded for
// this account are carried over rather than lost on every renewal.
func (c loginCredential) authData(authDir string) (pluginapi.AuthData, error) {
	fileName := c.fileName()
	storage := existingAuthStorage(authDir, fileName)
	for _, field := range credentialFields {
		delete(storage, field)
	}
	storage["type"] = providerIdentifier
	storage["api_key"] = c.apiKey
	if c.email != "" {
		storage["email"] = c.email
	}
	if !c.expiresAt.IsZero() {
		storage["expires_at"] = c.expiresAt.Format(time.RFC3339)
	}
	raw, errMarshal := json.Marshal(storage)
	if errMarshal != nil {
		return pluginapi.AuthData{}, errMarshal
	}
	return pluginapi.AuthData{
		Provider: providerIdentifier,
		ID:       fileName,
		FileName: fileName,
		Label:    authLabel(raw, fileName),
		Prefix:   strings.TrimSpace(gjson.GetBytes(raw, "prefix").String()),
		ProxyURL: strings.TrimSpace(gjson.GetBytes(raw, "proxy_url").String()),
		// Read back rather than defaulted: the host writes this flag into the file from the
		// record it persists, so a preserved "disabled" would be reset by a false here.
		Disabled:         gjson.GetBytes(raw, "disabled").Bool(),
		StorageJSON:      raw,
		Metadata:         map[string]any{"type": providerIdentifier},
		NextRefreshAfter: refreshDeadline(c.expiresAt),
	}, nil
}

// existingAuthStorage reads the auth file this login is about to replace. A missing,
// unreadable or malformed file simply yields nothing to carry over.
func existingAuthStorage(authDir, fileName string) map[string]any {
	authDir = strings.TrimSpace(authDir)
	if authDir == "" || fileName == "" {
		return make(map[string]any)
	}
	raw, errRead := os.ReadFile(filepath.Join(authDir, fileName))
	if errRead != nil || len(raw) == 0 {
		return make(map[string]any)
	}
	var existing map[string]any
	if errUnmarshal := json.Unmarshal(raw, &existing); errUnmarshal != nil || existing == nil {
		return make(map[string]any)
	}
	return existing
}

// fileName derives a stable per-account file name so logging in again with the same account
// replaces its credential instead of accumulating duplicates.
func (c loginCredential) fileName() string {
	account := sanitizeFileComponent(c.email)
	if account == "" {
		return fmt.Sprintf("%s-%d.json", providerIdentifier, time.Now().UTC().Unix())
	}
	return fmt.Sprintf("%s-%s.json", providerIdentifier, account)
}

func (c loginCredential) summary() string {
	var builder strings.Builder
	builder.WriteString("Cursor login successful")
	if c.email != "" {
		builder.WriteString(" for " + c.email)
	}
	builder.WriteString(".\n")
	if !c.expiresAt.IsZero() {
		builder.WriteString(fmt.Sprintf("The minted API key expires at %s; run --%s again to renew it.\n",
			c.expiresAt.Format(time.RFC3339), loginFlagName))
	}
	return builder.String()
}

// sanitizeFileComponent reduces an email to a safe file name fragment.
func sanitizeFileComponent(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9':
			builder.WriteRune(char)
		case char == '-', char == '_', char == '.':
			builder.WriteRune(char)
		case char == '@', char == '+':
			builder.WriteRune('-')
		}
	}
	return strings.Trim(builder.String(), "-._")
}
