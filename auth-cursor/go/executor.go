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
	generateReq, model, errBuild := buildGenerateRequest(req.ExecutorRequest)
	if errBuild != nil {
		return errorEnvelope("executor_error", errBuild.Error()), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logCtx := newRequestLogContext(req.ExecutorRequest, model)
	result, errRun := runGenerate(ctx, generateReq, nil)
	if errRun != nil {
		failure := failureFrom(errRun)
		logCtx.failed(failure.Message)
		return upstreamErrorEnvelope("executor_error", failure.Message, failure.HTTPStatus, failure.Retryable), nil
	}
	payload, errBody := buildCompletion(newCompletionID(), model, result.text, result.usage)
	if errBody != nil {
		logCtx.failed(errBody.Error())
		return errorEnvelope("executor_error", errBody.Error()), nil
	}
	logCtx.completed(result.usage)
	return okEnvelope(pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	})
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
	generateReq, model, errBuild := buildGenerateRequest(req.ExecutorRequest)
	if errBuild != nil {
		return errorEnvelope("executor_error", errBuild.Error()), nil
	}

	framing := streamFramingFor(req.ExecutorRequest)
	logCtx := newRequestLogContext(req.ExecutorRequest, model)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				message := fmt.Sprintf("cursor stream panic: %v", recovered)
				logCtx.failed(message)
				closePluginStream(streamID, message)
			}
		}()
		if errRun := forwardStream(streamID, model, framing, generateReq, &logCtx); errRun != nil {
			closePluginStream(streamID, errRun.Error())
			return
		}
		closePluginStream(streamID, "")
	}()

	return okEnvelope(map[string]any{
		"headers": http.Header{"Content-Type": []string{"text/event-stream"}},
	})
}

// forwardStream translates run deltas into OpenAI SSE chunks. It applies no deadline: once the
// upstream Cursor run is live the plugin must not time it out.
func forwardStream(streamID, model string, framing streamFraming, generateReq generateRequest, logCtx *requestLogContext) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	completionID := newCompletionID()
	roleSent := false
	onDelta := func(text string) error {
		logCtx.markFirstDelta()
		// The role rides along with the first content delta. Emitting it in a chunk of its own
		// makes the Gemini translator report a finished turn before any text.
		delta := chatCompletionDelta{Content: text}
		if !roleSent {
			delta.Role = "assistant"
			roleSent = true
		}
		chunk := buildStreamChunk(completionID, model, delta, nil, nil)
		return emitPluginStreamChunk(streamID, framing.frame(chunk))
	}

	result, errRun := runGenerate(ctx, generateReq, onDelta)
	if errRun != nil {
		// The stream bridge carries only text, so the classification has to be in it.
		message := failureFrom(errRun).Message
		logCtx.failed(message)
		return fmt.Errorf("%s", message)
	}
	finish := "stop"
	final := buildStreamChunk(completionID, model, chatCompletionDelta{}, &finish, result.usage)
	if errEmit := emitPluginStreamChunk(streamID, framing.frame(final)); errEmit != nil {
		logCtx.failed(errEmit.Error())
		return errEmit
	}
	if errEmit := emitPluginStreamChunk(streamID, framing.terminator()); errEmit != nil {
		logCtx.failed(errEmit.Error())
		return errEmit
	}
	logCtx.completed(result.usage)
	return nil
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

// buildGenerateRequest converts the host's chat-completions payload into a bridge run request.
func buildGenerateRequest(req pluginapi.ExecutorRequest) (generateRequest, string, error) {
	apiKey, errKey := requireAPIKey(req.StorageJSON)
	if errKey != nil {
		return generateRequest{}, "", errKey
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = strings.TrimSpace(gjson.GetBytes(req.Payload, "model").String())
	}
	if model == "" {
		return generateRequest{}, "", fmt.Errorf("request does not specify a model")
	}
	chat, errParse := parseChatRequest(req.Payload)
	if errParse != nil {
		return generateRequest{}, "", errParse
	}
	return generateRequest{
		apiKey:      apiKey,
		proxyURL:    resolveProxyURL(req.StorageJSON),
		model:       model,
		params:      chat.Params,
		optimizeFor: loadedConfig().OptimizeFor,
		prompt:      chat.Prompt,
		images:      chat.Images,
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
