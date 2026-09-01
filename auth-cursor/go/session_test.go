package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"

	sdkv1 "github.com/UNICKCHENG/cliproxyapi-plugins/auth-cursor/go/internal/sdk/v1"
)

func TestChatParamsKeepsTheFirstReasoningEffort(t *testing.T) {
	params := chatParams([]byte(`{"reasoning_effort":"high","reasoning":{"effort":"low"}}`))
	if len(params) != 1 || params[0].ID != "reasoning_effort" || params[0].Value != "high" {
		t.Fatalf("params = %+v, want a single reasoning_effort=high", params)
	}
}

func TestCheckPromptBudgetRejectsAnOversizedPrompt(t *testing.T) {
	previous := promptMaxRunes
	promptMaxRunes = 8
	t.Cleanup(func() { promptMaxRunes = previous })

	errBudget := checkPromptBudget("this is far too long")
	if errBudget == nil {
		t.Fatal("expected an oversized prompt to fail")
	}
	failure := failureFrom(errBudget)
	if failure.HTTPStatus != 400 {
		t.Errorf("status = %d, want 400", failure.HTTPStatus)
	}
	if got := gjson.Get(failure.Message, "error.code").String(); got != "context_length_exceeded" {
		t.Errorf("code = %q, want context_length_exceeded in %s", got, failure.Message)
	}
}

func TestExecuteReusesAnAgentForTheNextTurn(t *testing.T) {
	bridge := useFakeBridge(t)
	mustExecute(t, "good-key", map[string]any{
		"model":    "fake-model",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if created := len(bridge.createdAgents()); created != 1 {
		t.Fatalf("first turn created %d agents, want 1", created)
	}

	mustExecute(t, "good-key", map[string]any{
		"model": "fake-model",
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "Hello world"},
			{"role": "user", "content": "again"},
		},
	})
	if created := len(bridge.createdAgents()); created != 1 {
		t.Fatalf("second turn created %d agents, want the first reused", created)
	}
	prompts := bridge.recordedPrompts()
	if len(prompts) != 2 {
		t.Fatalf("sends = %d, want 2", len(prompts))
	}
	if prompts[1] != "User: again" {
		t.Errorf("second send = %q, want the increment only", prompts[1])
	}
}

func TestExecuteDoesNotReuseAcrossCredentialsOrModels(t *testing.T) {
	bridge := useFakeBridge(t)
	mustExecute(t, "good-key", map[string]any{
		"model":    "fake-model",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	mustExecute(t, "good-key", map[string]any{
		"model": "other-model",
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "Hello world"},
			{"role": "user", "content": "again"},
		},
	})
	if created := len(bridge.createdAgents()); created != 2 {
		t.Fatalf("created = %d, want a new agent for the other model", created)
	}

	mustExecute(t, "other-key", map[string]any{
		"model": "fake-model",
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "Hello world"},
			{"role": "user", "content": "again"},
		},
	})
	if created := len(bridge.createdAgents()); created != 3 {
		t.Fatalf("created = %d, want a new agent for the other credential", created)
	}
}

func TestExecuteDoesNotReuseAnEditedHistory(t *testing.T) {
	bridge := useFakeBridge(t)
	mustExecute(t, "good-key", map[string]any{
		"model":    "fake-model",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	mustExecute(t, "good-key", map[string]any{
		"model": "fake-model",
		"messages": []map[string]any{
			{"role": "user", "content": "edited"},
			{"role": "assistant", "content": "Hello world"},
			{"role": "user", "content": "again"},
		},
	})
	if created := len(bridge.createdAgents()); created != 2 {
		t.Fatalf("created = %d, want a new agent after the history was edited", created)
	}
}

func TestExecuteFallsBackWhenTheSessionIsInUse(t *testing.T) {
	bridge := useFakeBridge(t)
	mustExecute(t, "good-key", map[string]any{
		"model":    "fake-model",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})

	payload := map[string]any{
		"model": "fake-model",
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "Hello world"},
			{"role": "user", "content": "again"},
		},
	}
	generateReq, _, errBuild := buildGenerateRequest(mustExecutorRequest(t, "good-key", payload))
	if errBuild != nil {
		t.Fatalf("buildGenerateRequest: %v", errBuild)
	}
	entry, _, _, ok := occupySession(generateReq)
	if !ok {
		t.Fatal("expected the first session to be occupiable")
	}
	t.Cleanup(func() { releaseSession(entry, true) })

	mustExecute(t, "good-key", payload)
	if created := len(bridge.createdAgents()); created != 2 {
		t.Fatalf("created = %d, want a one-shot agent while the session is in use", created)
	}
}

func TestSessionEvictionClosesTheOldestAgent(t *testing.T) {
	bridge := useFakeBridge(t)
	previous := sessionLimit
	sessionLimit = 1
	t.Cleanup(func() { sessionLimit = previous })

	mustExecute(t, "good-key", map[string]any{
		"model":    "fake-model",
		"messages": []map[string]any{{"role": "user", "content": "first"}},
	})
	mustExecute(t, "good-key", map[string]any{
		"model":    "fake-model",
		"messages": []map[string]any{{"role": "user", "content": "second"}},
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(bridge.closedAgents()) >= 1 && sessionCount() <= 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("closed = %v sessions = %d, want the overflow agent closed", bridge.closedAgents(), sessionCount())
}

func TestQuiesceCancelsRegisteredWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	id := registerInFlight(cancel)
	t.Cleanup(func() { unregisterInFlight(id) })
	quiesce()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("quiesce did not cancel the registered handle")
	}
}

func TestQuiesceCancelsAnInFlightExecute(t *testing.T) {
	bridge := newFakeBridge()
	bridge.hold = make(chan struct{})
	useFakeBridgeService(t, bridge)

	done := make(chan []byte, 1)
	go func() {
		raw, _ := execute(executorRequest(t, "good-key", map[string]any{
			"model":    "fake-model",
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		}))
		done <- raw
	}()
	select {
	case <-bridge.announced:
	case <-time.After(2 * time.Second):
		t.Fatal("the held run never started")
	}

	raw, errQuiesce := handleMethod(pluginabi.MethodPluginQuiesce, nil)
	if errQuiesce != nil {
		t.Fatalf("quiesce: %v", errQuiesce)
	}
	if env := decodeEnvelope(t, raw); !env.OK {
		t.Fatalf("quiesce envelope: %+v", env)
	}

	select {
	case result := <-done:
		if env := decodeEnvelope(t, result); env.OK {
			t.Fatal("expected the cancelled execute to fail")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("quiesce did not cancel the in-flight execute")
	}
}

func TestFailedRunEvictsTheSession(t *testing.T) {
	bridge := useFakeBridge(t)
	mustExecute(t, "good-key", map[string]any{
		"model":    "fake-model",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	payload := map[string]any{
		"model": "fake-model",
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "Hello world"},
			{"role": "user", "content": "again"},
		},
	}
	generateReq, _, errBuild := buildGenerateRequest(mustExecutorRequest(t, "good-key", payload))
	if errBuild != nil {
		t.Fatalf("buildGenerateRequest: %v", errBuild)
	}
	entry, increment, _, ok := occupySession(generateReq)
	if !ok {
		t.Fatal("expected the first session to be occupiable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	incrementReq := generateReq
	incrementReq.prompt = increment
	_, errSend := sendAndConsume(ctx, entry.process, entry.agentID, incrementReq, nil, nil)
	releaseSession(entry, errSend == nil)
	if errSend == nil {
		t.Fatal("expected a cancelled send to fail")
	}

	mustExecute(t, "good-key", payload)
	if created := len(bridge.createdAgents()); created != 2 {
		t.Fatalf("created = %d, want a new agent after the failed turn was evicted", created)
	}
}

func TestRunFailureFromResultKeepsCredentialStatuses(t *testing.T) {
	unauthorized := "UNAUTHORIZED"
	failure := runFailureFromResult(&sdkv1.RunStreamResult{
		Status:    sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_ERROR,
		ErrorCode: &unauthorized,
	}, "denied")
	if failure.HTTPStatus != http.StatusUnauthorized {
		t.Errorf("unauthorized status = %d, want 401", failure.HTTPStatus)
	}

	rate := "RATE_LIMIT_EXCEEDED"
	failure = runFailureFromResult(&sdkv1.RunStreamResult{
		Status:    sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_ERROR,
		ErrorCode: &rate,
	}, "slow down")
	if failure.HTTPStatus != http.StatusTooManyRequests {
		t.Errorf("rate limit status = %d, want 429", failure.HTTPStatus)
	}

	busy := "AGENT_BUSY"
	failure = runFailureFromResult(&sdkv1.RunStreamResult{
		Status:    sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_ERROR,
		ErrorCode: &busy,
	}, "busy")
	if failure.HTTPStatus != 0 {
		t.Errorf("agent busy status = %d, want unclassified", failure.HTTPStatus)
	}
}

func TestListenEgressFallsBackWhenTheFirstAddressFails(t *testing.T) {
	previous := egressListenAddrs
	egressListenAddrs = []string{"127.0.0.1:1", "127.0.0.2:0"}
	t.Cleanup(func() { egressListenAddrs = previous })

	listener, errListen := listenEgress()
	if errListen != nil {
		t.Skipf("127.0.0.2 listen unavailable: %v", errListen)
	}
	defer listener.Close()
	if strings.Contains(listener.Addr().String(), "127.0.0.1") {
		t.Fatalf("egress bound %s after refusing 127.0.0.1 as a backend URL", listener.Addr())
	}
}

func TestListenEgressFailsWhenNoAddressWorks(t *testing.T) {
	previous := egressListenAddrs
	egressListenAddrs = []string{"255.255.255.255:1"}
	t.Cleanup(func() { egressListenAddrs = previous })
	if _, errListen := listenEgress(); errListen == nil {
		t.Fatal("expected listenEgress to fail when every address is unusable")
	}
}

func TestBridgeAuthTokenRejectsAFileOutsideTheAllowedRoots(t *testing.T) {
	useTempCacheDir(t)
	escaped := filepath.Join(t.TempDir(), "stolen")
	if errWrite := os.WriteFile(escaped, []byte("token\n"), 0o600); errWrite != nil {
		t.Fatalf("write: %v", errWrite)
	}
	_, errToken := bridgeAuthToken(bridgeDiscovery{AuthTokenFile: escaped})
	if errToken == nil || !strings.Contains(errToken.Error(), "outside") {
		t.Fatalf("error = %v, want the path to be refused", errToken)
	}
}

// A current bridge writes its token to its own directory under the system temp directory, not
// under --state-root, so that shape has to be accepted or every handshake fails.
func TestBridgeAuthTokenAcceptsTheBridgeTempDirectory(t *testing.T) {
	useTempCacheDir(t)
	dir, errDir := os.MkdirTemp("", bridgeTokenDirPrefix+"*")
	if errDir != nil {
		t.Fatalf("mkdtemp: %v", errDir)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "auth-token")
	if errWrite := os.WriteFile(path, []byte("token-from-temp\n"), 0o600); errWrite != nil {
		t.Fatalf("write: %v", errWrite)
	}
	token, errToken := bridgeAuthToken(bridgeDiscovery{AuthTokenFile: path})
	if errToken != nil || token != "token-from-temp" {
		t.Fatalf("token = %q err = %v, want token-from-temp", token, errToken)
	}
}

func TestBridgeAuthTokenRejectsAWorldWritableTempToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits do not describe sharing on windows")
	}
	useTempCacheDir(t)
	dir, errDir := os.MkdirTemp("", bridgeTokenDirPrefix+"*")
	if errDir != nil {
		t.Fatalf("mkdtemp: %v", errDir)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "auth-token")
	if errWrite := os.WriteFile(path, []byte("token\n"), 0o600); errWrite != nil {
		t.Fatalf("write: %v", errWrite)
	}
	// Written through umask first, so the sharing bits have to be set explicitly.
	if errChmod := os.Chmod(path, 0o666); errChmod != nil {
		t.Fatalf("chmod: %v", errChmod)
	}
	if _, errToken := bridgeAuthToken(bridgeDiscovery{AuthTokenFile: path}); errToken == nil {
		t.Fatal("expected a world-writable token file to be refused")
	}
}

// A temp path that is not the bridge's own directory must stay refused: the temp allowance is
// for one known layout, not for any file the bridge cares to name.
func TestBridgeAuthTokenRejectsAnUnrelatedTempFile(t *testing.T) {
	useTempCacheDir(t)
	path := filepath.Join(os.TempDir(), "unrelated-auth-token")
	if errWrite := os.WriteFile(path, []byte("token\n"), 0o600); errWrite != nil {
		t.Fatalf("write: %v", errWrite)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	if _, errToken := bridgeAuthToken(bridgeDiscovery{AuthTokenFile: path}); errToken == nil {
		t.Fatal("expected a bare temp-directory file to be refused")
	}
}

func TestListenEgressDoesNotBind127001(t *testing.T) {
	listener, errListen := listenEgress()
	if errListen != nil {
		t.Skipf("loopback listen unavailable: %v", errListen)
	}
	defer listener.Close()
	if strings.Contains(listener.Addr().String(), "127.0.0.1") {
		t.Fatalf("egress bound %s, which would disable TLS verification in the bridge", listener.Addr())
	}
}

func TestReadStderrAcceptsALaterValidReadyLine(t *testing.T) {
	process := &bridgeProcess{stderr: &tailBuffer{}}
	discovered := make(chan bridgeDiscovery, 1)
	failed := make(chan error, 1)
	drained := make(chan struct{})
	pipe := strings.NewReader(bridgeReadyPrefix + `{"schemaVersion":1,"transport":"stdio"}` + "\n" +
		bridgeReadyPrefix + `{"schemaVersion":1,"transport":"tcp","protocol":"connect","url":"http://127.0.0.1:9"}` + "\n")
	go process.readStderr(pipe, discovered, failed, drained)
	select {
	case discovery := <-discovered:
		if discovery.URL != "http://127.0.0.1:9" {
			t.Errorf("discovery URL = %q", discovery.URL)
		}
	case errFailed := <-failed:
		t.Fatalf("failed: %v", errFailed)
	case <-time.After(2 * time.Second):
		t.Fatal("the second ready line was ignored")
	}
	<-drained
}

func TestInboundRequestLimitRejectsOversizedPayloads(t *testing.T) {
	if !inboundRequestTooLarge(pluginRequestMaxBytes + 1) {
		t.Fatal("expected a payload just over the cap to be rejected")
	}
	if inboundRequestTooLarge(pluginRequestMaxBytes) {
		t.Fatal("a payload at the cap must still be accepted")
	}
}

func mustExecutorRequest(t *testing.T, apiKey string, payload map[string]any) pluginapi.ExecutorRequest {
	t.Helper()
	raw := executorRequest(t, apiKey, payload)
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		t.Fatalf("decode executor request: %v", errUnmarshal)
	}
	return req.ExecutorRequest
}

func TestConcurrentAcquireBridgeDoesNotHoldThePoolLock(t *testing.T) {
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

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errAcquire := acquireBridge("")
			errs <- errAcquire
		}()
	}
	wg.Wait()
	close(errs)
	for errAcquire := range errs {
		if errAcquire != nil {
			t.Fatalf("acquireBridge: %v", errAcquire)
		}
	}
	if got := len(bridgePool.procs); got != 1 {
		t.Fatalf("pooled bridges = %d, want 1", got)
	}
}

func TestExecuteRejectsAnOversizedTranscript(t *testing.T) {
	useFakeBridge(t)
	previous := promptMaxRunes
	promptMaxRunes = 8
	t.Cleanup(func() { promptMaxRunes = previous })

	raw, errExecute := execute(executorRequest(t, "good-key", map[string]any{
		"model":    "fake-model",
		"messages": []map[string]any{{"role": "user", "content": "this is far too long"}},
	}))
	if errExecute != nil {
		t.Fatalf("execute: %v", errExecute)
	}
	env := decodeEnvelope(t, raw)
	if env.OK || env.Error == nil {
		t.Fatalf("envelope = %+v, want a request fault", env)
	}
	if env.Error.HTTPStatus != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", env.Error.HTTPStatus)
	}
	if got := gjson.Get(env.Error.Message, "error.code").String(); got != "context_length_exceeded" {
		t.Errorf("code = %q in %s", got, env.Error.Message)
	}
}

func TestVerifyInstalledBridgeRejectsAMismatchedStamp(t *testing.T) {
	root := t.TempDir()
	if errMkdir := os.MkdirAll(filepath.Join(root, "bin"), 0o700); errMkdir != nil {
		t.Fatalf("mkdir: %v", errMkdir)
	}
	if errWrite := os.WriteFile(filepath.Join(root, "bin", bridgeExecutableName()), []byte("fake-bridge"), 0o700); errWrite != nil {
		t.Fatalf("write binary: %v", errWrite)
	}
	if errWrite := os.WriteFile(filepath.Join(root, "archive.sha256"), []byte("deadbeef\n"), 0o600); errWrite != nil {
		t.Fatalf("write archive stamp: %v", errWrite)
	}
	if errWrite := os.WriteFile(filepath.Join(root, "bin.sha256"), []byte("deadbeef\n"), 0o600); errWrite != nil {
		t.Fatalf("write binary stamp: %v", errWrite)
	}
	if errVerify := verifyInstalledBridge(root); errVerify == nil {
		t.Fatal("expected a mismatched install stamp to fail")
	}
}
