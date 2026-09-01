package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	sdkv1 "github.com/UNICKCHENG/cliproxyapi-plugins/auth-cursor/go/internal/sdk/v1"
	"github.com/UNICKCHENG/cliproxyapi-plugins/auth-cursor/go/internal/sdk/v1/sdkv1connect"
)

const (
	// bridgeReadyPrefix introduces the single discovery line the bridge prints on stderr once it
	// is listening. The trailing space is part of the contract.
	bridgeReadyPrefix = "cursor-sdk-bridge ready "
	// bridgeDiscoverySchema is the only discovery payload schema this plugin understands.
	bridgeDiscoverySchema = 1
	// bridgeStderrTail caps how much bridge stderr is retained for diagnostics.
	bridgeStderrTail = 4096
	// bridgeStderrLineMax caps one stderr line so a runaway diagnostic cannot exhaust memory.
	bridgeStderrLineMax = 1 << 20
)

// These bound the process lifecycle. They are variables rather than constants so that the tests
// covering the startup and shutdown paths do not have to wait out the production values.
var (
	// bridgeStartupTimeout bounds the wait for the discovery line, matching the timeout the
	// reference adapters use.
	bridgeStartupTimeout = 30 * time.Second
	// bridgeShutdownTimeout bounds the graceful stop before the process is killed.
	bridgeShutdownTimeout = 5 * time.Second
)

// bridgeDiscovery is the JSON object following bridgeReadyPrefix. Unknown fields are
// forward-compatible additions and are ignored.
type bridgeDiscovery struct {
	SchemaVersion int    `json:"schemaVersion"`
	ServerVersion string `json:"serverVersion"`
	PID           int    `json:"pid"`
	Transport     string `json:"transport"`
	Protocol      string `json:"protocol"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	URL           string `json:"url"`
	// AuthToken is only present on older bridges; current ones write the token to a file.
	AuthToken     string `json:"authToken"`
	AuthTokenFile string `json:"authTokenFile"`
	StateRoot     string `json:"stateRoot"`
}

// validate rejects a discovery payload this plugin cannot speak to. Guessing at an unexpected
// transport would turn a clear startup failure into an unexplained connection error later.
func (d bridgeDiscovery) validate() error {
	if d.SchemaVersion != bridgeDiscoverySchema {
		return fmt.Errorf("unsupported bridge discovery schema %d", d.SchemaVersion)
	}
	if d.Transport != "tcp" {
		return fmt.Errorf("unsupported bridge transport %q", d.Transport)
	}
	if d.Protocol != "connect" {
		return fmt.Errorf("unsupported bridge protocol %q", d.Protocol)
	}
	if d.baseURL() == "" {
		return fmt.Errorf("bridge discovery carries no address")
	}
	parsed, errURL := url.Parse(d.baseURL())
	if errURL != nil {
		return fmt.Errorf("bridge discovery URL: %w", errURL)
	}
	if !strings.EqualFold(parsed.Scheme, "http") {
		return fmt.Errorf("bridge discovery URL must be http, got %q", parsed.Scheme)
	}
	if !isLoopbackHost(parsed.Hostname()) {
		return fmt.Errorf("bridge discovery host %q is not loopback", parsed.Hostname())
	}
	return nil
}

// baseURL prefers the url field and falls back to host and port, bracketing IPv6 literals.
func (d bridgeDiscovery) baseURL() string {
	if trimmed := strings.TrimRight(strings.TrimSpace(d.URL), "/"); trimmed != "" {
		return trimmed
	}
	host := strings.TrimSpace(d.Host)
	if host == "" || d.Port <= 0 {
		return ""
	}
	return "http://" + net.JoinHostPort(host, fmt.Sprint(d.Port))
}

// bridgeProcess is one running cursor-sdk-bridge together with the clients bound to it.
type bridgeProcess struct {
	cmd       *exec.Cmd
	workspace string
	stderr    *tailBuffer
	exited    chan struct{}
	// egress is the loopback hop this process reaches Cursor through, or nil when it connects
	// directly.
	egress *bridgeEgress

	callbackSrv   *http.Server
	callbackURL   string
	callbackToken string

	agent   sdkv1connect.SdkAgentServiceClient
	cursor  sdkv1connect.SdkCursorServiceClient
	control sdkv1connect.SdkBridgeControlServiceClient

	stopOnce sync.Once
}

// alive reports whether the process is still running.
func (p *bridgeProcess) alive() bool {
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

// exitReason renders why the bridge died, including the retained stderr tail.
func (p *bridgeProcess) exitReason() string {
	reason := "cursor sdk bridge exited"
	if tail := p.stderr.String(); tail != "" {
		reason = reason + ": " + tail
	}
	return reason
}

// stop asks the bridge to drain, escalates to a kill, and removes its scratch workspace.
func (p *bridgeProcess) stop() {
	p.stopOnce.Do(func() {
		failToolRunsForProcess(p)
		evictSessionsForProcess(p)
		ctx, cancel := context.WithTimeout(context.Background(), bridgeShutdownTimeout)
		defer cancel()
		if p.control != nil {
			_, _ = p.control.Shutdown(ctx, connect.NewRequest(&sdkv1.ShutdownRequest{GraceSeconds: 1}))
		}
		if p.exited != nil {
			select {
			case <-p.exited:
			case <-time.After(bridgeShutdownTimeout):
				if p.cmd != nil && p.cmd.Process != nil {
					_ = p.cmd.Process.Kill()
				}
				<-p.exited
			}
		}
		if p.workspace != "" {
			_ = os.RemoveAll(p.workspace)
		}
		// After the process, so a graceful drain still has somewhere to send its last calls.
		if p.egress != nil {
			p.egress.stop()
		}
		stopToolCallback(p)
	})
}

// bridgePool holds one bridge process per resolved proxy URL.
//
// The proxy is a property of the process (the bridge reaches Cursor itself, and its runtime only
// takes a proxy from the environment), while the API key is a property of every request. So
// credentials that share a proxy share a bridge, and the pool is keyed by nothing else.
var bridgePool = struct {
	mu     sync.Mutex
	procs  map[string]*bridgeProcess
	gen    uint64
	starts map[string]*bridgeStart
}{procs: make(map[string]*bridgeProcess), starts: make(map[string]*bridgeStart)}

type bridgeStart struct {
	done    chan struct{}
	process *bridgeProcess
	err     error
}

// acquireBridge returns the bridge serving proxyURL, starting one if there is none.
//
// The pool lock only protects the map. Starting and installing happen outside it so a 15-minute
// download for one proxy cannot block model discovery or requests that use another proxy.
func acquireBridge(proxyURL string) (*bridgeProcess, error) {
	bridgePool.mu.Lock()
	if existing := bridgePool.procs[proxyURL]; existing != nil {
		if existing.alive() {
			bridgePool.mu.Unlock()
			return existing, nil
		}
		hostLog("warn", "cursor sdk bridge restarting", map[string]any{"reason": existing.exitReason()})
		delete(bridgePool.procs, proxyURL)
	}
	if inFlight := bridgePool.starts[proxyURL]; inFlight != nil {
		bridgePool.mu.Unlock()
		<-inFlight.done
		if inFlight.err != nil {
			return nil, inFlight.err
		}
		if inFlight.process != nil && inFlight.process.alive() {
			return inFlight.process, nil
		}
		return acquireBridge(proxyURL)
	}
	start := &bridgeStart{done: make(chan struct{})}
	bridgePool.starts[proxyURL] = start
	gen := bridgePool.gen
	bridgePool.mu.Unlock()

	process, errStart := startBridge(loadedConfig(), proxyURL)

	bridgePool.mu.Lock()
	delete(bridgePool.starts, proxyURL)
	if errStart != nil {
		start.err = errStart
		close(start.done)
		bridgePool.mu.Unlock()
		return nil, errStart
	}
	if bridgePool.gen != gen {
		bridgePool.mu.Unlock()
		process.stop()
		start.err = fmt.Errorf("cursor sdk bridge was stopped before it was registered")
		close(start.done)
		return nil, start.err
	}
	bridgePool.procs[proxyURL] = process
	start.process = process
	close(start.done)
	bridgePool.mu.Unlock()
	return process, nil
}

func stopBridges() {
	bridgePool.mu.Lock()
	bridgePool.gen++
	processes := bridgePool.procs
	bridgePool.procs = make(map[string]*bridgeProcess)
	bridgePool.mu.Unlock()
	for _, process := range processes {
		process.stop()
	}
}

// startBridge spawns the bridge, completes the handshake and verifies it answers RPCs.
func startBridge(cfg pluginConfig, proxyURL string) (*bridgeProcess, error) {
	binary, errBinary := resolveBridgeBinary(cfg)
	if errBinary != nil {
		return nil, errBinary
	}
	stateRoot, errState := bridgeStateRoot()
	if errState != nil {
		return nil, errState
	}
	if errMkdir := os.MkdirAll(stateRoot, 0o700); errMkdir != nil {
		return nil, fmt.Errorf("create cursor sdk bridge state directory %s: %w", stateRoot, errMkdir)
	}
	// Runs execute against a throwaway empty directory: the agent is used purely as a text
	// generator, so it must not see the host's working tree.
	workspace, errWorkspace := os.MkdirTemp("", "cliproxy-cursor-")
	if errWorkspace != nil {
		return nil, fmt.Errorf("create cursor sdk bridge workspace: %w", errWorkspace)
	}

	// A proxy has to be applied here rather than in the child: the bridge's runtime ignores the
	// proxy environment variables on the path its backend calls take. Failing the start when the
	// egress cannot come up is deliberate, because falling back to a direct connection would
	// quietly reinstate the timeout the egress exists to prevent.
	egress, errEgress := startBridgeEgress(proxyURL)
	if errEgress != nil {
		_ = os.RemoveAll(workspace)
		return nil, errEgress
	}
	backendURL := ""
	if egress != nil {
		backendURL = egress.baseURL
	}

	cmd := exec.Command(binary, "--workspace", workspace, "--state-root", stateRoot)
	cmd.Env = bridgeEnv(proxyURL, backendURL)
	// The bridge speaks only on stderr, but a child inheriting the host's stdout could still
	// corrupt its log stream.
	cmd.Stdout = io.Discard
	stderrPipe, errStderr := cmd.StderrPipe()
	if errStderr != nil {
		_ = os.RemoveAll(workspace)
		egress.stop()
		return nil, fmt.Errorf("open cursor sdk bridge stderr: %w", errStderr)
	}
	if errStart := cmd.Start(); errStart != nil {
		_ = os.RemoveAll(workspace)
		egress.stop()
		return nil, fmt.Errorf("start cursor sdk bridge (%s): %w", binary, errStart)
	}

	process := &bridgeProcess{
		cmd:       cmd,
		workspace: workspace,
		stderr:    &tailBuffer{},
		exited:    make(chan struct{}),
		egress:    egress,
	}
	discovered := make(chan bridgeDiscovery, 1)
	failed := make(chan error, 1)
	drained := make(chan struct{})
	go process.readStderr(stderrPipe, discovered, failed, drained)
	go func() {
		// stderr must be fully consumed before Wait, otherwise Wait closes the pipe under the
		// reader. Draining also matters for the lifetime of the process: a full stderr buffer
		// blocks the bridge.
		<-drained
		_ = cmd.Wait()
		close(process.exited)
	}()

	discovery, errHandshake := process.handshake(discovered, failed)
	if errHandshake != nil {
		process.control = nil
		process.stop()
		return nil, errHandshake
	}
	if errClients := process.bindClients(discovery); errClients != nil {
		process.stop()
		return nil, errClients
	}
	if errPing := process.ping(); errPing != nil {
		process.stop()
		return nil, errPing
	}
	if errCallback := startToolCallback(process); errCallback != nil {
		process.stop()
		return nil, errCallback
	}
	hostLog("info", "cursor sdk bridge ready", map[string]any{
		"bridge_version": discovery.ServerVersion,
		"pid":            discovery.PID,
		"proxy":          proxyURL != "",
		"egress":         egress != nil,
	})
	return process, nil
}

// readStderr scans for the discovery line and forwards every other line to the host log.
//
// The discovery line itself is never logged and never enters the stderr tail: older bridges
// inline the bearer token in it.
func (p *bridgeProcess) readStderr(pipe io.Reader, discovered chan<- bridgeDiscovery, failed chan<- error, drained chan<- struct{}) {
	defer close(drained)
	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 0, 64*1024), bridgeStderrLineMax)
	announced := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, bridgeReadyPrefix) {
			var discovery bridgeDiscovery
			if errUnmarshal := json.Unmarshal([]byte(line[len(bridgeReadyPrefix):]), &discovery); errUnmarshal != nil {
				p.stderr.WriteString("invalid discovery line: " + errUnmarshal.Error())
				continue
			}
			if errValidate := discovery.validate(); errValidate != nil {
				p.stderr.WriteString(errValidate.Error())
				continue
			}
			if !announced {
				announced = true
				discovered <- discovery
			}
			continue
		}
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			p.stderr.WriteString(trimmed)
		}
	}
	if errScan := scanner.Err(); errScan != nil && !announced {
		select {
		case failed <- fmt.Errorf("read cursor sdk bridge stderr: %w", errScan):
		default:
		}
	}
}

// handshake waits for the discovery line, the process to die, or the startup timeout.
func (p *bridgeProcess) handshake(discovered <-chan bridgeDiscovery, failed <-chan error) (bridgeDiscovery, error) {
	timeout := time.NewTimer(bridgeStartupTimeout)
	defer timeout.Stop()
	select {
	case discovery := <-discovered:
		return discovery, nil
	case errFailed := <-failed:
		return bridgeDiscovery{}, errFailed
	case <-p.exited:
		return bridgeDiscovery{}, fmt.Errorf("cursor sdk bridge exited before it was ready: %s", p.stderr.String())
	case <-timeout.C:
		return bridgeDiscovery{}, fmt.Errorf("cursor sdk bridge was not ready within %s: %s", bridgeStartupTimeout, p.stderr.String())
	}
}

// bindClients reads the bearer token and constructs the three service clients.
func (p *bridgeProcess) bindClients(discovery bridgeDiscovery) error {
	token, errToken := bridgeAuthToken(discovery)
	if errToken != nil {
		return errToken
	}
	// The bridge serves Connect over HTTP/1.1 only, and it listens on loopback, so the client
	// must neither negotiate HTTP/2 nor honour a proxy. No client timeout either: a run has no
	// bound the plugin is entitled to impose.
	client := &http.Client{Transport: &http.Transport{
		Proxy:               nil,
		ForceAttemptHTTP2:   false,
		MaxIdleConnsPerHost: 8,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
	}}
	options := connect.WithInterceptors(bridgeBearerAuth("Bearer " + token))
	base := discovery.baseURL()
	p.agent = sdkv1connect.NewSdkAgentServiceClient(client, base, options)
	p.cursor = sdkv1connect.NewSdkCursorServiceClient(client, base, options)
	p.control = sdkv1connect.NewSdkBridgeControlServiceClient(client, base, options)
	return nil
}

func (p *bridgeProcess) ping() error {
	ctx, cancel := context.WithTimeout(context.Background(), bridgeStartupTimeout)
	defer cancel()
	if _, errPing := p.control.Ping(ctx, connect.NewRequest(&sdkv1.PingRequest{})); errPing != nil {
		return fmt.Errorf("cursor sdk bridge did not answer Ping: %w", errPing)
	}
	return nil
}

// bridgeAuthToken resolves the per-process bearer token from the discovery payload.
func bridgeAuthToken(discovery bridgeDiscovery) (string, error) {
	if inline := strings.TrimSpace(discovery.AuthToken); inline != "" {
		return inline, nil
	}
	path := strings.TrimSpace(discovery.AuthTokenFile)
	if path == "" {
		return "", fmt.Errorf("cursor sdk bridge discovery carries no auth token")
	}
	if errPath := validateBridgeTokenPath(path); errPath != nil {
		return "", errPath
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		return "", fmt.Errorf("read cursor sdk bridge auth token: %w", errRead)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("cursor sdk bridge auth token file %s is empty", path)
	}
	return token, nil
}

// bridgeBearerAuth attaches the per-process bearer token to every RPC. Streaming calls need it
// as much as unary ones, which is why this is a full interceptor rather than a unary func: a
// Send stream opened without the header is rejected with UNAUTHENTICATED.
type bridgeBearerAuth string

func (a bridgeBearerAuth) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, request connect.AnyRequest) (connect.AnyResponse, error) {
		request.Header().Set("Authorization", string(a))
		return next(ctx, request)
	}
}

func (a bridgeBearerAuth) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", string(a))
		return conn
	}
}

// WrapStreamingHandler is a no-op: this plugin serves no sdk.v1 handlers.
func (a bridgeBearerAuth) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// bridgeProxyEnvVars are the variables the bridge's runtime reads for its outbound proxy. Both
// cases are written because runtimes disagree on which they honour.
//
// These only reach the part of the SDK that goes through bun's own fetch(); the backend calls run
// on bun's node:http default Agent, which ignores them entirely. That is what the egress in
// bridge_egress.go exists for, and why these variables are not enough on their own.
var bridgeProxyEnvVars = []string{
	"HTTP_PROXY", "http_proxy",
	"HTTPS_PROXY", "https_proxy",
	"ALL_PROXY", "all_proxy",
}

// bridgeNoProxyEnvVars are the exemption lists, in both cases for the same reason as above.
var bridgeNoProxyEnvVars = []string{"NO_PROXY", "no_proxy"}

// bridgeLoopbackNoProxy are the hosts the bridge has to reach directly once its backend URL is
// the plugin's own egress: sending that hop through the user's proxy would route a local
// connection out to the internet and back.
var bridgeLoopbackNoProxy = []string{"127.0.0.1", "::1", "localhost"}

// bridgeStrippedEnvVars are dropped from the inherited environment.
//
// CURSOR_API_KEY is dropped because a pooled bridge is shared by every credential using the same
// proxy: there is no single key to inject, so the plugin passes one explicitly on every request
// instead and must not let an ambient key silently become the fallback. The rest would compete
// with the flags this plugin passes.
var bridgeStrippedEnvVars = []string{
	"CURSOR_API_KEY",
	"CURSOR_SDK_BRIDGE_WORKSPACE",
	"CURSOR_SDK_BRIDGE_STATE_ROOT",
	"CURSOR_SDK_BRIDGE_HOST",
	"CURSOR_SDK_BRIDGE_PORT",
}

// bridgeEnv builds the child environment.
//
// An inherited proxy setting is left alone when the plugin has none of its own, so a machine that
// only reaches the internet through a proxy keeps working without extra configuration; a resolved
// proxyURL overrides it.
//
// backendURL, when set, is the plugin's egress, and the bridge is pointed at it instead of Cursor.
// The proxy variables are still written alongside it: they remain the only thing that proxies the
// SDK's fetch() paths, and the loopback hop to the egress is exempted rather than dropped.
func bridgeEnv(proxyURL, backendURL string) []string {
	stripped := make(map[string]struct{}, len(bridgeStrippedEnvVars)+len(bridgeProxyEnvVars)+len(bridgeNoProxyEnvVars)+1)
	for _, name := range bridgeStrippedEnvVars {
		stripped[name] = struct{}{}
	}
	if proxyURL != "" {
		for _, name := range bridgeProxyEnvVars {
			stripped[name] = struct{}{}
		}
	}
	if backendURL != "" {
		// An inherited backend URL is replaced rather than honoured: the egress is the whole
		// point of this process, and a second opinion on where Cursor lives would defeat it.
		stripped["CURSOR_BACKEND_URL"] = struct{}{}
		for _, name := range bridgeNoProxyEnvVars {
			stripped[name] = struct{}{}
		}
	}
	env := make([]string, 0, len(os.Environ())+len(bridgeProxyEnvVars)+len(bridgeNoProxyEnvVars)+2)
	for _, entry := range os.Environ() {
		name, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		if _, drop := stripped[name]; drop {
			continue
		}
		env = append(env, entry)
	}
	if proxyURL != "" {
		for _, name := range bridgeProxyEnvVars {
			env = append(env, name+"="+proxyURL)
		}
	}
	if backendURL != "" {
		env = append(env, "CURSOR_BACKEND_URL="+backendURL)
		noProxy := noProxyWithLoopback(os.Getenv("NO_PROXY"), os.Getenv("no_proxy"))
		for _, name := range bridgeNoProxyEnvVars {
			env = append(env, name+"="+noProxy)
		}
	}
	// Lets Cursor attribute this traffic to the Go adapter surface.
	env = append(env, "CURSOR_SDK_CLIENT_LANGUAGE=go")
	return env
}

// noProxyWithLoopback adds the loopback hosts to the exemption lists the host already had, so an
// operator's own entries survive.
func noProxyWithLoopback(inherited ...string) string {
	entries := make([]string, 0, len(bridgeLoopbackNoProxy)+4)
	seen := make(map[string]struct{}, len(bridgeLoopbackNoProxy)+4)
	add := func(list string) {
		for _, entry := range strings.Split(list, ",") {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			key := strings.ToLower(entry)
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			entries = append(entries, entry)
		}
	}
	for _, list := range inherited {
		add(list)
	}
	for _, host := range bridgeLoopbackNoProxy {
		add(host)
	}
	return strings.Join(entries, ",")
}

// tailBuffer keeps the most recent bytes written to it, so a crashed process can report why it
// died without retaining unbounded output.
type tailBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (t *tailBuffer) WriteString(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.data) > 0 {
		t.data = append(t.data, '\n')
	}
	t.data = append(t.data, s...)
	if len(t.data) > bridgeStderrTail {
		t.data = t.data[len(t.data)-bridgeStderrTail:]
	}
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.data))
}

func fileExists(path string) bool {
	info, errStat := os.Stat(path)
	return errStat == nil && !info.IsDir()
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		ips, errLookup := net.LookupIP(host)
		if errLookup != nil || len(ips) == 0 {
			return false
		}
		for _, resolved := range ips {
			if !resolved.IsLoopback() {
				return false
			}
		}
		return true
	}
	return ip.IsLoopback()
}

// bridgeTokenDirPrefix names the private directory the bridge creates under the system temp
// directory for its bearer token. Verified against cursor-sdk-bridge 1.0.30, which reports
// $TMPDIR/cursor-sdk-bridge-XXXXXX/auth-token rather than a path under --state-root.
const bridgeTokenDirPrefix = "cursor-sdk-bridge-"

// validateBridgeTokenPath keeps a substituted bridge from naming an arbitrary local file as its
// bearer token, which the plugin would otherwise read and send upstream.
//
// Two locations are accepted: the state directory this process passed to the bridge, and the
// bridge's own private directory under the system temp directory. The temp case is where a
// current bridge actually writes, so it also carries the ownership checks that a shared /tmp
// needs: a regular file, reached without a symlink hop, and not writable by other users.
func validateBridgeTokenPath(path string) error {
	resolved, errResolve := filepath.EvalSymlinks(path)
	if errResolve != nil {
		return fmt.Errorf("resolve cursor sdk bridge auth token file: %w", errResolve)
	}
	if stateRoot, errRoot := bridgeStateRoot(); errRoot == nil && pathInsideRoot(resolved, stateRoot) {
		return nil
	}
	if errTemp := tokenPathInBridgeTempDir(resolved); errTemp != nil {
		return errTemp
	}
	info, errStat := os.Lstat(resolved)
	if errStat != nil {
		return fmt.Errorf("inspect cursor sdk bridge auth token file: %w", errStat)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("cursor sdk bridge auth token file is not a regular file")
	}
	// Windows reports POSIX-looking bits that do not describe sharing, so the check is limited
	// to the platforms where a world-writable temp directory is a real substitution vector.
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("cursor sdk bridge auth token file is writable by other users")
	}
	return nil
}

// tokenPathInBridgeTempDir accepts $TMPDIR/cursor-sdk-bridge-XXXXXX/auth-token and nothing else
// under the temp directory, so a token file cannot be an arbitrary path a hostile bridge names.
func tokenPathInBridgeTempDir(resolved string) error {
	outside := fmt.Errorf("cursor sdk bridge auth token file is outside the state and bridge temp directories")
	tempRoot, errTemp := filepath.EvalSymlinks(os.TempDir())
	if errTemp != nil {
		return outside
	}
	parent := filepath.Dir(resolved)
	if filepath.Clean(filepath.Dir(parent)) != filepath.Clean(tempRoot) {
		return outside
	}
	if !strings.HasPrefix(filepath.Base(parent), bridgeTokenDirPrefix) {
		return outside
	}
	return nil
}

func pathInsideRoot(path, root string) bool {
	if strings.TrimSpace(path) == "" || strings.TrimSpace(root) == "" {
		return false
	}
	resolvedRoot, errRoot := filepath.EvalSymlinks(root)
	if errRoot != nil {
		resolvedRoot = filepath.Clean(root)
	}
	rel, errRel := filepath.Rel(resolvedRoot, filepath.Clean(path))
	if errRel != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
