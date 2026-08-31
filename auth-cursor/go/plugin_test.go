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
	"github.com/tidwall/gjson"
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

// loginExecute runs the login command against the fake sidecar with authDir as the host's
// auth directory, which is where the merge looks for the file it is about to replace.
func loginExecute(t *testing.T, authDir string) pluginapi.CommandLineExecutionResponse {
	t.Helper()
	raw, errExecute := commandLineExecute(mustJSON(t, pluginapi.CommandLineExecutionRequest{
		Program: "cli-proxy-api",
		Args:    []string{"--" + loginFlagName},
		Host:    pluginapi.HostConfigSummary{AuthDir: authDir},
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
	return response
}

// Renewing a key must not discard the routing settings an operator put in the auth file,
// because the host rewrites that file from what this command returns.
func TestCommandLineExecutePreservesExistingAuthFileSettings(t *testing.T) {
	useFakeSidecar(t)
	authDir := t.TempDir()
	existing := map[string]any{
		"type":       providerIdentifier,
		"api_key":    "key_previous",
		"email":      "Dev.User+cli@example.com",
		"expires_at": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
		"label":      "team pool",
		"prefix":     "cursor-a",
		"proxy_url":  "socks5://127.0.0.1:1080",
		"note":       "shared account",
		"model_aliases": []map[string]string{
			{"name": "grok-4.3", "alias": "grok-latest"},
		},
	}
	fileName := "cursor-dev.user-cli-example.com.json"
	if errWrite := os.WriteFile(filepath.Join(authDir, fileName), mustJSON(t, existing), 0o600); errWrite != nil {
		t.Fatalf("write existing auth file: %v", errWrite)
	}

	auth := loginExecute(t, authDir).Auths[0]
	if auth.FileName != fileName {
		t.Fatalf("file name = %q, want %q so the login replaces the same file", auth.FileName, fileName)
	}

	// The freshly minted credential has to win over everything the old file recorded.
	if got := apiKeyFromStorage(auth.StorageJSON); got != "key_minted" {
		t.Errorf("api key = %q, want the newly minted key_minted", got)
	}
	expiry := expiryFromStorage(auth.StorageJSON)
	if expiry.IsZero() || !expiry.After(time.Now().UTC()) {
		t.Errorf("expiry = %s, want the new future deadline rather than the stale one", expiry)
	}

	// Operator-owned settings have to survive.
	for field, want := range map[string]string{
		"label":     "team pool",
		"prefix":    "cursor-a",
		"proxy_url": "socks5://127.0.0.1:1080",
		"note":      "shared account",
	} {
		if got := gjson.GetBytes(auth.StorageJSON, field).String(); got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}
	if got := gjson.GetBytes(auth.StorageJSON, "model_aliases.0.alias").String(); got != "grok-latest" {
		t.Errorf("model_aliases.0.alias = %q, want grok-latest", got)
	}
	if auth.Prefix != "cursor-a" {
		t.Errorf("auth prefix = %q, want the preserved cursor-a", auth.Prefix)
	}
	if auth.Label != "team pool" {
		t.Errorf("auth label = %q, want the preserved label", auth.Label)
	}
}

// A stale expiry recorded under a different spelling must not survive, otherwise a renewed
// credential could be reported as already expired.
func TestCommandLineExecuteClearsStaleCredentialSpellings(t *testing.T) {
	useFakeSidecar(t)
	authDir := t.TempDir()
	fileName := "cursor-dev.user-cli-example.com.json"
	existing := map[string]any{
		"type":              providerIdentifier,
		"apiKey":            "key_previous",
		"expiresAt":         time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
		"apiKeyExpiresAtMs": time.Now().UTC().Add(-time.Hour).UnixMilli(),
		"prefix":            "cursor-a",
	}
	if errWrite := os.WriteFile(filepath.Join(authDir, fileName), mustJSON(t, existing), 0o600); errWrite != nil {
		t.Fatalf("write existing auth file: %v", errWrite)
	}

	auth := loginExecute(t, authDir).Auths[0]
	for _, field := range []string{"apiKey", "expiresAt", "expires_at_ms", "apiKeyExpiresAtMs"} {
		if gjson.GetBytes(auth.StorageJSON, field).Exists() {
			t.Errorf("%s survived the merge: %s", field, auth.StorageJSON)
		}
	}
	if expiry := expiryFromStorage(auth.StorageJSON); expiry.IsZero() || !expiry.After(time.Now().UTC()) {
		t.Errorf("expiry = %s, want the new future deadline", expiry)
	}
	if got := gjson.GetBytes(auth.StorageJSON, "prefix").String(); got != "cursor-a" {
		t.Errorf("prefix = %q, want it preserved alongside the credential reset", got)
	}
}

func TestCommandLineExecuteWithoutExistingFile(t *testing.T) {
	useFakeSidecar(t)

	for _, test := range []struct {
		name    string
		authDir func(t *testing.T) string
	}{
		{name: "empty auth dir", authDir: func(*testing.T) string { return "" }},
		{name: "no file for this account", authDir: func(t *testing.T) string { return t.TempDir() }},
		{
			name: "malformed existing file",
			authDir: func(t *testing.T) string {
				dir := t.TempDir()
				path := filepath.Join(dir, "cursor-dev.user-cli-example.com.json")
				if errWrite := os.WriteFile(path, []byte("{not json"), 0o600); errWrite != nil {
					t.Fatalf("write malformed auth file: %v", errWrite)
				}
				return dir
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			auth := loginExecute(t, test.authDir(t)).Auths[0]
			if got := apiKeyFromStorage(auth.StorageJSON); got != "key_minted" {
				t.Errorf("api key = %q, want key_minted", got)
			}
			if got := gjson.GetBytes(auth.StorageJSON, "email").String(); got != "Dev.User+cli@example.com" {
				t.Errorf("email = %q, want the account from the login", got)
			}
		})
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

// useWeights installs a plugin config from YAML so the tests exercise the same decoding and
// normalization the host performs when it hands the plugin its config block.
func useWeights(t *testing.T, raw string) {
	t.Helper()
	cfg, errDecode := decodeConfig([]byte(raw))
	if errDecode != nil {
		t.Fatalf("decodeConfig: %v", errDecode)
	}
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(defaultPluginConfig()) })
}

func parseAuthData(t *testing.T, fileName, storage string) pluginapi.AuthData {
	t.Helper()
	raw, errParse := parseAuth(mustJSON(t, pluginapi.AuthParseRequest{
		FileName: fileName,
		RawJSON:  []byte(storage),
	}))
	if errParse != nil {
		t.Fatalf("parseAuth: %v", errParse)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("parseAuth failed: %+v", env.Error)
	}
	var response pluginapi.AuthParseResponse
	if errUnmarshal := json.Unmarshal(env.Result, &response); errUnmarshal != nil {
		t.Fatalf("decode auth response: %v", errUnmarshal)
	}
	if !response.Handled {
		t.Fatal("parseAuth declined a cursor auth file")
	}
	return response.Auth
}

func refreshAuthData(t *testing.T, req pluginapi.AuthRefreshRequest) pluginapi.AuthData {
	t.Helper()
	raw, errRefresh := refreshAuth(mustJSON(t, req))
	if errRefresh != nil {
		t.Fatalf("refreshAuth: %v", errRefresh)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("refreshAuth failed: %+v", env.Error)
	}
	var response pluginapi.AuthRefreshResponse
	if errUnmarshal := json.Unmarshal(env.Result, &response); errUnmarshal != nil {
		t.Fatalf("decode refresh: %v", errUnmarshal)
	}
	return response.Auth
}

func TestParseAuthAppliesConfiguredWeight(t *testing.T) {
	useWeights(t, "weights:\n  dev@example.com: 5\n  cursor-team.json: 3\n  cursor-solo: 7\n")

	tests := []struct {
		name     string
		fileName string
		storage  string
		want     string
	}{
		{
			name:     "account email",
			fileName: "cursor-dev.json",
			storage:  `{"type":"cursor","api_key":"key_a","email":"dev@example.com"}`,
			want:     "5",
		},
		{
			name:     "email matched case-insensitively",
			fileName: "cursor-dev.json",
			storage:  `{"type":"cursor","api_key":"key_a","email":"Dev@Example.com"}`,
			want:     "5",
		},
		{
			name:     "auth file name",
			fileName: "cursor-team.json",
			storage:  `{"type":"cursor","api_key":"key_b"}`,
			want:     "3",
		},
		{
			name:     "auth file name without the json suffix",
			fileName: "cursor-solo.json",
			storage:  `{"type":"cursor","api_key":"key_c"}`,
			want:     "7",
		},
		{
			name:     "email outranks the file name",
			fileName: "cursor-team.json",
			storage:  `{"type":"cursor","api_key":"key_d","email":"dev@example.com"}`,
			want:     "5",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			auth := parseAuthData(t, test.fileName, test.storage)
			if got := auth.Attributes[weightAttribute]; got != test.want {
				t.Fatalf("weight = %q, want %q", got, test.want)
			}
		})
	}
}

// An unconfigured credential must carry no weight attribute at all, because the host reads
// an absent attribute as its default share rather than as zero.
func TestParseAuthOmitsWeightWhenUnconfigured(t *testing.T) {
	useWeights(t, "weights:\n  other@example.com: 4\n")

	auth := parseAuthData(t, "cursor-main.json", `{"type":"cursor","api_key":"key_a","email":"dev@example.com"}`)
	if got, exists := auth.Attributes[weightAttribute]; exists {
		t.Fatalf("weight = %q, want no attribute", got)
	}
}

// A non-positive weight is the documented way to park a credential while the weighted
// strategy is active, so it has to reach the host as an explicit zero.
func TestParseAuthWritesZeroForNonPositiveWeight(t *testing.T) {
	useWeights(t, "weights:\n  zeroed@example.com: 0\n  parked@example.com: -3\n")

	for _, email := range []string{"zeroed@example.com", "parked@example.com"} {
		auth := parseAuthData(t, "cursor-main.json", `{"type":"cursor","api_key":"key_a","email":"`+email+`"}`)
		if got := auth.Attributes[weightAttribute]; got != "0" {
			t.Errorf("%s weight = %q, want 0", email, got)
		}
	}
}

func TestDecodeConfigRejectsUnusableWeights(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "above the host maximum", raw: "weights:\n  dev@example.com: 1000001\n"},
		{name: "fractional", raw: "weights:\n  dev@example.com: 1.5\n"},
		{name: "non-numeric", raw: "weights:\n  dev@example.com: heavy\n"},
		{name: "empty credential key", raw: "weights:\n  \"  \": 5\n"},
		{name: "keys collide once normalized", raw: "weights:\n  Dev@Example.com: 5\n  dev@example.com: 2\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, errDecode := decodeConfig([]byte(test.raw)); errDecode == nil {
				t.Fatal("decodeConfig accepted a weight the host would reject")
			}
		})
	}
}

// A refresh must re-read the config instead of echoing the attribute it was handed, so that
// an edited weight takes effect without waiting for the credential to be parsed again.
func TestRefreshAuthReresolvesWeight(t *testing.T) {
	useWeights(t, "weights:\n  dev@example.com: 6\n")

	auth := refreshAuthData(t, pluginapi.AuthRefreshRequest{
		AuthID:      "cursor-dev.json",
		StorageJSON: []byte(`{"type":"cursor","api_key":"key_live","email":"dev@example.com"}`),
		Attributes:  map[string]string{weightAttribute: "1", "path": "/auths/cursor-dev.json"},
	})
	if got := auth.Attributes[weightAttribute]; got != "6" {
		t.Errorf("weight = %q, want the currently configured 6", got)
	}
	if got := auth.Attributes["path"]; got != "/auths/cursor-dev.json" {
		t.Errorf("path = %q, want the host attribute preserved", got)
	}
}

func TestRefreshAuthDropsWeightRemovedFromConfig(t *testing.T) {
	useWeights(t, "weights: {}\n")

	auth := refreshAuthData(t, pluginapi.AuthRefreshRequest{
		AuthID:      "cursor-dev.json",
		StorageJSON: []byte(`{"type":"cursor","api_key":"key_live","email":"dev@example.com"}`),
		Attributes:  map[string]string{weightAttribute: "9", "path": "/auths/cursor-dev.json"},
	})
	if got, exists := auth.Attributes[weightAttribute]; exists {
		t.Errorf("weight = %q, want the stale attribute dropped", got)
	}
	if got := auth.Attributes["path"]; got != "/auths/cursor-dev.json" {
		t.Errorf("path = %q, want the host attribute preserved", got)
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
