package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"

	sdkv1 "github.com/UNICKCHENG/cliproxyapi-plugins/auth-cursor/go/internal/sdk/v1"
	"github.com/UNICKCHENG/cliproxyapi-plugins/auth-cursor/go/internal/sdk/v1/sdkv1connect"
)

// fakeBridgeToken is the bearer token the fake service expects, standing in for the per-process
// token a real bridge writes to its auth token file.
const fakeBridgeToken = "fake-bridge-token"

// fakeBridge implements the sdk.v1 services the plugin uses, keyed on the API key so one server
// can play a healthy account, a rejected key, a refused model and a rate limit.
//
// Behaviour is selected by credential rather than by endpoint because that is how the plugin
// itself is structured: every RPC carries the key, and nothing else distinguishes the callers.
type fakeBridge struct {
	sdkv1connect.UnimplementedSdkAgentServiceHandler
	sdkv1connect.UnimplementedSdkCursorServiceHandler
	sdkv1connect.UnimplementedSdkBridgeControlServiceHandler

	mu           sync.Mutex
	created      []*sdkv1.AgentOptions
	agentByID    map[string]*sdkv1.AgentOptions
	agentSeq     int
	closed       []string
	deleted      []string
	cancelled    []string
	sendAPIKeys  []string
	enableDeltas []bool
	// hold blocks the Send stream after the run id has been announced, so a test can cancel a
	// run that is genuinely in flight.
	hold chan struct{}
	// announced is closed once the stream has reported its run id, which is the earliest point
	// at which a cancellation has something to cancel.
	announced     chan struct{}
	announcedOnce sync.Once

	callbackURL   string
	callbackToken string
	toolRounds    []fakeToolRound
	toolResults   []*sdkv1.CallCustomToolResponse
	inFlightTools int
	sendPrompts   []string
}

type fakeToolCall struct {
	Name string
	Args map[string]any
}

type fakeToolRound struct {
	Deltas     []string
	Calls      []fakeToolCall
	ExtraGate  <-chan struct{}
	ExtraCalls []fakeToolCall
}

func newFakeBridge() *fakeBridge {
	return &fakeBridge{announced: make(chan struct{})}
}

// serveFakeBridge starts the fake service and returns its base URL.
func serveFakeBridge(t *testing.T, bridge *fakeBridge) string {
	t.Helper()
	mux := http.NewServeMux()
	options := connect.WithHandlerOptions(connect.WithInterceptors(requireFakeBridgeToken{}))
	mux.Handle(sdkv1connect.NewSdkAgentServiceHandler(bridge, options))
	mux.Handle(sdkv1connect.NewSdkCursorServiceHandler(bridge, options))
	mux.Handle(sdkv1connect.NewSdkBridgeControlServiceHandler(bridge, options))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL
}

// requireFakeBridgeToken mirrors the bridge's own bearer check, so a client that forgets the
// header fails here the same way it would against a real bridge.
type requireFakeBridgeToken struct{}

func (requireFakeBridgeToken) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, request connect.AnyRequest) (connect.AnyResponse, error) {
		if request.Header().Get("Authorization") != "Bearer "+fakeBridgeToken {
			return nil, connect.NewError(connect.CodeUnauthenticated, errUnauthorizedBridge)
		}
		return next(ctx, request)
	}
}

func (requireFakeBridgeToken) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (requireFakeBridgeToken) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if conn.RequestHeader().Get("Authorization") != "Bearer "+fakeBridgeToken {
			return connect.NewError(connect.CodeUnauthenticated, errUnauthorizedBridge)
		}
		return next(ctx, conn)
	}
}

type bridgeAuthError struct{}

func (bridgeAuthError) Error() string { return "Unauthorized" }

var errUnauthorizedBridge = bridgeAuthError{}

func (f *fakeBridge) Ping(context.Context, *connect.Request[sdkv1.PingRequest]) (*connect.Response[sdkv1.PingResponse], error) {
	return connect.NewResponse(&sdkv1.PingResponse{Message: "ok"}), nil
}

func (f *fakeBridge) SetToolCallback(_ context.Context, req *connect.Request[sdkv1.SetToolCallbackRequest]) (*connect.Response[sdkv1.SetToolCallbackResponse], error) {
	f.mu.Lock()
	f.callbackURL = req.Msg.GetUrl()
	f.callbackToken = req.Msg.GetAuthToken()
	f.mu.Unlock()
	return connect.NewResponse(&sdkv1.SetToolCallbackResponse{}), nil
}

func (f *fakeBridge) Me(_ context.Context, req *connect.Request[sdkv1.MeRequest]) (*connect.Response[sdkv1.MeResponse], error) {
	if errKey := fakeBridgeKeyError(req.Msg.GetOptions().GetApiKey()); errKey != nil {
		return nil, errKey
	}
	return connect.NewResponse(&sdkv1.MeResponse{User: &sdkv1.SdkUser{
		UserEmail: "Dev.User+cli@example.com",
		UserId:    42,
	}}), nil
}

func (f *fakeBridge) ListModels(_ context.Context, req *connect.Request[sdkv1.ListModelsRequest]) (*connect.Response[sdkv1.ListModelsResponse], error) {
	if errKey := fakeBridgeKeyError(req.Msg.GetOptions().GetApiKey()); errKey != nil {
		return nil, errKey
	}
	return connect.NewResponse(&sdkv1.ListModelsResponse{Items: []*sdkv1.SdkModel{
		{
			Id:          "default",
			DisplayName: "Default",
		},
		{
			Id:          "fake-model",
			DisplayName: "Fake",
			Description: "a fake model",
			Parameters: []*sdkv1.ModelParameterDefinition{{
				Id: "reasoning_effort",
				Values: []*sdkv1.ModelParameterDefinitionValue{
					{Value: "low"},
					{Value: "high"},
				},
			}},
		},
		{
			Id:          "auto-smart",
			DisplayName: "Auto",
			Parameters: []*sdkv1.ModelParameterDefinition{{
				Id: optimizeForParameter,
				Values: []*sdkv1.ModelParameterDefinitionValue{
					{Value: "cost"},
					{Value: "balanced"},
					{Value: "intelligence"},
				},
			}},
		},
	}}), nil
}

func (f *fakeBridge) CreateAgent(_ context.Context, req *connect.Request[sdkv1.CreateAgentRequest]) (*connect.Response[sdkv1.CreateAgentResponse], error) {
	options := req.Msg.GetOptions()
	if errKey := fakeBridgeKeyError(options.GetApiKey()); errKey != nil {
		return nil, errKey
	}
	f.mu.Lock()
	f.agentSeq++
	id := fmt.Sprintf("agent-%d", f.agentSeq)
	f.created = append(f.created, options)
	if f.agentByID == nil {
		f.agentByID = make(map[string]*sdkv1.AgentOptions)
	}
	f.agentByID[id] = options
	f.mu.Unlock()
	return connect.NewResponse(&sdkv1.CreateAgentResponse{AgentId: id, Model: options.GetModel()}), nil
}

func (f *fakeBridge) CloseAgent(_ context.Context, req *connect.Request[sdkv1.CloseAgentRequest]) (*connect.Response[sdkv1.CloseAgentResponse], error) {
	f.mu.Lock()
	f.closed = append(f.closed, req.Msg.GetAgentId())
	f.mu.Unlock()
	return connect.NewResponse(&sdkv1.CloseAgentResponse{}), nil
}

func (f *fakeBridge) DeleteAgent(_ context.Context, req *connect.Request[sdkv1.DeleteAgentRequest]) (*connect.Response[sdkv1.DeleteAgentResponse], error) {
	f.mu.Lock()
	f.deleted = append(f.deleted, req.Msg.GetAgentId())
	f.mu.Unlock()
	return connect.NewResponse(&sdkv1.DeleteAgentResponse{}), nil
}

func (f *fakeBridge) CancelRun(_ context.Context, req *connect.Request[sdkv1.CancelRunRequest]) (*connect.Response[sdkv1.CancelRunResponse], error) {
	f.mu.Lock()
	f.cancelled = append(f.cancelled, req.Msg.GetRunId())
	hold := f.hold
	f.mu.Unlock()
	if hold != nil {
		// Releasing the held stream is what a real bridge does on cancellation: the run ends and
		// the stream delivers its terminal messages.
		select {
		case <-hold:
		default:
			close(hold)
		}
	}
	return connect.NewResponse(&sdkv1.CancelRunResponse{}), nil
}

// Send streams one run. The event order mirrors a live bridge stream: conversation messages, then
// opt-in deltas, then the terminal result and the end-of-stream marker, with a keepalive mixed in.
func (f *fakeBridge) Send(ctx context.Context, req *connect.Request[sdkv1.SendRequest], stream *connect.ServerStream[sdkv1.RunStreamMessage]) error {
	agentID := req.Msg.GetAgentId()
	apiKey := fakeBridgeSendKey(f, agentID)
	f.mu.Lock()
	f.sendAPIKeys = append(f.sendAPIKeys, apiKey)
	f.enableDeltas = append(f.enableDeltas, req.Msg.GetOptions().GetEnableDeltas())
	f.sendPrompts = append(f.sendPrompts, req.Msg.GetMessage().GetText())
	hold := f.hold
	customTools := len(f.agentByID[agentID].GetLocal().GetCustomTools()) > 0
	f.mu.Unlock()

	if errSend := stream.Send(&sdkv1.RunStreamMessage{
		Envelope: &sdkv1.RunStreamMessage_SdkMessage{SdkMessage: &sdkv1.SdkMessage{
			Type:    "system",
			Message: mustStruct(map[string]any{"subtype": "init", "run_id": "run-1", "agent_id": agentID}),
		}},
	}); errSend != nil {
		return errSend
	}
	f.announcedOnce.Do(func() { close(f.announced) })
	// A keepalive: no envelope case and no offset. Adapters must ignore it silently.
	if errSend := stream.Send(&sdkv1.RunStreamMessage{}); errSend != nil {
		return errSend
	}

	if hold != nil {
		// One delta before blocking, so a test can tell that the client has consumed the run id
		// rather than merely that the server has written it.
		if errSend := stream.Send(textDeltaMessage("...")); errSend != nil {
			return errSend
		}
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
			return connect.NewError(connect.CodeDeadlineExceeded, errUnauthorizedBridge)
		}
	}

	if apiKey == "region-key" {
		// A run that fails inside the agent: the reason arrives in a status message and the
		// terminal envelope carries no error code.
		if errSend := stream.Send(&sdkv1.RunStreamMessage{
			Envelope: &sdkv1.RunStreamMessage_SdkMessage{SdkMessage: &sdkv1.SdkMessage{
				Type: "status",
				Message: mustStruct(map[string]any{
					"status":  "error",
					"run_id":  "run-1",
					"message": "Model not available This model provider is not supported in your region.",
				}),
			}},
		}); errSend != nil {
			return errSend
		}
		return stream.Send(&sdkv1.RunStreamMessage{
			Envelope: &sdkv1.RunStreamMessage_Result{Result: &sdkv1.RunStreamResult{
				AgentId: agentID,
				RunId:   "run-1",
				Status:  sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_ERROR,
			}},
		})
	}

	if customTools {
		if errTools := f.sendToolRounds(ctx, stream, agentID); errTools != nil {
			return errTools
		}
		return f.sendTerminal(stream, agentID, false)
	}
	return f.sendTerminal(stream, agentID, true)
}
func (f *fakeBridge) sendToolRounds(ctx context.Context, stream *connect.ServerStream[sdkv1.RunStreamMessage], agentID string) error {
	f.mu.Lock()
	url := f.callbackURL
	token := f.callbackToken
	rounds := append([]fakeToolRound(nil), f.toolRounds...)
	f.mu.Unlock()
	if url == "" {
		return errors.New("fake bridge has no tool callback URL")
	}
	client := sdkv1connect.NewSdkCustomToolCallbackServiceClient(http.DefaultClient, strings.TrimRight(url, "/"))
	for _, round := range rounds {
		if errRound := f.sendOneToolRound(ctx, stream, client, token, agentID, round); errRound != nil {
			return errRound
		}
	}
	return nil
}

func (f *fakeBridge) sendOneToolRound(ctx context.Context, stream *connect.ServerStream[sdkv1.RunStreamMessage], client sdkv1connect.SdkCustomToolCallbackServiceClient, token, agentID string, round fakeToolRound) error {
	var wg sync.WaitGroup
	errCh := make(chan error, len(round.Calls)+len(round.ExtraCalls)+1)
	start := func(call fakeToolCall) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- f.callCustomTool(ctx, client, token, agentID, call)
		}()
	}
	for _, call := range round.Calls {
		start(call)
	}
	if round.ExtraGate != nil && len(round.ExtraCalls) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-round.ExtraGate:
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}
			var extraWG sync.WaitGroup
			for _, call := range round.ExtraCalls {
				call := call
				extraWG.Add(1)
				go func() {
					defer extraWG.Done()
					errCh <- f.callCustomTool(ctx, client, token, agentID, call)
				}()
			}
			extraWG.Wait()
		}()
	}
	for _, delta := range round.Deltas {
		if errSend := stream.Send(textDeltaMessage(delta)); errSend != nil {
			return errSend
		}
	}
	wg.Wait()
	close(errCh)
	for errCall := range errCh {
		if errCall != nil {
			return errCall
		}
	}
	return nil
}

func (f *fakeBridge) callCustomTool(_ context.Context, client sdkv1connect.SdkCustomToolCallbackServiceClient, token, agentID string, call fakeToolCall) error {
	request := connect.NewRequest(&sdkv1.CallCustomToolRequest{
		ToolName: call.Name,
		AgentId:  agentID,
	})
	if call.Args != nil {
		request.Msg.Args = mustStruct(call.Args)
	}
	request.Header().Set("Authorization", "Bearer "+token)
	f.mu.Lock()
	f.inFlightTools++
	f.mu.Unlock()
	// The callback RPC is independent of the Send stream context: aborting a parked run
	// must still deliver the isError result to the bridge.
	response, errCall := client.CallCustomTool(context.Background(), request)
	f.mu.Lock()
	f.inFlightTools--
	if errCall == nil && response != nil {
		f.toolResults = append(f.toolResults, response.Msg)
	}
	f.mu.Unlock()
	return errCall
}

func (f *fakeBridge) sendTerminal(stream *connect.ServerStream[sdkv1.RunStreamMessage], agentID string, includeIgnoredDelta bool) error {
	updates := []*sdkv1.RunStreamMessage{textDeltaMessage("Hello")}
	if includeIgnoredDelta {
		updates = append(updates, updateMessage("tool-call-delta", "IGNORED"))
	}
	updates = append(updates, textDeltaMessage(" world"))
	for _, update := range updates {
		if errSend := stream.Send(update); errSend != nil {
			return errSend
		}
	}
	status := sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED
	if len(f.cancelledRuns()) > 0 {
		status = sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_CANCELLED
	}
	if errSend := stream.Send(&sdkv1.RunStreamMessage{
		Envelope: &sdkv1.RunStreamMessage_Result{Result: &sdkv1.RunStreamResult{
			AgentId: agentID,
			RunId:   "run-1",
			Status:  status,
			Result: &sdkv1.RunResult{
				RunId:  "run-1",
				Result: "Hello world",
				Usage: &sdkv1.TokenUsage{
					InputTokens:     7,
					OutputTokens:    2,
					CacheReadTokens: 3,
					TotalTokens:     12,
				},
			},
		}},
	}); errSend != nil {
		return errSend
	}
	return stream.Send(&sdkv1.RunStreamMessage{
		Envelope: &sdkv1.RunStreamMessage_Done{Done: &sdkv1.RunStreamDone{AgentId: agentID, RunId: "run-1"}},
	})
}

func textDeltaMessage(text string) *sdkv1.RunStreamMessage {
	return updateMessage(textDeltaUpdate, text)
}

func updateMessage(kind, text string) *sdkv1.RunStreamMessage {
	return &sdkv1.RunStreamMessage{
		Envelope: &sdkv1.RunStreamMessage_InteractionUpdate{InteractionUpdate: &sdkv1.InteractionUpdate{
			Type:   kind,
			Update: mustStruct(map[string]any{"type": kind, "text": text}),
		}},
	}
}

func (f *fakeBridge) cancelledRuns() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cancelled...)
}

func (f *fakeBridge) createdAgents() []*sdkv1.AgentOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*sdkv1.AgentOptions(nil), f.created...)
}

func (f *fakeBridge) closedAgents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.closed...)
}

func (f *fakeBridge) deletedAgents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

func (f *fakeBridge) inflightToolCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inFlightTools
}

func (f *fakeBridge) recordedToolResults() []*sdkv1.CallCustomToolResponse {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*sdkv1.CallCustomToolResponse(nil), f.toolResults...)
}

func (f *fakeBridge) recordedPrompts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sendPrompts...)
}

// fakeBridgeSendKey recovers the credential a Send belongs to. SendRequest carries no key, so the
// fake looks it up from the agent it was created with, exactly as a real bridge would.
func fakeBridgeSendKey(f *fakeBridge, agentID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if options, ok := f.agentByID[agentID]; ok {
		return options.GetApiKey()
	}
	if agentID == "" || len(f.created) == 0 {
		return ""
	}
	return f.created[len(f.created)-1].GetApiKey()
}

// fakeBridgeKeyError turns a credential into the error the bridge would report for it.
func fakeBridgeKeyError(apiKey string) error {
	switch apiKey {
	case "bad-key":
		return sdkErrorResponse(connect.CodeUnauthenticated, &sdkv1.SdkErrorDetails{
			SdkErrorCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_UNAUTHORIZED,
			Message:      "Invalid User API Key",
			RequestId:    strPtr("req-abc"),
		})
	case "throttled-key":
		return sdkErrorResponse(connect.CodeResourceExhausted, &sdkv1.SdkErrorDetails{
			SdkErrorCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_RATE_LIMIT_EXCEEDED,
			Message:      "Too many requests",
			RetryAfter:   durationpb.New(30 * time.Second),
		})
	case "wrong-model-key":
		return sdkErrorResponse(connect.CodeInvalidArgument, &sdkv1.SdkErrorDetails{
			SdkErrorCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_INVALID_MODEL,
			Message:      "claude-opus-5 is not available on your plan",
		})
	case "broken-key":
		return sdkErrorResponse(connect.CodeInternal, &sdkv1.SdkErrorDetails{
			SdkErrorCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_INTERNAL_ERROR,
			Message:      "cursor is having a bad day",
		})
	default:
		return nil
	}
}

func sdkErrorResponse(code connect.Code, details *sdkv1.SdkErrorDetails) error {
	err := connect.NewError(code, errMessage(details.GetMessage()))
	detail, errDetail := connect.NewErrorDetail(details)
	if errDetail == nil {
		err.AddDetail(detail)
	}
	return err
}

type errMessage string

func (e errMessage) Error() string { return string(e) }

func strPtr(value string) *string { return &value }

func mustStruct(fields map[string]any) *structpb.Struct {
	value, errStruct := structpb.NewStruct(fields)
	if errStruct != nil {
		panic(errStruct)
	}
	return value
}

// useFakeBridge points the bridge pool at an in-process fake service for the duration of a test,
// so the executor, model and login paths can be exercised without spawning a bridge or holding a
// Cursor credential.
func useFakeBridge(t *testing.T) *fakeBridge {
	t.Helper()
	bridge := newFakeBridge()
	useFakeBridgeService(t, bridge)
	return bridge
}

func useFakeBridgeService(t *testing.T, bridge *fakeBridge) {
	t.Helper()
	baseURL := serveFakeBridge(t, bridge)
	process := &bridgeProcess{
		workspace: t.TempDir(),
		stderr:    &tailBuffer{},
		exited:    make(chan struct{}),
	}
	if errBind := process.bindClients(bridgeDiscovery{
		SchemaVersion: bridgeDiscoverySchema,
		Transport:     "tcp",
		Protocol:      "connect",
		URL:           baseURL,
		AuthToken:     fakeBridgeToken,
	}); errBind != nil {
		t.Fatalf("bind fake bridge clients: %v", errBind)
	}
	if errCallback := startToolCallback(process); errCallback != nil {
		t.Fatalf("start tool callback: %v", errCallback)
	}

	bridgePool.mu.Lock()
	previous := bridgePool.procs
	bridgePool.procs = map[string]*bridgeProcess{"": process}
	bridgePool.mu.Unlock()

	currentConfig.Store(defaultPluginConfig())
	resetModelCatalogs()
	t.Cleanup(func() {
		failToolRunsForProcess(process)
		evictSessionsForProcess(process)
		stopToolCallback(process)
		bridgePool.mu.Lock()
		bridgePool.procs = previous
		bridgePool.mu.Unlock()
		currentConfig.Store(defaultPluginConfig())
		resetModelCatalogs()
	})
}

// resetModelCatalogs drops the per-credential catalog cache, which otherwise leaks between tests
// that expect a specific number of ListModels calls.
func resetModelCatalogs() {
	modelCatalogs.Range(func(key, _ any) bool {
		modelCatalogs.Delete(key)
		return true
	})
	discoveredCatalog.Store([]pluginapi.ModelInfo(nil))
}

func storageJSON(apiKey string) []byte {
	return []byte(`{"type":"cursor","api_key":"` + apiKey + `"}`)
}

// unauthenticatedClient builds a client that omits the bearer token, to confirm the bridge's own
// auth is what rejects it rather than something further upstream.
func unauthenticatedClient(baseURL string) sdkv1connect.SdkBridgeControlServiceClient {
	return sdkv1connect.NewSdkBridgeControlServiceClient(http.DefaultClient, strings.TrimRight(baseURL, "/"))
}
