package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestChatToAnthropicBasicAndTools(t *testing.T) {
	payload := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[
			{"role":"system","content":"be brief"},
			{"role":"user","content":"hi"},
			{"role":"assistant","content":"ok","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"found"}
		],
		"tools":[{"type":"function","function":{"name":"lookup","description":"d","parameters":{"type":"object"}}}]
	}`)
	raw, errConvert := chatToAnthropic(context.Background(), payload, "claude-sonnet-4-6", "", false)
	if errConvert != nil {
		t.Fatalf("convert: %v", errConvert)
	}
	if gjson.GetBytes(raw, "system").String() != "be brief" {
		t.Fatalf("system = %s", raw)
	}
	if gjson.GetBytes(raw, "max_tokens").Int() != defaultAnthropicMaxTokens {
		t.Fatalf("max_tokens = %s", raw)
	}
	if gjson.GetBytes(raw, "messages.#").Int() != 3 {
		t.Fatalf("messages = %s", raw)
	}
	if gjson.GetBytes(raw, "messages.1.content.1.type").String() != "tool_use" {
		t.Fatalf("tool_use missing: %s", raw)
	}
	if gjson.GetBytes(raw, "messages.2.content.0.type").String() != "tool_result" {
		t.Fatalf("tool_result missing: %s", raw)
	}
	if gjson.GetBytes(raw, "tools.0.name").String() != "lookup" {
		t.Fatalf("tools = %s", raw)
	}
}

func TestChatToAnthropicRejectsInvalidToolArguments(t *testing.T) {
	payload := []byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"x","arguments":"not-json"}}]}
		]
	}`)
	_, errConvert := chatToAnthropic(context.Background(), payload, "claude-sonnet-4-6", "", false)
	if errConvert == nil {
		t.Fatal("expected invalid arguments error")
	}
	if failureFrom(errConvert).HTTPStatus != 400 {
		t.Fatalf("status = %+v", failureFrom(errConvert))
	}
}

func TestChatToAnthropicRejectsNonFunctionTools(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom"}]}`)
	_, errConvert := chatToAnthropic(context.Background(), payload, "claude-sonnet-4-6", "", false)
	if errConvert == nil {
		t.Fatal("expected tool type error")
	}
}

func TestChatToAnthropicMaxTokens(t *testing.T) {
	_, errZero := chatToAnthropic(context.Background(), []byte(`{"messages":[{"role":"user","content":"hi"}],"max_tokens":0}`), "claude-sonnet-4-6", "", false)
	if errZero == nil {
		t.Fatal("expected zero max_tokens error")
	}
	_, errHuge := chatToAnthropic(context.Background(), []byte(`{"messages":[{"role":"user","content":"hi"}],"max_tokens":999999}`), "claude-sonnet-4-6", "", false)
	if errHuge == nil {
		t.Fatal("expected oversized max_tokens error")
	}
	raw, errOK := chatToAnthropic(context.Background(), []byte(`{"messages":[{"role":"user","content":"hi"}],"max_tokens":128}`), "claude-sonnet-4-6", "", false)
	if errOK != nil {
		t.Fatalf("convert: %v", errOK)
	}
	if gjson.GetBytes(raw, "max_tokens").Int() != 128 {
		t.Fatalf("max_tokens = %s", raw)
	}
}

func TestChatToAnthropicDataURLImage(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}]}`)
	raw, errConvert := chatToAnthropic(context.Background(), payload, "claude-sonnet-4-6", "", false)
	if errConvert != nil {
		t.Fatalf("convert: %v", errConvert)
	}
	if gjson.GetBytes(raw, "messages.0.content.0.type").String() != "image" {
		t.Fatalf("image missing: %s", raw)
	}
	if gjson.GetBytes(raw, "messages.0.content.0.source.media_type").String() != "image/png" {
		t.Fatalf("mime = %s", raw)
	}
}

func TestAnthropicToChatCompletion(t *testing.T) {
	raw := []byte(`{
		"id":"msg_1",
		"stop_reason":"tool_use",
		"content":[
			{"type":"text","text":"hi"},
			{"type":"tool_use","id":"call_1","name":"lookup","input":{"q":"x"}}
		],
		"usage":{"input_tokens":9,"output_tokens":2}
	}`)
	payload, usage, errConvert := anthropicToChatCompletion(raw, "claude-sonnet-4-6")
	if errConvert != nil {
		t.Fatalf("convert: %v", errConvert)
	}
	if gjson.GetBytes(payload, "choices.0.message.content").String() != "hi" {
		t.Fatalf("content = %s", payload)
	}
	if gjson.GetBytes(payload, "choices.0.finish_reason").String() != "tool_calls" {
		t.Fatalf("finish = %s", payload)
	}
	if gjson.GetBytes(payload, "choices.0.message.tool_calls.0.function.name").String() != "lookup" {
		t.Fatalf("tool = %s", payload)
	}
	if usage == nil || usage.PromptTokens != 9 || usage.CompletionTokens != 2 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestExecuteRoutesClaudeToMessages(t *testing.T) {
	var paths []string
	useRoundTripServer(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	})
	raw, errExec := execute(executorRequest(t, "user_ok", map[string]any{
		"model":    "claude-sonnet-4-6",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}))
	if errExec != nil {
		t.Fatalf("execute: %v", errExec)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("failed: %+v", env.Error)
	}
	if len(paths) != 1 || !strings.HasSuffix(paths[0], "/provider/v1/messages") {
		t.Fatalf("paths = %v", paths)
	}
}

func TestUnknownModelUsesOpenAIEndpoint(t *testing.T) {
	var paths []string
	useRoundTripServer(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		http.Error(w, `{"error":{"message":"use /messages","type":"invalid_request_error"}}`, http.StatusBadRequest)
	})
	raw, errExec := execute(executorRequest(t, "user_ok", map[string]any{
		"model":    "mystery-model",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}))
	if errExec != nil {
		t.Fatalf("execute: %v", errExec)
	}
	env := decodeEnvelope(t, raw)
	if env.OK || env.Error.HTTPStatus != 400 {
		t.Fatalf("expected 400 from OpenAI endpoint, got %+v", env.Error)
	}
	if len(paths) != 1 || !strings.HasSuffix(paths[0], "/provider/v1/chat/completions") {
		t.Fatalf("paths = %v", paths)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	var msg chatCompletionMessage
	if errUnmarshal := json.Unmarshal([]byte(`{"role":"assistant","content":"hi"}`), &msg); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
}
