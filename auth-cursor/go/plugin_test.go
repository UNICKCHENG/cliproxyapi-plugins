package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/types/known/durationpb"

	sdkv1 "github.com/UNICKCHENG/cliproxyapi-plugins/auth-cursor/go/internal/sdk/v1"
)

func executorRequest(t *testing.T, apiKey string, payload map[string]any) []byte {
	t.Helper()
	rawPayload, errPayload := json.Marshal(payload)
	if errPayload != nil {
		t.Fatalf("marshal payload: %v", errPayload)
	}
	raw, errMarshal := json.Marshal(rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:       "fake-model",
			Payload:     rawPayload,
			StorageJSON: storageJSON(apiKey),
		},
	})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	return raw
}

func decodeEnvelope(t *testing.T, raw []byte) envelope {
	t.Helper()
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	return env
}

func TestExecuteAssemblesCompletion(t *testing.T) {
	bridge := useFakeBridge(t)

	raw, errExecute := execute(executorRequest(t, "good-key", map[string]any{
		"model":    "fake-model",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}))
	if errExecute != nil {
		t.Fatalf("execute: %v", errExecute)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("execute failed: %+v", env.Error)
	}
	var response pluginapi.ExecutorResponse
	if errUnmarshal := json.Unmarshal(env.Result, &response); errUnmarshal != nil {
		t.Fatalf("decode executor response: %v", errUnmarshal)
	}
	var completion chatCompletion
	if errUnmarshal := json.Unmarshal(response.Payload, &completion); errUnmarshal != nil {
		t.Fatalf("decode completion: %v", errUnmarshal)
	}
	if completion.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", completion.Object)
	}
	if len(completion.Choices) != 1 || completion.Choices[0].Message == nil {
		t.Fatalf("unexpected choices: %+v", completion.Choices)
	}
	if got := completion.Choices[0].Message.Content; got != "Hello world" {
		t.Errorf("content = %q, want %q", got, "Hello world")
	}
	// Cache reads are prompt tokens served from cache, so they belong on the prompt side.
	if completion.Usage == nil || completion.Usage.PromptTokens != 10 || completion.Usage.CompletionTokens != 2 {
		t.Errorf("usage = %+v, want prompt=10 completion=2", completion.Usage)
	}
	// A non-streaming request has nothing to do with deltas, so it must not ask for them.
	if len(bridge.enableDeltas) != 1 || bridge.enableDeltas[0] {
		t.Errorf("enable_deltas = %v, want [false] for a non-streaming request", bridge.enableDeltas)
	}
}

// Every request creates a throwaway agent, so both teardown calls have to happen: CloseAgent
// releases the local resources and DeleteAgent removes the durable row the bridge wrote for it.
func TestExecuteReleasesAndDeletesTheAgent(t *testing.T) {
	bridge := useFakeBridge(t)

	if _, errExecute := execute(executorRequest(t, "good-key", map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})); errExecute != nil {
		t.Fatalf("execute: %v", errExecute)
	}
	if got := bridge.closedAgents(); len(got) != 1 || got[0] != "agent-1" {
		t.Errorf("closed agents = %v, want [agent-1]", got)
	}
	if got := bridge.deletedAgents(); len(got) != 1 || got[0] != "agent-1" {
		t.Errorf("deleted agents = %v, want [agent-1] so bridge state does not accumulate", got)
	}
}

// The agent is a text generator, so it must be created with an explicit empty tool list. An unset
// list would give it the default toolset, which can reach the host filesystem.
func TestCreateAgentRequestsNoToolsAndAThrowawayWorkspace(t *testing.T) {
	bridge := useFakeBridge(t)

	if _, errExecute := execute(executorRequest(t, "good-key", map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})); errExecute != nil {
		t.Fatalf("execute: %v", errExecute)
	}
	created := bridge.createdAgents()
	if len(created) != 1 {
		t.Fatalf("created agents = %d, want 1", len(created))
	}
	options := created[0]
	if options.GetTools() == nil {
		t.Error("tools is unset, want an empty ToolList so no built-in tools are offered")
	}
	if names := options.GetTools().GetNames(); len(names) != 0 {
		t.Errorf("tool names = %v, want none", names)
	}
	// The key travels on the request rather than in the bridge environment, because one bridge
	// process serves every credential that shares its proxy.
	if options.GetApiKey() != "good-key" {
		t.Errorf("api key = %q, want it set explicitly on the request", options.GetApiKey())
	}
	if cwd := options.GetLocal().GetCwd(); len(cwd) != 1 || cwd[0] == "" {
		t.Errorf("local cwd = %v, want a single scratch directory", cwd)
	}
}

func TestExecuteSurfacesUpstreamStatus(t *testing.T) {
	useFakeBridge(t)

	raw, errExecute := execute(executorRequest(t, "bad-key", map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}))
	if errExecute != nil {
		t.Fatalf("execute: %v", errExecute)
	}
	env := decodeEnvelope(t, raw)
	if env.OK {
		t.Fatal("expected an error envelope for a rejected key")
	}
	if !strings.Contains(env.Error.Message, "401") || !strings.Contains(env.Error.Message, "Invalid User API Key") {
		t.Errorf("message = %q, want upstream status and reason", env.Error.Message)
	}
	// The status has to travel as a field: the host classifies the failure from it, and
	// without one a rejected key looks like an unclassified fault.
	if env.Error.HTTPStatus != 401 {
		t.Errorf("http status = %d, want 401", env.Error.HTTPStatus)
	}
}

// A model the account's region cannot reach must fail as a request fault. Reported as an
// unclassified failure it would park the credential and take down the models it can reach.
func TestExecuteReportsModelUnavailableAsRequestFault(t *testing.T) {
	useFakeBridge(t)

	raw, errExecute := execute(executorRequest(t, "region-key", map[string]any{
		"model":    "fake-model",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}))
	if errExecute != nil {
		t.Fatalf("execute: %v", errExecute)
	}
	env := decodeEnvelope(t, raw)
	if env.OK {
		t.Fatal("expected an error envelope for an unavailable model")
	}
	if env.Error.HTTPStatus != 400 {
		t.Errorf("http status = %d, want 400 so the host reads it as a request fault", env.Error.HTTPStatus)
	}
	// The host also recognises a request fault from error.type when it has no status, which
	// is the only channel the stream bridge offers.
	if got := gjson.Get(env.Error.Message, "error.type").String(); got != "invalid_request_error" {
		t.Errorf("error.type = %q, want invalid_request_error in %s", got, env.Error.Message)
	}
	if got := gjson.Get(env.Error.Message, "error.message").String(); !strings.Contains(got, "not supported in your region") {
		t.Errorf("error.message = %q, want Cursor's reason preserved", got)
	}
}

func TestFailureFromDetailsMapsErrorCodes(t *testing.T) {
	for _, test := range []struct {
		name          string
		details       *sdkv1.SdkErrorDetails
		wantStatus    int
		wantType      string
		wantRetryable bool
	}{
		{
			name:       "rejected key",
			details:    &sdkv1.SdkErrorDetails{SdkErrorCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_UNAUTHORIZED, Message: "Invalid User API Key"},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "no key reached the bridge",
			details:    &sdkv1.SdkErrorDetails{SdkErrorCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_API_KEY_NOT_FOUND, Message: "missing key"},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "rate limit carries its suggested wait",
			details: &sdkv1.SdkErrorDetails{
				SdkErrorCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_RATE_LIMIT_EXCEEDED,
				Message:      "Too many requests",
				RetryAfter:   durationpb.New(30 * time.Second),
			},
			wantStatus:    http.StatusTooManyRequests,
			wantRetryable: true,
		},
		{
			name:       "usage limit is throttling too",
			details:    &sdkv1.SdkErrorDetails{SdkErrorCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_USAGE_LIMIT_EXCEEDED, Message: "monthly limit reached"},
			wantStatus: http.StatusTooManyRequests,
		},
		{
			name:       "unknown model is the request's fault",
			details:    &sdkv1.SdkErrorDetails{SdkErrorCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_INVALID_MODEL, Message: "no such model"},
			wantStatus: http.StatusBadRequest,
			wantType:   "invalid_request_error",
		},
		{
			name:       "plan restriction is the request's fault",
			details:    &sdkv1.SdkErrorDetails{SdkErrorCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_PLAN_REQUIRED, Message: "upgrade required"},
			wantStatus: http.StatusBadRequest,
			wantType:   "invalid_request_error",
		},
		{
			name:       "validation failure is the request's fault",
			details:    &sdkv1.SdkErrorDetails{SdkErrorCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_VALIDATION_ERROR, Message: "bad options"},
			wantStatus: http.StatusBadRequest,
			wantType:   "invalid_request_error",
		},
		{
			name:       "forbidden role",
			details:    &sdkv1.SdkErrorDetails{SdkErrorCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_ROLE_FORBIDDEN, Message: "not permitted"},
			wantStatus: http.StatusForbidden,
		},
		{
			// A transient bridge or provider fault says nothing about the credential or the
			// request, so inventing a status for it would misdirect the host.
			name:       "internal error stays unclassified",
			details:    &sdkv1.SdkErrorDetails{SdkErrorCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_INTERNAL_ERROR, Message: "boom"},
			wantStatus: 0,
		},
		{
			name:       "unknown code stays unclassified",
			details:    &sdkv1.SdkErrorDetails{SdkErrorCode: sdkv1.SdkErrorCode(9999), Message: "from the future"},
			wantStatus: 0,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := failureFromDetails(test.details)
			if failure.HTTPStatus != test.wantStatus {
				t.Errorf("status = %d, want %d", failure.HTTPStatus, test.wantStatus)
			}
			if got := gjson.Get(failure.Message, "error.type").String(); got != test.wantType {
				t.Errorf("error.type = %q, want %q in %s", got, test.wantType, failure.Message)
			}
			if failure.Retryable != test.wantRetryable {
				t.Errorf("retryable = %v, want %v", failure.Retryable, test.wantRetryable)
			}
		})
	}
}

// The bridge does not always fill sdk_error_code in: a key Cursor rejects arrives as
// UNAUTHENTICATED carrying SDK_ERROR_CODE_UNSPECIFIED, which is what a real bridge sends. An
// unspecified code is not a classification, so the Connect code has to be consulted instead —
// otherwise a dead credential is cooled for a while rather than retired.
func TestFailureFromFallsBackToTheTransportCode(t *testing.T) {
	for _, test := range []struct {
		name          string
		err           error
		wantStatus    int
		wantMessage   string
		wantRetryable bool
	}{
		{
			name:        "rejected key carries no code",
			err:         sdkErrorResponse(connect.CodeUnauthenticated, &sdkv1.SdkErrorDetails{Message: "Invalid User API Key"}),
			wantStatus:  http.StatusUnauthorized,
			wantMessage: "cursor upstream error 401: Invalid User API Key",
		},
		{
			// Nothing structured at all, which is what a bridge-side failure looks like.
			name:        "no detail at all",
			err:         connect.NewError(connect.CodeUnauthenticated, errors.New("Invalid User API Key")),
			wantStatus:  http.StatusUnauthorized,
			wantMessage: "cursor upstream error 401: Invalid User API Key",
		},
		{
			name:        "forbidden",
			err:         sdkErrorResponse(connect.CodePermissionDenied, &sdkv1.SdkErrorDetails{Message: "not permitted"}),
			wantStatus:  http.StatusForbidden,
			wantMessage: "cursor upstream error 403: not permitted",
		},
		{
			name:          "throttled",
			err:           sdkErrorResponse(connect.CodeResourceExhausted, &sdkv1.SdkErrorDetails{Message: "Too many requests"}),
			wantStatus:    http.StatusTooManyRequests,
			wantMessage:   "cursor upstream error 429: Too many requests",
			wantRetryable: true,
		},
		{
			// A code that says nothing about whose fault it is must stay unclassified, and
			// keep the whole error so there is something to diagnose from.
			name:        "unclassifiable code",
			err:         connect.NewError(connect.CodeInvalidArgument, errors.New("bad options")),
			wantMessage: "invalid_argument: bad options",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := failureFrom(test.err)
			if failure.HTTPStatus != test.wantStatus {
				t.Errorf("status = %d, want %d", failure.HTTPStatus, test.wantStatus)
			}
			if failure.Message != test.wantMessage {
				t.Errorf("message = %q, want %q", failure.Message, test.wantMessage)
			}
			if failure.Retryable != test.wantRetryable {
				t.Errorf("retryable = %v, want %v", failure.Retryable, test.wantRetryable)
			}
		})
	}
}

// sdk_error_code is the finer signal and has to keep winning: the Connect code cannot tell a
// fault of the request from one of the credential, and here it would blame the wrong one.
func TestFailureFromPrefersTheSdkErrorCode(t *testing.T) {
	failure := failureFrom(sdkErrorResponse(connect.CodeUnauthenticated, &sdkv1.SdkErrorDetails{
		SdkErrorCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_INVALID_MODEL,
		Message:      "claude-opus-5 is not available on your plan",
	}))
	if failure.HTTPStatus != http.StatusBadRequest {
		t.Errorf("status = %d, want the request blamed rather than the credential", failure.HTTPStatus)
	}
	if got := gjson.Get(failure.Message, "error.code").String(); got != "model_not_available" {
		t.Errorf("error.code = %q, want model_not_available in %s", got, failure.Message)
	}
}

// A deadline on a bridge that already answered its handshake means the bridge is stuck reaching
// Cursor. Reported verbatim it reads as if the plugin hung, which sends operators looking in the
// wrong place.
func TestFailureFromExplainsAnUnreachableCursor(t *testing.T) {
	for _, code := range []connect.Code{connect.CodeDeadlineExceeded, connect.CodeUnavailable} {
		failure := failureFrom(connect.NewError(code, errors.New("context deadline exceeded")))
		if !strings.Contains(failure.Message, cursorBackendHost) {
			t.Errorf("%s message = %q, want it to name %s", code, failure.Message, cursorBackendHost)
		}
		if !strings.Contains(failure.Message, "proxy-url") {
			t.Errorf("%s message = %q, want it to point at the proxy setting", code, failure.Message)
		}
		if !failure.Retryable {
			t.Errorf("%s is not retryable, want a network fault treated as transient", code)
		}
		// A network outage affects every credential equally, so it must not be reported with
		// a status that would discredit this one.
		if failure.HTTPStatus != 0 {
			t.Errorf("%s status = %d, want it unclassified", code, failure.HTTPStatus)
		}
	}
}

// A run that fails inside the agent reports free-form text rather than an error code, so the
// classification has to come from the wording.
func TestRunFailureClassification(t *testing.T) {
	for _, test := range []struct {
		name       string
		message    string
		wantStatus int
		wantType   string
	}{
		{
			name:       "region refusal",
			message:    "Model not available This model provider is not supported in your region.",
			wantStatus: http.StatusBadRequest,
			wantType:   "invalid_request_error",
		},
		{
			name:       "plan restriction",
			message:    "claude-opus-5 is not available on your plan",
			wantStatus: http.StatusBadRequest,
			wantType:   "invalid_request_error",
		},
		{
			name:       "unrecognised failure stays unclassified",
			message:    "cursor run failed",
			wantStatus: 0,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := runFailure(test.message)
			if failure.HTTPStatus != test.wantStatus {
				t.Errorf("status = %d, want %d", failure.HTTPStatus, test.wantStatus)
			}
			if got := gjson.Get(failure.Message, "error.type").String(); got != test.wantType {
				t.Errorf("error.type = %q, want %q in %s", got, test.wantType, failure.Message)
			}
		})
	}
}

func TestSdkErrorDetailsAreRecoveredFromAConnectError(t *testing.T) {
	bridge := useFakeBridge(t)
	process := poolBridge(t)

	_, errList := process.cursor.ListModels(context.Background(), connect.NewRequest(&sdkv1.ListModelsRequest{
		Options: &sdkv1.CursorRequestOptions{ApiKey: "throttled-key"},
	}))
	if errList == nil {
		t.Fatal("expected the throttled key to be rejected")
	}
	failure := failureFrom(errList)
	if failure.HTTPStatus != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", failure.HTTPStatus)
	}
	if !failure.Retryable {
		t.Error("retryable = false, want true because the bridge suggested a retry_after")
	}
	if !strings.Contains(failure.Message, "retry after 30s") {
		t.Errorf("message = %q, want the suggested wait", failure.Message)
	}
	_ = bridge
}

// The bridge rejects any RPC without its per-process token, so a client that loses the header must
// fail loudly rather than appear to work against an unauthenticated endpoint.
func TestBridgeRejectsRPCsWithoutTheBearerToken(t *testing.T) {
	bridge := newFakeBridge()
	baseURL := serveFakeBridge(t, bridge)

	_, errPing := unauthenticatedClient(baseURL).Ping(context.Background(), connect.NewRequest(&sdkv1.PingRequest{}))
	if errPing == nil {
		t.Fatal("expected an unauthenticated ping to be rejected")
	}
	if got := connect.CodeOf(errPing); got != connect.CodeUnauthenticated {
		t.Errorf("code = %v, want unauthenticated", got)
	}
}

// The streaming path is the one that has to skip keepalives and unknown update kinds: a keepalive
// treated as end-of-stream would truncate the answer, and an unknown update kind forwarded as text
// would corrupt it.
func TestRunGenerateStreamsTextDeltasOnly(t *testing.T) {
	bridge := useFakeBridge(t)

	var deltas []string
	result, errRun := runGenerate(context.Background(), generateRequest{
		apiKey:      "good-key",
		model:       "fake-model",
		optimizeFor: defaultOptimizeFor,
		prompt:      "hi",
	}, func(text string) error {
		deltas = append(deltas, text)
		return nil
	})
	if errRun != nil {
		t.Fatalf("runGenerate: %v", errRun)
	}
	if strings.Join(deltas, "") != "Hello world" {
		t.Errorf("deltas = %q, want incremental \"Hello world\"", deltas)
	}
	if result.text != "Hello world" {
		t.Errorf("text = %q, want the terminal result", result.text)
	}
	if len(bridge.enableDeltas) != 1 || !bridge.enableDeltas[0] {
		t.Errorf("enable_deltas = %v, want [true] for a streaming request", bridge.enableDeltas)
	}
}

// Dropping the Send stream does not stop a run, so a cancelled request has to issue an explicit
// CancelRun for the run id the stream reported.
func TestRunGenerateCancelsTheRunWhenTheCallerGoesAway(t *testing.T) {
	bridge := newFakeBridge()
	bridge.hold = make(chan struct{})
	useFakeBridgeService(t, bridge)

	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	// CancelRun takes the run id the stream reported, so the cancellation is only meaningful
	// once the client has read it. The first delta is the proof that it has.
	streaming := make(chan struct{})
	go func() {
		streamed := false
		_, errRun := runGenerate(ctx, generateRequest{
			apiKey:      "good-key",
			model:       "fake-model",
			optimizeFor: defaultOptimizeFor,
			prompt:      "hi",
		}, func(string) error {
			if !streamed {
				streamed = true
				close(streaming)
			}
			return nil
		})
		finished <- errRun
	}()

	select {
	case <-streaming:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never started streaming")
	}
	cancel()

	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("runGenerate did not return after the caller went away")
	}
	// The cancellation is issued before the request returns, so that it reaches the bridge
	// ahead of the agent teardown that follows it.
	if cancelled := bridge.cancelledRuns(); len(cancelled) != 1 || cancelled[0] != "run-1" {
		t.Fatalf("cancelled runs = %v, want [run-1] from the first stream message", cancelled)
	}
	if closed := bridge.closedAgents(); len(closed) != 1 {
		t.Errorf("closed agents = %v, want the abandoned agent torn down", closed)
	}
}

func TestResolveModelSelectionFiltersAndBackfillsParameters(t *testing.T) {
	catalog := []*sdkv1.SdkModel{
		{
			Id: "fake-model",
			Parameters: []*sdkv1.ModelParameterDefinition{{
				Id:     "reasoning_effort",
				Values: []*sdkv1.ModelParameterDefinitionValue{{Value: "low"}, {Value: "high"}},
			}},
		},
		{
			Id: "auto-smart",
			Parameters: []*sdkv1.ModelParameterDefinition{{
				Id:     optimizeForParameter,
				Values: []*sdkv1.ModelParameterDefinitionValue{{Value: "cost"}, {Value: "balanced"}},
			}},
		},
	}

	for _, test := range []struct {
		name        string
		model       string
		requested   []modelParam
		optimizeFor string
		wantID      string
		wantParams  map[string]string
	}{
		{
			name:       "a supported value is forwarded",
			model:      "fake-model",
			requested:  []modelParam{{ID: "reasoning_effort", Value: "high"}},
			wantID:     "fake-model",
			wantParams: map[string]string{"reasoning_effort": "high"},
		},
		{
			// Cursor rejects an unknown parameter value outright, so it is dropped rather than
			// forwarded and turned into a failed request.
			name:       "an unsupported value is dropped",
			model:      "fake-model",
			requested:  []modelParam{{ID: "reasoning_effort", Value: "ludicrous"}},
			wantID:     "fake-model",
			wantParams: map[string]string{},
		},
		{
			name:       "a parameter the model does not expose is dropped",
			model:      "fake-model",
			requested:  []modelParam{{ID: "temperature", Value: "0.2"}},
			wantID:     "fake-model",
			wantParams: map[string]string{},
		},
		{
			// The router rejects a request that omits optimize_for, so the configured mode fills
			// the gap.
			name:        "the router gets the configured optimize_for",
			model:       "auto-smart",
			optimizeFor: "cost",
			wantID:      "auto-smart",
			wantParams:  map[string]string{optimizeForParameter: "cost"},
		},
		{
			name:        "an unsupported optimize_for falls back to the first offered value",
			model:       "auto-smart",
			optimizeFor: "intelligence",
			wantID:      "auto-smart",
			wantParams:  map[string]string{optimizeForParameter: "cost"},
		},
		{
			// A catalog miss must not block generation: the request goes upstream as it came.
			name:       "an unknown model is forwarded verbatim",
			model:      "brand-new-model",
			requested:  []modelParam{{ID: "reasoning_effort", Value: "high"}},
			wantID:     "brand-new-model",
			wantParams: map[string]string{"reasoning_effort": "high"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			selection := resolveModelSelection(catalog, test.model, test.requested, test.optimizeFor)
			if selection.GetId() != test.wantID {
				t.Errorf("id = %q, want %q", selection.GetId(), test.wantID)
			}
			got := map[string]string{}
			for _, param := range selection.GetParams() {
				got[param.GetId()] = param.GetValue()
			}
			if len(got) != len(test.wantParams) {
				t.Fatalf("params = %v, want %v", got, test.wantParams)
			}
			for id, want := range test.wantParams {
				if got[id] != want {
					t.Errorf("params[%s] = %q, want %q", id, got[id], want)
				}
			}
		})
	}
}

// An empty catalog is what a failed ListModels leaves behind, and it must not stop a run.
func TestResolveModelSelectionWithoutACatalog(t *testing.T) {
	selection := resolveModelSelection(nil, "fake-model", []modelParam{{ID: "reasoning_effort", Value: "high"}}, "balanced")
	if selection.GetId() != "fake-model" {
		t.Errorf("id = %q, want the requested model", selection.GetId())
	}
	if len(selection.GetParams()) != 1 {
		t.Errorf("params = %v, want the request's own parameters", selection.GetParams())
	}
}

func TestModelsForAuthReportsTheAccountCatalog(t *testing.T) {
	useFakeBridge(t)

	raw, errModels := modelsForAuth(mustJSON(t, pluginapi.AuthModelRequest{
		AuthID:      "cursor-dev.json",
		StorageJSON: storageJSON("good-key"),
	}))
	if errModels != nil {
		t.Fatalf("modelsForAuth: %v", errModels)
	}
	var response pluginapi.ModelResponse
	if errUnmarshal := json.Unmarshal(decodeEnvelope(t, raw).Result, &response); errUnmarshal != nil {
		t.Fatalf("decode models: %v", errUnmarshal)
	}
	if response.Provider != providerIdentifier {
		t.Errorf("provider = %q, want %q", response.Provider, providerIdentifier)
	}
	if len(response.Models) != 3 {
		t.Fatalf("models = %+v, want the three catalog entries", response.Models)
	}
	fake := modelByID(t, response.Models, "fake-model")
	if fake.DisplayName != "Fake" {
		t.Errorf("fake-model display name = %q, want Fake", fake.DisplayName)
	}
	// Parameter ids are what the host advertises as supported parameters.
	if len(fake.SupportedParameters) != 1 || fake.SupportedParameters[0] != "reasoning_effort" {
		t.Errorf("supported parameters = %v, want [reasoning_effort]", fake.SupportedParameters)
	}
}

// A credential whose catalog cannot be read keeps no models, which is what the host reads as
// "this credential currently serves nothing".
func TestModelsForAuthReturnsNothingForARejectedKey(t *testing.T) {
	useFakeBridge(t)

	raw, errModels := modelsForAuth(mustJSON(t, pluginapi.AuthModelRequest{
		AuthID:      "cursor-dev.json",
		StorageJSON: storageJSON("bad-key"),
	}))
	if errModels != nil {
		t.Fatalf("modelsForAuth: %v", errModels)
	}
	var response pluginapi.ModelResponse
	if errUnmarshal := json.Unmarshal(decodeEnvelope(t, raw).Result, &response); errUnmarshal != nil {
		t.Fatalf("decode models: %v", errUnmarshal)
	}
	if len(response.Models) != 0 {
		t.Errorf("models = %+v, want none", response.Models)
	}
}

func TestApplyExcludedModels(t *testing.T) {
	models := []pluginapi.ModelInfo{
		{ID: "default"},
		{ID: "fake-model"},
		{ID: "auto-smart"},
		{ID: "grok-4.5"},
	}
	tests := []struct {
		name     string
		excluded []string
		want     []string
	}{
		{name: "nothing excluded", want: []string{"default", "fake-model", "auto-smart", "grok-4.5"}},
		{name: "exact default id", excluded: []string{"default"}, want: []string{"fake-model", "auto-smart", "grok-4.5"}},
		{name: "quoted default is still default", excluded: []string{" Default "}, want: []string{"fake-model", "auto-smart", "grok-4.5"}},
		{name: "wildcard suffix", excluded: []string{"grok-*"}, want: []string{"default", "fake-model", "auto-smart"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := applyExcludedModels(models, test.excluded)
			if len(got) != len(test.want) {
				t.Fatalf("models = %+v, want %v", got, test.want)
			}
			for i, id := range test.want {
				if got[i].ID != id {
					t.Errorf("models[%d] = %q, want %q", i, got[i].ID, id)
				}
			}
		})
	}
}

func TestModelsForAuthHidesHostExcludedDefault(t *testing.T) {
	useFakeBridge(t)

	raw, errModels := modelsForAuth(mustJSON(t, pluginapi.AuthModelRequest{
		AuthID:      "cursor-dev.json",
		StorageJSON: storageJSON("good-key"),
		Host: pluginapi.HostConfigSummary{
			ExcludedModels: map[string][]string{
				"cursor": {"default"},
			},
		},
	}))
	if errModels != nil {
		t.Fatalf("modelsForAuth: %v", errModels)
	}
	var response pluginapi.ModelResponse
	if errUnmarshal := json.Unmarshal(decodeEnvelope(t, raw).Result, &response); errUnmarshal != nil {
		t.Fatalf("decode models: %v", errUnmarshal)
	}
	assertHiddenModel(t, response.Models, "default")
	modelByID(t, response.Models, "fake-model")
	modelByID(t, response.Models, "auto-smart")
}

func TestModelsForAuthHidesPerAccountExcludedModels(t *testing.T) {
	useFakeBridge(t)

	raw, errModels := modelsForAuth(mustJSON(t, pluginapi.AuthModelRequest{
		AuthID:      "cursor-dev.json",
		StorageJSON: []byte(`{"type":"cursor","api_key":"good-key","excluded-models":["default"]}`),
	}))
	if errModels != nil {
		t.Fatalf("modelsForAuth: %v", errModels)
	}
	var response pluginapi.ModelResponse
	if errUnmarshal := json.Unmarshal(decodeEnvelope(t, raw).Result, &response); errUnmarshal != nil {
		t.Fatalf("decode models: %v", errUnmarshal)
	}
	assertHiddenModel(t, response.Models, "default")
}

func TestModelsForAuthPrefersMergedAttributeExclusions(t *testing.T) {
	useFakeBridge(t)

	raw, errModels := modelsForAuth(mustJSON(t, pluginapi.AuthModelRequest{
		AuthID:      "cursor-dev.json",
		StorageJSON: []byte(`{"type":"cursor","api_key":"good-key","excluded-models":["fake-model"]}`),
		Host: pluginapi.HostConfigSummary{
			ExcludedModels: map[string][]string{
				"cursor": {"auto-smart"},
			},
		},
		Attributes: map[string]string{
			"excluded_models": "default",
		},
	}))
	if errModels != nil {
		t.Fatalf("modelsForAuth: %v", errModels)
	}
	var response pluginapi.ModelResponse
	if errUnmarshal := json.Unmarshal(decodeEnvelope(t, raw).Result, &response); errUnmarshal != nil {
		t.Fatalf("decode models: %v", errUnmarshal)
	}
	assertHiddenModel(t, response.Models, "default")
	modelByID(t, response.Models, "fake-model")
	modelByID(t, response.Models, "auto-smart")
}

func TestStaticModelsHidesHostExcludedDefault(t *testing.T) {
	useFakeBridge(t)

	_, errModels := modelsForAuth(mustJSON(t, pluginapi.AuthModelRequest{
		AuthID:      "cursor-dev.json",
		StorageJSON: storageJSON("good-key"),
	}))
	if errModels != nil {
		t.Fatalf("modelsForAuth: %v", errModels)
	}

	raw, errStatic := staticModels(mustJSON(t, pluginapi.StaticModelRequest{
		Host: pluginapi.HostConfigSummary{
			ExcludedModels: map[string][]string{
				"cursor": {"default"},
			},
		},
	}))
	if errStatic != nil {
		t.Fatalf("staticModels: %v", errStatic)
	}
	var response pluginapi.ModelResponse
	if errUnmarshal := json.Unmarshal(decodeEnvelope(t, raw).Result, &response); errUnmarshal != nil {
		t.Fatalf("decode models: %v", errUnmarshal)
	}
	assertHiddenModel(t, response.Models, "default")
	modelByID(t, response.Models, "fake-model")
}

func modelByID(t *testing.T, models []pluginapi.ModelInfo, id string) pluginapi.ModelInfo {
	t.Helper()
	for _, model := range models {
		if model.ID == id {
			return model
		}
	}
	t.Fatalf("missing model %q in %+v", id, models)
	return pluginapi.ModelInfo{}
}

func assertHiddenModel(t *testing.T, models []pluginapi.ModelInfo, id string) {
	t.Helper()
	for _, model := range models {
		if model.ID == id {
			t.Fatalf("model %q still listed: %+v", id, models)
		}
	}
}

// A local agent rejects a remote image reference, so the plugin has to fetch it and send the bytes.
func TestBuildSdkImagesInlinesRemoteReferences(t *testing.T) {
	fetched := ""
	images, errBuild := buildSdkImages([]chatImage{
		{Data: base64.StdEncoding.EncodeToString([]byte("inline")), MimeType: "image/jpeg"},
		{URL: "https://example.com/a.png"},
	}, func(reference string) (string, string, error) {
		fetched = reference
		return base64.StdEncoding.EncodeToString([]byte("downloaded")), "image/png", nil
	})
	if errBuild != nil {
		t.Fatalf("buildSdkImages: %v", errBuild)
	}
	if fetched != "https://example.com/a.png" {
		t.Errorf("fetched = %q, want the remote reference", fetched)
	}
	if len(images) != 2 {
		t.Fatalf("images = %d, want 2", len(images))
	}
	for index, want := range []struct{ data, mime string }{
		{data: "inline", mime: "image/jpeg"},
		{data: "downloaded", mime: "image/png"},
	} {
		data := images[index].GetData()
		if data == nil {
			t.Fatalf("images[%d] carries no inline data: %+v", index, images[index])
		}
		decoded, errDecode := base64.StdEncoding.DecodeString(data.GetData())
		if errDecode != nil {
			t.Fatalf("images[%d] data is not base64: %v", index, errDecode)
		}
		if string(decoded) != want.data || data.GetMimeType() != want.mime {
			t.Errorf("images[%d] = %q/%q, want %q/%q", index, decoded, data.GetMimeType(), want.data, want.mime)
		}
	}
}

// An unfetchable image is the caller's problem, so it must fail the request rather than the
// credential.
func TestBuildSdkImagesReportsAFetchFailureAsARequestFault(t *testing.T) {
	_, errBuild := buildSdkImages([]chatImage{{URL: "https://example.com/gone.png"}},
		func(string) (string, string, error) { return "", "", errors.New("404") })
	if errBuild == nil {
		t.Fatal("expected a fetch failure to be reported")
	}
	failure := failureFrom(errBuild)
	if failure.HTTPStatus != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", failure.HTTPStatus)
	}
	if got := gjson.Get(failure.Message, "error.code").String(); got != "image_unavailable" {
		t.Errorf("error.code = %q, want image_unavailable in %s", got, failure.Message)
	}
}

func TestResponseMimeTypeKeepsOnlyImageMediaTypes(t *testing.T) {
	for _, test := range []struct {
		contentType string
		want        string
	}{
		{contentType: "image/png", want: "image/png"},
		// A charset parameter is not part of the media type Cursor is given.
		{contentType: "image/jpeg; charset=binary", want: "image/jpeg"},
		// A reference that answers with a web page is not an attachment, so the plugin falls
		// back to its default rather than declaring a type Cursor would reject.
		{contentType: "text/html", want: ""},
		{contentType: "", want: ""},
	} {
		if got := responseMimeType(http.Header{"Content-Type": []string{test.contentType}}); got != test.want {
			t.Errorf("responseMimeType(%q) = %q, want %q", test.contentType, got, test.want)
		}
	}
}

// An image the plugin cannot fetch has to fail the request rather than the credential, and the
// executor is where that classification has to survive. The host callback is unavailable in a
// test, which is exactly the failure being classified.
func TestExecuteReportsAnUnfetchableImageAsARequestFault(t *testing.T) {
	useFakeBridge(t)

	raw, errExecute := execute(executorRequest(t, "good-key", map[string]any{
		"messages": []map[string]any{{"role": "user", "content": []map[string]any{
			{"type": "text", "text": "what is this"},
			{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/a.png"}},
		}}},
	}))
	if errExecute != nil {
		t.Fatalf("execute: %v", errExecute)
	}
	env := decodeEnvelope(t, raw)
	if env.OK {
		t.Fatal("expected an unfetchable image to fail the request")
	}
	if env.Error.HTTPStatus != http.StatusBadRequest {
		t.Errorf("http status = %d, want 400", env.Error.HTTPStatus)
	}
	if got := gjson.Get(env.Error.Message, "error.code").String(); got != "image_unavailable" {
		t.Errorf("error.code = %q, want image_unavailable in %s", got, env.Error.Message)
	}
}

func TestBuildStreamChunkEmitsBareChunkJSON(t *testing.T) {
	finish := "stop"
	chunk := buildStreamChunk("chatcmpl-1", "fake-model", chatCompletionDelta{Content: "Hello"}, &finish, nil)
	// The chat-completions SSE writer adds the framing, so the chunk itself must stay bare.
	if strings.HasPrefix(string(chunk), "data:") {
		t.Fatalf("chunk carries SSE framing: %q", chunk)
	}
	var parsed chatCompletion
	if errUnmarshal := json.Unmarshal(chunk, &parsed); errUnmarshal != nil {
		t.Fatalf("decode chunk: %v", errUnmarshal)
	}
	if parsed.Object != "chat.completion.chunk" {
		t.Errorf("object = %q, want chat.completion.chunk", parsed.Object)
	}
	if parsed.Choices[0].Delta == nil || parsed.Choices[0].Delta.Content != "Hello" {
		t.Errorf("delta = %+v, want content Hello", parsed.Choices[0].Delta)
	}
	if parsed.Choices[0].FinishReason == nil || *parsed.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %v, want stop", parsed.Choices[0].FinishReason)
	}
}

func TestStreamFramingMatchesClientProtocol(t *testing.T) {
	for _, testCase := range []struct {
		path string
		want streamFraming
	}{
		// Chat-completions clients receive chunks verbatim and the host adds the framing.
		{"/v1/chat/completions", framingRaw},
		{"/v1/completions", framingRaw},
		{"", framingRaw},
		// Everything else is translated first, and translators only accept SSE frames.
		{"/v1/messages", framingSSE},
		{"/v1/responses", framingSSE},
		{"/v1beta/models/fake-model:streamGenerateContent", framingSSE},
	} {
		got := streamFramingFor(pluginapi.ExecutorRequest{
			Metadata: map[string]any{"request_path": testCase.path},
		})
		if got != testCase.want {
			t.Errorf("framing for %q = %v, want %v", testCase.path, got, testCase.want)
		}
	}
}

func TestStreamFramingWrapsAndTerminatesOnlyForTranslatedClients(t *testing.T) {
	payload := []byte(`{"id":"chatcmpl-1"}`)
	if got := framingRaw.frame(payload); string(got) != string(payload) {
		t.Errorf("raw framing altered the chunk: %q", got)
	}
	if got := framingRaw.terminator(); len(got) != 0 {
		// A second [DONE] would reach the client after the host writes its own.
		t.Errorf("raw framing emitted a terminator: %q", got)
	}
	if got := string(framingSSE.frame(payload)); got != "data: "+string(payload)+"\n\n" {
		t.Errorf("sse framing = %q, want a data frame", got)
	}
	if got := string(framingSSE.terminator()); got != "data: [DONE]\n\n" {
		t.Errorf("sse terminator = %q, want data: [DONE]", got)
	}
}

func TestParseChatRequestForwardsLoneUserMessageVerbatim(t *testing.T) {
	chat, errParse := parseChatRequest([]byte(`{"messages":[{"role":"user","content":"ping"}]}`))
	if errParse != nil {
		t.Fatalf("parseChatRequest: %v", errParse)
	}
	if chat.Prompt != "ping" {
		t.Errorf("prompt = %q, want %q", chat.Prompt, "ping")
	}
}

func TestParseChatRequestRendersTranscript(t *testing.T) {
	payload := []byte(`{
		"reasoning_effort": "high",
		"messages": [
			{"role": "system", "content": "Be terse."},
			{"role": "user", "content": "first"},
			{"role": "assistant", "content": "second"},
			{"role": "user", "content": [
				{"type": "text", "text": "third"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,QUJD"}}
			]}
		]
	}`)
	chat, errParse := parseChatRequest(payload)
	if errParse != nil {
		t.Fatalf("parseChatRequest: %v", errParse)
	}
	for _, want := range []string{"Be terse.", "User: first", "Assistant: second", "User: third"} {
		if !strings.Contains(chat.Prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, chat.Prompt)
		}
	}
	if len(chat.Images) != 1 || chat.Images[0].Data != "QUJD" || chat.Images[0].MimeType != "image/png" {
		t.Errorf("images = %+v, want one inline png", chat.Images)
	}
	if len(chat.Params) != 1 || chat.Params[0].ID != "reasoning_effort" || chat.Params[0].Value != "high" {
		t.Errorf("params = %+v, want reasoning_effort=high", chat.Params)
	}
}

func TestParseImageAcceptsRemoteAndInline(t *testing.T) {
	remote, ok := parseImage("https://example.com/a.png")
	if !ok || remote.URL != "https://example.com/a.png" || remote.Data != "" {
		t.Errorf("remote = %+v ok=%v, want URL form", remote, ok)
	}
	inline, ok := parseImage("data:image/jpeg;base64,QUJD")
	if !ok || inline.MimeType != "image/jpeg" || inline.Data != "QUJD" {
		t.Errorf("inline = %+v ok=%v, want inline form", inline, ok)
	}
	if _, ok := parseImage("data:image/png,QUJD"); ok {
		t.Error("expected non-base64 data URL to be rejected")
	}
}

func TestParseAuthClaimsOnlyCursorFiles(t *testing.T) {
	raw, errParse := parseAuth(mustJSON(t, pluginapi.AuthParseRequest{
		FileName: "cursor-main.json",
		RawJSON:  []byte(`{"type":"gemini","api_key":"x"}`),
	}))
	if errParse != nil {
		t.Fatalf("parseAuth: %v", errParse)
	}
	var response pluginapi.AuthParseResponse
	if errUnmarshal := json.Unmarshal(decodeEnvelope(t, raw).Result, &response); errUnmarshal != nil {
		t.Fatalf("decode auth response: %v", errUnmarshal)
	}
	if response.Handled {
		t.Error("plugin claimed an auth file belonging to another provider")
	}

	raw, errParse = parseAuth(mustJSON(t, pluginapi.AuthParseRequest{
		FileName: "cursor-main.json",
		RawJSON:  []byte(`{"type":"cursor","apiKey":"key_123","label":"team"}`),
	}))
	if errParse != nil {
		t.Fatalf("parseAuth: %v", errParse)
	}
	response = pluginapi.AuthParseResponse{}
	if errUnmarshal := json.Unmarshal(decodeEnvelope(t, raw).Result, &response); errUnmarshal != nil {
		t.Fatalf("decode auth response: %v", errUnmarshal)
	}
	if !response.Handled {
		t.Fatal("plugin declined a cursor auth file")
	}
	if response.Auth.Provider != providerIdentifier || response.Auth.Label != "team" {
		t.Errorf("auth = %+v, want cursor provider labelled team", response.Auth)
	}
}

// A credential's own proxy_url wins over the plugin-level fallback, because a pool of accounts
// routed through different proxies is the case the per-credential field exists for.
func TestResolveProxyURLPrefersTheCredential(t *testing.T) {
	useWeights(t, "proxy-url: \"http://fallback:3128\"\n")

	if got := resolveProxyURL([]byte(`{"type":"cursor","api_key":"k","proxy_url":"socks5://127.0.0.1:1080"}`)); got != "socks5://127.0.0.1:1080" {
		t.Errorf("proxy = %q, want the credential's own proxy", got)
	}
	if got := resolveProxyURL([]byte(`{"type":"cursor","api_key":"k"}`)); got != "http://fallback:3128" {
		t.Errorf("proxy = %q, want the configured fallback", got)
	}
	if got := resolveProxyURL(nil); got != "http://fallback:3128" {
		t.Errorf("proxy = %q, want the configured fallback", got)
	}
}

func TestCommandLineRegisterDeclaresLoginFlags(t *testing.T) {
	raw, errRegister := commandLineRegister()
	if errRegister != nil {
		t.Fatalf("commandLineRegister: %v", errRegister)
	}
	var response pluginapi.CommandLineRegistrationResponse
	if errUnmarshal := json.Unmarshal(decodeEnvelope(t, raw).Result, &response); errUnmarshal != nil {
		t.Fatalf("decode registration: %v", errUnmarshal)
	}
	types := map[string]string{}
	for _, flag := range response.Flags {
		types[flag.Name] = flag.Type
	}
	if types[loginFlagName] != "bool" {
		t.Errorf("--%s = %q, want bool", loginFlagName, types[loginFlagName])
	}
	if types[apiKeyFlagName] != "string" {
		t.Errorf("--%s = %q, want string", apiKeyFlagName, types[apiKeyFlagName])
	}
	if len(response.Flags) != 2 {
		t.Errorf("flags = %+v, want exactly the two login flags", response.Flags)
	}
}

func TestReadLoginAPIKeyPrefersTheFlag(t *testing.T) {
	key, errRead := readLoginAPIKey("  key_from_flag  ")
	if errRead != nil {
		t.Fatalf("readLoginAPIKey: %v", errRead)
	}
	if key != "key_from_flag" {
		t.Errorf("key = %q, want the trimmed flag value", key)
	}
}

// Without the flag the key is pasted at a prompt, which means it arrives with the newline that
// submitted it and often with clipboard whitespace.
func TestReadAPIKeyFromTrimsPastedInput(t *testing.T) {
	key, errRead := readAPIKeyFrom(strings.NewReader("  key_pasted \nignored second line\n"))
	if errRead != nil {
		t.Fatalf("readAPIKeyFrom: %v", errRead)
	}
	if key != "key_pasted" {
		t.Errorf("key = %q, want the trimmed first line", key)
	}
	if _, errEmpty := readAPIKeyFrom(strings.NewReader("   \n")); errEmpty == nil {
		t.Error("expected an empty paste to be rejected")
	}
	// A key pasted without a trailing newline still has to be accepted.
	if key, errEOF := readAPIKeyFrom(strings.NewReader("key_no_newline")); errEOF != nil || key != "key_no_newline" {
		t.Errorf("key = %q err = %v, want the key accepted at EOF", key, errEOF)
	}
}

// go test runs with stdin detached, which is the same situation as systemd or brew services: there
// is nowhere to prompt, so the command has to say so instead of blocking or reading nothing.
func TestReadLoginAPIKeyRequiresATerminalWithoutTheFlag(t *testing.T) {
	if stdinIsTerminal() {
		t.Skip("stdin is a terminal in this environment")
	}
	_, errRead := readLoginAPIKey("")
	if errRead == nil {
		t.Fatal("expected the missing terminal to be reported")
	}
	if !strings.Contains(errRead.Error(), apiKeyFlagName) {
		t.Errorf("error = %q, want a pointer at -%s", errRead, apiKeyFlagName)
	}
}

func TestCommandLineExecuteReturnsImportedAuth(t *testing.T) {
	useFakeBridge(t)

	response := loginExecute(t, "", "key_imported")
	auth := response.Auths[0]
	if auth.Provider != providerIdentifier {
		t.Errorf("provider = %q, want %q", auth.Provider, providerIdentifier)
	}
	// The file name is derived from the account so a repeat import replaces its credential.
	if auth.FileName != "cursor-dev.user-cli-example.com.json" || auth.ID != auth.FileName {
		t.Errorf("file name = %q id = %q, want account-derived name", auth.FileName, auth.ID)
	}
	if auth.Label != "Dev.User+cli@example.com" {
		t.Errorf("label = %q, want the account email", auth.Label)
	}
	if got := apiKeyFromStorage(auth.StorageJSON); got != "key_imported" {
		t.Errorf("stored api key = %q, want key_imported", got)
	}
	// A Cursor API key reports no expiry, so the host is told to leave it alone.
	if !expiryFromStorage(auth.StorageJSON).IsZero() {
		t.Errorf("stored expiry = %s, want none", expiryFromStorage(auth.StorageJSON))
	}
	if !auth.NextRefreshAfter.After(time.Now().UTC().Add(300 * 24 * time.Hour)) {
		t.Errorf("next refresh = %s, want a far-future deadline", auth.NextRefreshAfter)
	}
	// The parser must accept what the import produced, otherwise the saved file would be
	// claimed by nobody on the next startup.
	parsed, errParse := parseAuth(mustJSON(t, pluginapi.AuthParseRequest{
		FileName: auth.FileName,
		RawJSON:  auth.StorageJSON,
	}))
	if errParse != nil {
		t.Fatalf("parseAuth: %v", errParse)
	}
	var parseResponse pluginapi.AuthParseResponse
	if errUnmarshal := json.Unmarshal(decodeEnvelope(t, parsed).Result, &parseResponse); errUnmarshal != nil {
		t.Fatalf("decode auth response: %v", errUnmarshal)
	}
	if !parseResponse.Handled {
		t.Error("parseAuth declined the credential produced by the import")
	}
}

// A key the account cannot use must not be written to disk, which is the whole point of checking
// it before saving.
func TestCommandLineExecuteRejectsAnInvalidKey(t *testing.T) {
	useFakeBridge(t)

	raw, errExecute := commandLineExecute(mustJSON(t, pluginapi.CommandLineExecutionRequest{
		Program: "cli-proxy-api",
		TriggeredFlags: map[string]pluginapi.CommandLineFlagValue{
			loginFlagName:  {Name: loginFlagName, Type: "bool", Value: "true", Set: true},
			apiKeyFlagName: {Name: apiKeyFlagName, Type: "string", Value: "bad-key", Set: true},
		},
	}))
	if errExecute != nil {
		t.Fatalf("commandLineExecute: %v", errExecute)
	}
	var response pluginapi.CommandLineExecutionResponse
	if errUnmarshal := json.Unmarshal(decodeEnvelope(t, raw).Result, &response); errUnmarshal != nil {
		t.Fatalf("decode execution: %v", errUnmarshal)
	}
	if response.ExitCode == 0 {
		t.Fatalf("exit code = 0, want a failure for a rejected key")
	}
	if len(response.Auths) != 0 {
		t.Errorf("auths = %+v, want none saved", response.Auths)
	}
	if !strings.Contains(string(response.Stderr), "Invalid User API Key") {
		t.Errorf("stderr = %q, want Cursor's reason", response.Stderr)
	}
}

// loginExecute runs the import command against the fake bridge with authDir as the host's
// auth directory, which is where the merge looks for the file it is about to replace.
func loginExecute(t *testing.T, authDir, apiKey string) pluginapi.CommandLineExecutionResponse {
	t.Helper()
	raw, errExecute := commandLineExecute(mustJSON(t, pluginapi.CommandLineExecutionRequest{
		Program: "cli-proxy-api",
		Args:    []string{"--" + loginFlagName, "--" + apiKeyFlagName, apiKey},
		Host:    pluginapi.HostConfigSummary{AuthDir: authDir},
		TriggeredFlags: map[string]pluginapi.CommandLineFlagValue{
			loginFlagName:  {Name: loginFlagName, Type: "bool", Value: "true", Set: true},
			apiKeyFlagName: {Name: apiKeyFlagName, Type: "string", Value: apiKey, Set: true},
		},
	}))
	if errExecute != nil {
		t.Fatalf("commandLineExecute: %v", errExecute)
	}
	var response pluginapi.CommandLineExecutionResponse
	if errUnmarshal := json.Unmarshal(decodeEnvelope(t, raw).Result, &response); errUnmarshal != nil {
		t.Fatalf("decode execution: %v", errUnmarshal)
	}
	if response.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %s", response.ExitCode, response.Stderr)
	}
	if len(response.Auths) != 1 {
		t.Fatalf("auths = %+v, want exactly one", response.Auths)
	}
	return response
}

// Replacing a key must not discard the routing settings an operator put in the auth file,
// because the host rewrites that file from what this command returns.
func TestCommandLineExecutePreservesExistingAuthFileSettings(t *testing.T) {
	useFakeBridge(t)
	authDir := t.TempDir()
	existing := map[string]any{
		"type":      providerIdentifier,
		"api_key":   "key_previous",
		"email":     "Dev.User+cli@example.com",
		"label":     "team pool",
		"prefix":    "cursor-a",
		"proxy_url": "socks5://127.0.0.1:1080",
		"note":      "shared account",
		"model_aliases": []map[string]string{
			{"name": "grok-4.3", "alias": "grok-latest"},
		},
	}
	fileName := "cursor-dev.user-cli-example.com.json"
	if errWrite := os.WriteFile(filepath.Join(authDir, fileName), mustJSON(t, existing), 0o600); errWrite != nil {
		t.Fatalf("write existing auth file: %v", errWrite)
	}

	auth := loginExecute(t, authDir, "key_imported").Auths[0]
	if auth.FileName != fileName {
		t.Fatalf("file name = %q, want %q so the import replaces the same file", auth.FileName, fileName)
	}
	if got := apiKeyFromStorage(auth.StorageJSON); got != "key_imported" {
		t.Errorf("api key = %q, want the newly imported key", got)
	}

	// Operator-owned settings have to survive.
	for field, want := range map[string]string{
		"label":     "team pool",
		"prefix":    "cursor-a",
		"proxy_url": "socks5://127.0.0.1:1080",
		"note":      "shared account",
	} {
		if got := gjson.GetBytes(auth.StorageJSON, field).String(); got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}
	if got := gjson.GetBytes(auth.StorageJSON, "model_aliases.0.alias").String(); got != "grok-latest" {
		t.Errorf("model_aliases.0.alias = %q, want grok-latest", got)
	}
	if auth.Prefix != "cursor-a" {
		t.Errorf("auth prefix = %q, want the preserved cursor-a", auth.Prefix)
	}
	if auth.Label != "team pool" {
		t.Errorf("auth label = %q, want the preserved label", auth.Label)
	}
	if auth.ProxyURL != "socks5://127.0.0.1:1080" {
		t.Errorf("auth proxy = %q, want the preserved proxy", auth.ProxyURL)
	}
}

// An expiry left behind by the old browser login flow must not survive, otherwise a freshly
// imported credential could be reported as already expired.
func TestCommandLineExecuteClearsStaleCredentialSpellings(t *testing.T) {
	useFakeBridge(t)
	authDir := t.TempDir()
	fileName := "cursor-dev.user-cli-example.com.json"
	existing := map[string]any{
		"type":              providerIdentifier,
		"apiKey":            "key_previous",
		"expiresAt":         time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
		"apiKeyExpiresAtMs": time.Now().UTC().Add(-time.Hour).UnixMilli(),
		"prefix":            "cursor-a",
	}
	if errWrite := os.WriteFile(filepath.Join(authDir, fileName), mustJSON(t, existing), 0o600); errWrite != nil {
		t.Fatalf("write existing auth file: %v", errWrite)
	}

	auth := loginExecute(t, authDir, "key_imported").Auths[0]
	for _, field := range []string{"apiKey", "expiresAt", "expires_at", "expires_at_ms", "apiKeyExpiresAtMs"} {
		if gjson.GetBytes(auth.StorageJSON, field).Exists() {
			t.Errorf("%s survived the merge: %s", field, auth.StorageJSON)
		}
	}
	if got := gjson.GetBytes(auth.StorageJSON, "prefix").String(); got != "cursor-a" {
		t.Errorf("prefix = %q, want it preserved alongside the credential reset", got)
	}
}

func TestCommandLineExecuteWithoutExistingFile(t *testing.T) {
	useFakeBridge(t)

	for _, test := range []struct {
		name    string
		authDir func(t *testing.T) string
	}{
		{name: "empty auth dir", authDir: func(*testing.T) string { return "" }},
		{name: "no file for this account", authDir: func(t *testing.T) string { return t.TempDir() }},
		{
			name: "malformed existing file",
			authDir: func(t *testing.T) string {
				dir := t.TempDir()
				path := filepath.Join(dir, "cursor-dev.user-cli-example.com.json")
				if errWrite := os.WriteFile(path, []byte("{not json"), 0o600); errWrite != nil {
					t.Fatalf("write malformed auth file: %v", errWrite)
				}
				return dir
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			auth := loginExecute(t, test.authDir(t), "key_imported").Auths[0]
			if got := apiKeyFromStorage(auth.StorageJSON); got != "key_imported" {
				t.Errorf("api key = %q, want key_imported", got)
			}
			if got := gjson.GetBytes(auth.StorageJSON, "email").String(); got != "Dev.User+cli@example.com" {
				t.Errorf("email = %q, want the account the key belongs to", got)
			}
		})
	}
}

func TestCommandLineExecuteIgnoresUntriggeredInvocation(t *testing.T) {
	raw, errExecute := commandLineExecute(mustJSON(t, pluginapi.CommandLineExecutionRequest{
		Program: "cli-proxy-api",
	}))
	if errExecute != nil {
		t.Fatalf("commandLineExecute: %v", errExecute)
	}
	var response pluginapi.CommandLineExecutionResponse
	if errUnmarshal := json.Unmarshal(decodeEnvelope(t, raw).Result, &response); errUnmarshal != nil {
		t.Fatalf("decode execution: %v", errUnmarshal)
	}
	if len(response.Auths) != 0 || response.ExitCode != 0 || len(response.Stdout) != 0 {
		t.Errorf("response = %+v, want a no-op", response)
	}
}

func TestParseAuthRejectsExpiredKey(t *testing.T) {
	expired := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	raw, errParse := parseAuth(mustJSON(t, pluginapi.AuthParseRequest{
		FileName: "cursor-main.json",
		RawJSON:  []byte(`{"type":"cursor","api_key":"key_old","expires_at":"` + expired + `"}`),
	}))
	if errParse != nil {
		t.Fatalf("parseAuth: %v", errParse)
	}
	env := decodeEnvelope(t, raw)
	if env.OK {
		t.Fatal("expected an expired key to be rejected")
	}
	if !strings.Contains(env.Error.Message, loginFlagName) {
		t.Errorf("message = %q, want a pointer at --%s", env.Error.Message, loginFlagName)
	}
}

func TestRefreshAuthReportsExpiry(t *testing.T) {
	expiry := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
	raw, errRefresh := refreshAuth(mustJSON(t, pluginapi.AuthRefreshRequest{
		AuthID:      "cursor-main.json",
		StorageJSON: []byte(`{"type":"cursor","api_key":"key_live","expires_at":"` + expiry.Format(time.RFC3339) + `"}`),
	}))
	if errRefresh != nil {
		t.Fatalf("refreshAuth: %v", errRefresh)
	}
	var response pluginapi.AuthRefreshResponse
	if errUnmarshal := json.Unmarshal(decodeEnvelope(t, raw).Result, &response); errUnmarshal != nil {
		t.Fatalf("decode refresh: %v", errUnmarshal)
	}
	if !response.NextRefreshAfter.Equal(expiry) {
		t.Errorf("next refresh = %s, want the key expiry %s", response.NextRefreshAfter, expiry)
	}
}

// useWeights installs a plugin config from YAML so the tests exercise the same decoding and
// normalization the host performs when it hands the plugin its config block.
func useWeights(t *testing.T, raw string) {
	t.Helper()
	cfg, errDecode := decodeConfig([]byte(raw))
	if errDecode != nil {
		t.Fatalf("decodeConfig: %v", errDecode)
	}
	currentConfig.Store(cfg)
	t.Cleanup(func() { currentConfig.Store(defaultPluginConfig()) })
}

func parseAuthData(t *testing.T, fileName, storage string) pluginapi.AuthData {
	t.Helper()
	raw, errParse := parseAuth(mustJSON(t, pluginapi.AuthParseRequest{
		FileName: fileName,
		RawJSON:  []byte(storage),
	}))
	if errParse != nil {
		t.Fatalf("parseAuth: %v", errParse)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("parseAuth failed: %+v", env.Error)
	}
	var response pluginapi.AuthParseResponse
	if errUnmarshal := json.Unmarshal(env.Result, &response); errUnmarshal != nil {
		t.Fatalf("decode auth response: %v", errUnmarshal)
	}
	if !response.Handled {
		t.Fatal("parseAuth declined a cursor auth file")
	}
	return response.Auth
}

func refreshAuthData(t *testing.T, req pluginapi.AuthRefreshRequest) pluginapi.AuthData {
	t.Helper()
	raw, errRefresh := refreshAuth(mustJSON(t, req))
	if errRefresh != nil {
		t.Fatalf("refreshAuth: %v", errRefresh)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("refreshAuth failed: %+v", env.Error)
	}
	var response pluginapi.AuthRefreshResponse
	if errUnmarshal := json.Unmarshal(env.Result, &response); errUnmarshal != nil {
		t.Fatalf("decode refresh: %v", errUnmarshal)
	}
	return response.Auth
}

func TestParseAuthAppliesConfiguredWeight(t *testing.T) {
	useWeights(t, "weights:\n  dev@example.com: 5\n  cursor-team.json: 3\n  cursor-solo: 7\n")

	tests := []struct {
		name     string
		fileName string
		storage  string
		want     string
	}{
		{
			name:     "account email",
			fileName: "cursor-dev.json",
			storage:  `{"type":"cursor","api_key":"key_a","email":"dev@example.com"}`,
			want:     "5",
		},
		{
			name:     "email matched case-insensitively",
			fileName: "cursor-dev.json",
			storage:  `{"type":"cursor","api_key":"key_a","email":"Dev@Example.com"}`,
			want:     "5",
		},
		{
			name:     "auth file name",
			fileName: "cursor-team.json",
			storage:  `{"type":"cursor","api_key":"key_b"}`,
			want:     "3",
		},
		{
			name:     "auth file name without the json suffix",
			fileName: "cursor-solo.json",
			storage:  `{"type":"cursor","api_key":"key_c"}`,
			want:     "7",
		},
		{
			name:     "email outranks the file name",
			fileName: "cursor-team.json",
			storage:  `{"type":"cursor","api_key":"key_d","email":"dev@example.com"}`,
			want:     "5",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			auth := parseAuthData(t, test.fileName, test.storage)
			if got := auth.Attributes[weightAttribute]; got != test.want {
				t.Fatalf("weight = %q, want %q", got, test.want)
			}
		})
	}
}

// An unconfigured credential must carry no weight attribute at all, because the host reads
// an absent attribute as its default share rather than as zero.
func TestParseAuthOmitsWeightWhenUnconfigured(t *testing.T) {
	useWeights(t, "weights:\n  other@example.com: 4\n")

	auth := parseAuthData(t, "cursor-main.json", `{"type":"cursor","api_key":"key_a","email":"dev@example.com"}`)
	if got, exists := auth.Attributes[weightAttribute]; exists {
		t.Fatalf("weight = %q, want no attribute", got)
	}
}

// A non-positive weight is the documented way to park a credential while the weighted
// strategy is active, so it has to reach the host as an explicit zero.
func TestParseAuthWritesZeroForNonPositiveWeight(t *testing.T) {
	useWeights(t, "weights:\n  zeroed@example.com: 0\n  parked@example.com: -3\n")

	for _, email := range []string{"zeroed@example.com", "parked@example.com"} {
		auth := parseAuthData(t, "cursor-main.json", `{"type":"cursor","api_key":"key_a","email":"`+email+`"}`)
		if got := auth.Attributes[weightAttribute]; got != "0" {
			t.Errorf("%s weight = %q, want 0", email, got)
		}
	}
}

func TestDecodeConfigRejectsUnusableWeights(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "above the host maximum", raw: "weights:\n  dev@example.com: 1000001\n"},
		{name: "fractional", raw: "weights:\n  dev@example.com: 1.5\n"},
		{name: "non-numeric", raw: "weights:\n  dev@example.com: heavy\n"},
		{name: "empty credential key", raw: "weights:\n  \"  \": 5\n"},
		{name: "keys collide once normalized", raw: "weights:\n  Dev@Example.com: 5\n  dev@example.com: 2\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, errDecode := decodeConfig([]byte(test.raw)); errDecode == nil {
				t.Fatal("decodeConfig accepted a weight the host would reject")
			}
		})
	}
}

// A refresh must re-read the config instead of echoing the attribute it was handed, so that
// an edited weight takes effect without waiting for the credential to be parsed again.
func TestRefreshAuthReresolvesWeight(t *testing.T) {
	useWeights(t, "weights:\n  dev@example.com: 6\n")

	auth := refreshAuthData(t, pluginapi.AuthRefreshRequest{
		AuthID:      "cursor-dev.json",
		StorageJSON: []byte(`{"type":"cursor","api_key":"key_live","email":"dev@example.com"}`),
		Attributes:  map[string]string{weightAttribute: "1", "path": "/auths/cursor-dev.json"},
	})
	if got := auth.Attributes[weightAttribute]; got != "6" {
		t.Errorf("weight = %q, want the currently configured 6", got)
	}
	if got := auth.Attributes["path"]; got != "/auths/cursor-dev.json" {
		t.Errorf("path = %q, want the host attribute preserved", got)
	}
}

func TestRefreshAuthDropsWeightRemovedFromConfig(t *testing.T) {
	useWeights(t, "weights: {}\n")

	auth := refreshAuthData(t, pluginapi.AuthRefreshRequest{
		AuthID:      "cursor-dev.json",
		StorageJSON: []byte(`{"type":"cursor","api_key":"key_live","email":"dev@example.com"}`),
		Attributes:  map[string]string{weightAttribute: "9", "path": "/auths/cursor-dev.json"},
	})
	if got, exists := auth.Attributes[weightAttribute]; exists {
		t.Errorf("weight = %q, want the stale attribute dropped", got)
	}
	if got := auth.Attributes["path"]; got != "/auths/cursor-dev.json" {
		t.Errorf("path = %q, want the host attribute preserved", got)
	}
}

func TestParseAuthKeepsUndatedKeysAlive(t *testing.T) {
	raw, errParse := parseAuth(mustJSON(t, pluginapi.AuthParseRequest{
		FileName: "cursor-main.json",
		RawJSON:  []byte(`{"type":"cursor","api_key":"key_dashboard"}`),
	}))
	if errParse != nil {
		t.Fatalf("parseAuth: %v", errParse)
	}
	var response pluginapi.AuthParseResponse
	if errUnmarshal := json.Unmarshal(decodeEnvelope(t, raw).Result, &response); errUnmarshal != nil {
		t.Fatalf("decode auth response: %v", errUnmarshal)
	}
	if !response.Handled {
		t.Fatal("a dashboard key without an expiry was declined")
	}
	if !response.Auth.NextRefreshAfter.After(time.Now().UTC().Add(300 * 24 * time.Hour)) {
		t.Errorf("next refresh = %s, want a far-future deadline", response.Auth.NextRefreshAfter)
	}
}

func TestParseAuthRejectsMissingKey(t *testing.T) {
	raw, errParse := parseAuth(mustJSON(t, pluginapi.AuthParseRequest{
		FileName: "cursor-main.json",
		RawJSON:  []byte(`{"type":"cursor"}`),
	}))
	if errParse != nil {
		t.Fatalf("parseAuth: %v", errParse)
	}
	if env := decodeEnvelope(t, raw); env.OK {
		t.Error("expected a keyless cursor auth file to be rejected")
	}
}

// poolBridge returns the process the fake harness installed, for tests that talk to it directly.
func poolBridge(t *testing.T) *bridgeProcess {
	t.Helper()
	bridgePool.mu.Lock()
	defer bridgePool.mu.Unlock()
	process := bridgePool.procs[""]
	if process == nil {
		t.Fatal("no fake bridge is installed in the pool")
	}
	return process
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	return raw
}
