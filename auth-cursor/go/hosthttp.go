package main

import (
	"encoding/json"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// hostHTTPDo performs one request through the host's HTTP client.
//
// Cursor traffic itself no longer goes through here — the bridge reaches the API directly — but
// image attachments still do: routing them through the host keeps them under the same proxy and
// transport policy as every other outbound request the process makes.
func hostHTTPDo(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	raw, errCall := callHost(pluginabi.MethodHostHTTPDo, req)
	if errCall != nil {
		return pluginapi.HTTPResponse{}, errCall
	}
	var resp pluginapi.HTTPResponse
	if errDecode := json.Unmarshal(raw, &resp); errDecode != nil {
		return pluginapi.HTTPResponse{}, fmt.Errorf("decode host http response: %w", errDecode)
	}
	return resp, nil
}
