package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"

	sdkv1 "github.com/UNICKCHENG/cliproxyapi-plugins/auth-cursor/go/internal/sdk/v1"
)

const (
	// loginFlagName is the host-facing flag that imports a Cursor API key.
	loginFlagName = "cursor-login"
	// apiKeyFlagName supplies the key non-interactively, for scripts and CI.
	apiKeyFlagName = "cursor-api-key"
)

// loginValidateTimeout bounds the Me call that checks the key. A timeout is acceptable here
// because this is credential acquisition, not an established upstream call.
const loginValidateTimeout = 60 * time.Second

// commandLineRegister publishes the plugin-owned flags so they appear in -help and can be
// parsed by the host. Declaring the flags here keeps them tied to the plugin's lifetime: remove
// the plugin and they disappear with it.
func commandLineRegister() ([]byte, error) {
	return okEnvelope(pluginapi.CommandLineRegistrationResponse{
		Flags: []pluginapi.CommandLineFlag{
			{
				Name:  loginFlagName,
				Usage: "Import a Cursor API key as an auth file, prompting for it unless -" + apiKeyFlagName + " is given",
				Type:  "bool",
			},
			{
				Name:  apiKeyFlagName,
				Usage: "Cursor API key to import, for non-interactive use. Prefer the prompt: an argument is visible in the process list",
				Type:  "string",
			},
		},
	})
}

// commandLineExecute imports a Cursor API key and hands the resulting credential to the host,
// which persists it into the configured auth directory.
//
// There is no interactive sign-in to drive: sdk.v1 exposes no login RPC, and a Cursor credential
// is a key the operator creates in the dashboard. So the command's job is to accept that key,
// prove it works, and record which account it belongs to.
func commandLineExecute(raw []byte) ([]byte, error) {
	var req pluginapi.CommandLineExecutionRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if !req.TriggeredFlags[loginFlagName].Set && !req.TriggeredFlags[apiKeyFlagName].Set {
		return okEnvelope(pluginapi.CommandLineExecutionResponse{})
	}

	apiKey, errKey := readLoginAPIKey(req.TriggeredFlags[apiKeyFlagName].Value)
	if errKey != nil {
		return okEnvelope(pluginapi.CommandLineExecutionResponse{
			Stderr:   []byte(fmt.Sprintf("cursor login failed: %v\n", errKey)),
			ExitCode: 1,
		})
	}

	credential, errLogin := importAPIKey(apiKey)
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

// readLoginAPIKey resolves the key from the flag, otherwise by prompting on the terminal.
//
// The environment is deliberately not consulted: a credential that can be picked up from the
// ambient environment is one an operator cannot see the plugin using.
func readLoginAPIKey(flagValue string) (string, error) {
	if key := strings.TrimSpace(flagValue); key != "" {
		return key, nil
	}
	if !stdinIsTerminal() {
		return "", fmt.Errorf("no terminal to prompt on; pass the key with -%s", apiKeyFlagName)
	}
	// The host only prints the response streams after the command returns, so the prompt has to
	// reach the terminal directly to appear before the read.
	fmt.Fprint(os.Stderr, "Paste your Cursor API key (https://cursor.com/dashboard): ")
	key, errRead := readAPIKeyFrom(os.Stdin)
	fmt.Fprintln(os.Stderr)
	return key, errRead
}

// readAPIKeyFrom takes one line and trims it, because a pasted key arrives with the newline that
// submitted it and often with whitespace from the clipboard.
func readAPIKeyFrom(source io.Reader) (string, error) {
	key, errRead := bufio.NewReader(source).ReadString('\n')
	if errRead != nil && !errors.Is(errRead, io.EOF) {
		return "", fmt.Errorf("read the api key: %w", errRead)
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", errors.New("no api key was entered")
	}
	return key, nil
}

func stdinIsTerminal() bool {
	info, errStat := os.Stdin.Stat()
	if errStat != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// loginCredential is a validated Cursor API key together with the account it belongs to.
type loginCredential struct {
	apiKey string
	email  string
}

// importAPIKey proves the key works and reads back the account it authenticates as.
//
// The Me round trip is what makes the import worth having over hand-writing the auth file: it
// rejects a mistyped key immediately instead of at the first request, and the email it returns is
// what names the auth file, so logging in again replaces the credential for that account.
func importAPIKey(apiKey string) (loginCredential, error) {
	process, errProcess := acquireBridge(loadedConfig().ProxyURL)
	if errProcess != nil {
		return loginCredential{}, errProcess
	}
	ctx, cancel := context.WithTimeout(context.Background(), loginValidateTimeout)
	defer cancel()
	response, errMe := process.cursor.Me(ctx, connect.NewRequest(&sdkv1.MeRequest{
		Options: &sdkv1.CursorRequestOptions{ApiKey: apiKey},
	}))
	if errMe != nil {
		return loginCredential{}, errors.New(failureFrom(errMe).Message)
	}
	return loginCredential{
		apiKey: apiKey,
		email:  strings.TrimSpace(response.Msg.GetUser().GetUserEmail()),
	}, nil
}

// credentialFields are the fields an import owns. Every spelling is cleared before the new
// values are written, so a stale key or an expiry left behind by an earlier login flow can never
// outrank the credential that was just imported.
var credentialFields = []string{
	"type", "email",
	"api_key", "apiKey",
	"expires_at", "expiresAt", "expires_at_ms", "apiKeyExpiresAtMs",
}

// authData renders the credential in the same shape parseAuth already accepts, so an imported key
// and a hand-written key are indistinguishable to the rest of the plugin.
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
		Disabled:    gjson.GetBytes(raw, "disabled").Bool(),
		StorageJSON: raw,
		Metadata:    map[string]any{"type": providerIdentifier},
		// A Cursor API key reports no expiry, so there is nothing for the host to re-check.
		NextRefreshAfter: refreshDeadline(time.Time{}),
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

// fileName derives a stable per-account file name so importing a key for the same account
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
	builder.WriteString("Cursor API key accepted")
	if c.email != "" {
		builder.WriteString(" for " + c.email)
	}
	builder.WriteString(".\n")
	builder.WriteString(fmt.Sprintf("Saved as %s. Revoke or rotate the key in the Cursor dashboard, then run --%s again.\n",
		c.fileName(), loginFlagName))
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
