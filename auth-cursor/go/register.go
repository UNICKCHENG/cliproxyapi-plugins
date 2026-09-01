package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

// defaultOptimizeFor is applied to the auto-smart router model, which rejects requests
// that omit the optimize_for parameter.
const defaultOptimizeFor = "balanced"

// maxCredentialWeight mirrors the ceiling the host enforces on credential weights. The
// plugin cannot import the host's internal weight package, so the bound is repeated here to
// reject a misconfigured value while the config is loading rather than at request time.
const maxCredentialWeight = 1_000_000

// pluginVersion is reported to the host and keys the bridge state directory. Release builds
// override it with -ldflags "-X main.pluginVersion=<release version>".
var pluginVersion = "0.0.0-dev"

var currentConfig atomic.Value

type pluginConfig struct {
	BridgePath string `yaml:"bridge-path"`
	// ProxyURL is the fallback proxy for credentials whose auth file sets no proxy_url. Cursor
	// traffic no longer passes through the host's HTTP client, so the host's global proxy setting
	// does not reach it and has to be restated here.
	ProxyURL    string `yaml:"proxy-url"`
	OptimizeFor string `yaml:"optimize-for"`
	// Weights maps a credential, named by account email or auth file name, to its
	// weighted-round-robin share. Weights live here instead of in the auth file because the
	// login flow rewrites that file and would drop them on every renewal.
	Weights map[string]credentialWeight `yaml:"weights"`
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
	return pluginConfig{OptimizeFor: defaultOptimizeFor}
}

// configure decodes the plugin-owned YAML block and swaps it in atomically. A configuration
// change also drops the running bridges so the next request picks up the new settings.
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
	previous, hadPrevious := currentConfig.Load().(pluginConfig)
	currentConfig.Store(cfg)
	// A bridge process is bound to the binary it was launched from and to the proxy in its
	// environment, so neither setting can be changed on a running one.
	if hadPrevious && (previous.BridgePath != cfg.BridgePath || previous.ProxyURL != cfg.ProxyURL) {
		stopBridges()
	}
	return nil
}

func decodeConfig(raw []byte) (pluginConfig, error) {
	cfg := defaultPluginConfig()
	if errUnmarshal := yaml.Unmarshal(raw, &cfg); errUnmarshal != nil {
		return pluginConfig{}, errUnmarshal
	}
	cfg.BridgePath = strings.TrimSpace(cfg.BridgePath)
	cfg.ProxyURL = strings.TrimSpace(cfg.ProxyURL)
	cfg.OptimizeFor = strings.ToLower(strings.TrimSpace(cfg.OptimizeFor))
	if cfg.OptimizeFor == "" {
		cfg.OptimizeFor = defaultOptimizeFor
	}
	weights, errWeights := normalizeWeights(cfg.Weights)
	if errWeights != nil {
		return pluginConfig{}, errWeights
	}
	cfg.Weights = weights
	return cfg, nil
}

// normalizeWeights lowercases the credential keys so that lookups are case-insensitive and
// rejects values the host would refuse, which would otherwise park the credential when the
// synthesized auth reaches the scheduler. Non-positive weights are kept as zero, the value
// the host reads as "exclude this credential from weighted routing".
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
			Name:             "auth-cursor",
			Version:          pluginVersion,
			Author:           "UNICKCHENG",
			GitHubRepository: "https://github.com/UNICKCHENG/cliproxyapi-plugins",
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        "bridge-path",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Path to the cursor-sdk-bridge executable or the directory holding it. Empty downloads the pinned release into <cache>/cli-proxy-api/auth-cursor-bridge on first use.",
				},
				{
					Name:        "proxy-url",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Proxy for credentials whose auth file sets no proxy_url. The bridge reaches Cursor itself, so the host's global proxy-url does not apply to it.",
				},
				{
					Name:        "optimize-for",
					Type:        pluginapi.ConfigFieldTypeEnum,
					EnumValues:  []string{"cost", "balanced", "intelligence"},
					Description: "Cursor Router optimization mode applied to the auto-smart model.",
				},
				{
					Name:        "weights",
					Type:        pluginapi.ConfigFieldTypeObject,
					Description: "Weighted-round-robin share per credential, keyed by account email or auth file name. Requires routing.strategy \"weighted-round-robin\".",
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
