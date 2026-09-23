package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestCommandLineExecuteValidatesWithModels(t *testing.T) {
	var requests []*http.Request
	useRoundTripServer(t, recordedHandler(t, http.StatusOK, modelsListBody("deepseek/deepseek-v4-flash"), &requests))
	previousList := listHostAuths
	listHostAuths = func() ([]pluginapi.HostAuthFileEntry, error) { return nil, nil }
	t.Cleanup(func() { listHostAuths = previousList })

	raw, errExec := commandLineExecute(mustJSON(t, pluginapi.CommandLineExecutionRequest{
		TriggeredFlags: map[string]pluginapi.CommandLineFlagValue{
			loginFlagName:  {Name: loginFlagName, Set: true, Value: "true"},
			apiKeyFlagName: {Name: apiKeyFlagName, Set: true, Value: "user_test_key"},
		},
	}))
	if errExec != nil {
		t.Fatalf("execute: %v", errExec)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("login failed: %+v", env.Error)
	}
	var resp pluginapi.CommandLineExecutionResponse
	if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if resp.ExitCode != 0 || len(resp.Auths) != 1 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if got := apiKeyFromStorage(resp.Auths[0].StorageJSON); got != "user_test_key" {
		t.Fatalf("stored key = %q", got)
	}
	if !strings.HasPrefix(resp.Auths[0].FileName, "commandcode-") {
		t.Fatalf("filename = %q", resp.Auths[0].FileName)
	}
	if len(requests) != 1 || !strings.HasSuffix(requests[0].URL.Path, "/provider/v1/models") {
		t.Fatalf("expected models validation, got %+v", requests)
	}
	if auth := requests[0].Header.Get("Authorization"); auth != "Bearer user_test_key" {
		t.Fatalf("authorization = %q", auth)
	}
}

func TestCommandLineExecuteRejectsUnauthorized(t *testing.T) {
	var requests []*http.Request
	useRoundTripServer(t, recordedHandler(t, http.StatusUnauthorized, []byte(`{"error":{"message":"bad","type":"authentication_error"}}`), &requests))
	raw, errExec := commandLineExecute(mustJSON(t, pluginapi.CommandLineExecutionRequest{
		TriggeredFlags: map[string]pluginapi.CommandLineFlagValue{
			loginFlagName:  {Set: true, Value: "true"},
			apiKeyFlagName: {Set: true, Value: "user_bad"},
		},
	}))
	if errExec != nil {
		t.Fatalf("execute: %v", errExec)
	}
	env := decodeEnvelope(t, raw)
	var resp pluginapi.CommandLineExecutionResponse
	if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if resp.ExitCode != 1 || len(resp.Auths) != 0 {
		t.Fatalf("expected failed login: %+v", resp)
	}
	if !strings.Contains(string(resp.Stderr), "invalid Command Code API key") {
		t.Fatalf("stderr = %s", resp.Stderr)
	}
}

func TestCommandLineSkipValidate(t *testing.T) {
	var requests []*http.Request
	useRoundTripServer(t, recordedHandler(t, http.StatusUnauthorized, []byte(`{}`), &requests))
	previousList := listHostAuths
	listHostAuths = func() ([]pluginapi.HostAuthFileEntry, error) { return nil, nil }
	t.Cleanup(func() { listHostAuths = previousList })

	raw, errExec := commandLineExecute(mustJSON(t, pluginapi.CommandLineExecutionRequest{
		TriggeredFlags: map[string]pluginapi.CommandLineFlagValue{
			loginFlagName:        {Set: true, Value: "true"},
			apiKeyFlagName:       {Set: true, Value: "user_offline"},
			skipValidateFlagName: {Set: true, Value: "true"},
		},
	}))
	if errExec != nil {
		t.Fatalf("execute: %v", errExec)
	}
	env := decodeEnvelope(t, raw)
	var resp pluginapi.CommandLineExecutionResponse
	if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if resp.ExitCode != 0 || len(resp.Auths) != 1 {
		t.Fatalf("offline import failed: %+v", resp)
	}
	if len(requests) != 0 {
		t.Fatalf("skip-validate still called upstream: %d", len(requests))
	}
}

func TestCommandLineMergesExistingAuth(t *testing.T) {
	useRoundTripServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(modelsListBody("m1"))
	})
	previousList := listHostAuths
	previousGet := getHostAuthJSON
	listHostAuths = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{
			Name:      "commandcode-custom.json",
			Provider:  providerIdentifier,
			AuthIndex: "1",
		}}, nil
	}
	getHostAuthJSON = func(pluginapi.HostAuthFileEntry) ([]byte, error) {
		return []byte(`{"type":"commandcode","api_key":"user_same","label":"kept","proxy_url":"http://proxy.example","disabled":true}`), nil
	}
	t.Cleanup(func() {
		listHostAuths = previousList
		getHostAuthJSON = previousGet
	})

	raw, errExec := commandLineExecute(mustJSON(t, pluginapi.CommandLineExecutionRequest{
		TriggeredFlags: map[string]pluginapi.CommandLineFlagValue{
			loginFlagName:  {Set: true, Value: "true"},
			apiKeyFlagName: {Set: true, Value: "user_same"},
		},
	}))
	if errExec != nil {
		t.Fatalf("execute: %v", errExec)
	}
	var resp pluginapi.CommandLineExecutionResponse
	if errUnmarshal := json.Unmarshal(decodeEnvelope(t, raw).Result, &resp); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if resp.Auths[0].FileName != "commandcode-custom.json" {
		t.Fatalf("filename = %q", resp.Auths[0].FileName)
	}
	if resp.Auths[0].Label != "kept" || resp.Auths[0].ProxyURL != "http://proxy.example" || !resp.Auths[0].Disabled {
		t.Fatalf("merged fields not preserved: %+v", resp.Auths[0])
	}
}

func TestCommandLineImportIgnoresUnreadableOtherAuth(t *testing.T) {
	useRoundTripServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(modelsListBody("m1"))
	})
	previousList := listHostAuths
	previousGet := getHostAuthJSON
	listHostAuths = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{
			{Name: "commandcode-broken.json", Provider: providerIdentifier, AuthIndex: "1"},
			{Name: "commandcode-other.json", Provider: providerIdentifier, AuthIndex: "2"},
		}, nil
	}
	getHostAuthJSON = func(entry pluginapi.HostAuthFileEntry) ([]byte, error) {
		if entry.Name == "commandcode-broken.json" {
			return nil, fmt.Errorf("permission denied")
		}
		return []byte(`{"type":"commandcode","api_key":"user_other","label":"other"}`), nil
	}
	t.Cleanup(func() {
		listHostAuths = previousList
		getHostAuthJSON = previousGet
	})

	raw, errExec := commandLineExecute(mustJSON(t, pluginapi.CommandLineExecutionRequest{
		TriggeredFlags: map[string]pluginapi.CommandLineFlagValue{
			loginFlagName:  {Set: true, Value: "true"},
			apiKeyFlagName: {Set: true, Value: "user_new_key"},
		},
	}))
	if errExec != nil {
		t.Fatalf("execute: %v", errExec)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("import failed: %+v", env.Error)
	}
	var resp pluginapi.CommandLineExecutionResponse
	if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if resp.ExitCode != 0 || len(resp.Auths) != 1 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if got := apiKeyFromStorage(resp.Auths[0].StorageJSON); got != "user_new_key" {
		t.Fatalf("stored key = %q", got)
	}
	if !strings.HasPrefix(resp.Auths[0].FileName, "commandcode-") {
		t.Fatalf("filename = %q", resp.Auths[0].FileName)
	}
	if resp.Auths[0].FileName == "commandcode-broken.json" || resp.Auths[0].FileName == "commandcode-other.json" {
		t.Fatalf("imported into unrelated file: %q", resp.Auths[0].FileName)
	}
}

func TestCommandLineMergeSkipsUnreadableSibling(t *testing.T) {
	useRoundTripServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(modelsListBody("m1"))
	})
	previousList := listHostAuths
	previousGet := getHostAuthJSON
	listHostAuths = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{
			{Name: "commandcode-broken.json", Provider: providerIdentifier, AuthIndex: "1"},
			{Name: "commandcode-custom.json", Provider: providerIdentifier, AuthIndex: "2"},
		}, nil
	}
	getHostAuthJSON = func(entry pluginapi.HostAuthFileEntry) ([]byte, error) {
		if entry.Name == "commandcode-broken.json" {
			return nil, fmt.Errorf("permission denied")
		}
		return []byte(`{"type":"commandcode","api_key":"user_same","label":"kept"}`), nil
	}
	t.Cleanup(func() {
		listHostAuths = previousList
		getHostAuthJSON = previousGet
	})

	raw, errExec := commandLineExecute(mustJSON(t, pluginapi.CommandLineExecutionRequest{
		TriggeredFlags: map[string]pluginapi.CommandLineFlagValue{
			loginFlagName:  {Set: true, Value: "true"},
			apiKeyFlagName: {Set: true, Value: "user_same"},
		},
	}))
	if errExec != nil {
		t.Fatalf("execute: %v", errExec)
	}
	var resp pluginapi.CommandLineExecutionResponse
	if errUnmarshal := json.Unmarshal(decodeEnvelope(t, raw).Result, &resp); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if len(resp.Auths) != 1 || resp.Auths[0].FileName != "commandcode-custom.json" {
		t.Fatalf("filename = %+v", resp.Auths)
	}
	if resp.Auths[0].Label != "kept" {
		t.Fatalf("merged fields not preserved: %+v", resp.Auths[0])
	}
}

func TestReadAPIKeyFrom(t *testing.T) {
	key, errRead := readAPIKeyFrom(strings.NewReader("  user_pasted \n"))
	if errRead != nil {
		t.Fatalf("read: %v", errRead)
	}
	if key != "user_pasted" {
		t.Fatalf("key = %q", key)
	}
	if _, errEmpty := readAPIKeyFrom(strings.NewReader("\n")); errEmpty == nil {
		t.Fatal("expected empty key error")
	}
	if _, errEOF := readAPIKeyFrom(strings.NewReader("")); errEOF == nil {
		t.Fatal("expected empty key on EOF")
	}
}
