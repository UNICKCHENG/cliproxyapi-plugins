package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// sidecarStderrTail caps how much sidecar stderr is retained for diagnostics.
const sidecarStderrTail = 4096

type sidecarParam struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

// sidecarImage mirrors the Cursor SDK's SDKImage union: either a remote URL or inline
// base64 data with its media type.
type sidecarImage struct {
	URL      string `json:"url,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

type sidecarRequest struct {
	ID          string         `json:"id"`
	Op          string         `json:"op"`
	APIKey      string         `json:"api_key,omitempty"`
	Model       string         `json:"model,omitempty"`
	Params      []sidecarParam `json:"params,omitempty"`
	OptimizeFor string         `json:"optimize_for,omitempty"`
	Prompt      string         `json:"prompt,omitempty"`
	Images      []sidecarImage `json:"images,omitempty"`
	Stream      bool           `json:"stream,omitempty"`
	// NoBrowser is inverted relative to the SDK option so that omitting it keeps the
	// default browser-opening behaviour for every non-login request.
	NoBrowser  bool   `json:"no_browser,omitempty"`
	APIKeyName string `json:"api_key_name,omitempty"`
}

type sidecarUsage struct {
	InputTokens     int64 `json:"inputTokens"`
	OutputTokens    int64 `json:"outputTokens"`
	CacheReadTokens int64 `json:"cacheReadTokens"`
	TotalTokens     int64 `json:"totalTokens"`
	ReasoningTokens int64 `json:"reasoningTokens"`
}

type sidecarModel struct {
	ID          string   `json:"id"`
	DisplayName string   `json:"display_name"`
	Description string   `json:"description"`
	Aliases     []string `json:"aliases"`
	Parameters  []string `json:"parameters"`
}

type sidecarEvent struct {
	ID         string              `json:"id"`
	Event      string              `json:"event"`
	Text       string              `json:"text"`
	Status     string              `json:"status"`
	Message    string              `json:"message"`
	Code       string              `json:"code"`
	HTTPStatus int                 `json:"http_status"`
	Retryable  bool                `json:"retryable"`
	Models     []sidecarModel      `json:"models"`
	Usage      *sidecarUsage       `json:"usage"`
	URL        string              `json:"url"`
	APIKey     string              `json:"api_key"`
	Email      string              `json:"email"`
	ExpiresAt  int64               `json:"expires_at_ms"`
	Method     string              `json:"method,omitempty"`
	Headers    map[string][]string `json:"headers,omitempty"`
	Body       string              `json:"body,omitempty"`
	Stream     bool                `json:"stream,omitempty"`
}

// errorText renders an error event for the host, keeping the upstream HTTP status visible
// because it is what distinguishes a bad key from a rate limit.
func (e sidecarEvent) errorText() string {
	message := strings.TrimSpace(e.Message)
	if message == "" {
		message = "cursor request failed"
	}
	if e.HTTPStatus > 0 {
		return fmt.Sprintf("cursor upstream error %d: %s", e.HTTPStatus, message)
	}
	return message
}

func (e sidecarEvent) terminal() bool {
	switch e.Event {
	case "done", "error", "models", "login":
		return true
	default:
		return false
	}
}

type pendingCall struct {
	events chan sidecarEvent
	done   chan struct{}
}

type sidecarProcess struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stderr  *tailBuffer
	writeMu sync.Mutex

	mu      sync.Mutex
	pending map[string]*pendingCall
	closed  bool
}

var (
	sidecarMu      sync.Mutex
	sidecarCurrent *sidecarProcess
	sidecarSeq     atomic.Uint64
)

// tailBuffer keeps the most recent bytes written to it, so a crashed sidecar can report
// why it died without retaining unbounded output.
type tailBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data = append(t.data, p...)
	if len(t.data) > sidecarStderrTail {
		t.data = t.data[len(t.data)-sidecarStderrTail:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.data))
}

// callSidecar sends one request and streams the resulting events. The returned release
// function must always be called; it cancels an in-flight run and frees the routing slot.
//
// No deadline is applied to the exchange: once the sidecar has an upstream Cursor
// connection, the plugin must not impose its own timeout on the response.
func callSidecar(ctx context.Context, req sidecarRequest) (<-chan sidecarEvent, func(), error) {
	process, errProcess := acquireSidecar()
	if errProcess != nil {
		return nil, func() {}, errProcess
	}
	req.ID = strconv.FormatUint(sidecarSeq.Add(1), 10)

	call := &pendingCall{events: make(chan sidecarEvent, 64), done: make(chan struct{})}
	process.mu.Lock()
	if process.closed {
		process.mu.Unlock()
		return nil, func() {}, fmt.Errorf("cursor sidecar is not running")
	}
	process.pending[req.ID] = call
	process.mu.Unlock()

	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(call.done)
			process.mu.Lock()
			if process.pending[req.ID] == call {
				delete(process.pending, req.ID)
			}
			process.mu.Unlock()
		})
	}

	if errWrite := process.write(req); errWrite != nil {
		release()
		return nil, func() {}, errWrite
	}

	// Cancel the upstream run when the caller goes away so the sidecar stops billing work
	// nobody is reading.
	if ctx != nil && ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				_ = process.write(sidecarRequest{ID: req.ID, Op: "cancel"})
			case <-call.done:
			}
		}()
	}

	return call.events, release, nil
}

func (p *sidecarProcess) write(req sidecarRequest) error {
	raw, errMarshal := json.Marshal(req)
	if errMarshal != nil {
		return fmt.Errorf("encode cursor sidecar request: %w", errMarshal)
	}
	raw = append(raw, '\n')
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if _, errWrite := p.stdin.Write(raw); errWrite != nil {
		return fmt.Errorf("write to cursor sidecar: %w (%s)", errWrite, p.stderr.String())
	}
	return nil
}

// acquireSidecar returns the running sidecar, starting one if needed.
func acquireSidecar() (*sidecarProcess, error) {
	sidecarMu.Lock()
	defer sidecarMu.Unlock()
	if sidecarCurrent != nil {
		sidecarCurrent.mu.Lock()
		alive := !sidecarCurrent.closed
		sidecarCurrent.mu.Unlock()
		if alive {
			return sidecarCurrent, nil
		}
		sidecarCurrent = nil
	}
	process, errStart := startSidecar(loadedConfig())
	if errStart != nil {
		return nil, errStart
	}
	sidecarCurrent = process
	return process, nil
}

func stopSidecar() {
	sidecarMu.Lock()
	process := sidecarCurrent
	sidecarCurrent = nil
	sidecarMu.Unlock()
	if process == nil {
		return
	}
	// Closing stdin asks the sidecar to exit; killing covers a wedged process.
	_ = process.stdin.Close()
	if process.cmd.Process != nil {
		_ = process.cmd.Process.Kill()
	}
}

func startSidecar(cfg pluginConfig) (*sidecarProcess, error) {
	script, errScript := resolveSidecarScript(cfg)
	if errScript != nil {
		return nil, errScript
	}
	cmd := exec.Command(cfg.NodePath, script)
	// Run inside the sidecar directory so Node resolves its own node_modules.
	cmd.Dir = filepath.Dir(script)
	cmd.Env = os.Environ()

	stdin, errStdin := cmd.StdinPipe()
	if errStdin != nil {
		return nil, fmt.Errorf("open cursor sidecar stdin: %w", errStdin)
	}
	stdout, errStdout := cmd.StdoutPipe()
	if errStdout != nil {
		return nil, fmt.Errorf("open cursor sidecar stdout: %w", errStdout)
	}
	stderr := &tailBuffer{}
	cmd.Stderr = stderr

	if errStart := cmd.Start(); errStart != nil {
		return nil, fmt.Errorf("start cursor sidecar (%s %s): %w", cfg.NodePath, script, errStart)
	}

	process := &sidecarProcess{
		cmd:     cmd,
		stdin:   stdin,
		stderr:  stderr,
		pending: make(map[string]*pendingCall),
	}
	go process.readLoop(stdout)
	return process, nil
}

// readLoop fans stdout events out to the waiting callers until the sidecar exits.
func (p *sidecarProcess) readLoop(stdout io.Reader) {
	reader := bufio.NewReader(stdout)
	for {
		line, errRead := reader.ReadBytes('\n')
		if len(line) > 0 {
			p.dispatch(line)
		}
		if errRead != nil {
			break
		}
	}
	errWait := p.cmd.Wait()
	reason := "cursor sidecar exited"
	if errWait != nil {
		reason = fmt.Sprintf("cursor sidecar exited: %v", errWait)
	}
	if tail := p.stderr.String(); tail != "" {
		reason = fmt.Sprintf("%s: %s", reason, tail)
	}
	p.failAll(reason)
}

func (p *sidecarProcess) dispatch(line []byte) {
	if len(strings.TrimSpace(string(line))) == 0 {
		return
	}
	var event sidecarEvent
	if errUnmarshal := json.Unmarshal(line, &event); errUnmarshal != nil {
		// A dropped event would strand its caller, so make protocol drift loud rather than
		// letting the request hang waiting for a terminal event that never arrives.
		hostLog("error", "cursor sidecar sent an undecodable event", map[string]any{
			"error": errUnmarshal.Error(),
		})
		return
	}
	if event.Event == "http" {
		p.handleSidecarHTTP(event)
		return
	}
	p.mu.Lock()
	call := p.pending[event.ID]
	if call != nil && event.terminal() {
		delete(p.pending, event.ID)
	}
	p.mu.Unlock()
	if call == nil {
		return
	}
	select {
	case call.events <- event:
	case <-call.done:
		return
	}
	if event.terminal() {
		close(call.events)
	}
}

func (p *sidecarProcess) failAll(reason string) {
	p.mu.Lock()
	p.closed = true
	pending := p.pending
	p.pending = make(map[string]*pendingCall)
	p.mu.Unlock()
	for id, call := range pending {
		select {
		case call.events <- sidecarEvent{ID: id, Event: "error", Message: reason}:
		case <-call.done:
			continue
		}
		close(call.events)
	}
}

// resolveSidecarScript locates index.mjs from explicit configuration, from a development
// checkout next to the plugin library, or from the embedded copy released into the home
// directory.
func resolveSidecarScript(cfg pluginConfig) (string, error) {
	if cfg.SidecarPath != "" {
		candidate := cfg.SidecarPath
		if !strings.HasSuffix(candidate, ".mjs") && !strings.HasSuffix(candidate, ".js") {
			candidate = filepath.Join(candidate, "index.mjs")
		}
		if fileExists(candidate) {
			return filepath.Abs(candidate)
		}
		return "", fmt.Errorf("cursor sidecar not found at configured sidecar-path: %s", candidate)
	}
	// Working-directory candidates only serve a development checkout; a deployed host may
	// run from anywhere, which is why the embedded bootstrap is the fallback below.
	candidates := []string{
		filepath.Join("plugins", runtime.GOOS, runtime.GOARCH, sidecarDirName, "index.mjs"),
		filepath.Join("plugins", sidecarDirName, "index.mjs"),
		filepath.Join(sidecarDirName, "index.mjs"),
	}
	for _, candidate := range candidates {
		if fileExists(candidate) {
			return filepath.Abs(candidate)
		}
	}
	script, errBootstrap := bootstrapSidecarScript(cfg)
	if errBootstrap != nil {
		return "", fmt.Errorf("cursor sidecar not found (searched %s relative to %s) and bootstrap failed: %w",
			strings.Join(candidates, ", "), workingDirectory(), errBootstrap)
	}
	return script, nil
}

func fileExists(path string) bool {
	info, errStat := os.Stat(path)
	return errStat == nil && !info.IsDir()
}

func workingDirectory() string {
	dir, errDir := os.Getwd()
	if errDir != nil {
		return "."
	}
	return dir
}
