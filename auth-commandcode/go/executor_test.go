package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

func TestExecuteOpenAIPassthrough(t *testing.T) {
	var requests []*http.Request
	body := []byte(`{"id":"cmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
	useRoundTripServer(t, recordedHandler(t, http.StatusOK, body, &requests))

	raw, errExec := execute(executorRequest(t, "user_ok", map[string]any{
		"model":       "deepseek/deepseek-v4-flash",
		"messages":    []map[string]any{{"role": "user", "content": "hello"}},
		"temperature": 0.2,
	}))
	if errExec != nil {
		t.Fatalf("execute: %v", errExec)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("execute failed: %+v", env.Error)
	}
	var resp pluginapi.ExecutorResponse
	if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if gjson.GetBytes(resp.Payload, "choices.0.message.content").String() != "hi" {
		t.Fatalf("payload = %s", resp.Payload)
	}
	if len(requests) != 1 {
		t.Fatalf("requests = %d", len(requests))
	}
	req := requests[0]
	if !strings.HasSuffix(req.URL.Path, "/provider/v1/chat/completions") {
		t.Fatalf("path = %s", req.URL.Path)
	}
	if req.Header.Get("Authorization") != "Bearer user_ok" {
		t.Fatalf("auth = %q", req.Header.Get("Authorization"))
	}
	rawBody, _ := io.ReadAll(req.Body)
	if gjson.GetBytes(rawBody, "temperature").Float() != 0.2 {
		t.Fatalf("body rewritten too aggressively: %s", rawBody)
	}
	if req.Header.Get("x-cmd-zdr") != "" {
		t.Fatal("zdr header should be absent by default")
	}
}

func TestExecuteDoesNotForwardClientAuthorization(t *testing.T) {
	var requests []*http.Request
	useRoundTripServer(t, recordedHandler(t, http.StatusOK, []byte(`{"choices":[]}`), &requests))
	payload, _ := json.Marshal(map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	raw, errExec := execute(mustJSON(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:       "deepseek/deepseek-v4-flash",
			Payload:     payload,
			StorageJSON: storageJSON("user_ok"),
			Headers:     http.Header{"Authorization": []string{"Bearer inbound-secret"}},
		},
	}))
	if errExec != nil {
		t.Fatalf("execute: %v", errExec)
	}
	if !decodeEnvelope(t, raw).OK {
		t.Fatalf("failed: %s", raw)
	}
	if requests[0].Header.Get("Authorization") != "Bearer user_ok" {
		t.Fatalf("leaked inbound auth: %q", requests[0].Header.Get("Authorization"))
	}
}

func TestExecuteZDRHeader(t *testing.T) {
	var requests []*http.Request
	server := useRoundTripServer(t, recordedHandler(t, http.StatusOK, []byte(`{"choices":[]}`), &requests))
	currentConfig.Store(pluginConfig{APIBase: server.URL, ZDR: true})

	raw, errExec := execute(executorRequest(t, "user_ok", map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}))
	if errExec != nil {
		t.Fatalf("execute: %v", errExec)
	}
	if !decodeEnvelope(t, raw).OK {
		t.Fatalf("failed: %s", raw)
	}
	if requests[0].Header.Get("x-cmd-zdr") != "1" {
		t.Fatalf("zdr = %q", requests[0].Header.Get("x-cmd-zdr"))
	}
}

func TestExecuteClassifiesStatuses(t *testing.T) {
	cases := []struct {
		status    int
		body      string
		want      int
		retryable bool
	}{
		{401, `{"error":{"message":"bad key","type":"authentication_error"}}`, 401, false},
		{400, `{"error":{"message":"bad model","type":"invalid_request_error","code":"unsupported_model"}}`, 400, false},
		{403, `{"error":{"message":"upgrade","type":"permission_error","code":"upgrade_required"}}`, 403, false},
		{422, `{"error":{"message":"no zdr","code":"cmd_zdr_no_providers"}}`, 422, false},
		{429, `{"error":{"message":"slow down","type":"rate_limit_error"}}`, 429, true},
		{503, `{"error":{"message":"upstream","type":"server_error"}}`, 503, true},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			var requests []*http.Request
			useRoundTripServer(t, recordedHandler(t, tc.status, []byte(tc.body), &requests))
			raw, errExec := execute(executorRequest(t, "user_x", map[string]any{
				"model":    "deepseek/deepseek-v4-flash",
				"messages": []map[string]any{{"role": "user", "content": "hi"}},
			}))
			if errExec != nil {
				t.Fatalf("execute: %v", errExec)
			}
			env := decodeEnvelope(t, raw)
			if env.OK {
				t.Fatal("expected error envelope")
			}
			if env.Error.HTTPStatus != tc.want {
				t.Fatalf("status = %d want %d (%s)", env.Error.HTTPStatus, tc.want, env.Error.Message)
			}
			if env.Error.Retryable != tc.retryable {
				t.Fatalf("retryable = %v want %v", env.Error.Retryable, tc.retryable)
			}
			if strings.Contains(env.Error.Message, "user_x") {
				t.Fatal("error echoed api key")
			}
		})
	}
}

func TestExecuteRejectsMissingMessages(t *testing.T) {
	useRoundTripServer(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("should not call upstream")
	})
	raw, errExec := execute(executorRequest(t, "user_ok", map[string]any{
		"model": "deepseek/deepseek-v4-flash",
	}))
	if errExec != nil {
		t.Fatalf("execute: %v", errExec)
	}
	env := decodeEnvelope(t, raw)
	if env.OK || env.Error.HTTPStatus != 400 {
		t.Fatalf("expected 400, got %+v", env.Error)
	}
}

func TestCountTokensAndHTTPRequest(t *testing.T) {
	raw, errCount := countTokens(executorRequest(t, "user_ok", map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "abcd"}},
	}))
	if errCount != nil {
		t.Fatalf("count: %v", errCount)
	}
	if !decodeEnvelope(t, raw).OK {
		t.Fatalf("count failed: %s", raw)
	}
	httpRaw, errHTTP := httpRequest()
	if errHTTP != nil {
		t.Fatalf("httpRequest: %v", errHTTP)
	}
	var resp pluginapi.ExecutorHTTPResponse
	if errUnmarshal := json.Unmarshal(decodeEnvelope(t, httpRaw).Result, &resp); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}
