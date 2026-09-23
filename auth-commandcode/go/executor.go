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

type executorCall struct {
	apiKey    string
	proxyURL  string
	model     string
	payload   []byte
	zdr       bool
	clientZDR bool
	anthropic bool
	framing   streamFraming
}

func execute(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	call, errBuild := buildExecutorCall(req.ExecutorRequest)
	if errBuild != nil {
		failure := failureFrom(errBuild)
		return upstreamErrorEnvelope("executor_error", failure.Message, failure.HTTPStatus, failure.Retryable), nil
	}

	ctx, cancel := context.WithCancel(withHostCallbackID(context.Background(), req.HostCallbackID))
	defer cancel()
	flight := registerInFlight(cancel)
	defer unregisterInFlight(flight)
	logCtx := newRequestLogContext(ctx, req.ExecutorRequest, call.model)

	var (
		payload []byte
		usage   *chatCompletionUsage
		errRun  error
	)
	if call.anthropic {
		payload, usage, errRun = executeAnthropic(ctx, call)
	} else {
		payload, usage, errRun = executeOpenAI(ctx, call)
	}
	if errRun != nil {
		failure := failureFrom(errRun)
		logCtx.failed(failure.Message)
		return upstreamErrorEnvelope("executor_error", failure.Message, failure.HTTPStatus, failure.Retryable), nil
	}
	logCtx.completed(usage)
	return okEnvelope(pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	})
}

func executeStream(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	streamID := strings.TrimSpace(req.StreamID)
	if streamID == "" {
		return errorEnvelope("executor_error", "stream_id is required for executor.execute_stream"), nil
	}
	call, errBuild := buildExecutorCall(req.ExecutorRequest)
	if errBuild != nil {
		failure := failureFrom(errBuild)
		return upstreamErrorEnvelope("executor_error", failure.Message, failure.HTTPStatus, failure.Retryable), nil
	}
	call.framing = streamFramingFor(req.ExecutorRequest)

	ctx, cancel := context.WithCancel(withHostCallbackID(context.Background(), req.HostCallbackID))
	flight := registerInFlight(cancel)
	logCtx := newRequestLogContext(ctx, req.ExecutorRequest, call.model)
	go func() {
		defer unregisterInFlight(flight)
		defer cancel()
		defer func() {
			if recovered := recover(); recovered != nil {
				message := fmt.Sprintf("commandcode stream panic: %v", recovered)
				logCtx.failed(message)
				closePluginStream(streamID, message)
			}
		}()
		emit := func(payload []byte) error {
			logCtx.markFirstDelta()
			return emitPluginStreamChunk(streamID, payload)
		}
		var (
			usage  *chatCompletionUsage
			errRun error
		)
		if call.anthropic {
			usage, errRun = streamAnthropic(ctx, call, emit)
		} else {
			usage, errRun = streamOpenAI(ctx, call, emit)
		}
		if errRun != nil {
			message := failureFrom(errRun).Message
			logCtx.failed(message)
			closePluginStream(streamID, message)
			return
		}
		logCtx.completed(usage)
		closePluginStream(streamID, "")
	}()

	return okEnvelope(map[string]any{
		"headers": http.Header{"Content-Type": []string{"text/event-stream"}},
	})
}

func buildExecutorCall(req pluginapi.ExecutorRequest) (executorCall, error) {
	apiKey, errKey := requireAPIKey(req.StorageJSON)
	if errKey != nil {
		return executorCall{}, errKey
	}
	model := payloadModel(req.Payload, req.Model)
	if model == "" {
		return executorCall{}, requestFault("request does not specify a model")
	}
	return executorCall{
		apiKey:    apiKey,
		proxyURL:  resolveProxyURL(req.StorageJSON),
		model:     model,
		payload:   req.Payload,
		zdr:       loadedConfig().ZDR,
		clientZDR: clientRequestedZDR(req.Headers),
		anthropic: usesAnthropicMessages(model, lookupCatalog(apiKey)),
	}, nil
}

type streamFraming int

const (
	framingRaw streamFraming = iota
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

func (f streamFraming) terminator() []byte {
	if f != framingSSE {
		return nil
	}
	return []byte("data: [DONE]\n\n")
}

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

func countTokens(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	estimate := 0
	messages := gjson.GetBytes(req.Payload, "messages")
	if messages.IsArray() {
		var builder strings.Builder
		for _, item := range messages.Array() {
			builder.WriteString(item.Get("content").String())
		}
		estimate = (builder.Len() + 3) / 4
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

func httpRequest() ([]byte, error) {
	body := []byte(`{"error":{"message":"commandcode provider does not support raw HTTP passthrough","type":"unsupported"}}`)
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
