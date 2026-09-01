package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	sdkv1 "github.com/UNICKCHENG/cliproxyapi-plugins/auth-cursor/go/internal/sdk/v1"
)

// useTempCacheDir redirects the user cache directory, which is where the bridge tree and its
// durable state live, so a test never touches the operator's own installation.
func useTempCacheDir(t *testing.T) string {
	t.Helper()
	cache := t.TempDir()
	// os.UserCacheDir reads a different variable on each platform.
	t.Setenv("HOME", cache)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(cache, "cache"))
	t.Setenv("LocalAppData", filepath.Join(cache, "AppData", "Local"))
	return cache
}

// useShortBridgeTimeouts trims the startup and shutdown budgets so the lifecycle tests finish in
// milliseconds instead of tens of seconds.
func useShortBridgeTimeouts(t *testing.T) {
	t.Helper()
	startup, shutdown := bridgeStartupTimeout, bridgeShutdownTimeout
	bridgeStartupTimeout = 3 * time.Second
	bridgeShutdownTimeout = 200 * time.Millisecond
	t.Cleanup(func() {
		bridgeStartupTimeout, bridgeShutdownTimeout = startup, shutdown
	})
}

// fakeBridgeExecutable writes a shell script that plays the bridge's side of the startup
// handshake, so the process management can be tested without the real binary.
//
// script is the body; it runs with the discovery line already available as $READY.
func fakeBridgeExecutable(t *testing.T, discovery bridgeDiscovery, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the handshake tests drive a shell script, which needs a POSIX shell")
	}
	payload, errMarshal := json.Marshal(discovery)
	if errMarshal != nil {
		t.Fatalf("marshal discovery: %v", errMarshal)
	}
	path := filepath.Join(t.TempDir(), "cursor-sdk-bridge")
	body := "#!/bin/sh\nREADY='" + bridgeReadyPrefix + string(payload) + "'\n" + script
	if errWrite := os.WriteFile(path, []byte(body), 0o700); errWrite != nil {
		t.Fatalf("write fake bridge: %v", errWrite)
	}
	t.Setenv(bridgeBinaryEnv, path)
	return path
}

// authTokenFile stores the bearer token where the discovery payload says it is, which is how a
// current bridge hands it over.
func authTokenFile(t *testing.T, token string) string {
	t.Helper()
	root, errRoot := bridgeStateRoot()
	if errRoot != nil {
		t.Fatalf("bridgeStateRoot: %v", errRoot)
	}
	if errMkdir := os.MkdirAll(root, 0o700); errMkdir != nil {
		t.Fatalf("create state root: %v", errMkdir)
	}
	path := filepath.Join(root, "token")
	if errWrite := os.WriteFile(path, []byte(token+"\n"), 0o600); errWrite != nil {
		t.Fatalf("write auth token: %v", errWrite)
	}
	return path
}

func TestStartBridgeCompletesTheHandshake(t *testing.T) {
	useTempCacheDir(t)
	useShortBridgeTimeouts(t)
	baseURL := serveFakeBridge(t, newFakeBridge())
	fakeBridgeExecutable(t, bridgeDiscovery{
		SchemaVersion: bridgeDiscoverySchema,
		ServerVersion: bridgeVersion,
		Transport:     "tcp",
		Protocol:      "connect",
		URL:           baseURL,
		AuthTokenFile: authTokenFile(t, fakeBridgeToken),
	}, `
echo "starting up" >&2
echo "$READY" >&2
# Stay alive until killed, the way a listening server does.
while true; do sleep 1; done
`)

	process, errStart := startBridge(defaultPluginConfig(), "")
	if errStart != nil {
		t.Fatalf("startBridge: %v", errStart)
	}
	defer process.stop()

	if !process.alive() {
		t.Error("the bridge is not running after a successful handshake")
	}
	// A successful start means Ping answered, which is only possible with the token the
	// discovery payload pointed at.
	if errPing := process.ping(); errPing != nil {
		t.Errorf("ping: %v", errPing)
	}
	// Ordinary stderr is kept for diagnostics; the discovery line is not, because older
	// bridges inline the bearer token in it.
	tail := process.stderr.String()
	if !strings.Contains(tail, "starting up") {
		t.Errorf("stderr tail = %q, want the bridge's own output", tail)
	}
	if strings.Contains(tail, bridgeReadyPrefix) || strings.Contains(tail, fakeBridgeToken) {
		t.Errorf("stderr tail leaked the discovery line: %q", tail)
	}
	// The workspace is a scratch directory of the plugin's own making: a run must not see the
	// host's working tree.
	if entries, errRead := os.ReadDir(process.workspace); errRead != nil || len(entries) != 0 {
		t.Errorf("workspace %s = %v (err %v), want an empty directory", process.workspace, entries, errRead)
	}
}

func TestStartBridgeReportsAnExitBeforeReady(t *testing.T) {
	useTempCacheDir(t)
	useShortBridgeTimeouts(t)
	fakeBridgeExecutable(t, bridgeDiscovery{}, `
echo "bridge could not bind port 0" >&2
exit 3
`)

	_, errStart := startBridge(defaultPluginConfig(), "")
	if errStart == nil {
		t.Fatal("expected a bridge that dies during startup to be reported")
	}
	// Without the process's own words the operator has nothing to act on.
	if !strings.Contains(errStart.Error(), "could not bind") {
		t.Errorf("error = %q, want the bridge's stderr included", errStart)
	}
}

func TestStartBridgeTimesOutWithoutADiscoveryLine(t *testing.T) {
	useTempCacheDir(t)
	useShortBridgeTimeouts(t)
	bridgeStartupTimeout = 300 * time.Millisecond
	fakeBridgeExecutable(t, bridgeDiscovery{}, `
while true; do sleep 1; done
`)

	started := time.Now()
	_, errStart := startBridge(defaultPluginConfig(), "")
	if errStart == nil {
		t.Fatal("expected a silent bridge to time out")
	}
	if !strings.Contains(errStart.Error(), "was not ready within") {
		t.Errorf("error = %q, want a startup timeout", errStart)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("startBridge blocked for %s, want it bounded by the startup timeout", elapsed)
	}
}

// A discovery payload this plugin cannot speak to has to fail the start. Connecting anyway would
// turn a clear startup error into an unexplained failure on the first request.
func TestStartBridgeRejectsAnUnsupportedDiscovery(t *testing.T) {
	for _, test := range []struct {
		name      string
		discovery bridgeDiscovery
		want      string
	}{
		{
			name:      "future schema",
			discovery: bridgeDiscovery{SchemaVersion: 2, Transport: "tcp", Protocol: "connect", URL: "http://127.0.0.1:1"},
			want:      "discovery schema",
		},
		{
			name:      "another transport",
			discovery: bridgeDiscovery{SchemaVersion: bridgeDiscoverySchema, Transport: "stdio", Protocol: "connect"},
			want:      "transport",
		},
		{
			name:      "another protocol",
			discovery: bridgeDiscovery{SchemaVersion: bridgeDiscoverySchema, Transport: "tcp", Protocol: "grpc", URL: "http://127.0.0.1:1"},
			want:      "protocol",
		},
		{
			name:      "no address",
			discovery: bridgeDiscovery{SchemaVersion: bridgeDiscoverySchema, Transport: "tcp", Protocol: "connect"},
			want:      "no address",
		},
		{
			name:      "https url",
			discovery: bridgeDiscovery{SchemaVersion: bridgeDiscoverySchema, Transport: "tcp", Protocol: "connect", URL: "https://127.0.0.1:1"},
			want:      "must be http",
		},
		{
			name:      "non-loopback host",
			discovery: bridgeDiscovery{SchemaVersion: bridgeDiscoverySchema, Transport: "tcp", Protocol: "connect", URL: "http://example.com:1"},
			want:      "loopback",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			useTempCacheDir(t)
			useShortBridgeTimeouts(t)
			fakeBridgeExecutable(t, test.discovery, `
echo "$READY" >&2
while true; do sleep 1; done
`)
			_, errStart := startBridge(defaultPluginConfig(), "")
			if errStart == nil {
				t.Fatal("expected an unusable discovery payload to be rejected")
			}
			if !strings.Contains(errStart.Error(), test.want) {
				t.Errorf("error = %q, want it to name the %s", errStart, test.want)
			}
		})
	}
}

// An older bridge inlines the token in the discovery line; a current one writes it to a file.
func TestBridgeAuthTokenAcceptsBothDeliveries(t *testing.T) {
	useTempCacheDir(t)
	inline, errInline := bridgeAuthToken(bridgeDiscovery{AuthToken: " token-inline "})
	if errInline != nil || inline != "token-inline" {
		t.Errorf("inline token = %q err = %v, want token-inline", inline, errInline)
	}
	fromFile, errFile := bridgeAuthToken(bridgeDiscovery{AuthTokenFile: authTokenFile(t, "token-from-file")})
	if errFile != nil || fromFile != "token-from-file" {
		t.Errorf("file token = %q err = %v, want token-from-file", fromFile, errFile)
	}
	if _, errNone := bridgeAuthToken(bridgeDiscovery{}); errNone == nil {
		t.Error("expected a discovery payload without a token to be rejected")
	}
	if _, errEmpty := bridgeAuthToken(bridgeDiscovery{AuthTokenFile: authTokenFile(t, "")}); errEmpty == nil {
		t.Error("expected an empty token file to be rejected")
	}
}

func TestBridgeDiscoveryBaseURL(t *testing.T) {
	for _, test := range []struct {
		name      string
		discovery bridgeDiscovery
		want      string
	}{
		{name: "url wins", discovery: bridgeDiscovery{URL: "http://127.0.0.1:9000/", Host: "10.0.0.1", Port: 1}, want: "http://127.0.0.1:9000"},
		{name: "host and port", discovery: bridgeDiscovery{Host: "127.0.0.1", Port: 9000}, want: "http://127.0.0.1:9000"},
		// An IPv6 literal has to be bracketed or the address is unparseable.
		{name: "ipv6 literal", discovery: bridgeDiscovery{Host: "::1", Port: 9000}, want: "http://[::1]:9000"},
		{name: "nothing usable", discovery: bridgeDiscovery{Host: "127.0.0.1"}, want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.discovery.baseURL(); got != test.want {
				t.Errorf("baseURL = %q, want %q", got, test.want)
			}
		})
	}
}

// The bridge takes its proxy from the environment, and it must never take a credential from
// there: one process serves every key that shares its proxy, so an ambient key would silently
// become the fallback for all of them.
func TestBridgeEnvCarriesTheProxyAndNoCredential(t *testing.T) {
	t.Setenv("CURSOR_API_KEY", "key_ambient")
	t.Setenv("HTTPS_PROXY", "http://inherited:3128")
	t.Setenv("CURSOR_SDK_BRIDGE_PORT", "9999")

	resolved := envMap(bridgeEnv("socks5://127.0.0.1:1080", ""))
	if _, present := resolved["CURSOR_API_KEY"]; present {
		t.Error("CURSOR_API_KEY reached the bridge environment")
	}
	if _, present := resolved["CURSOR_SDK_BRIDGE_PORT"]; present {
		t.Error("CURSOR_SDK_BRIDGE_PORT reached the bridge environment and would fight the flags")
	}
	// The proxy variables are still written even though the bridge's backend calls ignore them:
	// they are what proxies the parts of the SDK that go through bun's own fetch().
	for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		if got := resolved[name]; got != "socks5://127.0.0.1:1080" {
			t.Errorf("%s = %q, want the resolved proxy", name, got)
		}
	}
	if got := resolved["CURSOR_SDK_CLIENT_LANGUAGE"]; got != "go" {
		t.Errorf("CURSOR_SDK_CLIENT_LANGUAGE = %q, want go", got)
	}

	// With no proxy of its own the plugin leaves the inherited one alone, so a machine that
	// only reaches the internet through a proxy keeps working.
	inherited := envMap(bridgeEnv("", ""))
	if got := inherited["HTTPS_PROXY"]; got != "http://inherited:3128" {
		t.Errorf("HTTPS_PROXY = %q, want the inherited proxy preserved", got)
	}
	if _, present := inherited["CURSOR_API_KEY"]; present {
		t.Error("CURSOR_API_KEY reached the bridge environment")
	}
}

// An egress only works if the bridge is actually pointed at it, and only if the bridge then
// reaches it directly: routing that loopback hop through the user's proxy would send a local
// connection out to the internet and back.
func TestBridgeEnvPointsTheBridgeAtTheEgress(t *testing.T) {
	t.Setenv("NO_PROXY", "internal.example")
	t.Setenv("CURSOR_BACKEND_URL", "https://api.example")

	resolved := envMap(bridgeEnv("http://127.0.0.1:9527", "http://[::1]:41234"))
	if got := resolved["CURSOR_BACKEND_URL"]; got != "http://[::1]:41234" {
		t.Errorf("CURSOR_BACKEND_URL = %q, want the egress to replace the inherited value", got)
	}
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		got := resolved[name]
		for _, want := range []string{"internal.example", "127.0.0.1", "::1", "localhost"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s = %q, want %q included", name, got, want)
			}
		}
	}
	// The proxy is still handed over for the fetch() paths, which do honour it.
	if got := resolved["HTTPS_PROXY"]; got != "http://127.0.0.1:9527" {
		t.Errorf("HTTPS_PROXY = %q, want the proxy still written", got)
	}

	// Without an egress the bridge connects on its own, and nothing about that changes.
	direct := envMap(bridgeEnv("", ""))
	if got, present := direct["CURSOR_BACKEND_URL"]; !present || got != "https://api.example" {
		t.Errorf("CURSOR_BACKEND_URL = %q present = %v, want the inherited value untouched", got, present)
	}
	if got, present := direct["NO_PROXY"]; !present || got != "internal.example" {
		t.Errorf("NO_PROXY = %q present = %v, want the inherited value untouched", got, present)
	}
}

func TestNoProxyWithLoopbackDeduplicates(t *testing.T) {
	got := noProxyWithLoopback("localhost, internal.example", "LOCALHOST,127.0.0.1")
	want := "localhost,internal.example,127.0.0.1,::1"
	if got != want {
		t.Errorf("noProxyWithLoopback = %q, want %q", got, want)
	}
}

func envMap(env []string) map[string]string {
	resolved := make(map[string]string, len(env))
	for _, entry := range env {
		name, value, found := strings.Cut(entry, "=")
		if found {
			resolved[name] = value
		}
	}
	return resolved
}

// One bridge per proxy: the proxy is a property of the process, the credential is a property of
// the request, so credentials sharing a proxy must share a process.
func TestAcquireBridgePoolsByProxy(t *testing.T) {
	useTempCacheDir(t)
	useShortBridgeTimeouts(t)
	baseURL := serveFakeBridge(t, newFakeBridge())
	fakeBridgeExecutable(t, bridgeDiscovery{
		SchemaVersion: bridgeDiscoverySchema,
		Transport:     "tcp",
		Protocol:      "connect",
		URL:           baseURL,
		AuthTokenFile: authTokenFile(t, fakeBridgeToken),
	}, `
echo "$READY" >&2
while true; do sleep 1; done
`)
	t.Cleanup(stopBridges)

	direct, errDirect := acquireBridge("")
	if errDirect != nil {
		t.Fatalf("acquireBridge: %v", errDirect)
	}
	again, errAgain := acquireBridge("")
	if errAgain != nil {
		t.Fatalf("acquireBridge: %v", errAgain)
	}
	if direct != again {
		t.Error("a second credential on the same proxy started a second bridge")
	}
	proxied, errProxied := acquireBridge("http://127.0.0.1:3128")
	if errProxied != nil {
		t.Fatalf("acquireBridge: %v", errProxied)
	}
	if proxied == direct {
		t.Error("a credential on another proxy reused the direct bridge")
	}

	stopBridges()
	if direct.alive() || proxied.alive() {
		t.Error("stopBridges left a bridge running")
	}
	// The scratch workspace goes with the process it belonged to.
	if _, errStat := os.Stat(direct.workspace); !os.IsNotExist(errStat) {
		t.Errorf("workspace survived shutdown: %v", errStat)
	}
}

// A dead bridge must be replaced rather than handed out: every RPC on it would fail, and the
// failure would look like a Cursor outage.
func TestAcquireBridgeReplacesADeadProcess(t *testing.T) {
	bridge := useFakeBridge(t)
	stale := poolBridge(t)
	close(stale.exited)

	useTempCacheDir(t)
	useShortBridgeTimeouts(t)
	baseURL := serveFakeBridge(t, bridge)
	fakeBridgeExecutable(t, bridgeDiscovery{
		SchemaVersion: bridgeDiscoverySchema,
		Transport:     "tcp",
		Protocol:      "connect",
		URL:           baseURL,
		AuthTokenFile: authTokenFile(t, fakeBridgeToken),
	}, `
echo "$READY" >&2
while true; do sleep 1; done
`)

	replacement, errAcquire := acquireBridge("")
	if errAcquire != nil {
		t.Fatalf("acquireBridge: %v", errAcquire)
	}
	defer replacement.stop()
	if replacement == stale {
		t.Fatal("acquireBridge handed out the dead process")
	}
	if !replacement.alive() {
		t.Error("the replacement bridge is not running")
	}
}

func TestValidateBridgeBinaryAcceptsFileOrDirectory(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "bin")
	if errMkdir := os.MkdirAll(nested, 0o700); errMkdir != nil {
		t.Fatalf("mkdir: %v", errMkdir)
	}
	binary := filepath.Join(nested, bridgeExecutableName())
	if errWrite := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o700); errWrite != nil {
		t.Fatalf("write binary: %v", errWrite)
	}

	// The three shapes an operator plausibly configures.
	for _, candidate := range []string{binary, nested, root} {
		resolved, errResolve := validateBridgeBinary(candidate, "bridge-path")
		if errResolve != nil {
			t.Errorf("validateBridgeBinary(%q): %v", candidate, errResolve)
			continue
		}
		if resolved != binary {
			t.Errorf("validateBridgeBinary(%q) = %q, want %q", candidate, resolved, binary)
		}
	}
	_, errMissing := validateBridgeBinary(filepath.Join(root, "absent"), "bridge-path")
	if errMissing == nil {
		t.Error("expected a missing bridge to be reported")
	}
	if errMissing != nil && !strings.Contains(errMissing.Error(), "bridge-path") {
		t.Errorf("error = %q, want it to name the setting that was wrong", errMissing)
	}
}

// bridgeArchive builds a gzipped tar in the shape the real release uses.
func bridgeArchive(t *testing.T, members map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressor := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressor)
	for name, body := range members {
		mode := int64(0o600)
		if strings.HasSuffix(name, bridgeExecutableName()) {
			mode = 0o700
		}
		if errHeader := archive.WriteHeader(&tar.Header{
			Name: name,
			Mode: mode,
			Size: int64(len(body)),
		}); errHeader != nil {
			t.Fatalf("write tar header: %v", errHeader)
		}
		if _, errWrite := archive.Write([]byte(body)); errWrite != nil {
			t.Fatalf("write tar body: %v", errWrite)
		}
	}
	if errClose := archive.Close(); errClose != nil {
		t.Fatalf("close tar: %v", errClose)
	}
	if errClose := compressor.Close(); errClose != nil {
		t.Fatalf("close gzip: %v", errClose)
	}
	return buffer.Bytes()
}

// serveBridgeRelease stands in for the GitHub release, so the install path can be tested without
// reaching the network. It returns the platform token and the archive's real digest.
func serveBridgeRelease(t *testing.T, archive []byte) (string, string) {
	t.Helper()
	platform, errPlatform := bridgePlatform(runtime.GOOS, runtime.GOARCH)
	if errPlatform != nil {
		t.Skipf("no pinned bridge for this platform: %v", errPlatform)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, fmt.Sprintf("cursor-sdk-bridge-standalone-%s.tar.gz", platform)) {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(archive)
	}))
	t.Cleanup(server.Close)

	previous := bridgeReleaseBaseURL
	bridgeReleaseBaseURL = server.URL
	t.Cleanup(func() { bridgeReleaseBaseURL = previous })

	digest := sha256.Sum256(archive)
	return platform, hex.EncodeToString(digest[:])
}

// usePinnedChecksum swaps in the checksum for the archive a test serves, restoring the release's
// own afterwards.
func usePinnedChecksum(t *testing.T, platform, checksum string) {
	t.Helper()
	previous := bridgeArchiveChecksums[platform]
	bridgeArchiveChecksums[platform] = checksum
	t.Cleanup(func() { bridgeArchiveChecksums[platform] = previous })
}

func TestInstallBridgeUnpacksAVerifiedArchive(t *testing.T) {
	useTempCacheDir(t)
	archive := bridgeArchive(t, map[string]string{
		"bin/" + bridgeExecutableName(): "#!/bin/sh\n",
		"manifest.json":                 `{"sdkVersion":"1.0.30"}`,
	})
	platform, checksum := serveBridgeRelease(t, archive)
	usePinnedChecksum(t, platform, checksum)

	binary, errResolve := resolveBridgeBinary(defaultPluginConfig())
	if errResolve != nil {
		t.Fatalf("resolveBridgeBinary: %v", errResolve)
	}
	info, errStat := os.Stat(binary)
	if errStat != nil {
		t.Fatalf("stat installed bridge: %v", errStat)
	}
	// The entrypoint has to stay runnable, and nothing in the tree should be readable by
	// other users of the machine.
	if runtime.GOOS != "windows" {
		if mode := info.Mode().Perm(); mode != 0o700 {
			t.Errorf("mode = %o, want 0700", mode)
		}
	}
	// A second resolution reuses the unpacked tree rather than downloading again.
	bridgeReleaseBaseURL = "http://127.0.0.1:1"
	reused, errReuse := resolveBridgeBinary(defaultPluginConfig())
	if errReuse != nil || reused != binary {
		t.Errorf("second resolve = %q err = %v, want the installed %q", reused, errReuse, binary)
	}
}

// The checksums are compiled in precisely so that a substituted archive fails here. Nothing may
// be left behind that a later start would mistake for a usable bridge.
func TestInstallBridgeRejectsAChecksumMismatch(t *testing.T) {
	useTempCacheDir(t)
	platform, _ := serveBridgeRelease(t, bridgeArchive(t, map[string]string{
		"bin/" + bridgeExecutableName(): "#!/bin/sh\necho substituted\n",
	}))
	usePinnedChecksum(t, platform, strings.Repeat("0", 64))

	_, errResolve := resolveBridgeBinary(defaultPluginConfig())
	if errResolve == nil {
		t.Fatal("expected a checksum mismatch to fail the install")
	}
	if !strings.Contains(errResolve.Error(), "sha256 mismatch") {
		t.Errorf("error = %q, want a checksum mismatch", errResolve)
	}
	root, errRoot := bridgeInstallRoot()
	if errRoot != nil {
		t.Fatalf("bridgeInstallRoot: %v", errRoot)
	}
	if _, errStat := os.Stat(root); !os.IsNotExist(errStat) {
		t.Errorf("a rejected archive left %s behind: %v", root, errStat)
	}
}

func TestInstallBridgeReportsAnUnavailableRelease(t *testing.T) {
	useTempCacheDir(t)
	platform, checksum := serveBridgeRelease(t, bridgeArchive(t, map[string]string{
		"bin/" + bridgeExecutableName(): "#!/bin/sh\n",
	}))
	usePinnedChecksum(t, platform, checksum)
	// A release that is not reachable is the case an operator hits behind a firewall, and the
	// message is what tells them to configure bridge-path instead.
	bridgeReleaseBaseURL = "http://127.0.0.1:1/releases"

	_, errResolve := resolveBridgeBinary(defaultPluginConfig())
	if errResolve == nil {
		t.Fatal("expected an unreachable release to fail the install")
	}
	if !strings.Contains(errResolve.Error(), "download") {
		t.Errorf("error = %q, want it to name the download", errResolve)
	}
}

// An archive is untrusted input: a member that resolves outside the extraction directory would
// let it write anywhere the host can.
func TestExtractTarGzRejectsUnsafeArchives(t *testing.T) {
	for _, test := range []struct {
		name    string
		members map[string]string
	}{
		{name: "parent traversal", members: map[string]string{"../escaped": "x"}},
		{name: "nested traversal", members: map[string]string{"bin/../../escaped": "x"}},
		{name: "absolute path", members: map[string]string{"/etc/escaped": "x"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			archivePath := filepath.Join(dir, "bridge.tar.gz")
			if errWrite := os.WriteFile(archivePath, bridgeArchive(t, test.members), 0o600); errWrite != nil {
				t.Fatalf("write archive: %v", errWrite)
			}
			errExtract := extractTarGz(archivePath, filepath.Join(dir, "unpacked"))
			if errExtract == nil {
				t.Fatal("expected an escaping archive member to be rejected")
			}
			if !strings.Contains(errExtract.Error(), "escapes") {
				t.Errorf("error = %q, want it to name the escape", errExtract)
			}
		})
	}
}

func TestBridgePlatformNamesTheReleaseArtifacts(t *testing.T) {
	for _, test := range []struct {
		goos, goarch string
		want         string
	}{
		{goos: "darwin", goarch: "arm64", want: "darwin-arm64"},
		{goos: "darwin", goarch: "amd64", want: "darwin-x64"},
		{goos: "linux", goarch: "amd64", want: "linux-x64"},
		{goos: "linux", goarch: "arm64", want: "linux-arm64"},
		// The bridge follows Node's naming, so windows is win32.
		{goos: "windows", goarch: "amd64", want: "win32-x64"},
	} {
		got, errPlatform := bridgePlatform(test.goos, test.goarch)
		if errPlatform != nil || got != test.want {
			t.Errorf("bridgePlatform(%s, %s) = %q err = %v, want %q", test.goos, test.goarch, got, errPlatform, test.want)
		}
	}
	// The release publishes no arm64 Windows archive, and an unsupported platform has to say
	// so rather than download an archive that does not exist.
	if _, errWindows := bridgePlatform("windows", "arm64"); errWindows == nil {
		t.Error("expected windows/arm64 to be reported as unpublished")
	}
	if _, errOS := bridgePlatform("plan9", "amd64"); errOS == nil {
		t.Error("expected an unsupported OS to be reported")
	}
}

func TestBridgeArchiveURLMatchesTheReleaseLayout(t *testing.T) {
	previous := bridgeReleaseBaseURL
	bridgeReleaseBaseURL = "https://github.com/cursor/sdk-bridge/releases/download"
	t.Cleanup(func() { bridgeReleaseBaseURL = previous })

	want := "https://github.com/cursor/sdk-bridge/releases/download/v" + bridgeVersion +
		"/cursor-sdk-bridge-standalone-linux-x64.tar.gz"
	if got := bridgeArchiveURL("linux-x64"); got != want {
		t.Errorf("bridgeArchiveURL = %q, want %q", got, want)
	}
}

// Every platform the plugin is released for needs a pinned checksum, or its first start would
// fail with "no checksum is recorded" instead of installing.
func TestPinnedChecksumsCoverEveryReleasedPlatform(t *testing.T) {
	for _, platform := range []string{"darwin-arm64", "darwin-x64", "linux-arm64", "linux-x64", "win32-x64"} {
		checksum := bridgeArchiveChecksums[platform]
		if len(checksum) != 64 {
			t.Errorf("checksum for %s = %q, want a sha256 digest", platform, checksum)
		}
	}
}

// TestLiveBridgeHandshake runs the real cursor-sdk-bridge, which is the only way to confirm the
// startup contract this plugin depends on: the ready line, the discovery fields, and that the
// bearer token it hands over is accepted. It needs no Cursor credential, but it does need the
// binary, so it is opt-in:
//
//	CURSOR_SDK_BRIDGE_BIN=/path/to/cursor-sdk-bridge go test -run TestLiveBridgeHandshake ./...
func TestLiveBridgeHandshake(t *testing.T) {
	binary := strings.TrimSpace(os.Getenv(bridgeBinaryEnv))
	if binary == "" {
		t.Skipf("set %s to run against a real bridge", bridgeBinaryEnv)
	}
	useTempCacheDir(t)
	// The real bridge needs its own environment variable back: useTempCacheDir does not touch
	// it, but the cache redirect above must not send the state root somewhere unwritable.
	t.Setenv(bridgeBinaryEnv, binary)

	process, errStart := startBridge(defaultPluginConfig(), "")
	if errStart != nil {
		t.Fatalf("startBridge: %v", errStart)
	}
	defer process.stop()

	if errPing := process.ping(); errPing != nil {
		t.Errorf("ping: %v", errPing)
	}
	// Me without a credential must be refused, which proves the plugin reaches the service and
	// that the bridge takes no key from the environment.
	_, errMe := process.cursor.Me(context.Background(), connect.NewRequest(&sdkv1.MeRequest{}))
	if errMe == nil {
		t.Error("Me succeeded without a credential, so the bridge found a key somewhere")
	} else {
		t.Logf("Me without a credential: %v", errMe)
	}
}

// The bridge's durable agent state is keyed by plugin version and kept out of the auth
// directory, where the host scans every file as a credential candidate.
func TestBridgeStateRootIsVersionedAndOutsideTheAuthDirectory(t *testing.T) {
	cache := useTempCacheDir(t)
	root, errRoot := bridgeStateRoot()
	if errRoot != nil {
		t.Fatalf("bridgeStateRoot: %v", errRoot)
	}
	if !strings.Contains(root, pluginVersion) {
		t.Errorf("state root = %q, want it keyed by the plugin version %q", root, pluginVersion)
	}
	if strings.Contains(root, ".cli-proxy-api"+string(filepath.Separator)) {
		t.Errorf("state root = %q, want it outside the host's default auth directory", root)
	}
	if !strings.HasPrefix(root, cache) {
		t.Errorf("state root = %q, want it under the cache directory %q", root, cache)
	}
}
