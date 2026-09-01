package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// rpcHostHTTPRequest is the host.http.do / do_stream envelope. pluginapi.HTTPRequest has no
// JSON tags and no host_callback_id, so the plugin speaks this shape itself.
type rpcHostHTTPRequest struct {
	HostCallbackID string      `json:"host_callback_id,omitempty"`
	Method         string      `json:"method,omitempty"`
	URL            string      `json:"url,omitempty"`
	Headers        http.Header `json:"headers,omitempty"`
	Body           []byte      `json:"body,omitempty"`
}

type rpcHostHTTPStreamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers,omitempty"`
	StreamID   string      `json:"stream_id,omitempty"`
}

type rpcHostHTTPStreamReadRequest struct {
	StreamID string `json:"stream_id"`
}

type rpcHostHTTPStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

type rpcHostHTTPStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
}

// hostHTTPStream is the streaming host HTTP surface. Tests replace it so image downloads can be
// exercised without a live C ABI host.
var hostHTTPStream hostHTTPStreamer = liveHostHTTPStreamer{}

type hostHTTPStreamer interface {
	Open(ctx context.Context, req rpcHostHTTPRequest) (rpcHostHTTPStreamResponse, error)
	Read(ctx context.Context, streamID string) (rpcHostHTTPStreamReadResponse, error)
	Close(streamID string) error
}

type liveHostHTTPStreamer struct{}

func (liveHostHTTPStreamer) Open(ctx context.Context, req rpcHostHTTPRequest) (rpcHostHTTPStreamResponse, error) {
	req.HostCallbackID = hostCallbackIDFrom(ctx)
	raw, errCall := callHost(pluginabi.MethodHostHTTPDoStream, req)
	if errCall != nil {
		return rpcHostHTTPStreamResponse{}, errCall
	}
	var resp rpcHostHTTPStreamResponse
	if errDecode := json.Unmarshal(raw, &resp); errDecode != nil {
		return rpcHostHTTPStreamResponse{}, fmt.Errorf("decode host http stream: %w", errDecode)
	}
	return resp, nil
}

func (liveHostHTTPStreamer) Read(ctx context.Context, streamID string) (rpcHostHTTPStreamReadResponse, error) {
	raw, errCall := callHost(pluginabi.MethodHostHTTPStreamRead, rpcHostHTTPStreamReadRequest{StreamID: streamID})
	if errCall != nil {
		return rpcHostHTTPStreamReadResponse{}, errCall
	}
	var resp rpcHostHTTPStreamReadResponse
	if errDecode := json.Unmarshal(raw, &resp); errDecode != nil {
		return rpcHostHTTPStreamReadResponse{}, fmt.Errorf("decode host http stream chunk: %w", errDecode)
	}
	return resp, nil
}

func (liveHostHTTPStreamer) Close(streamID string) error {
	_, errCall := callHost(pluginabi.MethodHostHTTPStreamClose, rpcHostHTTPStreamCloseRequest{StreamID: streamID})
	return errCall
}
