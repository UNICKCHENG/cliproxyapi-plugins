package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	defaultAPIBase            = "https://api.commandcode.ai"
	defaultAnthropicMaxTokens = 8192
	maxAnthropicMaxTokens     = 200_000
	maxCredentialWeight       = 1_000_000
)

// pluginVersion is reported to the host. Release builds override it with
// -ldflags "-X main.pluginVersion=<release version>".
var pluginVersion = "0.0.0-dev"

var currentConfig atomic.Value

type pluginConfig struct {
	APIBase  string                      `yaml:"api-base"`
	ProxyURL string                      `yaml:"proxy-url"`
	ZDR      bool                        `yaml:"zdr"`
	Weights  map[string]credentialWeight `yaml:"weights"`
}

// credentialWeight is an integer weight decoded strictly. Decoding YAML into a plain int
// truncates a fractional value, while the host rejects one outright, so the raw scalar is
// parsed here to keep the plugin from accepting a weight the host would refuse.
type credentialWeight int

func (w *credentialWeight) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("weight must be an integer")
	}
	parsed, errParse := strconv.Atoi(strings.TrimSpace(node.Value))
	if errParse != nil {
		return fmt.Errorf("weight must be an integer, got %q", node.Value)
	}
	*w = credentialWeight(parsed)
	return nil
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	AuthProvider          bool     `json:"auth_provider"`
	ModelProvider         bool     `json:"model_provider"`
	CommandLinePlugin     bool     `json:"command_line_plugin"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{APIBase: defaultAPIBase}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	cfg := defaultPluginConfig()
	if len(req.ConfigYAML) > 0 {
		decoded, errDecode := decodeConfig(req.ConfigYAML)
		if errDecode != nil {
			return errDecode
		}
		cfg = decoded
	}
	currentConfig.Store(cfg)
	return nil
}

func decodeConfig(raw []byte) (pluginConfig, error) {
	cfg := defaultPluginConfig()
	if errUnmarshal := yaml.Unmarshal(raw, &cfg); errUnmarshal != nil {
		return pluginConfig{}, errUnmarshal
	}
	apiBase, errBase := normalizeAPIBase(cfg.APIBase)
	if errBase != nil {
		return pluginConfig{}, errBase
	}
	cfg.APIBase = apiBase
	cfg.ProxyURL = strings.TrimSpace(cfg.ProxyURL)
	if cfg.ProxyURL != "" {
		if _, errProxy := url.Parse(cfg.ProxyURL); errProxy != nil {
			return pluginConfig{}, fmt.Errorf("proxy-url is not a valid URL")
		}
	}
	weights, errWeights := normalizeWeights(cfg.Weights)
	if errWeights != nil {
		return pluginConfig{}, errWeights
	}
	cfg.Weights = weights
	return cfg, nil
}

func normalizeAPIBase(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return defaultAPIBase, nil
	}
	parsed, errParse := url.Parse(value)
	if errParse != nil {
		return "", fmt.Errorf("api-base is not a valid URL")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("api-base must be an http(s) URL")
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return "", fmt.Errorf("api-base must include a host")
	}
	if parsed.User != nil {
		return "", fmt.Errorf("api-base must not include credentials")
	}
	parsed.Fragment = ""
	parsed.RawQuery = ""
	normalized := strings.TrimRight(parsed.String(), "/")
	return normalized, nil
}

func normalizeWeights(weights map[string]credentialWeight) (map[string]credentialWeight, error) {
	if len(weights) == 0 {
		return nil, nil
	}
	out := make(map[string]credentialWeight, len(weights))
	seen := make(map[string]string, len(weights))
	for rawKey, weight := range weights {
		key := strings.ToLower(strings.TrimSpace(rawKey))
		if key == "" {
			return nil, fmt.Errorf("weights: credential key must not be empty")
		}
		if original, duplicated := seen[key]; duplicated {
			return nil, fmt.Errorf("weights: %q and %q name the same credential", original, rawKey)
		}
		if weight > maxCredentialWeight {
			return nil, fmt.Errorf("weights[%s]: weight must not exceed %d", rawKey, maxCredentialWeight)
		}
		if weight < 0 {
			weight = 0
		}
		seen[key] = rawKey
		out[key] = weight
	}
	return out, nil
}

func loadedConfig() pluginConfig {
	if cfg, ok := currentConfig.Load().(pluginConfig); ok {
		return cfg
	}
	return defaultPluginConfig()
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "auth-commandcode",
			Version:          pluginVersion,
			Author:           "UNICKCHENG",
			GitHubRepository: "https://github.com/UNICKCHENG/cliproxyapi-plugins",
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        "api-base",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Command Code API origin. Default https://api.commandcode.ai. Provider paths are appended; do not put a model request path here.",
				},
				{
					Name:        "proxy-url",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Proxy for credentials whose auth file sets no proxy_url. When empty, host.http uses the host-level proxy.",
				},
				{
					Name:        "zdr",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "Send x-cmd-zdr: 1 on Provider API requests unless the client already set that header.",
				},
				{
					Name:        "weights",
					Type:        pluginapi.ConfigFieldTypeObject,
					Description: "Weighted-round-robin share per credential, keyed by auth file name. Requires routing.strategy \"weighted-round-robin\".",
				},
			},
		},
		Capabilities: registrationCapability{
			AuthProvider:          true,
			ModelProvider:         true,
			CommandLinePlugin:     true,
			Executor:              true,
			ExecutorModelScope:    string(pluginapi.ExecutorModelScopeBoth),
			ExecutorInputFormats:  []string{"chat-completions"},
			ExecutorOutputFormats: []string{"chat-completions"},
		},
	}
}

func userAgent() string {
	return "cliproxyapi-auth-commandcode/" + pluginVersion
}
