package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
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
		ctx, cancel := context.WithTimeout(context.Background(), bridgeShutdownTimeout)
		defer cancel()
		if p.control != nil {
			_, _ = p.control.Shutdown(ctx, connect.NewRequest(&sdkv1.ShutdownRequest{GraceSeconds: 1}))
		}
		select {
		case <-p.exited:
		case <-time.After(bridgeShutdownTimeout):
			if p.cmd.Process != nil {
				_ = p.cmd.Process.Kill()
			}
			<-p.exited
		}
		if p.workspace != "" {
			_ = os.RemoveAll(p.workspace)
		}
		// After the process, so a graceful drain still has somewhere to send its last calls.
		p.egress.stop()
	})
}

// bridgePool holds one bridge process per resolved proxy URL.
//
// The proxy is a property of the process (the bridge reaches Cursor itself, and its runtime only
// takes a proxy from the environment), while the API key is a property of every request. So
// credentials that share a proxy share a bridge, and the pool is keyed by nothing else.
var bridgePool = struct {
	mu    sync.Mutex
	procs map[string]*bridgeProcess
}{procs: make(map[string]*bridgeProcess)}

// acquireBridge returns the bridge serving proxyURL, starting one if there is none.
func acquireBridge(proxyURL string) (*bridgeProcess, error) {
	bridgePool.mu.Lock()
	defer bridgePool.mu.Unlock()
	if existing := bridgePool.procs[proxyURL]; existing != nil {
		if existing.alive() {
			return existing, nil
		}
		hostLog("warn", "cursor sdk bridge restarting", map[string]any{"reason": existing.exitReason()})
		delete(bridgePool.procs, proxyURL)
	}
	process, errStart := startBridge(loadedConfig(), proxyURL)
	if errStart != nil {
		return nil, errStart
	}
	bridgePool.procs[proxyURL] = process
	return process, nil
}

// stopBridges shuts every pooled bridge down. It runs on plugin shutdown and whenever a
// configuration change invalidates the running processes.
func stopBridges() {
	bridgePool.mu.Lock()
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
		if !announced && strings.HasPrefix(line, bridgeReadyPrefix) {
			announced = true
			var discovery bridgeDiscovery
			if errUnmarshal := json.Unmarshal([]byte(line[len(bridgeReadyPrefix):]), &discovery); errUnmarshal != nil {
				failed <- fmt.Errorf("decode cursor sdk bridge discovery line: %w", errUnmarshal)
				continue
			}
			if errValidate := discovery.validate(); errValidate != nil {
				failed <- errValidate
				continue
			}
			discovered <- discovery
			continue
		}
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			p.stderr.WriteString(trimmed)
			hostLog("debug", "cursor sdk bridge stderr", map[string]any{"line": trimmed})
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
