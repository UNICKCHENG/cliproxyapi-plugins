package main

import (
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	sdkv1 "github.com/UNICKCHENG/cliproxyapi-plugins/auth-cursor/go/internal/sdk/v1"
)

func TestRequestLogFieldsIdentifyTheCredentialAndModel(t *testing.T) {
	logCtx := newRequestLogContext(pluginapi.ExecutorRequest{
		AuthID:       "cursor-main.json",
		AuthProvider: "cursor",
		StorageJSON:  []byte(`{"type":"cursor","api_key":"key_live_secret","email":"ops@example.com"}`),
		Stream:       true,
		SourceFormat: "openai",
		Metadata:     map[string]any{"request_path": "/v1/messages"},
	}, "claude-4.5-sonnet")
	logCtx.ttft = 250 * time.Millisecond

	fields := requestLogFields(logCtx, requestLogOutcome{
		latency: 2 * time.Second,
		usage: &sdkv1.TokenUsage{
			InputTokens:     7,
			OutputTokens:    2,
			CacheReadTokens: 3,
		},
	})

	expected := map[string]any{
		"auth_id":       "cursor-main.json",
		"auth_provider": "cursor",
		"auth_label":    "ops@example.com",
		"model":         "claude-4.5-sonnet",
		"stream":        true,
		"source":        "openai",
		"request_path":  "/v1/messages",
		"latency_ms":    int64(2000),
		"ttft_ms":       int64(250),
		"input_tokens":  int64(10),
		"output_tokens": int64(2),
		"total_tokens":  int64(12),
	}
	for key, want := range expected {
		if got := fields[key]; got != want {
			t.Fatalf("field %s = %v, want %v", key, got, want)
		}
	}
	if _, present := fields["failed"]; present {
		t.Fatalf("a completed request must not be marked failed: %v", fields)
	}
}

func TestRequestLogFieldsNeverCarryTheApiKey(t *testing.T) {
	logCtx := newRequestLogContext(pluginapi.ExecutorRequest{
		AuthID:      "cursor-main.json",
		StorageJSON: []byte(`{"type":"cursor","api_key":"key_live_secret"}`),
	}, "auto")

	for key, value := range requestLogFields(logCtx, requestLogOutcome{}) {
		text, isText := value.(string)
		if isText && strings.Contains(text, "key_live_secret") {
			t.Fatalf("field %s leaked the api key: %q", key, text)
		}
	}
}

func TestRequestLogFieldsTruncateALongFailure(t *testing.T) {
	fields := requestLogFields(requestLogContext{}, requestLogOutcome{
		err: strings.Repeat("上", requestLogErrorLimit+50),
	})

	if fields["failed"] != true {
		t.Fatalf("a failure must be marked failed: %v", fields)
	}
	message, isText := fields["error"].(string)
	if !isText {
		t.Fatalf("error field is %T, want string", fields["error"])
	}
	if want := strings.Repeat("上", requestLogErrorLimit) + "..."; message != want {
		t.Fatalf("error = %q, want it clipped on a rune boundary", message)
	}
}
