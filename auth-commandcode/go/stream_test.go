package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestForwardOpenAISSERawAndFramed(t *testing.T) {
	upstream := "data: {\"choices\":[{\"delta\":{\"content\":\"h\"}}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	var rawChunks []string
	usage, errFwd := forwardOpenAISSE(strings.NewReader(upstream), framingRaw, func(payload []byte) error {
		rawChunks = append(rawChunks, string(payload))
		return nil
	})
	if errFwd != nil {
		t.Fatalf("raw: %v", errFwd)
	}
	if len(rawChunks) != 2 || strings.HasPrefix(rawChunks[0], "data:") {
		t.Fatalf("raw chunks = %#v", rawChunks)
	}
	if usage == nil || usage.TotalTokens != 2 {
		t.Fatalf("usage = %+v", usage)
	}

	var sseChunks []string
	_, errSSE := forwardOpenAISSE(strings.NewReader(upstream), framingSSE, func(payload []byte) error {
		sseChunks = append(sseChunks, string(payload))
		return nil
	})
	if errSSE != nil {
		t.Fatalf("sse: %v", errSSE)
	}
	if !strings.HasPrefix(sseChunks[0], "data: ") || sseChunks[len(sseChunks)-1] != "data: [DONE]\n\n" {
		t.Fatalf("sse chunks = %#v", sseChunks)
	}
}

func TestForwardOpenAISSESplitChunks(t *testing.T) {
	parts := []string{"data: {\"id\":\"", "1\"}\n", "\ndata: [DONE]\n\n"}
	reader := io.MultiReader(strings.NewReader(parts[0]), strings.NewReader(parts[1]), strings.NewReader(parts[2]))
	var chunks []string
	_, errFwd := forwardOpenAISSE(reader, framingRaw, func(payload []byte) error {
		chunks = append(chunks, string(payload))
		return nil
	})
	if errFwd != nil {
		t.Fatalf("forward: %v", errFwd)
	}
	if len(chunks) != 1 || chunks[0] != `{"id":"1"}` {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestForwardAnthropicSSE(t *testing.T) {
	upstream := strings.Join([]string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":4,\"output_tokens\":0}}}\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"text\"}}\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"c1\",\"name\":\"lookup\"}}\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"q\\\"\"}}\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"input_tokens\":4,\"output_tokens\":2}}\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n",
		"",
	}, "\n")
	var chunks []string
	usage, errFwd := forwardAnthropicSSE(strings.NewReader(upstream), "claude-sonnet-4-6", framingRaw, func(payload []byte) error {
		chunks = append(chunks, string(payload))
		return nil
	})
	if errFwd != nil {
		t.Fatalf("forward: %v", errFwd)
	}
	joined := strings.Join(chunks, "\n")
	if !strings.Contains(joined, `"content":"Hi"`) {
		t.Fatalf("missing text delta: %s", joined)
	}
	if !strings.Contains(joined, `"name":"lookup"`) {
		t.Fatalf("missing tool start: %s", joined)
	}
	if usage == nil || usage.CompletionTokens != 2 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestStreamOpenAIUsesCompletions(t *testing.T) {
	useRoundTripServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("accept = %q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.Copy(w, bytes.NewReader([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")))
	})
	var chunks int
	_, errStream := streamOpenAI(context.Background(), executorCall{
		apiKey:  "user_ok",
		model:   "deepseek/deepseek-v4-flash",
		payload: []byte(`{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`),
		framing: framingRaw,
	}, func([]byte) error {
		chunks++
		return nil
	})
	if errStream != nil {
		t.Fatalf("stream: %v", errStream)
	}
	if chunks == 0 {
		t.Fatal("expected chunks")
	}
}

func TestForwardOpenAISSEIncompleteEOF(t *testing.T) {
	upstream := "data: {\"choices\":[{\"delta\":{\"content\":\"h\"}}]}\n\n"
	var chunks []string
	_, errFwd := forwardOpenAISSE(strings.NewReader(upstream), framingSSE, func(payload []byte) error {
		chunks = append(chunks, string(payload))
		return nil
	})
	if errFwd == nil {
		t.Fatal("expected incomplete stream error")
	}
	var classified *upstreamError
	if !errors.As(errFwd, &classified) || classified.failure.HTTPStatus != http.StatusBadGateway || !classified.failure.Retryable {
		t.Fatalf("err = %v", errFwd)
	}
	if len(chunks) != 1 {
		t.Fatalf("chunks = %#v", chunks)
	}
	for _, chunk := range chunks {
		if strings.Contains(chunk, "[DONE]") {
			t.Fatalf("emitted terminator on incomplete stream: %#v", chunks)
		}
	}
}

func TestForwardAnthropicSSEIncompleteEOF(t *testing.T) {
	upstream := strings.Join([]string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":4,\"output_tokens\":0}}}\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n",
		"",
	}, "\n")
	var chunks []string
	_, errFwd := forwardAnthropicSSE(strings.NewReader(upstream), "claude-sonnet-4-6", framingSSE, func(payload []byte) error {
		chunks = append(chunks, string(payload))
		return nil
	})
	if errFwd == nil {
		t.Fatal("expected incomplete stream error")
	}
	var classified *upstreamError
	if !errors.As(errFwd, &classified) || classified.failure.HTTPStatus != http.StatusBadGateway || !classified.failure.Retryable {
		t.Fatalf("err = %v", errFwd)
	}
	for _, chunk := range chunks {
		if strings.Contains(chunk, "[DONE]") {
			t.Fatalf("emitted terminator on incomplete stream: %#v", chunks)
		}
		if strings.Contains(chunk, `"finish_reason":"stop"`) {
			t.Fatalf("emitted fake finish on incomplete stream: %#v", chunks)
		}
	}
	if len(chunks) == 0 {
		t.Fatal("expected content chunks before incomplete EOF")
	}
}

func TestReadSSEEventCommentsAndMultiline(t *testing.T) {
	input := ": keep-alive\n\nevent: ping\ndata: one\ndata: two\n\n"
	reader := newSSEReader(strings.NewReader(input))
	first, errFirst := readSSEEvent(reader)
	if errFirst != nil {
		t.Fatalf("first: %v", errFirst)
	}
	if first.Data != "" && first.Event != "" && first.Event != "ping" {
		// comment-only events may be empty; next event should be ping
	}
	event := first
	if event.Event != "ping" {
		second, errSecond := readSSEEvent(reader)
		if errSecond != nil {
			t.Fatalf("second: %v", errSecond)
		}
		event = second
	}
	if event.Event != "ping" || event.Data != "one\ntwo" {
		t.Fatalf("event = %+v", event)
	}
}
