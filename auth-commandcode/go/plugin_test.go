package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

func decodeEnvelope(t *testing.T, raw []byte) envelope {
	t.Helper()
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	return env
}

func storageJSON(apiKey string) []byte {
	raw, _ := json.Marshal(map[string]any{"type": providerIdentifier, "api_key": apiKey})
	return raw
}

func resetCatalog(t *testing.T) {
	t.Helper()
	modelCatalogs.mu.Lock()
	modelCatalogs.entries = make(map[string]catalogEntry)
	modelCatalogs.inflight = make(map[string]*catalogWait)
	modelCatalogs.mu.Unlock()
}

func useRoundTripServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	resetCatalog(t)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	currentConfig.Store(pluginConfig{APIBase: server.URL})
	previous := roundTrip
	roundTrip = func(ctx context.Context, _ string, req *http.Request) (*http.Response, error) {
		cloned := req.Clone(ctx)
		if req.Body != nil {
			body, errRead := io.ReadAll(req.Body)
			if errRead != nil {
				return nil, errRead
			}
			_ = req.Body.Close()
			cloned.Body = io.NopCloser(bytes.NewReader(body))
			cloned.ContentLength = int64(len(body))
		}
		return server.Client().Do(cloned)
	}
	t.Cleanup(func() {
		roundTrip = previous
		currentConfig.Store(defaultPluginConfig())
	})
	return server
}

func executorRequest(t *testing.T, apiKey string, payload map[string]any) []byte {
	t.Helper()
	rawPayload, errPayload := json.Marshal(payload)
	if errPayload != nil {
		t.Fatalf("marshal payload: %v", errPayload)
	}
	model := "deepseek/deepseek-v4-flash"
	if named, ok := payload["model"].(string); ok && strings.TrimSpace(named) != "" {
		model = named
	}
	raw, errMarshal := json.Marshal(rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:       model,
			Payload:     rawPayload,
			StorageJSON: storageJSON(apiKey),
		},
	})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	return raw
}

func recordedHandler(t *testing.T, status int, body []byte, into *[]*http.Request) http.HandlerFunc {
	t.Helper()
	var mu sync.Mutex
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		cloned := r.Clone(r.Context())
		cloned.Body = io.NopCloser(bytes.NewReader(raw))
		r.Body = io.NopCloser(bytes.NewReader(raw))
		mu.Lock()
		*into = append(*into, cloned)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

func modelsListBody(ids ...string) []byte {
	type item struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		ContextLength int    `json:"context_length"`
	}
	data := make([]item, 0, len(ids))
	for _, id := range ids {
		data = append(data, item{ID: id, Name: id, ContextLength: 128000})
	}
	raw, _ := json.Marshal(map[string]any{"object": "list", "data": data})
	return raw
}

func mustGJSON(t *testing.T, raw []byte, path string) string {
	t.Helper()
	if !gjson.ValidBytes(raw) {
		t.Fatalf("invalid json: %s", raw)
	}
	return gjson.GetBytes(raw, path).String()
}
