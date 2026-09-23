package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestModelsForAuth(t *testing.T) {
	var requests []*http.Request
	useRoundTripServer(t, recordedHandler(t, http.StatusOK, modelsListBody("deepseek/deepseek-v4-flash", "claude-sonnet-4-6", "deepseek/deepseek-v4-flash"), &requests))

	raw, errModels := modelsForAuth(mustJSON(t, pluginapi.AuthModelRequest{
		AuthID:      "acct.json",
		StorageJSON: storageJSON("user_test"),
		Host: pluginapi.HostConfigSummary{
			ExcludedModels: map[string][]string{providerIdentifier: {"claude-*"}},
		},
	}))
	if errModels != nil {
		t.Fatalf("modelsForAuth: %v", errModels)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("models failed: %+v", env.Error)
	}
	var resp pluginapi.ModelResponse
	if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if resp.Provider != providerIdentifier {
		t.Fatalf("provider = %q", resp.Provider)
	}
	if len(resp.Models) != 1 || resp.Models[0].ID != "deepseek/deepseek-v4-flash" {
		t.Fatalf("models = %+v", resp.Models)
	}
	if resp.Models[0].OwnedBy != providerIdentifier {
		t.Fatalf("owned_by = %q", resp.Models[0].OwnedBy)
	}
}

func TestCatalogCacheAndFailureKeepsLast(t *testing.T) {
	var calls atomic.Int32
	useRoundTripServer(t, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(modelsListBody("kept-model"))
			return
		}
		http.Error(w, "nope", http.StatusBadGateway)
	})
	first, errFirst := catalogFor(context.Background(), "user_cache", "", true)
	if errFirst != nil || len(first) != 1 {
		t.Fatalf("first catalog: %v %+v", errFirst, first)
	}
	second, errSecond := catalogFor(context.Background(), "user_cache", "", true)
	if errSecond != nil {
		t.Fatalf("expected cached catalog after failure: %v", errSecond)
	}
	if len(second) != 1 || second[0].ID != "kept-model" {
		t.Fatalf("second = %+v", second)
	}
}

func TestCatalogDoesNotLeakAcrossKeys(t *testing.T) {
	useRoundTripServer(t, func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Authorization")
		id := "model-a"
		if key == "Bearer user_b" {
			id = "model-b"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(modelsListBody(id))
	})
	a, errA := catalogFor(context.Background(), "user_a", "", true)
	b, errB := catalogFor(context.Background(), "user_b", "", true)
	if errA != nil || errB != nil {
		t.Fatalf("catalog: %v %v", errA, errB)
	}
	if a[0].ID != "model-a" || b[0].ID != "model-b" {
		t.Fatalf("leaked catalogs: %+v %+v", a, b)
	}
}

func TestCatalogCoalescesConcurrentFetches(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	useRoundTripServer(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(modelsListBody("shared"))
	})
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errFetch := catalogFor(context.Background(), "user_shared", "", true)
			errCh <- errFetch
		}()
	}
	<-started
	close(release)
	wg.Wait()
	close(errCh)
	for errFetch := range errCh {
		if errFetch != nil {
			t.Fatalf("fetch: %v", errFetch)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want coalesced 1", calls.Load())
	}
}

func TestUsesAnthropicMessages(t *testing.T) {
	if !usesAnthropicMessages("Claude-Sonnet-4-6", nil) {
		t.Fatal("expected claude- prefix fallback")
	}
	if usesAnthropicMessages("deepseek/deepseek-v4-flash", nil) {
		t.Fatal("deepseek should use OpenAI")
	}
	catalog := []pluginapi.ModelInfo{{ID: "special-claude", Description: "Anthropic Messages endpoint"}}
	if !usesAnthropicMessages("special-claude", catalog) {
		t.Fatal("catalog description should select Anthropic")
	}
}

func TestParseModelsRejectsEmpty(t *testing.T) {
	if _, errParse := parseModelsResponse([]byte(`{"object":"list","data":[{"name":"x"}]}`)); errParse == nil {
		t.Fatal("expected empty catalog error")
	}
}

func TestStaticModelsDoesNotExposeAuthCatalog(t *testing.T) {
	useRoundTripServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(modelsListBody("secret-model-a"))
	})
	raw, errModels := modelsForAuth(mustJSON(t, pluginapi.AuthModelRequest{
		AuthID:      "acct.json",
		StorageJSON: storageJSON("user_a"),
	}))
	if errModels != nil {
		t.Fatalf("modelsForAuth: %v", errModels)
	}
	authEnv := decodeEnvelope(t, raw)
	if !authEnv.OK {
		t.Fatalf("modelsForAuth failed: %+v", authEnv.Error)
	}
	var authResp pluginapi.ModelResponse
	if errUnmarshal := json.Unmarshal(authEnv.Result, &authResp); errUnmarshal != nil {
		t.Fatalf("decode auth models: %v", errUnmarshal)
	}
	if len(authResp.Models) != 1 || authResp.Models[0].ID != "secret-model-a" {
		t.Fatalf("auth models = %+v", authResp.Models)
	}

	staticRaw, errStatic := staticModels(nil)
	if errStatic != nil {
		t.Fatalf("staticModels: %v", errStatic)
	}
	staticEnv := decodeEnvelope(t, staticRaw)
	if !staticEnv.OK {
		t.Fatalf("staticModels failed: %+v", staticEnv.Error)
	}
	var staticResp pluginapi.ModelResponse
	if errUnmarshal := json.Unmarshal(staticEnv.Result, &staticResp); errUnmarshal != nil {
		t.Fatalf("decode static models: %v", errUnmarshal)
	}
	if staticResp.Provider != providerIdentifier {
		t.Fatalf("provider = %q", staticResp.Provider)
	}
	if len(staticResp.Models) != 0 {
		t.Fatalf("static models leaked auth catalog: %+v", staticResp.Models)
	}
}
