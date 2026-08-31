package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
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

	auth, errAuth := credential.authData()
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

// authData renders the credential in the same shape parseAuth already accepts, so a
// logged-in key and a hand-written key are indistinguishable to the rest of the plugin.
func (c loginCredential) authData() (pluginapi.AuthData, error) {
	storage := map[string]any{
		"type":    providerIdentifier,
		"api_key": c.apiKey,
	}
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
	fileName := c.fileName()
	return pluginapi.AuthData{
		Provider:         providerIdentifier,
		ID:               fileName,
		FileName:         fileName,
		Label:            firstNonEmpty(c.email, providerIdentifier),
		StorageJSON:      raw,
		Metadata:         map[string]any{"type": providerIdentifier},
		NextRefreshAfter: refreshDeadline(c.expiresAt),
	}, nil
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
