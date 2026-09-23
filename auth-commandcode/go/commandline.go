package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"golang.org/x/term"
)

const (
	loginFlagName         = "commandcode-login"
	apiKeyFlagName        = "commandcode-api-key"
	skipValidateFlagName  = "commandcode-skip-validate"
	loginValidateTimeout  = 60 * time.Second
	credentialDigestChars = 16
)

func commandLineRegister() ([]byte, error) {
	return okEnvelope(pluginapi.CommandLineRegistrationResponse{
		Flags: []pluginapi.CommandLineFlag{
			{
				Name:  loginFlagName,
				Usage: "Import a Command Code API key as an auth file, prompting for it unless -" + apiKeyFlagName + " is given",
				Type:  "bool",
			},
			{
				Name:  apiKeyFlagName,
				Usage: "Command Code API key to import, for non-interactive use. Prefer the prompt: an argument is visible in the process list",
				Type:  "string",
			},
			{
				Name:  skipValidateFlagName,
				Usage: "Skip the documented /provider/v1/models reachability check when importing a key",
				Type:  "bool",
			},
		},
	})
}

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
			Stderr:   []byte(fmt.Sprintf("commandcode login failed: %v\n", errKey)),
			ExitCode: 1,
		})
	}

	skipValidate := req.TriggeredFlags[skipValidateFlagName].Set &&
		parseBoolFlag(req.TriggeredFlags[skipValidateFlagName].Value)
	if !skipValidate {
		if errValidate := validateAPIKey(apiKey); errValidate != nil {
			return okEnvelope(pluginapi.CommandLineExecutionResponse{
				Stderr:   []byte(fmt.Sprintf("commandcode login failed: %v\n", errValidate)),
				ExitCode: 1,
			})
		}
	}

	auth, errAuth := importedAuthData(apiKey)
	if errAuth != nil {
		return okEnvelope(pluginapi.CommandLineExecutionResponse{
			Stderr:   []byte(fmt.Sprintf("commandcode login succeeded but the credential could not be prepared: %v\n", errAuth)),
			ExitCode: 1,
		})
	}
	return okEnvelope(pluginapi.CommandLineExecutionResponse{
		Stdout: []byte(loginSummary(auth.FileName)),
		Auths:  []pluginapi.AuthData{auth},
	})
}

func parseBoolFlag(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "1", "true", "t", "yes", "y":
		return true
	default:
		return false
	}
}

func readLoginAPIKey(flagValue string) (string, error) {
	if key := strings.TrimSpace(flagValue); key != "" {
		return key, nil
	}
	if !stdinIsTerminal() {
		return "", fmt.Errorf("no terminal to prompt on; pass the key with -%s", apiKeyFlagName)
	}
	fmt.Fprint(os.Stderr, "Paste your Command Code API key (https://commandcode.ai/studio): ")
	key, errRead := readHiddenAPIKey()
	fmt.Fprintln(os.Stderr)
	return key, errRead
}

func readHiddenAPIKey() (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		raw, errRead := term.ReadPassword(fd)
		if errRead != nil {
			return "", fmt.Errorf("read the api key: %w", errRead)
		}
		key := strings.TrimSpace(string(raw))
		if key == "" {
			return "", errors.New("no api key was entered")
		}
		return key, nil
	}
	return readAPIKeyFrom(os.Stdin)
}

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
	return term.IsTerminal(int(os.Stdin.Fd()))
}

func validateAPIKey(apiKey string) error {
	ctx, cancel := context.WithTimeout(context.Background(), loginValidateTimeout)
	defer cancel()
	_, errCatalog := fetchModels(ctx, apiKey, loadedConfig().ProxyURL)
	if errCatalog == nil {
		return nil
	}
	failure := failureFrom(errCatalog)
	if failure.HTTPStatus == 401 {
		return errors.New("invalid Command Code API key")
	}
	if failure.HTTPStatus == 403 {
		return errors.New("Command Code rejected the key (upgrade_required or no Provider API access)")
	}
	return fmt.Errorf("could not reach the Command Code API: %s", failure.Message)
}

var credentialFields = []string{
	"type",
	"api_key", "apiKey",
}

func importedAuthData(apiKey string) (pluginapi.AuthData, error) {
	fileName := credentialFileName(apiKey)
	storage := existingAuthStorage(fileName)
	if matched := existingAuthStorageForKey(apiKey); matched != nil {
		if name := strings.TrimSpace(gjson.GetBytes(mustMarshal(matched), "_file_name").String()); name != "" {
			fileName = name
		}
		delete(matched, "_file_name")
		storage = matched
	}
	for _, field := range credentialFields {
		delete(storage, field)
	}
	storage["type"] = providerIdentifier
	storage["api_key"] = apiKey
	raw, errMarshal := json.Marshal(storage)
	if errMarshal != nil {
		return pluginapi.AuthData{}, errMarshal
	}
	return pluginapi.AuthData{
		Provider:         providerIdentifier,
		ID:               fileName,
		FileName:         fileName,
		Label:            authLabel(raw, fileName),
		Prefix:           strings.TrimSpace(gjson.GetBytes(raw, "prefix").String()),
		ProxyURL:         strings.TrimSpace(gjson.GetBytes(raw, "proxy_url").String()),
		Disabled:         gjson.GetBytes(raw, "disabled").Bool(),
		StorageJSON:      raw,
		Metadata:         map[string]any{"type": providerIdentifier},
		NextRefreshAfter: time.Now().UTC().Add(apiKeyRefreshInterval),
	}, nil
}

func credentialFileName(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return fmt.Sprintf("%s-%s.json", providerIdentifier, hex.EncodeToString(sum[:])[:credentialDigestChars])
}

func loginSummary(fileName string) string {
	return fmt.Sprintf("Command Code API key accepted.\nSaved as %s. Revoke or rotate the key in Studio, then run --%s again.\n",
		fileName, loginFlagName)
}

func existingAuthStorage(fileName string) map[string]any {
	entries, errList := listHostAuths()
	if errList != nil {
		return make(map[string]any)
	}
	for _, entry := range entries {
		if !strings.EqualFold(strings.TrimSpace(entry.Name), fileName) &&
			!strings.EqualFold(strings.TrimSpace(entry.ID), fileName) {
			continue
		}
		if !isCommandCodeAuth(entry) {
			continue
		}
		raw, errGet := getHostAuthJSON(entry)
		if errGet != nil || len(raw) == 0 {
			continue
		}
		var existing map[string]any
		if errUnmarshal := json.Unmarshal(raw, &existing); errUnmarshal != nil || existing == nil {
			return make(map[string]any)
		}
		return existing
	}
	return make(map[string]any)
}

func existingAuthStorageForKey(apiKey string) map[string]any {
	entries, errList := listHostAuths()
	if errList != nil {
		return nil
	}
	for _, entry := range entries {
		if !isCommandCodeAuth(entry) {
			continue
		}
		raw, errGet := getHostAuthJSON(entry)
		if errGet != nil {
			hostLog("warn", "commandcode skipped unreadable credential during import", map[string]any{
				"auth_name": strings.TrimSpace(entry.Name),
			})
			continue
		}
		if apiKeyFromStorage(raw) != apiKey {
			continue
		}
		var existing map[string]any
		if errUnmarshal := json.Unmarshal(raw, &existing); errUnmarshal != nil || existing == nil {
			hostLog("warn", "commandcode skipped invalid credential during import", map[string]any{
				"auth_name": strings.TrimSpace(entry.Name),
			})
			continue
		}
		if name := strings.TrimSpace(entry.Name); name != "" {
			existing["_file_name"] = name
		}
		return existing
	}
	return nil
}

func isCommandCodeAuth(entry pluginapi.HostAuthFileEntry) bool {
	provider := strings.ToLower(strings.TrimSpace(entry.Provider))
	if provider == "" {
		provider = strings.ToLower(strings.TrimSpace(entry.Type))
	}
	return provider == providerIdentifier
}

func mustMarshal(value map[string]any) []byte {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil
	}
	return raw
}

var listHostAuths = liveListHostAuths
var getHostAuthJSON = liveGetHostAuthJSON

func liveListHostAuths() ([]pluginapi.HostAuthFileEntry, error) {
	raw, errCall := callHost(pluginabi.MethodHostAuthList, map[string]any{})
	if errCall != nil {
		return nil, errCall
	}
	var resp struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if errDecode := json.Unmarshal(raw, &resp); errDecode != nil {
		return nil, fmt.Errorf("decode host auth list: %w", errDecode)
	}
	return resp.Files, nil
}

func liveGetHostAuthJSON(entry pluginapi.HostAuthFileEntry) ([]byte, error) {
	index := strings.TrimSpace(entry.AuthIndex)
	if index == "" {
		return nil, fmt.Errorf("auth_index is required")
	}
	raw, errCall := callHost(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: index})
	if errCall != nil {
		return nil, errCall
	}
	var resp pluginapi.HostAuthGetResponse
	if errDecode := json.Unmarshal(raw, &resp); errDecode != nil {
		return nil, fmt.Errorf("decode host auth get: %w", errDecode)
	}
	return append([]byte(nil), resp.JSON...), nil
}
