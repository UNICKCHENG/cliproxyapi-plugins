package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"

	sdkv1 "github.com/UNICKCHENG/cliproxyapi-plugins/auth-cursor/go/internal/sdk/v1"
	"github.com/UNICKCHENG/cliproxyapi-plugins/auth-cursor/go/internal/sdk/v1/sdkv1connect"
)

func functionTool(name string, parameters any) map[string]any {
	fn := map[string]any{"name": name}
	if parameters != nil {
		fn["parameters"] = parameters
	}
	return map[string]any{"type": "function", "function": fn}
}

func weatherTool() map[string]any {
	return functionTool("get_weather", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"city": map[string]any{"type": "string"},
		},
	})
}

func decodeCompletion(t *testing.T, raw []byte) chatCompletion {
	t.Helper()
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("execute failed: %+v", env.Error)
	}
	var response pluginapi.ExecutorResponse
	if errUnmarshal := json.Unmarshal(env.Result, &response); errUnmarshal != nil {
		t.Fatalf("decode executor response: %v", errUnmarshal)
	}
	var completion chatCompletion
	if errUnmarshal := json.Unmarshal(response.Payload, &completion); errUnmarshal != nil {
		t.Fatalf("decode completion: %v", errUnmarshal)
	}
	return completion
}

func finishReason(t *testing.T, completion chatCompletion) string {
	t.Helper()
	if len(completion.Choices) != 1 || completion.Choices[0].FinishReason == nil {
		t.Fatalf("missing finish_reason: %+v", completion.Choices)
	}
	return *completion.Choices[0].FinishReason
}

func waitUntil(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		if ok() {
			return
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting")
		case <-tick.C:
		}
	}
}

func TestParseChatRequestFunctionSchemaAndToolChoice(t *testing.T) {
	t.Parallel()

	chat, errParse := parseChatRequest(mustJSON(t, map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"tools": []any{
			weatherTool(),
			map[string]any{"type": "code_interpreter"},
			map[string]any{"function": map[string]any{"name": "search"}},
		},
	}))
	if errParse != nil {
		t.Fatalf("parseChatRequest: %v", errParse)
	}
	if len(chat.ToolNames) != 2 || chat.ToolNames[0] != "get_weather" || chat.ToolNames[1] != "search" {
		t.Fatalf("tool names = %v, want get_weather then search", chat.ToolNames)
	}
	if chat.Tools["search"].GetInputSchema().AsMap()["type"] != "object" {
		t.Fatalf("missing parameters should become type=object, got %#v", chat.Tools["search"].GetInputSchema().AsMap())
	}

	none, errNone := parseChatRequest(mustJSON(t, map[string]any{
		"tool_choice": "none",
		"messages":    []map[string]any{{"role": "user", "content": "hi"}},
		"tools":       []any{weatherTool()},
	}))
	if errNone != nil {
		t.Fatalf("tool_choice none: %v", errNone)
	}
	if none.Tools != nil || len(none.ToolNames) != 0 {
		t.Fatalf("tool_choice none registered tools: %#v", none.ToolNames)
	}

	named, errNamed := parseChatRequest(mustJSON(t, map[string]any{
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}},
		"messages":    []map[string]any{{"role": "user", "content": "hi"}},
		"tools":       []any{weatherTool(), functionTool("search", map[string]any{"type": "object"})},
	}))
	if errNamed != nil {
		t.Fatalf("named tool_choice: %v", errNamed)
	}
	if len(named.ToolNames) != 1 || named.ToolNames[0] != "get_weather" || named.Tools["search"] != nil {
		t.Fatalf("named choice = %v tools=%v", named.ToolNames, named.Tools)
	}

	if _, errDup := parseChatRequest(mustJSON(t, map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"tools":    []any{weatherTool(), weatherTool()},
	})); errDup == nil {
		t.Fatal("expected duplicate tool name to fail")
	}
	if _, errEmpty := parseChatRequest(mustJSON(t, map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"tools":    []any{map[string]any{"type": "function", "function": map[string]any{"name": ""}}},
	})); errEmpty == nil {
		t.Fatal("expected empty function name to fail")
	}
	if _, errSchema := parseChatRequest(mustJSON(t, map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"tools":    []any{functionTool("get_weather", []any{"not", "an", "object"})},
	})); errSchema == nil {
		t.Fatal("expected non-object parameters to fail")
	}
	if _, errMissing := parseChatRequest(mustJSON(t, map[string]any{
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "missing"}},
		"messages":    []map[string]any{{"role": "user", "content": "hi"}},
		"tools":       []any{weatherTool()},
	})); errMissing == nil {
		t.Fatal("expected unknown named tool_choice to fail")
	}
}

func TestParseChatRequestKeepsEmptyAssistantToolCallsAndFinalToolResults(t *testing.T) {
	t.Parallel()

	chat, errParse := parseChatRequest(mustJSON(t, map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "weather?"},
			{"role": "assistant", "content": "", "tool_calls": []map[string]any{{
				"id":   "call_1",
				"type": "function",
				"function": map[string]any{
					"name":      "get_weather",
					"arguments": `{"city":"Paris"}`,
				},
			}}},
			{"role": "tool", "tool_call_id": "call_1", "content": "22C"},
		},
	}))
	if errParse != nil {
		t.Fatalf("parseChatRequest: %v", errParse)
	}
	if !strings.Contains(chat.Prompt, "Assistant requested tool get_weather with arguments {\"city\":\"Paris\"} [id call_1]") {
		t.Fatalf("prompt missing assistant tool call:\n%s", chat.Prompt)
	}
	if !strings.Contains(chat.Prompt, "Tool result for call_1: 22C") {
		t.Fatalf("prompt missing tool result:\n%s", chat.Prompt)
	}
	if len(chat.ToolResults) != 1 || chat.ToolResults[0].CallID != "call_1" || chat.ToolResults[0].Content != "22C" {
		t.Fatalf("tool results = %+v", chat.ToolResults)
	}

	onlyTools, errOnly := parseChatRequest(mustJSON(t, map[string]any{
		"messages": []map[string]any{
			{"role": "tool", "tool_call_id": "call_parked", "content": "ok"},
		},
	}))
	if errOnly != nil {
		t.Fatalf("tool-only continuation: %v", errOnly)
	}
	if len(onlyTools.ToolResults) != 1 || onlyTools.ToolResults[0].CallID != "call_parked" {
		t.Fatalf("tool-only results = %+v", onlyTools.ToolResults)
	}

	split, errSplit := parseChatRequest(mustJSON(t, map[string]any{
		"messages": []map[string]any{
			{"role": "tool", "tool_call_id": "old", "content": "stale"},
			{"role": "assistant", "content": "next"},
			{"role": "tool", "tool_call_id": "new", "content": "fresh"},
		},
	}))
	if errSplit != nil {
		t.Fatalf("split tool results: %v", errSplit)
	}
	if len(split.ToolResults) != 1 || split.ToolResults[0].CallID != "new" {
		t.Fatalf("final contiguous results = %+v, want only the trailing block", split.ToolResults)
	}
}

func TestToolCallbackRejectsWrongBearer(t *testing.T) {
	useFakeBridge(t)
	process := pooledProcess()
	if process == nil || process.callbackURL == "" {
		t.Fatal("expected a tool callback URL")
	}
	client := sdkv1connect.NewSdkCustomToolCallbackServiceClient(http.DefaultClient, process.callbackURL)
	request := connect.NewRequest(&sdkv1.CallCustomToolRequest{ToolName: "get_weather", AgentId: "agent-1"})
	request.Header().Set("Authorization", "Bearer wrong-token")
	_, errCall := client.CallCustomTool(context.Background(), request)
	if connect.CodeOf(errCall) != connect.CodeUnauthenticated {
		t.Fatalf("code = %v (%v), want unauthenticated", connect.CodeOf(errCall), errCall)
	}
}

func TestCreateAgentOffersClientCustomToolsWithEmptyBuiltIns(t *testing.T) {
	bridge := useFakeBridge(t)
	bridge.toolRounds = nil

	if _, errExecute := execute(executorRequest(t, "good-key", map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"tools":    []any{weatherTool()},
	})); errExecute != nil {
		t.Fatalf("execute: %v", errExecute)
	}
	created := bridge.createdAgents()
	if len(created) != 1 {
		t.Fatalf("created = %d, want 1", len(created))
	}
	options := created[0]
	if names := options.GetTools().GetNames(); len(names) != 1 || names[0] != "mcp" {
		t.Errorf("tool names = %v, want [mcp] so custom-user-tools can be discovered", names)
	}
	if _, ok := options.GetLocal().GetCustomTools()["get_weather"]; !ok {
		t.Errorf("custom tools = %v, want get_weather", options.GetLocal().GetCustomTools())
	}
	if len(bridge.enableDeltas) != 1 || !bridge.enableDeltas[0] {
		t.Errorf("enable_deltas = %v, want [true] for a tool-enabled run", bridge.enableDeltas)
	}
}

func TestExecuteToolCallRoundTrip(t *testing.T) {
	bridge := useFakeBridge(t)
	bridge.toolRounds = []fakeToolRound{{
		Calls: []fakeToolCall{{Name: "get_weather", Args: map[string]any{"city": "Paris"}}},
	}}

	first := decodeCompletion(t, mustExecute(t, "good-key", map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "weather in Paris?"}},
		"tools":    []any{weatherTool()},
	}))
	if finishReason(t, first) != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", finishReason(t, first))
	}
	if first.Usage != nil {
		t.Fatalf("usage = %+v, want none on a tool turn", first.Usage)
	}
	if first.Choices[0].Message == nil || len(first.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %+v", first.Choices[0].Message)
	}
	call := first.Choices[0].Message.ToolCalls[0]
	if call.Index != nil {
		t.Errorf("non-stream tool call index = %v, want omitted", call.Index)
	}
	if call.Type != "function" || call.Function.Name != "get_weather" {
		t.Errorf("tool call = %+v", call)
	}
	if got := bridge.closedAgents(); len(got) != 0 {
		t.Fatalf("closed agents = %v, want none while the callback is parked", got)
	}
	if got := bridge.deletedAgents(); len(got) != 0 {
		t.Fatalf("deleted agents = %v, want none while the callback is parked", got)
	}
	waitUntil(t, func() bool { return bridge.inflightToolCalls() == 1 })

	second := decodeCompletion(t, mustExecute(t, "good-key", map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "weather in Paris?"},
			{"role": "assistant", "content": first.Choices[0].Message.Content, "tool_calls": []map[string]any{{
				"id":       call.ID,
				"type":     "function",
				"function": map[string]any{"name": call.Function.Name, "arguments": call.Function.Arguments},
			}}},
			{"role": "tool", "tool_call_id": call.ID, "content": "22C"},
		},
		"tools": []any{weatherTool()},
	}))
	if finishReason(t, second) != "stop" {
		t.Fatalf("finish_reason = %q, want stop", finishReason(t, second))
	}
	if second.Choices[0].Message.Content != "Hello world" {
		t.Errorf("content = %q, want Hello world", second.Choices[0].Message.Content)
	}
	if second.Usage == nil || second.Usage.PromptTokens != 10 || second.Usage.CompletionTokens != 2 {
		t.Errorf("usage = %+v, want prompt=10 completion=2", second.Usage)
	}
	if got := bridge.closedAgents(); len(got) != 1 || got[0] != "agent-1" {
		t.Errorf("closed agents = %v, want [agent-1]", got)
	}
	if got := bridge.deletedAgents(); len(got) != 1 || got[0] != "agent-1" {
		t.Errorf("deleted agents = %v, want [agent-1]", got)
	}
	if !toolRegistryEmpty() {
		t.Error("tool registry still holds the finished run")
	}
}

func TestRunGenerateStreamsTextThenToolCallsThenContinuation(t *testing.T) {
	bridge := useFakeBridge(t)
	bridge.toolRounds = []fakeToolRound{{
		Deltas: []string{"Looking "},
		Calls:  []fakeToolCall{{Name: "get_weather", Args: map[string]any{"city": "Paris"}}},
	}}

	req := toolGenerateRequest(t, "good-key", "weather?")
	var firstDeltas []string
	first, errFirst := runGenerate(context.Background(), req, func(text string) error {
		firstDeltas = append(firstDeltas, text)
		return nil
	})
	if errFirst != nil {
		t.Fatalf("first turn: %v", errFirst)
	}
	if strings.Join(firstDeltas, "") != "Looking " {
		t.Errorf("first deltas = %q, want Looking ", firstDeltas)
	}
	if first.text != "Looking " || len(first.toolCalls) != 1 {
		t.Fatalf("first result = %+v", first)
	}

	firstCount := len(firstDeltas)
	var secondDeltas []string
	req.toolResults = []chatToolResult{{CallID: first.toolCalls[0].ID, Content: "22C"}}
	second, errSecond := runGenerate(context.Background(), req, func(text string) error {
		secondDeltas = append(secondDeltas, text)
		return nil
	})
	if errSecond != nil {
		t.Fatalf("continuation: %v", errSecond)
	}
	if len(firstDeltas) != firstCount {
		t.Fatalf("first onDelta ran during the continuation: %q", firstDeltas)
	}
	if strings.Join(secondDeltas, "") != "Hello world" {
		t.Errorf("continuation deltas = %q, want Hello world", secondDeltas)
	}
	if second.text != "Hello world" || len(second.toolCalls) != 0 {
		t.Fatalf("continuation = %+v", second)
	}
	_ = bridge
}

func TestOverlappingToolCallsBatchAndLateCallbackIsNextTurn(t *testing.T) {
	bridge := useFakeBridge(t)
	gate := make(chan struct{})
	bridge.toolRounds = []fakeToolRound{{
		Calls: []fakeToolCall{
			{Name: "get_weather", Args: map[string]any{"city": "Paris"}},
			{Name: "search", Args: map[string]any{"q": "today"}},
		},
		ExtraGate:  gate,
		ExtraCalls: []fakeToolCall{{Name: "search", Args: map[string]any{"q": "late"}}},
	}}

	req := generateRequest{
		apiKey:      "good-key",
		model:       "fake-model",
		optimizeFor: defaultOptimizeFor,
		prompt:      "hi",
		tools:       mustParseTools(t, weatherTool(), functionTool("search", map[string]any{"type": "object"})),
	}
	first, errFirst := runGenerate(context.Background(), req, nil)
	if errFirst != nil {
		t.Fatalf("first turn: %v", errFirst)
	}
	if len(first.toolCalls) != 2 {
		t.Fatalf("batched calls = %d, want 2", len(first.toolCalls))
	}

	close(gate)
	waitUntil(t, func() bool { return bridge.inflightToolCalls() == 3 })

	partial := req
	partial.toolResults = []chatToolResult{{CallID: first.toolCalls[0].ID, Content: "only-one"}}
	if _, errPartial := runGenerate(context.Background(), partial, nil); errPartial == nil {
		t.Fatal("expected resume with one of two results to fail")
	}
	if len(bridge.createdAgents()) != 1 {
		t.Fatalf("created = %d, want 1 after a partial resume", len(bridge.createdAgents()))
	}

	both := req
	both.toolResults = []chatToolResult{
		{CallID: first.toolCalls[0].ID, Content: "22C"},
		{CallID: first.toolCalls[1].ID, Content: "hits"},
	}
	second, errSecond := runGenerate(context.Background(), both, nil)
	if errSecond != nil {
		t.Fatalf("resume both: %v", errSecond)
	}
	if len(second.toolCalls) != 1 || second.toolCalls[0].Function.Name != "search" {
		t.Fatalf("late callback turn = %+v, want one search call", second.toolCalls)
	}

	third, errThird := runGenerate(context.Background(), generateRequest{
		apiKey:      "good-key",
		model:       "fake-model",
		optimizeFor: defaultOptimizeFor,
		toolResults: []chatToolResult{{CallID: second.toolCalls[0].ID, Content: "done"}},
	}, nil)
	if errThird != nil {
		t.Fatalf("final resume: %v", errThird)
	}
	if third.text != "Hello world" {
		t.Errorf("final text = %q", third.text)
	}
}

func TestSequentialToolRoundsOnOneSendStream(t *testing.T) {
	bridge := useFakeBridge(t)
	bridge.toolRounds = []fakeToolRound{
		{Calls: []fakeToolCall{{Name: "get_weather", Args: map[string]any{"city": "Paris"}}}},
		{Calls: []fakeToolCall{{Name: "get_weather", Args: map[string]any{"city": "London"}}}},
	}
	req := toolGenerateRequest(t, "good-key", "weather")

	first, errFirst := runGenerate(context.Background(), req, nil)
	if errFirst != nil {
		t.Fatalf("round 1: %v", errFirst)
	}
	second, errSecond := runGenerate(context.Background(), generateRequest{
		apiKey:      req.apiKey,
		model:       req.model,
		optimizeFor: req.optimizeFor,
		tools:       req.tools,
		toolResults: []chatToolResult{{CallID: first.toolCalls[0].ID, Content: "Paris 22C"}},
	}, nil)
	if errSecond != nil {
		t.Fatalf("round 2: %v", errSecond)
	}
	if len(second.toolCalls) != 1 {
		t.Fatalf("round 2 calls = %+v", second.toolCalls)
	}
	third, errThird := runGenerate(context.Background(), generateRequest{
		apiKey:      req.apiKey,
		model:       req.model,
		optimizeFor: req.optimizeFor,
		toolResults: []chatToolResult{{CallID: second.toolCalls[0].ID, Content: "London 16C"}},
	}, nil)
	if errThird != nil {
		t.Fatalf("terminal: %v", errThird)
	}
	if third.text != "Hello world" || len(bridge.createdAgents()) != 1 {
		t.Fatalf("terminal = %+v created=%d", third, len(bridge.createdAgents()))
	}
}

func TestUnknownToolCallIDStartsNewAgentWithTranscript(t *testing.T) {
	bridge := useFakeBridge(t)
	_, errExecute := execute(executorRequest(t, "good-key", map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "weather?"},
			{"role": "assistant", "content": "", "tool_calls": []map[string]any{{
				"id":   "call_unknown",
				"type": "function",
				"function": map[string]any{
					"name":      "get_weather",
					"arguments": `{"city":"Paris"}`,
				},
			}}},
			{"role": "tool", "tool_call_id": "call_unknown", "content": "22C"},
		},
	}))
	if errExecute != nil {
		t.Fatalf("execute: %v", errExecute)
	}
	prompts := bridge.recordedPrompts()
	if len(prompts) != 1 {
		t.Fatalf("prompts = %d, want 1 new one-shot agent", len(prompts))
	}
	if !strings.Contains(prompts[0], "Assistant requested tool get_weather") || !strings.Contains(prompts[0], "Tool result for call_unknown: 22C") {
		t.Fatalf("prompt missing both halves:\n%s", prompts[0])
	}
}

func TestResumeRequestErrorsDoNotStartAnotherAgent(t *testing.T) {
	bridge := useFakeBridge(t)
	bridge.toolRounds = []fakeToolRound{{
		Calls: []fakeToolCall{{Name: "get_weather", Args: map[string]any{"city": "Paris"}}},
	}}
	first, errFirst := runGenerate(context.Background(), toolGenerateRequest(t, "good-key", "hi"), nil)
	if errFirst != nil {
		t.Fatalf("park: %v", errFirst)
	}
	id := first.toolCalls[0].ID

	other, errOther := runGenerate(context.Background(), toolGenerateRequest(t, "other-key", "hi"), nil)
	if errOther != nil {
		t.Fatalf("second run: %v", errOther)
	}

	cases := []struct {
		name    string
		apiKey  string
		results []chatToolResult
	}{
		{"partial", "good-key", []chatToolResult{{CallID: id, Content: "x"}, {CallID: "call_missing", Content: "y"}}},
		{"duplicate", "good-key", []chatToolResult{{CallID: id, Content: "x"}, {CallID: id, Content: "y"}}},
		{"mixed", "good-key", []chatToolResult{{CallID: id, Content: "x"}, {CallID: other.toolCalls[0].ID, Content: "y"}}},
		{"wrong-fingerprint", "intruder-key", []chatToolResult{{CallID: id, Content: "x"}}},
	}
	createdBefore := len(bridge.createdAgents())
	for _, test := range cases {
		_, errResume := runGenerate(context.Background(), generateRequest{
			apiKey:      test.apiKey,
			model:       "fake-model",
			optimizeFor: defaultOptimizeFor,
			toolResults: test.results,
		}, nil)
		if errResume == nil {
			t.Fatalf("%s: expected request error", test.name)
		}
		if failureFrom(errResume).HTTPStatus != 400 {
			t.Errorf("%s: status = %d, want 400", test.name, failureFrom(errResume).HTTPStatus)
		}
	}

	toolRegistry.mu.Lock()
	run := toolRegistry.byAgentID["agent-1"]
	toolRegistry.mu.Unlock()
	if run == nil {
		t.Fatal("parked run missing from registry")
	}
	if _, errWaiter := run.attachWaiter(nil); errWaiter != nil {
		t.Fatalf("first waiter: %v", errWaiter)
	}
	if _, errWaiter := run.attachWaiter(nil); errWaiter == nil {
		t.Fatal("expected concurrent continuation to fail")
	}
	run.detachWaiter(run.waiter)
	if len(bridge.createdAgents()) != createdBefore {
		t.Fatalf("created agents grew from %d to %d", createdBefore, len(bridge.createdAgents()))
	}
}

func TestToolResultTimeoutReturnsIsErrorAndReleases(t *testing.T) {
	previous := toolResultTimeout
	toolResultTimeout = 20 * time.Millisecond
	t.Cleanup(func() { toolResultTimeout = previous })

	bridge := useFakeBridge(t)
	bridge.toolRounds = []fakeToolRound{{
		Calls: []fakeToolCall{{Name: "get_weather"}},
	}}
	if _, errRun := runGenerate(context.Background(), toolGenerateRequest(t, "good-key", "hi"), nil); errRun != nil {
		t.Fatalf("park: %v", errRun)
	}
	waitUntil(t, func() bool {
		return len(bridge.cancelledRuns()) == 1 && len(bridge.closedAgents()) == 1 && toolRegistryEmpty()
	})
	results := bridge.recordedToolResults()
	if len(results) == 0 || !toolResultIsError(results[len(results)-1]) {
		t.Fatalf("tool results = %+v, want isError", results)
	}
}

func TestOnDeltaErrorBeforeAnnounceFailsTheCallback(t *testing.T) {
	bridge := useFakeBridge(t)
	bridge.toolRounds = []fakeToolRound{{
		Deltas: []string{"partial"},
		Calls:  []fakeToolCall{{Name: "get_weather"}},
	}}
	_, errRun := runGenerate(context.Background(), toolGenerateRequest(t, "good-key", "hi"), func(string) error {
		waitUntil(t, func() bool { return parkedCallCount("agent-1") >= 1 })
		return errors.New("delta failed")
	})
	if errRun == nil {
		t.Fatal("expected onDelta error")
	}
	waitUntil(t, func() bool {
		return len(bridge.cancelledRuns()) == 1 && len(bridge.closedAgents()) == 1 && toolRegistryEmpty()
	})
	results := bridge.recordedToolResults()
	if len(results) == 0 || !toolResultIsError(results[len(results)-1]) {
		t.Fatalf("tool results = %+v, want isError", results)
	}
}

func TestProcessStopFailsParkedToolRuns(t *testing.T) {
	bridge := useFakeBridge(t)
	bridge.toolRounds = []fakeToolRound{{
		Calls: []fakeToolCall{{Name: "get_weather"}},
	}}
	if _, errRun := runGenerate(context.Background(), toolGenerateRequest(t, "good-key", "hi"), nil); errRun != nil {
		t.Fatalf("park: %v", errRun)
	}
	process := pooledProcess()
	select {
	case <-process.exited:
	default:
		close(process.exited)
	}
	process.stop()
	waitUntil(t, func() bool {
		return len(bridge.cancelledRuns()) == 1 && len(bridge.closedAgents()) == 1 && toolRegistryEmpty()
	})
	results := bridge.recordedToolResults()
	if len(results) == 0 || !toolResultIsError(results[len(results)-1]) {
		t.Fatalf("tool results = %+v, want isError", results)
	}
}

func TestBuildStreamChunkEmitsIndexedToolCalls(t *testing.T) {
	t.Parallel()
	idx := 0
	finish := "tool_calls"
	chunk := buildStreamChunk("chatcmpl-1", "fake-model", chatCompletionDelta{
		Role: "assistant",
		ToolCalls: []chatToolCall{{
			Index: &idx,
			ID:    "call_abc",
			Type:  "function",
			Function: chatToolCallFunction{
				Name:      "get_weather",
				Arguments: `{"city":"Paris"}`,
			},
		}},
	}, &finish, nil)
	if gjson.GetBytes(chunk, "choices.0.delta.tool_calls.0.index").Int() != 0 {
		t.Errorf("index omitted: %s", chunk)
	}
	if gjson.GetBytes(chunk, "choices.0.finish_reason").String() != "tool_calls" {
		t.Errorf("finish_reason = %s", chunk)
	}
	if framingRaw.terminator() != nil {
		t.Error("raw terminator must stay empty")
	}
}

func mustExecute(t *testing.T, apiKey string, payload map[string]any) []byte {
	t.Helper()
	raw, errExecute := execute(executorRequest(t, apiKey, payload))
	if errExecute != nil {
		t.Fatalf("execute: %v", errExecute)
	}
	return raw
}

func toolGenerateRequest(t *testing.T, apiKey, prompt string) generateRequest {
	t.Helper()
	return generateRequest{
		apiKey:      apiKey,
		model:       "fake-model",
		optimizeFor: defaultOptimizeFor,
		prompt:      prompt,
		tools:       mustParseTools(t, weatherTool()),
	}
}

func mustParseTools(t *testing.T, tools ...map[string]any) map[string]*sdkv1.CustomToolDefinition {
	t.Helper()
	chat, errParse := parseChatRequest(mustJSON(t, map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"tools":    tools,
	}))
	if errParse != nil {
		t.Fatalf("parse tools: %v", errParse)
	}
	return chat.Tools
}

func toolResultIsError(response *sdkv1.CallCustomToolResponse) bool {
	if response == nil || response.GetResult() == nil {
		return false
	}
	value, ok := response.GetResult().AsMap()["isError"].(bool)
	return ok && value
}

func pooledProcess() *bridgeProcess {
	bridgePool.mu.Lock()
	defer bridgePool.mu.Unlock()
	return bridgePool.procs[""]
}

func parkedCallCount(agentID string) int {
	toolRegistry.mu.Lock()
	run := toolRegistry.byAgentID[agentID]
	toolRegistry.mu.Unlock()
	if run == nil {
		return 0
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	return len(run.calls)
}

func toolRegistryEmpty() bool {
	toolRegistry.mu.Lock()
	defer toolRegistry.mu.Unlock()
	return len(toolRegistry.byAgentID) == 0 && len(toolRegistry.byExposedCallID) == 0
}
