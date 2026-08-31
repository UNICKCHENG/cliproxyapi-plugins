package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type sidecarHTTPReply struct {
	ID         string              `json:"id"`
	Op         string              `json:"op"`
	StatusCode int                 `json:"status_code,omitempty"`
	Headers    map[string][]string `json:"headers,omitempty"`
	Body       string              `json:"body,omitempty"`
	Payload    string              `json:"payload,omitempty"`
	Message    string              `json:"message,omitempty"`
}

type rpcHostHTTPStreamResponse struct {
	StatusCode int                 `json:"status_code"`
	Headers    map[string][]string `json:"headers,omitempty"`
	StreamID   string              `json:"stream_id,omitempty"`
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

func (p *sidecarProcess) handleSidecarHTTP(event sidecarEvent) {
	if p == nil {
		return
	}
	go func() {
		if errHandle := p.runSidecarHTTP(event); errHandle != nil {
			_ = p.writeJSON(sidecarHTTPReply{
				ID:      event.ID,
				Op:      "http_error",
				Message: errHandle.Error(),
			})
		}
	}()
}

func (p *sidecarProcess) runSidecarHTTP(event sidecarEvent) error {
	method := strings.TrimSpace(event.Method)
	if method == "" {
		method = "GET"
	}
	url := strings.TrimSpace(event.URL)
	if url == "" {
		return fmt.Errorf("cursor sidecar http request missing url")
	}

	body, errBody := decodeSidecarHTTPBody(event.Body)
	if errBody != nil {
		return errBody
	}

	httpReq := pluginapi.HTTPRequest{
		Method:  method,
		URL:     url,
		Headers: cloneHTTPHeader(event.Headers),
		Body:    body,
	}

	if !event.Stream {
		return p.runSidecarHTTPOnce(event.ID, httpReq)
	}
	return p.runSidecarHTTPStream(event.ID, httpReq)
}

func (p *sidecarProcess) runSidecarHTTPOnce(id string, req pluginapi.HTTPRequest) error {
	rawResult, errCall := callHost(pluginabi.MethodHostHTTPDo, req)
	if errCall != nil {
		return errCall
	}
	var resp pluginapi.HTTPResponse
	if errDecode := json.Unmarshal(rawResult, &resp); errDecode != nil {
		return fmt.Errorf("decode host http response: %w", errDecode)
	}
	return p.writeJSON(sidecarHTTPReply{
		ID:         id,
		Op:         "http_response",
		StatusCode: resp.StatusCode,
		Headers:    cloneHTTPHeader(resp.Headers),
		Body:       base64.StdEncoding.EncodeToString(resp.Body),
	})
}

func (p *sidecarProcess) runSidecarHTTPStream(id string, req pluginapi.HTTPRequest) error {
	rawResult, errCall := callHost(pluginabi.MethodHostHTTPDoStream, req)
	if errCall != nil {
		return errCall
	}
	var resp rpcHostHTTPStreamResponse
	if errDecode := json.Unmarshal(rawResult, &resp); errDecode != nil {
		return fmt.Errorf("decode host http stream response: %w", errDecode)
	}
	streamID := strings.TrimSpace(resp.StreamID)
	if streamID == "" {
		return fmt.Errorf("host http stream id is empty")
	}
	defer func() {
		_, _ = callHost(pluginabi.MethodHostHTTPStreamClose, rpcHostHTTPStreamCloseRequest{StreamID: streamID})
	}()

	if errWrite := p.writeJSON(sidecarHTTPReply{
		ID:         id,
		Op:         "http_headers",
		StatusCode: resp.StatusCode,
		Headers:    cloneHTTPHeader(resp.Headers),
	}); errWrite != nil {
		return errWrite
	}

	for {
		rawChunk, errRead := callHost(pluginabi.MethodHostHTTPStreamRead, rpcHostHTTPStreamReadRequest{StreamID: streamID})
		if errRead != nil {
			return errRead
		}
		var chunk rpcHostHTTPStreamReadResponse
		if errDecode := json.Unmarshal(rawChunk, &chunk); errDecode != nil {
			return fmt.Errorf("decode host http stream chunk: %w", errDecode)
		}
		if message := strings.TrimSpace(chunk.Error); message != "" {
			return fmt.Errorf("host http stream read: %s", message)
		}
		if len(chunk.Payload) > 0 {
			if errWrite := p.writeJSON(sidecarHTTPReply{
				ID:      id,
				Op:      "http_chunk",
				Payload: base64.StdEncoding.EncodeToString(chunk.Payload),
			}); errWrite != nil {
				return errWrite
			}
		}
		if chunk.Done {
			break
		}
	}
	return p.writeJSON(sidecarHTTPReply{ID: id, Op: "http_end"})
}

func decodeSidecarHTTPBody(raw string) ([]byte, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	decoded, errDecode := base64.StdEncoding.DecodeString(trimmed)
	if errDecode != nil {
		return nil, fmt.Errorf("decode sidecar http body: %w", errDecode)
	}
	return decoded, nil
}

func cloneHTTPHeader(headers map[string][]string) map[string][]string {
	if len(headers) == 0 {
		return map[string][]string{}
	}
	cloned := make(map[string][]string, len(headers))
	for key, values := range headers {
		if len(values) == 0 {
			continue
		}
		copied := make([]string, len(values))
		copy(copied, values)
		cloned[key] = copied
	}
	return cloned
}

func (p *sidecarProcess) writeJSON(payload any) error {
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return fmt.Errorf("encode sidecar payload: %w", errMarshal)
	}
	raw = append(raw, '\n')
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if _, errWrite := p.stdin.Write(raw); errWrite != nil {
		return fmt.Errorf("write to cursor sidecar: %w (%s)", errWrite, p.stderr.String())
	}
	return nil
}
