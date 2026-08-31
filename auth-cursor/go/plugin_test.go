package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// fakeSidecar speaks the same NDJSON protocol as the real sidecar but never contacts
// Cursor, so the executor and transport can be exercised without a live credential.
const fakeSidecar = `
import { createInterface } from "node:readline";

const send = (obj) => process.stdout.write(JSON.stringify(obj) + "\n");

createInterface({ input: process.stdin }).on("line", (line) => {
  if (!line.trim()) return;
  const req = JSON.parse(line);
  if (req.op === "cancel") return;
  if (req.op === "models") {
    send({ id: req.id, event: "models", models: [{ id: "fake-model", display_name: "Fake" }] });
    return;
  }
  if (req.op === "login") {
    send({ id: req.id, event: "login_url", url: "https://cursor.com/login?challenge=test" });
    send({
      id: req.id,
      event: "login",
      api_key: "key_minted",
      email: "Dev.User+cli@example.com",
      expires_at_ms: Date.now() + 90 * 24 * 60 * 60 * 1000,
    });
    return;
  }
  if (req.api_key === "bad-key") {
    send({ id: req.id, event: "error", message: "Invalid User API Key", code: "unauthorized", http_status: 401 });
    return;
  }
  send({ id: req.id, event: "delta", text: "Hello" });
  send({ id: req.id, event: "delta", text: " world" });
  send({
    id: req.id,
    event: "done",
    status: "done",
    text: "Hello world",
    usage: { inputTokens: 7, outputTokens: 2, cacheReadTokens: 3, totalTokens: 12 },
  });
});
`

// useFakeSidecar points the plugin at the fake sidecar for the duration of a test.
func useFakeSidecar(t *testing.T) {
	t.Helper()
	node, errLook := exec.LookPath("node")
	if errLook != nil {
		t.Skipf("node is required for sidecar tests: %v", errLook)
	}
	script := filepath.Join(t.TempDir(), "index.mjs")
	if errWrite := os.WriteFile(script, []byte(fakeSidecar), 0o600); errWrite != nil {
		t.Fatalf("write fake sidecar: %v", errWrite)
	}
	currentConfig.Store(pluginConfig{NodePath: node, SidecarPath: script, OptimizeFor: defaultOptimizeFor})
	stopSidecar()
	t.Cleanup(func() {
		stopSidecar()
		currentConfig.Store(defaultPluginConfig())
	})
}

func executorRequest(t *testing.T, apiKey string, payload map[string]any) []byte {
	t.Helper()
	rawPayload, errPayload := json.Marshal(payload)
	if errPayload != nil {
		t.Fatalf("marshal payload: %v", errPayload)
	}
	raw, errMarshal := json.Marshal(rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:       "fake-model",
			Payload:     rawPayload,
			StorageJSON: []byte(`{"type":"cursor","api_key":"` + apiKey + `"}`),
		},
	})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	return raw
}

func decodeEnvelope(t *testing.T, raw []byte) envelope {
	t.Helper()
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	return env
}

func TestExecuteAssemblesCompletion(t *testing.T) {
	useFakeSidecar(t)

	raw, errExecute := execute(executorRequest(t, "good-key", map[string]any{
		"model":    "fake-model",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}))
	if errExecute != nil {
		t.Fatalf("execute: %v", errExecute)
	}
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
	if completion.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", completion.Object)
	}
	if len(completion.Choices) != 1 || completion.Choices[0].Message == nil {
		t.Fatalf("unexpected choices: %+v", completion.Choices)
	}
	if got := completion.Choices[0].Message.Content; got != "Hello world" {
		t.Errorf("content = %q, want %q", got, "Hello world")
	}
	// Cache reads are prompt tokens served from cache, so they belong on the prompt side.
	if completion.Usage == nil || completion.Usage.PromptTokens != 10 || completion.Usage.CompletionTokens != 2 {
		t.Errorf("usage = %+v, want prompt=10 completion=2", completion.Usage)
	}
}

func TestExecuteSurfacesUpstreamStatus(t *testing.T) {
	useFakeSidecar(t)

	raw, errExecute := execute(executorRequest(t, "bad-key", map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}))
	if errExecute != nil {
		t.Fatalf("execute: %v", errExecute)
	}
	env := decodeEnvelope(t, raw)
	if env.OK {
		t.Fatal("expected an error envelope for a rejected key")
	}
	if !strings.Contains(env.Error.Message, "401") || !strings.Contains(env.Error.Message, "Invalid User API Key") {
		t.Errorf("message = %q, want upstream status and reason", env.Error.Message)
	}
}

func TestCallSidecarStreamsDeltasThenDone(t *testing.T) {
	useFakeSidecar(t)

	events, release, errCall := callSidecar(context.Background(), sidecarRequest{
		Op:     "generate",
		APIKey: "good-key",
		Model:  "fake-model",
		Prompt: "hi",
		Stream: true,
	})
	if errCall != nil {
		t.Fatalf("callSidecar: %v", errCall)
	}
	defer release()

	var deltas []string
	var sawDone bool
	for event := range events {
		switch event.Event {
		case "delta":
			deltas = append(deltas, event.Text)
		case "done":
			sawDone = true
		case "error":
			t.Fatalf("unexpected error event: %s", event.errorText())
		}
	}
	if strings.Join(deltas, "") != "Hello world" {
		t.Errorf("deltas = %q, want incremental \"Hello world\"", deltas)
	}
	if !sawDone {
		t.Error("stream ended without a done event")
	}
}

func TestBuildStreamChunkEmitsBareChunkJSON(t *testing.T) {
	finish := "stop"
	chunk := buildStreamChunk("chatcmpl-1", "fake-model", chatCompletionDelta{Content: "Hello"}, &finish, nil)
	// The chat-completions SSE writer adds the framing, so the chunk itself must stay bare.
	if strings.HasPrefix(string(chunk), "data:") {
		t.Fatalf("chunk carries SSE framing: %q", chunk)
	}
	var parsed chatCompletion
	if errUnmarshal := json.Unmarshal(chunk, &parsed); errUnmarshal != nil {
		t.Fatalf("decode chunk: %v", errUnmarshal)
	}
	if parsed.Object != "chat.completion.chunk" {
		t.Errorf("object = %q, want chat.completion.chunk", parsed.Object)
	}
	if parsed.Choices[0].Delta == nil || parsed.Choices[0].Delta.Content != "Hello" {
		t.Errorf("delta = %+v, want content Hello", parsed.Choices[0].Delta)
	}
	if parsed.Choices[0].FinishReason == nil || *parsed.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %v, want stop", parsed.Choices[0].FinishReason)
	}
}

func TestStreamFramingMatchesClientProtocol(t *testing.T) {
	for _, testCase := range []struct {
		path string
		want streamFraming
	}{
		// Chat-completions clients receive chunks verbatim and the host adds the framing.
		{"/v1/chat/completions", framingRaw},
		{"/v1/completions", framingRaw},
		{"", framingRaw},
		// Everything else is translated first, and translators only accept SSE frames.
		{"/v1/messages", framingSSE},
		{"/v1/responses", framingSSE},
		{"/v1beta/models/fake-model:streamGenerateContent", framingSSE},
	} {
		got := streamFramingFor(pluginapi.ExecutorRequest{
			Metadata: map[string]any{"request_path": testCase.path},
		})
		if got != testCase.want {
			t.Errorf("framing for %q = %v, want %v", testCase.path, got, testCase.want)
		}
	}
}

func TestStreamFramingWrapsAndTerminatesOnlyForTranslatedClients(t *testing.T) {
	payload := []byte(`{"id":"chatcmpl-1"}`)
	if got := framingRaw.frame(payload); string(got) != string(payload) {
		t.Errorf("raw framing altered the chunk: %q", got)
	}
	if got := framingRaw.terminator(); len(got) != 0 {
		// A second [DONE] would reach the client after the host writes its own.
		t.Errorf("raw framing emitted a terminator: %q", got)
	}
	if got := string(framingSSE.frame(payload)); got != "data: "+string(payload)+"\n\n" {
		t.Errorf("sse framing = %q, want a data frame", got)
	}
	if got := string(framingSSE.terminator()); got != "data: [DONE]\n\n" {
		t.Errorf("sse terminator = %q, want data: [DONE]", got)
	}
}

func TestParseChatRequestForwardsLoneUserMessageVerbatim(t *testing.T) {
	chat, errParse := parseChatRequest([]byte(`{"messages":[{"role":"user","content":"ping"}]}`))
	if errParse != nil {
		t.Fatalf("parseChatRequest: %v", errParse)
	}
	if chat.Prompt != "ping" {
		t.Errorf("prompt = %q, want %q", chat.Prompt, "ping")
	}
}

func TestParseChatRequestRendersTranscript(t *testing.T) {
	payload := []byte(`{
		"reasoning_effort": "high",
		"messages": [
			{"role": "system", "content": "Be terse."},
			{"role": "user", "content": "first"},
			{"role": "assistant", "content": "second"},
			{"role": "user", "content": [
				{"type": "text", "text": "third"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,QUJD"}}
			]}
		]
	}`)
	chat, errParse := parseChatRequest(payload)
	if errParse != nil {
		t.Fatalf("parseChatRequest: %v", errParse)
	}
	for _, want := range []string{"Be terse.", "User: first", "Assistant: second", "User: third"} {
		if !strings.Contains(chat.Prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, chat.Prompt)
		}
	}
	if len(chat.Images) != 1 || chat.Images[0].Data != "QUJD" || chat.Images[0].MimeType != "image/png" {
		t.Errorf("images = %+v, want one inline png", chat.Images)
	}
	if len(chat.Params) != 1 || chat.Params[0].ID != "reasoning_effort" || chat.Params[0].Value != "high" {
		t.Errorf("params = %+v, want reasoning_effort=high", chat.Params)
	}
}

func TestParseImageAcceptsRemoteAndInline(t *testing.T) {
	remote, ok := parseImage("https://example.com/a.png")
	if !ok || remote.URL != "https://example.com/a.png" || remote.Data != "" {
		t.Errorf("remote = %+v ok=%v, want URL form", remote, ok)
	}
	inline, ok := parseImage("data:image/jpeg;base64,QUJD")
	if !ok || inline.MimeType != "image/jpeg" || inline.Data != "QUJD" {
		t.Errorf("inline = %+v ok=%v, want inline form", inline, ok)
	}
	if _, ok := parseImage("data:image/png,QUJD"); ok {
		t.Error("expected non-base64 data URL to be rejected")
	}
}

func TestParseAuthClaimsOnlyCursorFiles(t *testing.T) {
	raw, errParse := parseAuth(mustJSON(t, pluginapi.AuthParseRequest{
		FileName: "cursor-main.json",
		RawJSON:  []byte(`{"type":"gemini","api_key":"x"}`),
	}))
	if errParse != nil {
		t.Fatalf("parseAuth: %v", errParse)
	}
	var response pluginapi.AuthParseResponse
	if errUnmarshal := json.Unmarshal(decodeEnvelope(t, raw).Result, &response); errUnmarshal != nil {
		t.Fatalf("decode auth response: %v", errUnmarshal)
	}
	if response.Handled {
		t.Error("plugin claimed an auth file belonging to another provider")
	}

	raw, errParse = parseAuth(mustJSON(t, pluginapi.AuthParseRequest{
		FileName: "cursor-main.json",
		RawJSON:  []byte(`{"type":"cursor","apiKey":"key_123","label":"team"}`),
	}))
	if errParse != nil {
		t.Fatalf("parseAuth: %v", errParse)
	}
	response = pluginapi.AuthParseResponse{}
	if errUnmarshal := json.Unmarshal(decodeEnvelope(t, raw).Result, &response); errUnmarshal != nil {
		t.Fatalf("decode auth response: %v", errUnmarshal)
	}
	if !response.Handled {
		t.Fatal("plugin declined a cursor auth file")
	}
	if response.Auth.Provider != providerIdentifier || response.Auth.Label != "team" {
		t.Errorf("auth = %+v, want cursor provider labelled team", response.Auth)
	}
}

func TestCommandLineRegisterDeclaresLoginFlag(t *testing.T) {
	raw, errRegister := commandLineRegister()
	if errRegister != nil {
		t.Fatalf("commandLineRegister: %v", errRegister)
	}
	var response pluginapi.CommandLineRegistrationResponse
	if errUnmarshal := json.Unmarshal(decodeEnvelope(t, raw).Result, &response); errUnmarshal != nil {
		t.Fatalf("decode registration: %v", errUnmarshal)
	}
	if len(response.Flags) != 1 {
		t.Fatalf("flags = %+v, want exactly one", response.Flags)
	}
	if response.Flags[0].Name != loginFlagName || response.Flags[0].Type != "bool" {
		t.Errorf("flag = %+v, want bool %s", response.Flags[0], loginFlagName)
	}
}

func TestCommandLineExecuteReturnsLoginAuth(t *testing.T) {
	useFakeSidecar(t)

	raw, errExecute := commandLineExecute(mustJSON(t, pluginapi.CommandLineExecutionRequest{
		Program: "cli-proxy-api",
		Args:    []string{"--" + loginFlagName},
		Flags: map[string]pluginapi.CommandLineFlagValue{
			"no-browser": {Name: "no-browser", Value: "true"},
		},
		TriggeredFlags: map[string]pluginapi.CommandLineFlagValue{
			loginFlagName: {Name: loginFlagName, Type: "bool", Value: "true", Set: true},
		},
	}))
	if errExecute != nil {
		t.Fatalf("commandLineExecute: %v", errExecute)
	}
	var response pluginapi.CommandLineExecutionResponse
	if errUnmarshal := json.Unmarshal(decodeEnvelope(t, raw).Result, &response); errUnmarshal != nil {
		t.Fatalf("decode execution: %v", errUnmarshal)
	}
	if response.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %s", response.ExitCode, response.Stderr)
	}
	if len(response.Auths) != 1 {
		t.Fatalf("auths = %+v, want exactly one", response.Auths)
	}
	auth := response.Auths[0]
	if auth.Provider != providerIdentifier {
		t.Errorf("provider = %q, want %q", auth.Provider, providerIdentifier)
	}
	// The file name is derived from the account so a repeat login replaces its credential.
	if auth.FileName != "cursor-dev.user-cli-example.com.json" || auth.ID != auth.FileName {
		t.Errorf("file name = %q id = %q, want account-derived name", auth.FileName, auth.ID)
	}
	if auth.Label != "Dev.User+cli@example.com" {
		t.Errorf("label = %q, want the account email", auth.Label)
	}
	if got := apiKeyFromStorage(auth.StorageJSON); got != "key_minted" {
		t.Errorf("stored api key = %q, want key_minted", got)
	}
	// The minted key is short-lived, so its expiry must reach both the stored credential and
	// the host's refresh schedule.
	storedExpiry := expiryFromStorage(auth.StorageJSON)
	if storedExpiry.IsZero() || !storedExpiry.After(time.Now().UTC()) {
		t.Errorf("stored expiry = %s, want a future deadline", storedExpiry)
	}
	if !auth.NextRefreshAfter.Equal(storedExpiry) {
		t.Errorf("next refresh = %s, want the key expiry %s", auth.NextRefreshAfter, storedExpiry)
	}
	// The parser must accept what the login produced, otherwise the saved file would be
	// claimed by nobody on the next startup.
	parsed, errParse := parseAuth(mustJSON(t, pluginapi.AuthParseRequest{
		FileName: auth.FileName,
		RawJSON:  auth.StorageJSON,
	}))
	if errParse != nil {
		t.Fatalf("parseAuth: %v", errParse)
	}
	var parseResponse pluginapi.AuthParseResponse
	if errUnmarshal := json.Unmarshal(decodeEnvelope(t, parsed).Result, &parseResponse); errUnmarshal != nil {
		t.Fatalf("decode auth response: %v", errUnmarshal)
	}
	if !parseResponse.Handled {
		t.Error("parseAuth declined the credential produced by the login flow")
	}
}

func TestCommandLineExecuteIgnoresUntriggeredInvocation(t *testing.T) {
	raw, errExecute := commandLineExecute(mustJSON(t, pluginapi.CommandLineExecutionRequest{
		Program: "cli-proxy-api",
	}))
	if errExecute != nil {
		t.Fatalf("commandLineExecute: %v", errExecute)
	}
	var response pluginapi.CommandLineExecutionResponse
	if errUnmarshal := json.Unmarshal(decodeEnvelope(t, raw).Result, &response); errUnmarshal != nil {
		t.Fatalf("decode execution: %v", errUnmarshal)
	}
	if len(response.Auths) != 0 || response.ExitCode != 0 || len(response.Stdout) != 0 {
		t.Errorf("response = %+v, want a no-op", response)
	}
}

func TestParseAuthRejectsExpiredKey(t *testing.T) {
	expired := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	raw, errParse := parseAuth(mustJSON(t, pluginapi.AuthParseRequest{
		FileName: "cursor-main.json",
		RawJSON:  []byte(`{"type":"cursor","api_key":"key_old","expires_at":"` + expired + `"}`),
	}))
	if errParse != nil {
		t.Fatalf("parseAuth: %v", errParse)
	}
	env := decodeEnvelope(t, raw)
	if env.OK {
		t.Fatal("expected an expired key to be rejected")
	}
	if !strings.Contains(env.Error.Message, loginFlagName) {
		t.Errorf("message = %q, want a pointer at --%s", env.Error.Message, loginFlagName)
	}
}

func TestRefreshAuthReportsExpiry(t *testing.T) {
	expiry := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
	raw, errRefresh := refreshAuth(mustJSON(t, pluginapi.AuthRefreshRequest{
		AuthID:      "cursor-main.json",
		StorageJSON: []byte(`{"type":"cursor","api_key":"key_live","expires_at":"` + expiry.Format(time.RFC3339) + `"}`),
	}))
	if errRefresh != nil {
		t.Fatalf("refreshAuth: %v", errRefresh)
	}
	var response pluginapi.AuthRefreshResponse
	if errUnmarshal := json.Unmarshal(decodeEnvelope(t, raw).Result, &response); errUnmarshal != nil {
		t.Fatalf("decode refresh: %v", errUnmarshal)
	}
	if !response.NextRefreshAfter.Equal(expiry) {
		t.Errorf("next refresh = %s, want the key expiry %s", response.NextRefreshAfter, expiry)
	}
}

func TestParseAuthKeepsUndatedKeysAlive(t *testing.T) {
	raw, errParse := parseAuth(mustJSON(t, pluginapi.AuthParseRequest{
		FileName: "cursor-main.json",
		RawJSON:  []byte(`{"type":"cursor","api_key":"key_dashboard"}`),
	}))
	if errParse != nil {
		t.Fatalf("parseAuth: %v", errParse)
	}
	var response pluginapi.AuthParseResponse
	if errUnmarshal := json.Unmarshal(decodeEnvelope(t, raw).Result, &response); errUnmarshal != nil {
		t.Fatalf("decode auth response: %v", errUnmarshal)
	}
	if !response.Handled {
		t.Fatal("a dashboard key without an expiry was declined")
	}
	if !response.Auth.NextRefreshAfter.After(time.Now().UTC().Add(300 * 24 * time.Hour)) {
		t.Errorf("next refresh = %s, want a far-future deadline", response.Auth.NextRefreshAfter)
	}
}

func TestParseAuthRejectsMissingKey(t *testing.T) {
	raw, errParse := parseAuth(mustJSON(t, pluginapi.AuthParseRequest{
		FileName: "cursor-main.json",
		RawJSON:  []byte(`{"type":"cursor"}`),
	}))
	if errParse != nil {
		t.Fatalf("parseAuth: %v", errParse)
	}
	if env := decodeEnvelope(t, raw); env.OK {
		t.Error("expected a keyless cursor auth file to be rejected")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	return raw
}
