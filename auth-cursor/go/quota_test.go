package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

const quotaTestKey = "synthetic-selected-key-canary"
const quotaTestToken = "synthetic-exchanged-token-canary"

var quotaTestPaths = []string{
	"/auth/exchange_user_api_key",
	"/aiserver.v1.DashboardService/GetMe",
	"/aiserver.v1.DashboardService/GetCurrentPeriodUsage",
	"/aiserver.v1.DashboardService/GetHardLimit",
}

type quotaTestTransport func(*http.Request) (*http.Response, error)

func (f quotaTestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func quotaTestSetup(t *testing.T) {
	t.Helper()
	for _, name := range egressProxyEnvVars {
		t.Setenv(name, "")
	}
	t.Setenv("CURSOR_BACKEND_URL", "")
	t.Setenv("CURSOR_API_KEY", "synthetic-environment-key-canary")
	t.Setenv("CURSOR_SDK_BRIDGE_BIN", "/synthetic-missing-bridge")
	previousConfig := loadedConfig()
	previousFactory := quotaTransportFactory
	currentConfig.Store(defaultPluginConfig())
	t.Cleanup(func() {
		currentConfig.Store(previousConfig)
		quotaTransportFactory = previousFactory
	})
}

func quotaTestRequest(t *testing.T, storage string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"auth_index": "selected-index", "auth_id": "selected-id", "provider": "cursor",
		"storage_json":     []byte(storage),
		"metadata":         map[string]any{"api_key": "synthetic-metadata-key-canary", "access_token": "synthetic-stale-token-canary"},
		"attributes":       map[string]string{"proxy_url": "http://ignored-attribute-proxy:8123"},
		"host":             map[string]any{"ProxyURL": "http://ignored-host-proxy:8124"},
		"host_callback_id": "synthetic-callback",
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func quotaTestUsage() string {
	now := time.Now().UTC()
	raw, _ := json.Marshal(map[string]any{
		"billingCycleStart": now.Add(-time.Hour).UnixMilli(),
		"billingCycleEnd":   now.Add(time.Hour).UnixMilli(),
		"planUsage":         map[string]any{"autoPercentUsed": 25, "apiPercentUsed": 75},
		"autoBucketModels":  []string{"synthetic-model-canary"},
	})
	return string(raw)
}

func quotaTestBodies() []string {
	return []string{`{"accessToken":"` + quotaTestToken + `"}`, `{"userId":17,"teamId":23}`, quotaTestUsage(), `{}`}
}

func quotaTestCapture(t *testing.T, bodies []string) *[]string {
	t.Helper()
	paths := []string{}
	quotaTransportFactory = func(proxy *url.URL) http.RoundTripper {
		if proxy != nil {
			t.Fatal("unexpected proxy")
		}
		return quotaTestTransport(func(req *http.Request) (*http.Response, error) {
			i := len(paths)
			paths = append(paths, req.URL.Path)
			if i >= len(quotaTestPaths) || req.URL.Path != quotaTestPaths[i] {
				t.Fatalf("unexpected outbound operation %d at %s", i, req.URL.Path)
			}
			if req.Method != "POST" || req.URL.Scheme != "https" || req.URL.Host != "api2.cursor.sh" || req.URL.RawQuery != "" {
				t.Fatal("unexpected method or destination")
			}
			body, err := io.ReadAll(req.Body)
			if err != nil || string(body) != "{}" {
				t.Fatal("request must use the empty default-team object")
			}
			bearer := quotaTestToken
			if i == 0 {
				bearer = quotaTestKey
			}
			if req.Header.Get("Authorization") != "Bearer "+bearer {
				t.Fatal("wrong credential or token")
			}
			if req.Header.Get("Content-Type") != "application/json" {
				t.Fatal("request must explicitly select JSON")
			}
			if i > 0 && req.Header.Get("Connect-Protocol-Version") != "1" {
				t.Fatal("missing Connect unary version")
			}
			if req.GetBody != nil {
				t.Fatal("transport must not receive a replayable credential request")
			}
			for name := range req.Header {
				if strings.Contains(strings.ToLower(name), "team") {
					t.Fatal("unexpected team selector")
				}
			}
			if _, ok := req.Context().Deadline(); !ok {
				t.Fatal("unbounded observation")
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(bodies[i])), Request: req}, nil
		})
	}
	return &paths
}

func quotaTestFetch(t *testing.T, request []byte) []byte {
	t.Helper()
	raw, err := handleMethod("quota.fetch", request)
	if err != nil {
		t.Fatalf("dispatch escaped the sanitized envelope: %v", err)
	}
	return raw
}

func quotaTestRefused(t *testing.T, raw []byte) {
	t.Helper()
	env := decodeEnvelope(t, raw)
	if env.OK || len(env.Result) != 0 || env.Error == nil {
		t.Fatalf("expected refusal with no quota result: %s", raw)
	}
	if env.Error.Code == "unknown_method" {
		t.Fatal("quota.fetch is not dispatched")
	}
	if !map[string]bool{"invalid_quota_request": true, "invalid_auth": true, "quota_transport": true, "quota_unavailable": true}[env.Error.Code] {
		t.Fatal("unexpected error code")
	}
	for _, canary := range []string{quotaTestKey, quotaTestToken, "synthetic-metadata-key-canary", "synthetic-stale-token-canary", "synthetic-provider-body-canary", "synthetic-proxy-secret-canary"} {
		if strings.Contains(string(raw), canary) {
			t.Fatal("refusal leaked a synthetic secret")
		}
	}
	if strings.Contains(string(raw), "remainingFraction") || strings.Contains(string(raw), "groups") {
		t.Fatal("refusal retained quota numbers")
	}
}

func quotaTestFractions(t *testing.T, raw []byte, wantAuto, wantAPI float64) {
	t.Helper()
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("quota.fetch failed: %s", raw)
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result["groups"] == nil {
		t.Fatal("unexpected public result fields")
	}
	var groups []struct {
		DisplayName string                       `json:"displayName"`
		Buckets     []map[string]json.RawMessage `json:"buckets"`
	}
	if err := json.Unmarshal(result["groups"], &groups); err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatal("expected distinct Auto and API groups")
	}
	for i, want := range []float64{wantAuto, wantAPI} {
		if groups[i].DisplayName != []string{"Auto", "API"}[i] || len(groups[i].Buckets) != 1 {
			t.Fatal("unexpected group")
		}
		bucket := groups[i].Buckets[0]
		fraction, present := bucket["remainingFraction"]
		if !present {
			t.Fatal("remainingFraction must be emitted even when zero")
		}
		var got float64
		if err := json.Unmarshal(fraction, &got); err != nil || got != want {
			t.Fatalf("fraction %s, want %v", fraction, want)
		}
		var reset string
		if err := json.Unmarshal(bucket["resetTime"], &reset); err != nil {
			t.Fatal(err)
		}
		parsed, err := time.Parse(time.RFC3339Nano, reset)
		if err != nil || !parsed.After(time.Now()) {
			t.Fatal("missing valid cycle reset")
		}
	}
}

func TestQuotaRegistration(t *testing.T) {
	quotaTestSetup(t)
	calls := quotaTestCapture(t, quotaTestBodies())
	raw, err := handleMethod(pluginabi.MethodPluginRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	var reg struct {
		SchemaVersion uint32 `json:"schema_version"`
	}
	env := decodeEnvelope(t, raw)
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	json.Unmarshal(env.Result, &fields)
	var capabilities map[string]json.RawMessage
	json.Unmarshal(fields["capabilities"], &capabilities)
	if reg.SchemaVersion != pluginabi.SchemaVersion || string(capabilities["quota_provider"]) != "true" {
		t.Fatal("registration must preserve the SDK schema and advertise quota")
	}
	for _, capability := range []string{"auth_provider", "model_provider", "executor", "command_line_plugin"} {
		if string(capabilities[capability]) != "true" {
			t.Fatal("existing capability lost")
		}
	}
	for _, method := range []string{"quota.identifier", "quota.describe", "quota.reset"} {
		out, err := handleMethod(method, nil)
		if err != nil {
			t.Fatal(err)
		}
		env = decodeEnvelope(t, out)
		if method == "quota.reset" {
			if env.OK || env.Error == nil || env.Error.Code != "unsupported" {
				t.Fatal("reset must explicitly refuse")
			}
		} else if !env.OK {
			t.Fatalf("missing dispatch for %s", method)
		}
		if method == "quota.identifier" && string(env.Result) != `{"identifier":"cursor"}` {
			t.Fatal("wrong quota provider")
		}
		if method == "quota.describe" {
			var description struct {
				Providers []string `json:"supported_providers"`
				Reset     bool     `json:"supports_reset"`
			}
			if err := json.Unmarshal(env.Result, &description); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(description.Providers, []string{"cursor"}) || description.Reset {
				t.Fatal("unexpected quota capabilities")
			}
		}
	}
	if len(*calls) != 0 {
		t.Fatal("registration or reset performed outbound work")
	}
}

func TestQuotaFetchSelectedStorage(t *testing.T) {
	quotaTestSetup(t)
	bodies := quotaTestBodies()
	paths := quotaTestCapture(t, bodies)
	disk := filepath.Join(t.TempDir(), "selected.json")
	if err := os.WriteFile(disk, []byte(`{"type":"cursor","api_key":"synthetic-disk-key-canary"}`), 0600); err != nil {
		t.Fatal(err)
	}
	request := quotaTestRequest(t, `{"type":"cursor","api_key":"  `+quotaTestKey+`  ","apiKey":"synthetic-other-key-canary"}`)
	var fields map[string]any
	json.Unmarshal(request, &fields)
	fields["auth_id"] = disk
	request, _ = json.Marshal(fields)
	raw := quotaTestFetch(t, request)
	quotaTestFractions(t, raw, .75, .25)
	var usage struct {
		End int64 `json:"billingCycleEnd"`
	}
	if err := json.Unmarshal([]byte(bodies[2]), &usage); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Groups []struct {
			Buckets []struct {
				Reset string `json:"resetTime"`
			} `json:"buckets"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(decodeEnvelope(t, raw).Result, &result); err != nil {
		t.Fatal(err)
	}
	for _, group := range result.Groups {
		if group.Buckets[0].Reset != time.UnixMilli(usage.End).UTC().Format(time.RFC3339Nano) {
			t.Fatal("reset must preserve the exact provider end")
		}
	}
	if !reflect.DeepEqual(*paths, quotaTestPaths) {
		t.Fatalf("required request sequence = %v", *paths)
	}
	for _, canary := range []string{quotaTestKey, quotaTestToken, "synthetic-model-canary", "synthetic-disk-key-canary"} {
		if strings.Contains(string(raw), canary) {
			t.Fatal("success leaked provider data")
		}
	}
}

func TestQuotaFetchRefusesCredentialContext(t *testing.T) {
	for name, edit := range map[string]func(map[string]any){
		"no index":                func(m map[string]any) { delete(m, "auth_index") },
		"blank index":             func(m map[string]any) { m["auth_index"] = "  " },
		"no id":                   func(m map[string]any) { delete(m, "auth_id") },
		"wrong provider":          func(m map[string]any) { m["provider"] = "other" },
		"missing storage":         func(m map[string]any) { delete(m, "storage_json") },
		"embedded storage object": func(m map[string]any) { m["storage_json"] = map[string]any{"api_key": quotaTestKey} },
		"empty selected key":      func(m map[string]any) { m["storage_json"] = []byte(`{"type":"cursor"}`) },
	} {
		t.Run(name, func(t *testing.T) {
			quotaTestSetup(t)
			paths := quotaTestCapture(t, quotaTestBodies())
			var req map[string]any
			json.Unmarshal(quotaTestRequest(t, `{"api_key":"`+quotaTestKey+`"}`), &req)
			edit(req)
			raw, _ := json.Marshal(req)
			quotaTestRefused(t, quotaTestFetch(t, raw))
			if len(*paths) != 0 {
				t.Fatal("invalid context sent a credential")
			}
		})
	}
}

func TestQuotaFetchPercentagePresence(t *testing.T) {
	for _, tc := range []struct {
		name, plan string
		success    bool
		auto, api  float64
	}{
		{"explicit zero and full", `{"autoPercentUsed":0,"apiPercentUsed":100}`, true, 1, 0},
		{"snake aliases", `{"auto_percent_used":100,"api_percent_used":0}`, true, 0, 1},
		{"missing auto", `{"apiPercentUsed":25}`, false, 0, 0},
		{"missing api", `{"autoPercentUsed":25}`, false, 0, 0},
		{"null", `{"autoPercentUsed":null,"apiPercentUsed":25}`, false, 0, 0},
		{"null api", `{"autoPercentUsed":25,"apiPercentUsed":null}`, false, 0, 0},
		{"string", `{"autoPercentUsed":"0","apiPercentUsed":25}`, false, 0, 0},
		{"negative", `{"autoPercentUsed":-1,"apiPercentUsed":25}`, false, 0, 0},
		{"over full", `{"autoPercentUsed":101,"apiPercentUsed":25}`, false, 0, 0},
		{"negative api", `{"autoPercentUsed":25,"apiPercentUsed":-1}`, false, 0, 0},
		{"over full api", `{"autoPercentUsed":25,"apiPercentUsed":101}`, false, 0, 0},
		{"overflow", `{"autoPercentUsed":1e10000,"apiPercentUsed":25}`, false, 0, 0},
		{"named infinity", `{"autoPercentUsed":"Infinity","apiPercentUsed":25}`, false, 0, 0},
		{"duplicate", `{"autoPercentUsed":0,"autoPercentUsed":0,"apiPercentUsed":25}`, false, 0, 0},
		{"alias collision", `{"autoPercentUsed":0,"auto_percent_used":0,"apiPercentUsed":25}`, false, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			quotaTestSetup(t)
			bodies := quotaTestBodies()
			bodies[2] = `{"billingCycleStart":1,"billingCycleEnd":"253402300799999","planUsage":` + tc.plan + `}`
			paths := quotaTestCapture(t, bodies)
			raw := quotaTestFetch(t, quotaTestRequest(t, `{"api_key":"`+quotaTestKey+`"}`))
			if tc.success {
				quotaTestFractions(t, raw, tc.auto, tc.api)
			} else {
				quotaTestRefused(t, raw)
				if len(*paths) != 3 {
					t.Fatal("invalid usage did not stop before HardLimit")
				}
			}
		})
	}
}

func TestQuotaFetchRequiredFailures(t *testing.T) {
	for step := range quotaTestPaths {
		for _, status := range []int{401, 403, 500} {
			t.Run(quotaTestPaths[step]+http.StatusText(status), func(t *testing.T) {
				quotaTestSetup(t)
				bodies := quotaTestBodies()
				paths := quotaTestCapture(t, bodies)
				factory := quotaTransportFactory
				quotaTransportFactory = func(proxy *url.URL) http.RoundTripper {
					next := factory(proxy)
					return quotaTestTransport(func(req *http.Request) (*http.Response, error) {
						resp, err := next.RoundTrip(req)
						if len(*paths) == step+1 {
							resp.StatusCode = status
							resp.Body = io.NopCloser(strings.NewReader(`{"code":"permission_denied","message":"synthetic-provider-body-canary"}`))
						}
						return resp, err
					})
				}
				quotaTestRefused(t, quotaTestFetch(t, quotaTestRequest(t, `{"api_key":"`+quotaTestKey+`"}`)))
				if len(*paths) != step+1 {
					t.Fatal("required failure continued or retried")
				}
			})
		}
	}
}

func TestQuotaFetchUsesChosenProxyWithoutDirectRetry(t *testing.T) {
	for _, tc := range []struct {
		name, storage, plugin string
		env                   map[string]string
		want                  string
	}{
		{"credential", `,"proxy_url":"http://credential-proxy:8101"`, "http://plugin-proxy:8102", map[string]string{"HTTPS_PROXY": "http://environment-proxy:8103"}, "credential-proxy:8101"},
		{"plugin", "", "http://plugin-proxy:8102", map[string]string{"HTTPS_PROXY": "http://environment-proxy:8103"}, "plugin-proxy:8102"},
		{"environment priority", "", "", map[string]string{"HTTPS_PROXY": "http://first-proxy:8103", "https_proxy": "http://second-proxy:8104", "ALL_PROXY": "http://last-proxy:8105"}, "first-proxy:8103"},
		{"lowercase environment", "", "", map[string]string{"all_proxy": "socks5h://socks-proxy:8106"}, "socks-proxy:8106"},
		{"direct", "", "", nil, "api2.cursor.sh:443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			quotaTestSetup(t)
			cfg := defaultPluginConfig()
			cfg.ProxyURL = tc.plugin
			currentConfig.Store(cfg)
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			dials := []string{}
			quotaTransportFactory = func(proxy *url.URL) http.RoundTripper {
				transport := egressTransport(proxy)
				transport.DialContext = func(_ context.Context, _, address string) (net.Conn, error) {
					dials = append(dials, address)
					return nil, errors.New("synthetic-proxy-secret-canary")
				}
				t.Cleanup(transport.CloseIdleConnections)
				return transport
			}
			quotaTestRefused(t, quotaTestFetch(t, quotaTestRequest(t, `{"api_key":"`+quotaTestKey+`"`+tc.storage+`}`)))
			if !reflect.DeepEqual(dials, []string{tc.want}) {
				t.Fatalf("dial destinations = %v, want one chosen hop %s", dials, tc.want)
			}
		})
	}
}

func TestQuotaFetchProviderValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		step    int
		body    string
		success bool
	}{
		{"empty token", 0, `{"accessToken":" "}`, false},
		{"wrong token type", 0, `{"accessToken":17}`, false},
		{"duplicate token", 0, `{"accessToken":"a","accessToken":"b"}`, false},
		{"no identity", 1, `{}`, false},
		{"zero user", 1, `{"userId":0}`, false},
		{"overflow user", 1, `{"userId":2147483648}`, false},
		{"null team", 1, `{"userId":17,"teamId":null}`, false},
		{"negative team", 1, `{"userId":17,"teamId":-1}`, false},
		{"identity aliases", 1, `{"user_id":17,"team_id":23}`, true},
		{"identity collision", 1, `{"userId":17,"user_id":17}`, false},
		{"no plan", 2, `{"billingCycleStart":1,"billingCycleEnd":253402300799999}`, false},
		{"null plan", 2, `{"billingCycleStart":1,"billingCycleEnd":253402300799999,"planUsage":null}`, false},
		{"limit array", 3, `[]`, false},
		{"limit null", 3, `null`, false},
		{"limit boolean string", 3, `{"noUsageBasedAllowed":"true"}`, false},
		{"limit boolean null", 3, `{"noUsageBasedAllowed":null}`, false},
		{"limit integer overflow", 3, `{"hardLimit":2147483648}`, false},
		{"limit fractional", 3, `{"hardLimit":1.5}`, false},
		{"limit alias collision", 3, `{"hardLimit":0,"hard_limit":0}`, false},
		{"limit duplicate", 3, `{"noUsageBasedAllowed":true,"noUsageBasedAllowed":true}`, false},
		{"policy default omission", 3, `{}`, true},
		{"policy false zero", 3, `{"hardLimit":0,"noUsageBasedAllowed":false}`, true},
		{"policy true historical limit", 3, `{"hard_limit":100,"no_usage_based_allowed":true}`, true},
		{"unlimited negative scalar", 3, `{"hardLimit":-1}`, true},
		{"unrelated fields ignored", 3, `{"futurePolicy":{"unknown":"synthetic-provider-body-canary"}}`, true},
		{"additional budget bool invalid", 3, `{"perUserFirstPartyModelsAdditionalBudgetUnlimited":1}`, false},
		{"additional budget scalar invalid", 3, `{"per_user_first_party_models_additional_budget_dollars":null}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			quotaTestSetup(t)
			bodies := quotaTestBodies()
			bodies[tc.step] = tc.body
			paths := quotaTestCapture(t, bodies)
			raw := quotaTestFetch(t, quotaTestRequest(t, `{"api_key":"`+quotaTestKey+`"}`))
			if tc.success {
				quotaTestFractions(t, raw, .75, .25)
			} else {
				quotaTestRefused(t, raw)
				if len(*paths) != tc.step+1 {
					t.Fatal("malformed response did not stop observation")
				}
			}
		})
	}
}

func TestQuotaFetchPeriodsAndModelLists(t *testing.T) {
	for _, tc := range []struct {
		name, extra string
		success     bool
	}{
		{"exact canonical strings", `"billing_cycle_start":"1","billing_cycle_end":"253402300799999","auto_bucket_models":["exact-model"]`, true},
		{"exact canonical numbers", `"billingCycleStart":1,"billingCycleEnd":253402300799999`, true},
		{"fractional period", `"billingCycleStart":1.0,"billingCycleEnd":253402300799999`, false},
		{"exponent period", `"billingCycleStart":1e0,"billingCycleEnd":253402300799999`, false},
		{"leading zero string", `"billingCycleStart":"01","billingCycleEnd":253402300799999`, false},
		{"negative zero string", `"billingCycleStart":"-0","billingCycleEnd":253402300799999`, false},
		{"int64 overflow", `"billingCycleStart":1,"billingCycleEnd":9223372036854775808`, false},
		{"lossy large integer", `"billingCycleStart":1,"billingCycleEnd":9007199254740993`, false},
		{"not representable", `"billingCycleStart":1,"billingCycleEnd":253402300800000`, false},
		{"before representable", `"billingCycleStart":-62135596800001,"billingCycleEnd":253402300799999`, false},
		{"reversed", `"billingCycleStart":10,"billingCycleEnd":1`, false},
		{"equal", `"billingCycleStart":1,"billingCycleEnd":1`, false},
		{"past product refusal", `"billingCycleStart":1,"billingCycleEnd":2`, false},
		{"future product refusal", `"billingCycleStart":253402300799998,"billingCycleEnd":253402300799999`, false},
		{"missing end", `"billingCycleStart":1`, false},
		{"period aliases collide", `"billingCycleStart":1,"billing_cycle_start":1,"billingCycleEnd":253402300799999`, false},
		{"plan aliases collide", `"billingCycleStart":1,"billingCycleEnd":253402300799999,"plan_usage":{}`, false},
		{"models null", `"billingCycleStart":1,"billingCycleEnd":253402300799999,"autoBucketModels":null`, false},
		{"models wrong scalar", `"billingCycleStart":1,"billingCycleEnd":253402300799999,"autoBucketModels":[17]`, false},
		{"models null item", `"billingCycleStart":1,"billingCycleEnd":253402300799999,"autoBucketModels":[null]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			quotaTestSetup(t)
			bodies := quotaTestBodies()
			bodies[2] = `{"planUsage":{"autoPercentUsed":25,"apiPercentUsed":75},` + tc.extra + `}`
			quotaTestCapture(t, bodies)
			raw := quotaTestFetch(t, quotaTestRequest(t, `{"api_key":"`+quotaTestKey+`"}`))
			if tc.success {
				quotaTestFractions(t, raw, .75, .25)
			} else {
				quotaTestRefused(t, raw)
			}
		})
	}
}

type quotaTestBrokenBody struct{}

func (quotaTestBrokenBody) Read([]byte) (int, error) {
	return 0, errors.New("synthetic-provider-body-canary")
}
func (quotaTestBrokenBody) Close() error { return nil }

func TestQuotaFetchHTTPFailures(t *testing.T) {
	for step := range quotaTestPaths {
		for _, failure := range []string{"network", "canceled", "deadline", "body error", "large body", "binary", "missing media", "malformed media", "malformed JSON", "trailing JSON", "redirect", "non-200 success"} {
			t.Run(quotaTestPaths[step]+"/"+failure, func(t *testing.T) {
				quotaTestSetup(t)
				paths := quotaTestCapture(t, quotaTestBodies())
				factory := quotaTransportFactory
				quotaTransportFactory = func(proxy *url.URL) http.RoundTripper {
					next := factory(proxy)
					return quotaTestTransport(func(req *http.Request) (*http.Response, error) {
						resp, err := next.RoundTrip(req)
						if len(*paths) != step+1 {
							return resp, err
						}
						switch failure {
						case "network":
							return nil, errors.New("synthetic-proxy-secret-canary")
						case "canceled":
							quiesce()
							return nil, req.Context().Err()
						case "deadline":
							return nil, context.DeadlineExceeded
						case "body error":
							resp.Body = quotaTestBrokenBody{}
						case "large body":
							resp.Body = io.NopCloser(strings.NewReader(strings.Repeat(" ", (1<<20)+1)))
						case "binary":
							resp.Header.Set("Content-Type", "application/proto")
						case "missing media":
							resp.Header.Del("Content-Type")
						case "malformed media":
							resp.Header.Set("Content-Type", "application/json;broken")
						case "malformed JSON":
							resp.Body = io.NopCloser(strings.NewReader("{"))
						case "trailing JSON":
							resp.Body = io.NopCloser(strings.NewReader(`{} {}`))
						case "redirect":
							resp.StatusCode = 307
							resp.Header.Set("Location", "https://synthetic-redirect.invalid/secret")
						case "non-200 success":
							resp.StatusCode = 201
						}
						return resp, err
					})
				}
				quotaTestRefused(t, quotaTestFetch(t, quotaTestRequest(t, `{"api_key":"`+quotaTestKey+`"}`)))
				if len(*paths) != step+1 {
					t.Fatal("failed HTTP boundary followed another request")
				}
			})
		}
	}
}

func TestQuotaFetchRefusesTransportBeforeExchange(t *testing.T) {
	for _, tc := range []struct{ name, proxy, backend string }{
		{"invalid scheme", "ftp://synthetic-proxy-secret-canary", ""},
		{"invalid proxy", "http://%synthetic-proxy-secret-canary", ""},
		{"missing proxy host", "http:///synthetic-proxy-secret-canary", ""},
		{"backend override", "", "https://synthetic-unsupported-backend.invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			quotaTestSetup(t)
			t.Setenv("CURSOR_BACKEND_URL", tc.backend)
			cfg := defaultPluginConfig()
			cfg.ProxyURL = tc.proxy
			currentConfig.Store(cfg)
			calls := 0
			quotaTransportFactory = func(*url.URL) http.RoundTripper {
				calls++
				t.Fatal("invalid transport must refuse before construction")
				return nil
			}
			quotaTestRefused(t, quotaTestFetch(t, quotaTestRequest(t, `{"api_key":"`+quotaTestKey+`"}`)))
			if calls != 0 {
				t.Fatal("invalid transport sent credential")
			}
		})
	}
}

func TestQuotaFetchDoesNotReuseEarlierSuccess(t *testing.T) {
	quotaTestSetup(t)
	request := quotaTestRequest(t, `{"api_key":"`+quotaTestKey+`"}`)
	quotaTestCapture(t, quotaTestBodies())
	quotaTestFractions(t, quotaTestFetch(t, request), .75, .25)
	paths := quotaTestCapture(t, quotaTestBodies())
	factory := quotaTransportFactory
	quotaTransportFactory = func(proxy *url.URL) http.RoundTripper {
		next := factory(proxy)
		return quotaTestTransport(func(req *http.Request) (*http.Response, error) {
			resp, err := next.RoundTrip(req)
			if len(*paths) == 4 {
				resp.StatusCode = 403
			}
			return resp, err
		})
	}
	quotaTestRefused(t, quotaTestFetch(t, request))
	if len(*paths) != 4 {
		t.Fatal("fresh observation was not complete through refusal")
	}
}
