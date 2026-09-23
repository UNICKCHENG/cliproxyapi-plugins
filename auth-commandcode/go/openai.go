package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

type chatCompletionMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []chatToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

type chatToolCall struct {
	Index    *int                 `json:"index,omitempty"`
	ID       string               `json:"id"`
	Type     string               `json:"type"`
	Function chatToolCallFunction `json:"function"`
}

type chatToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatCompletionDelta struct {
	Role      string         `json:"role,omitempty"`
	Content   string         `json:"content,omitempty"`
	ToolCalls []chatToolCall `json:"tool_calls,omitempty"`
}

type chatCompletionChoice struct {
	Index        int                    `json:"index"`
	Message      *chatCompletionMessage `json:"message,omitempty"`
	Delta        *chatCompletionDelta   `json:"delta,omitempty"`
	FinishReason *string                `json:"finish_reason"`
}

type chatCompletionUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

type chatCompletion struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []chatCompletionChoice `json:"choices"`
	Usage   *chatCompletionUsage   `json:"usage,omitempty"`
}

func newCompletionID() string {
	var raw [8]byte
	if _, errRead := rand.Read(raw[:]); errRead != nil {
		return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	return "chatcmpl-" + hex.EncodeToString(raw[:])
}

func payloadModel(payload []byte, fallback string) string {
	model := strings.TrimSpace(fallback)
	if model == "" {
		model = strings.TrimSpace(gjson.GetBytes(payload, "model").String())
	}
	return model
}

func ensureChatPayload(payload []byte, model string, stream bool) ([]byte, error) {
	if len(payload) == 0 {
		return nil, requestFault("request payload is empty")
	}
	if len(payload) > defaultRequestMaxBytes {
		return nil, requestFault(fmt.Sprintf("request exceeds %d bytes", defaultRequestMaxBytes))
	}
	if !gjson.ValidBytes(payload) {
		return nil, requestFault("request payload is not valid JSON")
	}
	if gjson.GetBytes(payload, "messages").Type != gjson.JSON || !gjson.GetBytes(payload, "messages").IsArray() {
		return nil, requestFault("request must include a messages array")
	}
	var body map[string]any
	if errUnmarshal := json.Unmarshal(payload, &body); errUnmarshal != nil {
		return nil, requestFault("request payload is not a JSON object")
	}
	body["model"] = model
	body["stream"] = stream
	if stream {
		if _, ok := body["stream_options"]; !ok {
			body["stream_options"] = map[string]any{"include_usage": true}
		}
	}
	encoded, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return encoded, nil
}

func executeOpenAI(ctx context.Context, call executorCall) ([]byte, *chatCompletionUsage, error) {
	endpoint, errURL := providerEndpoint(loadedConfig().APIBase, "chat/completions")
	if errURL != nil {
		return nil, nil, errURL
	}
	body, errBody := ensureChatPayload(call.payload, call.model, false)
	if errBody != nil {
		return nil, nil, errBody
	}
	status, _, raw, errDo := doJSON(ctx, upstreamCall{
		Method:    http.MethodPost,
		URL:       endpoint,
		APIKey:    call.apiKey,
		ProxyURL:  call.proxyURL,
		Body:      body,
		Timeout:   nonStreamTimeout,
		ZDR:       call.zdr,
		ClientZDR: call.clientZDR,
	})
	if errDo != nil {
		return nil, nil, errDo
	}
	if status < 200 || status > 299 {
		return nil, nil, &upstreamError{failure: classifyHTTPStatus(status, raw)}
	}
	if !gjson.ValidBytes(raw) {
		return nil, nil, fmt.Errorf("upstream returned invalid JSON")
	}
	usage := parseOpenAIUsage(raw)
	return append([]byte(nil), raw...), usage, nil
}

func streamOpenAI(ctx context.Context, call executorCall, emit func([]byte) error) (*chatCompletionUsage, error) {
	endpoint, errURL := providerEndpoint(loadedConfig().APIBase, "chat/completions")
	if errURL != nil {
		return nil, errURL
	}
	body, errBody := ensureChatPayload(call.payload, call.model, true)
	if errBody != nil {
		return nil, errBody
	}
	resp, errDo := doHTTP(ctx, upstreamCall{
		Method:    http.MethodPost,
		URL:       endpoint,
		APIKey:    call.apiKey,
		ProxyURL:  call.proxyURL,
		Body:      body,
		Accept:    "text/event-stream",
		ZDR:       call.zdr,
		ClientZDR: call.clientZDR,
		Stream:    true,
	})
	if errDo != nil {
		return nil, errDo
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, errorSummaryLimit+1))
		return nil, &upstreamError{failure: classifyHTTPStatus(resp.StatusCode, raw)}
	}
	return forwardOpenAISSE(resp.Body, call.framing, emit)
}

func forwardOpenAISSE(body io.Reader, framing streamFraming, emit func([]byte) error) (*chatCompletionUsage, error) {
	reader := newSSEReader(body)
	var usage *chatCompletionUsage
	for {
		event, errRead := readSSEEvent(reader)
		if errRead != nil {
			if errRead == io.EOF {
				return usage, incompleteStreamError()
			}
			return usage, errRead
		}
		if strings.EqualFold(event.Event, "error") {
			return usage, &upstreamError{failure: classifyHTTPStatus(http.StatusBadGateway, []byte(event.Data))}
		}
		if isSSEDone(event) {
			if framed := framing.terminator(); len(framed) > 0 {
				if errEmit := emit(framed); errEmit != nil {
					return usage, errEmit
				}
			}
			return usage, nil
		}
		data := strings.TrimSpace(event.Data)
		if data == "" {
			continue
		}
		if parsed := parseOpenAIUsage([]byte(data)); parsed != nil {
			usage = parsed
		}
		if errEmit := emit(framing.frame([]byte(data))); errEmit != nil {
			return usage, errEmit
		}
	}
}

func parseOpenAIUsage(raw []byte) *chatCompletionUsage {
	node := gjson.GetBytes(raw, "usage")
	if !node.Exists() || !node.IsObject() {
		return nil
	}
	usage := &chatCompletionUsage{
		PromptTokens:     node.Get("prompt_tokens").Int(),
		CompletionTokens: node.Get("completion_tokens").Int(),
		TotalTokens:      node.Get("total_tokens").Int(),
	}
	if usage.PromptTokens == 0 && usage.CompletionTokens == 0 && usage.TotalTokens == 0 {
		return nil
	}
	return usage
}

func buildStreamChunk(id, model string, delta chatCompletionDelta, finish *string, usage *chatCompletionUsage) []byte {
	chunk := chatCompletion{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []chatCompletionChoice{{
			Index:        0,
			Delta:        &delta,
			FinishReason: finish,
		}},
		Usage: usage,
	}
	raw, errMarshal := json.Marshal(chunk)
	if errMarshal != nil {
		return nil
	}
	return raw
}

func messageText(content json.RawMessage) string {
	if len(bytes.TrimSpace(content)) == 0 {
		return ""
	}
	var text string
	if errUnmarshal := json.Unmarshal(content, &text); errUnmarshal == nil {
		return text
	}
	var parts []map[string]any
	if errUnmarshal := json.Unmarshal(content, &parts); errUnmarshal != nil {
		return strings.Trim(string(content), `"`)
	}
	var builder strings.Builder
	for _, part := range parts {
		if fmt.Sprint(part["type"]) != "text" && part["type"] != nil {
			continue
		}
		if value, ok := part["text"].(string); ok {
			builder.WriteString(value)
		}
	}
	return builder.String()
}
