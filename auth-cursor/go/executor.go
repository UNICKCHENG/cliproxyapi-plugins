package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

type rpcStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}

type rpcStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

// execute runs a non-streaming completion and returns an OpenAI chat.completion body.
func execute(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	sidecarReq, model, errBuild := buildGenerateRequest(req.ExecutorRequest, false)
	if errBuild != nil {
		return errorEnvelope("executor_error", errBuild.Error()), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, release, errCall := callSidecar(ctx, sidecarReq)
	if errCall != nil {
		return errorEnvelope("executor_error", errCall.Error()), nil
	}
	defer release()

	var text strings.Builder
	for event := range events {
		switch event.Event {
		case "delta":
			text.WriteString(event.Text)
		case "done":
			body := event.Text
			if body == "" {
				body = text.String()
			}
			payload, errBody := buildCompletion(newCompletionID(), model, body, event.Usage)
			if errBody != nil {
				return errorEnvelope("executor_error", errBody.Error()), nil
			}
			return okEnvelope(pluginapi.ExecutorResponse{
				Payload: payload,
				Headers: http.Header{"Content-Type": []string{"application/json"}},
			})
		case "error":
			return errorEnvelope("executor_error", event.errorText()), nil
		}
	}
	return errorEnvelope("executor_error", "cursor sidecar closed the stream before completing"), nil
}

// executeStream hands the response back through the host stream bridge: the RPC returns as
// soon as headers are known, then chunks are pushed asynchronously with host.stream.emit.
func executeStream(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	streamID := strings.TrimSpace(req.StreamID)
	if streamID == "" {
		return errorEnvelope("executor_error", "stream_id is required for executor.execute_stream"), nil
	}
	sidecarReq, model, errBuild := buildGenerateRequest(req.ExecutorRequest, true)
	if errBuild != nil {
		return errorEnvelope("executor_error", errBuild.Error()), nil
	}

	framing := streamFramingFor(req.ExecutorRequest)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				closePluginStream(streamID, fmt.Sprintf("cursor stream panic: %v", recovered))
			}
		}()
		if errRun := forwardStream(streamID, model, framing, sidecarReq); errRun != nil {
			closePluginStream(streamID, errRun.Error())
			return
		}
		closePluginStream(streamID, "")
	}()

	return okEnvelope(map[string]any{
		"headers": http.Header{"Content-Type": []string{"text/event-stream"}},
	})
}

// forwardStream translates sidecar delta events into OpenAI SSE chunks. It applies no
// deadline: once the upstream Cursor run is live the plugin must not time it out.
func forwardStream(streamID, model string, framing streamFraming, sidecarReq sidecarRequest) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, release, errCall := callSidecar(ctx, sidecarReq)
	if errCall != nil {
		return errCall
	}
	defer release()

	completionID := newCompletionID()
	roleSent := false

	for event := range events {
		switch event.Event {
		case "delta":
			if event.Text == "" {
				continue
			}
			// The role rides along with the first content delta. Emitting it in a chunk of
			// its own makes the Gemini translator report a finished turn before any text.
			delta := chatCompletionDelta{Content: event.Text}
			if !roleSent {
				delta.Role = "assistant"
				roleSent = true
			}
			chunk := buildStreamChunk(completionID, model, delta, nil, nil)
			if errEmit := emitPluginStreamChunk(streamID, framing.frame(chunk)); errEmit != nil {
				// The downstream client is gone; stop the upstream run instead of draining it.
				cancel()
				return errEmit
			}
		case "done":
			finish := "stop"
			final := buildStreamChunk(completionID, model, chatCompletionDelta{}, &finish, event.Usage)
			if errEmit := emitPluginStreamChunk(streamID, framing.frame(final)); errEmit != nil {
				cancel()
				return errEmit
			}
			return emitPluginStreamChunk(streamID, framing.terminator())
		case "error":
			cancel()
			return fmt.Errorf("%s", event.errorText())
		}
	}
	return fmt.Errorf("cursor sidecar closed the stream before completing")
}

// streamFraming selects how chat-completions chunks are wrapped before they leave the plugin.
//
// The host treats the two cases differently and neither tolerates the other's framing. When the
// client also speaks chat-completions the host forwards chunks verbatim and its SSE writer adds
// the "data: " prefix and the terminating [DONE] line itself, so a prefixed chunk would reach
// the client as "data: data: {...}". Every other client protocol goes through a response
// translator, and those translators drop any chunk that is not a "data: " frame.
type streamFraming int

const (
	// framingRaw emits bare chat-completions JSON for the verbatim forwarding path.
	framingRaw streamFraming = iota
	// framingSSE emits full SSE frames for the response-translation path.
	framingSSE
)

func (f streamFraming) frame(payload []byte) []byte {
	if len(payload) == 0 || f != framingSSE {
		return payload
	}
	framed := make([]byte, 0, len(payload)+8)
	framed = append(framed, "data: "...)
	framed = append(framed, payload...)
	return append(framed, '\n', '\n')
}

// terminator returns the end-of-stream marker the translators expect. The chat-completions
// writer emits its own [DONE], so the raw path must stay silent to avoid a duplicate.
func (f streamFraming) terminator() []byte {
	if f != framingSSE {
		return nil
	}
	return []byte("data: [DONE]\n\n")
}

// streamFramingFor picks the framing from the inbound request path, which is the only signal
// carrying the client's protocol: the host rewrites SourceFormat to the plugin's own format
// before the executor is invoked.
func streamFramingFor(req pluginapi.ExecutorRequest) streamFraming {
	path := strings.TrimRight(strings.TrimSpace(requestPathMetadata(req.Metadata)), "/")
	if path == "" || strings.HasSuffix(path, "/completions") {
		return framingRaw
	}
	return framingSSE
}

func requestPathMetadata(metadata map[string]any) string {
	switch value := metadata["request_path"].(type) {
	case string:
		return value
	case []byte:
		return string(value)
	default:
		return ""
	}
}

// buildGenerateRequest converts the host's chat-completions payload into a sidecar request.
func buildGenerateRequest(req pluginapi.ExecutorRequest, stream bool) (sidecarRequest, string, error) {
	apiKey, errKey := requireAPIKey(req.StorageJSON)
	if errKey != nil {
		return sidecarRequest{}, "", errKey
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = strings.TrimSpace(gjson.GetBytes(req.Payload, "model").String())
	}
	if model == "" {
		return sidecarRequest{}, "", fmt.Errorf("request does not specify a model")
	}
	chat, errParse := parseChatRequest(req.Payload)
	if errParse != nil {
		return sidecarRequest{}, "", errParse
	}
	return sidecarRequest{
		Op:          "generate",
		APIKey:      apiKey,
		Model:       model,
		Params:      chat.Params,
		OptimizeFor: loadedConfig().OptimizeFor,
		Prompt:      chat.Prompt,
		Images:      chat.Images,
		Stream:      stream,
	}, model, nil
}

// countTokens approximates usage. The Agent SDK reports token counts only after a run, so
// there is no upstream endpoint to ask ahead of time.
func countTokens(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	estimate := 0
	if chat, errParse := parseChatRequest(req.Payload); errParse == nil {
		// Rough 4-characters-per-token heuristic; Cursor bills on its own measured counts.
		estimate = (len(chat.Prompt) + 3) / 4
	}
	payload, errMarshal := json.Marshal(map[string]any{
		"input_tokens": estimate,
		"total_tokens": estimate,
	})
	if errMarshal != nil {
		return nil, errMarshal
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: payload})
}

// httpRequest is unsupported: Cursor exposes no passthrough HTTP surface through the SDK.
func httpRequest() ([]byte, error) {
	body := []byte(`{"error":{"message":"cursor provider does not support raw HTTP passthrough","type":"unsupported"}}`)
	return okEnvelope(pluginapi.ExecutorHTTPResponse{
		StatusCode: http.StatusNotImplemented,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       body,
	})
}

func emitPluginStreamChunk(streamID string, payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	_, errCall := callHost(pluginabi.MethodHostStreamEmit, rpcStreamEmitRequest{
		StreamID: streamID,
		Payload:  payload,
	})
	return errCall
}

func closePluginStream(streamID, errMsg string) {
	_, _ = callHost(pluginabi.MethodHostStreamClose, rpcStreamCloseRequest{
		StreamID: streamID,
		Error:    strings.TrimSpace(errMsg),
	})
}
