package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// proxiedRequest is what a test asserts on: where the egress was sending a request, and what it
// asked for once it got there.
type proxiedRequest struct {
	host string
	path string
}

// serveFakeCursorProxy stands in for the operator's proxy. It answers rather than forwards, which
// is enough: the requests it receives prove the egress dialled through it, and their Host proves
// where the egress was routing them.
func serveFakeCursorProxy(t *testing.T, respond http.HandlerFunc) (string, func() []proxiedRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []proxiedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, proxiedRequest{host: r.Host, path: r.URL.Path})
		mu.Unlock()
		if respond != nil {
			respond(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	return server.URL, func() []proxiedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]proxiedRequest(nil), seen...)
	}
}

// usePlainCursorTargets makes the upstream targets unencrypted, so a test's fake proxy sees the
// absolute-form request net/http sends for a plain target instead of a CONNECT tunnel it would
// have to terminate with a certificate. The routing and the proxy wiring are what is under test;
// the TLS upstream is stdlib behaviour.
func usePlainCursorTargets(t *testing.T) {
	t.Helper()
	backend, cloud := cursorBackendTarget, cursorCloudTarget
	cursorBackendTarget = url.URL{Scheme: "http", Host: cursorBackendHost}
	cursorCloudTarget = url.URL{Scheme: "http", Host: cursorCloudHost}
	t.Cleanup(func() { cursorBackendTarget, cursorCloudTarget = backend, cloud })
}

// One environment variable redirects both of the SDK's clients, which default to different hosts,
// so the egress has to put each request back on the host it belonged to.
func TestEgressRoutesEachPathToItsCursorHost(t *testing.T) {
	usePlainCursorTargets(t)
	proxyURL, recorded := serveFakeCursorProxy(t, nil)
	egress, errStart := startBridgeEgress(proxyURL)
	if errStart != nil {
		t.Fatalf("startBridgeEgress: %v", errStart)
	}
	defer egress.stop()

	for _, test := range []struct {
		name string
		path string
		want string
	}{
		{name: "connect rpc", path: "/aiserver.v1.DashboardService/GetMe", want: cursorBackendHost},
		{name: "another service", path: "/aiserver.v1.ServerConfigService/GetServerConfig", want: cursorBackendHost},
		// The backend serves plain REST too. The SDK calls this one before its first request,
		// to trade the API key for an access token, and sending it to the cloud host fails
		// every request with "API key exchange endpoint not found".
		{name: "backend rest", path: "/auth/exchange_user_api_key", want: cursorBackendHost},
		{name: "cloud rest", path: "/v1/agents", want: cursorCloudHost},
		{name: "cloud rest with an id", path: "/v1/agents/bc-123/archive", want: cursorCloudHost},
		{name: "cloud me", path: "/v1/me", want: cursorCloudHost},
		// An unrecognised path belongs on the backend: that is where CURSOR_BACKEND_URL points
		// when nothing redirects it.
		{name: "unknown path", path: "/something/new", want: cursorBackendHost},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := len(recorded())
			response, errGet := http.Get(egress.baseURL + test.path)
			if errGet != nil {
				t.Fatalf("request through the egress: %v", errGet)
			}
			_ = response.Body.Close()
			requests := recorded()
			if len(requests) != before+1 {
				t.Fatalf("the proxy saw %d requests, want one more than %d", len(requests), before)
			}
			last := requests[len(requests)-1]
			if last.host != test.want {
				t.Errorf("Host = %q, want %q", last.host, test.want)
			}
			if last.path != test.path {
				t.Errorf("path = %q, want it preserved as %q", last.path, test.path)
			}
		})
	}
}

// The egress carries agent output, which arrives as stream frames the bridge has to see as they
// are produced. Buffering them until the response ends would stall every run behind its own
// completion.
func TestEgressStreamsWithoutBuffering(t *testing.T) {
	usePlainCursorTargets(t)
	release := make(chan struct{})
	proxyURL, _ := serveFakeCursorProxy(t, func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("the fake upstream cannot flush, so this test cannot prove anything")
			return
		}
		_, _ = fmt.Fprint(w, "first-frame\n")
		flusher.Flush()
		// Holding the response open is what makes buffering observable: a buffering hop
		// would not deliver the frame above until this returns.
		<-release
		_, _ = fmt.Fprint(w, "last-frame\n")
	})
	egress, errStart := startBridgeEgress(proxyURL)
	if errStart != nil {
		t.Fatalf("startBridgeEgress: %v", errStart)
	}
	defer egress.stop()
	defer close(release)

	response, errGet := http.Get(egress.baseURL + "/aiserver.v1.BidiService/StreamNomadTask")
	if errGet != nil {
		t.Fatalf("request through the egress: %v", errGet)
	}
	defer func() { _ = response.Body.Close() }()

	frame := make(chan string, 1)
	go func() {
		buffer := make([]byte, len("first-frame\n"))
		if _, errRead := io.ReadFull(response.Body, buffer); errRead != nil {
			return
		}
		frame <- string(buffer)
	}()
	select {
	case got := <-frame:
		if strings.TrimSpace(got) != "first-frame" {
			t.Errorf("first frame = %q, want first-frame", got)
		}
	case <-time.After(5 * time.Second):
		t.Error("the first frame did not arrive while the response was still open, so the egress buffered it")
	}
}

// With no proxy anywhere the bridge should connect on its own: a pass-through hop would add a
// failure mode and buy nothing.
func TestStartBridgeEgressIsSkippedWithoutAProxy(t *testing.T) {
	for _, name := range append([]string{}, egressProxyEnvVars...) {
		t.Setenv(name, "")
	}
	egress, errStart := startBridgeEgress("")
	if errStart != nil {
		t.Fatalf("startBridgeEgress: %v", errStart)
	}
	if egress != nil {
		egress.stop()
		t.Error("an egress was started with no proxy to apply")
	}
}

// A proxy the plugin was not given is still worth honouring: it is how a machine that only
// reaches the internet through a proxy works today.
func TestResolveEgressProxyFallsBackToTheEnvironment(t *testing.T) {
	for _, name := range append([]string{}, egressProxyEnvVars...) {
		t.Setenv(name, "")
	}
	t.Setenv("ALL_PROXY", "socks5://127.0.0.1:1080")
	if got := resolveEgressProxy(""); got != "socks5://127.0.0.1:1080" {
		t.Errorf("resolveEgressProxy = %q, want the inherited proxy", got)
	}
	// A configured proxy is the plugin's own answer and outranks the environment.
	if got := resolveEgressProxy(" http://127.0.0.1:9527 "); got != "http://127.0.0.1:9527" {
		t.Errorf("resolveEgressProxy = %q, want the configured proxy", got)
	}
}

// A misconfigured proxy has to be reported when the bridge starts. Left to the first Cursor call
// it surfaces as a timeout, which reads as if the plugin hung.
func TestParseEgressProxyRejectsWhatItCannotDial(t *testing.T) {
	for _, test := range []struct {
		name  string
		proxy string
		want  string
	}{
		{name: "bare host and port", proxy: "127.0.0.1:9527", want: "http://127.0.0.1:9527"},
		{name: "http", proxy: "http://127.0.0.1:9527", want: "http://127.0.0.1:9527"},
		{name: "socks5", proxy: "socks5://127.0.0.1:1080", want: "socks5://127.0.0.1:1080"},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed, errParse := parseEgressProxy(test.proxy)
			if errParse != nil {
				t.Fatalf("parseEgressProxy(%q): %v", test.proxy, errParse)
			}
			if parsed.String() != test.want {
				t.Errorf("parseEgressProxy(%q) = %q, want %q", test.proxy, parsed, test.want)
			}
		})
	}
	for _, test := range []struct {
		name  string
		proxy string
		want  string
	}{
		{name: "unsupported scheme", proxy: "ftp://127.0.0.1:21", want: "scheme"},
		{name: "no host", proxy: "http://", want: "no host"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, errParse := parseEgressProxy(test.proxy)
			if errParse == nil {
				t.Fatalf("expected %q to be rejected", test.proxy)
			}
			if !strings.Contains(errParse.Error(), test.want) {
				t.Errorf("error = %q, want it to name the %s", errParse, test.want)
			}
		})
	}
}

// A bridge started against an unusable proxy must fail loudly rather than fall back to a direct
// connection, which is the timeout the egress exists to prevent.
func TestStartBridgeRejectsAnUnusableProxy(t *testing.T) {
	useTempCacheDir(t)
	useShortBridgeTimeouts(t)
	fakeBridgeExecutable(t, bridgeDiscovery{}, `
while true; do sleep 1; done
`)
	_, errStart := startBridge(defaultPluginConfig(), "ftp://127.0.0.1:21")
	if errStart == nil {
		t.Fatal("expected a proxy the egress cannot dial to fail the start")
	}
	if !strings.Contains(errStart.Error(), "scheme") {
		t.Errorf("error = %q, want it to name the unusable scheme", errStart)
	}
}
