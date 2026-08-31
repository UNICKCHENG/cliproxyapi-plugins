package main

import "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"

type rpcHostLogRequest struct {
	Level   string         `json:"level,omitempty"`
	Message string         `json:"message,omitempty"`
	Fields  map[string]any `json:"fields,omitempty"`
}

// hostLog emits a structured record through the host logger. Plugins share the host process,
// so writing to stdout directly would corrupt its log stream.
func hostLog(level, message string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["plugin"] = providerIdentifier
	_, _ = callHost(pluginabi.MethodHostLog, rpcHostLogRequest{
		Level:   level,
		Message: message,
		Fields:  fields,
	})
}
