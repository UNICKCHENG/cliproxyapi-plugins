package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

type rpcHostHTTPRequest struct {
	HostCallbackID string      `json:"host_callback_id,omitempty"`
	Method         string      `json:"method,omitempty"`
	URL            string      `json:"url,omitempty"`
	Headers        http.Header `json:"headers,omitempty"`
	Body           []byte      `json:"body,omitempty"`
}

type rpcHostHTTPResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers,omitempty"`
	Body       []byte      `json:"body,omitempty"`
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

// roundTrip is the plugin's outbound HTTP. Tests replace it so no live host or network is required.
var roundTrip = liveRoundTrip

func liveRoundTrip(ctx context.Context, proxyURL string, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("http request is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	req = req.WithContext(ctx)
	if strings.TrimSpace(proxyURL) != "" {
		return proxyRoundTrip(ctx, proxyURL, req)
	}
	return hostRoundTrip(ctx, req)
}

func proxyRoundTrip(ctx context.Context, proxyURL string, req *http.Request) (*http.Response, error) {
	parsed, errParse := url.Parse(strings.TrimSpace(proxyURL))
	if errParse != nil {
		return nil, fmt.Errorf("proxy-url is not a valid URL")
	}
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(parsed)},
	}
	resp, errDo := client.Do(req.WithContext(ctx))
	if errDo != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("execute proxied request: %w", errDo)
	}
	return resp, nil
}

func hostRoundTrip(ctx context.Context, req *http.Request) (*http.Response, error) {
	body, errRead := readRequestBody(req)
	if errRead != nil {
		return nil, errRead
	}
	hostReq := rpcHostHTTPRequest{
		HostCallbackID: hostCallbackIDFrom(ctx),
		Method:         req.Method,
		URL:            req.URL.String(),
		Headers:        cloneHeader(req.Header),
		Body:           body,
	}
	if wantsStream(req) {
		return hostRoundTripStream(ctx, hostReq)
	}
	raw, errCall := callHost(pluginabi.MethodHostHTTPDo, hostReq)
	if errCall != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errCall
	}
	var resp rpcHostHTTPResponse
	if errDecode := json.Unmarshal(raw, &resp); errDecode != nil {
		return nil, fmt.Errorf("decode host http response: %w", errDecode)
	}
	return &http.Response{
		StatusCode: resp.StatusCode,
		Status:     fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode)),
		Header:     cloneHeader(resp.Headers),
		Body:       io.NopCloser(bytes.NewReader(resp.Body)),
		Request:    req,
	}, nil
}

func hostRoundTripStream(ctx context.Context, hostReq rpcHostHTTPRequest) (*http.Response, error) {
	raw, errCall := callHost(pluginabi.MethodHostHTTPDoStream, hostReq)
	if errCall != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errCall
	}
	var opened rpcHostHTTPStreamResponse
	if errDecode := json.Unmarshal(raw, &opened); errDecode != nil {
		return nil, fmt.Errorf("decode host http stream: %w", errDecode)
	}
	streamID := strings.TrimSpace(opened.StreamID)
	if streamID == "" {
		return nil, fmt.Errorf("host http stream did not return a stream id")
	}
	return &http.Response{
		StatusCode: opened.StatusCode,
		Status:     fmt.Sprintf("%d %s", opened.StatusCode, http.StatusText(opened.StatusCode)),
		Header:     cloneHeader(opened.Headers),
		Body:       &hostStreamBody{ctx: ctx, streamID: streamID},
	}, nil
}

func wantsStream(req *http.Request) bool {
	if req == nil {
		return false
	}
	accept := strings.ToLower(req.Header.Get("Accept"))
	return strings.Contains(accept, "text/event-stream")
}

func readRequestBody(req *http.Request) ([]byte, error) {
	if req == nil || req.Body == nil {
		return nil, nil
	}
	defer req.Body.Close()
	return io.ReadAll(req.Body)
}

func cloneHeader(header http.Header) http.Header {
	if len(header) == 0 {
		return nil
	}
	out := make(http.Header, len(header))
	for key, values := range header {
		out[key] = append([]string(nil), values...)
	}
	return out
}

type hostStreamBody struct {
	ctx      context.Context
	streamID string
	buf      []byte
	err      error
	done     bool
	closed   bool
	mu       sync.Mutex
}

func (b *hostStreamBody) Read(p []byte) (int, error) {
	if b == nil {
		return 0, io.EOF
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, io.ErrClosedPipe
	}
	for len(b.buf) == 0 && b.err == nil && !b.done {
		if b.ctx != nil && b.ctx.Err() != nil {
			return 0, b.ctx.Err()
		}
		raw, errCall := callHost(pluginabi.MethodHostHTTPStreamRead, rpcHostHTTPStreamReadRequest{StreamID: b.streamID})
		if errCall != nil {
			b.err = errCall
			break
		}
		var chunk rpcHostHTTPStreamReadResponse
		if errDecode := json.Unmarshal(raw, &chunk); errDecode != nil {
			b.err = fmt.Errorf("decode host http stream chunk: %w", errDecode)
			break
		}
		if chunk.Error != "" {
			b.err = fmt.Errorf("%s", chunk.Error)
			b.done = true
			break
		}
		if len(chunk.Payload) > 0 {
			b.buf = append(b.buf, chunk.Payload...)
		}
		if chunk.Done {
			b.done = true
		}
	}
	if len(b.buf) > 0 {
		n := copy(p, b.buf)
		b.buf = b.buf[n:]
		return n, nil
	}
	if b.err != nil {
		return 0, b.err
	}
	return 0, io.EOF
}

func (b *hostStreamBody) Close() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	if b.streamID == "" {
		return nil
	}
	_, errCall := callHost(pluginabi.MethodHostHTTPStreamClose, rpcHostHTTPStreamCloseRequest{StreamID: b.streamID})
	return errCall
}
