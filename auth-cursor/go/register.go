package main

import (
	"encoding/json"
	"strings"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

// defaultOptimizeFor is applied to the auto-smart router model, which rejects requests
// that omit the optimize_for parameter.
const defaultOptimizeFor = "balanced"

// pluginVersion is reported to the host and keys the sidecar bootstrap directory. Release
// builds override it with -ldflags "-X main.pluginVersion=<release version>".
var pluginVersion = "0.0.0-dev"

var currentConfig atomic.Value

type pluginConfig struct {
	NodePath    string   `yaml:"node-path"`
	SidecarPath string   `yaml:"sidecar-path"`
	OptimizeFor string   `yaml:"optimize-for"`
	Models      []string `yaml:"models"`
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
	return pluginConfig{
		NodePath:    "node",
		OptimizeFor: defaultOptimizeFor,
	}
}

// configure decodes the plugin-owned YAML block and swaps it in atomically. A configuration
// change also drops the running sidecar so the next request picks up the new settings.
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
	if hadPrevious && (previous.NodePath != cfg.NodePath || previous.SidecarPath != cfg.SidecarPath) {
		stopSidecar()
	}
	return nil
}

func decodeConfig(raw []byte) (pluginConfig, error) {
	cfg := defaultPluginConfig()
	if errUnmarshal := yaml.Unmarshal(raw, &cfg); errUnmarshal != nil {
		return pluginConfig{}, errUnmarshal
	}
	cfg.NodePath = strings.TrimSpace(cfg.NodePath)
	if cfg.NodePath == "" {
		cfg.NodePath = "node"
	}
	cfg.SidecarPath = strings.TrimSpace(cfg.SidecarPath)
	cfg.OptimizeFor = strings.ToLower(strings.TrimSpace(cfg.OptimizeFor))
	if cfg.OptimizeFor == "" {
		cfg.OptimizeFor = defaultOptimizeFor
	}
	models := make([]string, 0, len(cfg.Models))
	for _, model := range cfg.Models {
		if trimmed := strings.TrimSpace(model); trimmed != "" {
			models = append(models, trimmed)
		}
	}
	cfg.Models = models
	return cfg, nil
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
					Name:        "node-path",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Node.js executable used to run the Cursor SDK sidecar. Defaults to \"node\" resolved from PATH.",
				},
				{
					Name:        "sidecar-path",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Path to the sidecar directory or its index.mjs. Empty auto-discovers a local cursor-sidecar directory, then falls back to the embedded copy under ~/.cli-proxy-api.",
				},
				{
					Name:        "optimize-for",
					Type:        pluginapi.ConfigFieldTypeEnum,
					EnumValues:  []string{"cost", "balanced", "intelligence"},
					Description: "Cursor Router optimization mode applied to the auto-smart model.",
				},
				{
					Name:        "models",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Fallback model ids used when Cursor.models.list() is unavailable for a credential.",
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
