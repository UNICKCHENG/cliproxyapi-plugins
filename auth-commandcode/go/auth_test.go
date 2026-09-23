package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestParseAuthIgnoresOtherProviders(t *testing.T) {
	raw, errParse := parseAuth(mustJSON(t, pluginapi.AuthParseRequest{
		FileName: "other.json",
		RawJSON:  []byte(`{"type":"cursor","api_key":"secret"}`),
	}))
	if errParse != nil {
		t.Fatalf("parseAuth: %v", errParse)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("parseAuth failed: %+v", env.Error)
	}
	var resp pluginapi.AuthParseResponse
	if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if resp.Handled {
		t.Fatal("expected other providers to be declined")
	}
}

func TestParseAuthRejectsEmptyKey(t *testing.T) {
	raw, errParse := parseAuth(mustJSON(t, pluginapi.AuthParseRequest{
		FileName: "commandcode.json",
		RawJSON:  []byte(`{"type":"commandcode","api_key":" "}`),
	}))
	if errParse != nil {
		t.Fatalf("parseAuth: %v", errParse)
	}
	env := decodeEnvelope(t, raw)
	if env.OK {
		t.Fatal("expected invalid_auth")
	}
	if env.Error.Code != "invalid_auth" {
		t.Fatalf("code = %q", env.Error.Code)
	}
	if strings.Contains(env.Error.Message, "user_") {
		t.Fatal("error must not echo a key")
	}
}

func TestParseAuthAcceptsKey(t *testing.T) {
	currentConfig.Store(pluginConfig{
		APIBase: defaultAPIBase,
		Weights: map[string]credentialWeight{"acct.json": 5},
	})
	t.Cleanup(func() { currentConfig.Store(defaultPluginConfig()) })

	raw, errParse := parseAuth(mustJSON(t, pluginapi.AuthParseRequest{
		FileName: "acct.json",
		RawJSON:  []byte(`{"type":"commandcode","api_key":"user_test","label":"acct"}`),
	}))
	if errParse != nil {
		t.Fatalf("parseAuth: %v", errParse)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("parseAuth failed: %+v", env.Error)
	}
	var resp pluginapi.AuthParseResponse
	if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if !resp.Handled || resp.Auth.Provider != providerIdentifier {
		t.Fatalf("unexpected auth: %+v", resp)
	}
	if resp.Auth.Attributes["weight"] != "5" {
		t.Fatalf("weight = %q", resp.Auth.Attributes["weight"])
	}
}

func TestDecodeConfigRejectsBadAPIBase(t *testing.T) {
	_, errDecode := decodeConfig([]byte("api-base: not-a-url\n"))
	if errDecode == nil {
		t.Fatal("expected api-base error")
	}
	_, errScheme := decodeConfig([]byte("api-base: ftp://example.com\n"))
	if errScheme == nil {
		t.Fatal("expected scheme error")
	}
}

func TestNormalizeWeights(t *testing.T) {
	_, errDup := normalizeWeights(map[string]credentialWeight{"A.json": 1, "a.json": 2})
	if errDup == nil {
		t.Fatal("expected duplicate key error")
	}
	out, errNorm := normalizeWeights(map[string]credentialWeight{"acct": -3, "other": 2})
	if errNorm != nil {
		t.Fatalf("normalize: %v", errNorm)
	}
	if out["acct"] != 0 || out["other"] != 2 {
		t.Fatalf("weights = %+v", out)
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
