package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	sdkv1 "github.com/UNICKCHENG/cliproxyapi-plugins/auth-cursor/go/internal/sdk/v1"
)

// textDeltaUpdate is the InteractionUpdate discriminator that carries incremental output.
const textDeltaUpdate = "text-delta"

// agentCleanupTimeout bounds the cancellation and teardown calls made after a run, which run on
// their own context because the request context is usually already cancelled by then.
const agentCleanupTimeout = 15 * time.Second

// generateRequest is one inference request, already resolved down to what the bridge needs.
type generateRequest struct {
	apiKey           string
	proxyURL         string
	model            string
	params           []modelParam
	optimizeFor      string
	prompt           string
	images           []chatImage
	tools            map[string]*sdkv1.CustomToolDefinition
	toolResults      []chatToolResult
	sessionPrefix    string
	sessionIncrement string
	incrementImages  []chatImage
}

// generateResult is the outcome of a completed run or one parked tool-call turn.
type generateResult struct {
	text      string
	usage     *sdkv1.TokenUsage
	toolCalls []chatToolCall
}

// runGenerate drives a local agent through one HTTP turn.
//
// Text-only requests reuse a live agent when the conversation prefix matches; otherwise they
// create one and keep it for the next turn. Tool-enabled requests keep the agent and Send stream
// alive across HTTP turns: CallCustomTool is parked until the next request delivers role:tool
// results. onDelta is optional; when it is nil a text-only run does not ask for deltas.
// Tool-enabled runs always enable deltas so text before a tool call is captured. Returning an
// error from onDelta aborts the run.
//
// No deadline is applied: once the run is live upstream, the plugin must not impose its own
// timeout on the response.
func runGenerate(ctx context.Context, req generateRequest, onDelta func(string) error) (generateResult, error) {
	if len(req.toolResults) > 0 {
		result, resumed, errResume := resumeToolRun(ctx, req, onDelta)
		if resumed {
			return result, errResume
		}
	}
	if len(req.tools) == 0 {
		return runOneShot(ctx, req, onDelta)
	}
	return startToolRun(ctx, req, onDelta)
}

func runOneShot(ctx context.Context, req generateRequest, onDelta func(string) error) (generateResult, error) {
	process, errProcess := acquireBridge(req.proxyURL)
	if errProcess != nil {
		return generateResult{}, errProcess
	}

	if entry, increment, incrementImages, ok := occupySession(req); ok {
		if errBudget := checkPromptBudget(increment); errBudget != nil {
			releaseSession(entry, true)
			return generateResult{}, errBudget
		}
		images, errImages := buildSdkImages(ctx, incrementImages, downloadImage)
		if errImages != nil {
			releaseSession(entry, false)
			return generateResult{}, errImages
		}
		incrementReq := req
		incrementReq.prompt = increment
		result, errSend := sendAndConsume(ctx, entry.process, entry.agentID, incrementReq, images, onDelta)
		if errSend != nil {
			releaseSession(entry, false)
			return generateResult{}, errSend
		}
		rekeySession(entry, req, result.text)
		return result, nil
	}

	if errBudget := checkPromptBudget(req.prompt); errBudget != nil {
		return generateResult{}, errBudget
	}
	images, errImages := buildSdkImages(ctx, req.images, downloadImage)
	if errImages != nil {
		return generateResult{}, errImages
	}

	agentID, errCreate := createAgent(ctx, process, req)
	if errCreate != nil {
		return generateResult{}, errCreate
	}
	result, errRun := sendAndConsume(ctx, process, agentID, req, images, onDelta)
	if errRun != nil {
		releaseAgent(process, agentID, req.apiKey)
		return generateResult{}, errRun
	}
	if !rememberSession(req, process, agentID, result.text) {
		releaseAgent(process, agentID, req.apiKey)
	}
	return result, nil
}

func sendAndConsume(ctx context.Context, process *bridgeProcess, agentID string, req generateRequest, images []*sdkv1.SdkImage, onDelta func(string) error) (generateResult, error) {
	stream, errSend := openSend(ctx, process, agentID, req, images, onDelta != nil)
	if errSend != nil && isAgentNotFound(errSend) {
		if errResume := resumeParkedAgent(ctx, process, agentID, req); errResume == nil {
			stream, errSend = openSend(ctx, process, agentID, req, images, onDelta != nil)
		}
	}
	if errSend != nil {
		return generateResult{}, errSend
	}
	defer stream.Close()
	return consumeRunStream(process, agentID, stream, onDelta)
}

// promptMaxRunes is a conservative cap on the text handed to a single Send. The Agent harness
// adds its own system prompt and tool schema on top, and sdk.v1 exposes no context-window field
// to read, so this is a request-scoped guard rather than a model-accurate budget.
var promptMaxRunes = 80 * 1024

func checkPromptBudget(prompt string) error {
	if len([]rune(prompt)) <= promptMaxRunes {
		return nil
	}
	return &upstreamError{failure: requestFault("context_length_exceeded",
		"cursor prompt exceeds the plugin's send budget; shorten the conversation or switch to a native channel")}
}

func createAgent(ctx context.Context, process *bridgeProcess, req generateRequest) (string, error) {
	// Catalog failures are advisory: an unknown model is forwarded as-is and Cursor decides.
	models, errCatalog := catalogFor(ctx, process, req.apiKey, false)
	if errCatalog != nil {
		hostLog("debug", "cursor model catalog unavailable, forwarding the model as requested", map[string]any{
			"model": req.model,
			"error": errCatalog.Error(),
		})
	}
	selection := resolveModelSelection(models, req.model, req.params, req.optimizeFor)

	response, errCreate := process.agent.CreateAgent(ctx, connect.NewRequest(&sdkv1.CreateAgentRequest{
		Options: &sdkv1.AgentOptions{
			Model: selection,
			// The key is set per call rather than in the bridge environment: one bridge serves
			// every credential that shares its proxy.
			ApiKey: req.apiKey,
			Local:  localAgentOptions(process, req.tools),
			Tools:  agentToolList(req.tools),
		},
	}))
	if errCreate != nil {
		return "", errCreate
	}
	agentID := strings.TrimSpace(response.Msg.GetAgentId())
	if agentID == "" {
		return "", errors.New("cursor sdk bridge created an agent without an id")
	}
	return agentID, nil
}

func localAgentOptions(process *bridgeProcess, tools map[string]*sdkv1.CustomToolDefinition) *sdkv1.LocalAgentOptions {
	local := &sdkv1.LocalAgentOptions{Cwd: []string{process.workspace}}
	if len(tools) > 0 {
		local.CustomTools = maps.Clone(tools)
	}
	return local
}

// agentToolList keeps Cursor built-ins (shell, edit, search) off. An empty list is required
// for text-only runs: an unset Tools field would load the default toolset against the host.
// Custom tools are registered as the MCP server "custom-user-tools", so a tool-enabled run
// must allowlist the "mcp" capability group or the agent reports a tool-discovery failure.
func agentToolList(tools map[string]*sdkv1.CustomToolDefinition) *sdkv1.ToolList {
	if len(tools) == 0 {
		return &sdkv1.ToolList{}
	}
	return &sdkv1.ToolList{Names: []string{"mcp"}}
}

func openSend(ctx context.Context, process *bridgeProcess, agentID string, req generateRequest, images []*sdkv1.SdkImage, deltas bool) (*connect.ServerStreamForClient[sdkv1.RunStreamMessage], error) {
	return process.agent.Send(ctx, connect.NewRequest(&sdkv1.SendRequest{
		AgentId: agentID,
		Message: &sdkv1.UserMessage{Text: req.prompt, Images: images},
		Options: &sdkv1.SendOptions{EnableDeltas: deltas},
	}))
}

func resumeParkedAgent(ctx context.Context, process *bridgeProcess, agentID string, req generateRequest) error {
	models, _ := catalogFor(ctx, process, req.apiKey, false)
	_, errResume := process.agent.ResumeAgent(ctx, connect.NewRequest(&sdkv1.ResumeAgentRequest{
		AgentId: agentID,
		Options: &sdkv1.AgentOptions{
			Model:  resolveModelSelection(models, req.model, req.params, req.optimizeFor),
			ApiKey: req.apiKey,
			Local:  localAgentOptions(process, req.tools),
			Tools:  agentToolList(req.tools),
		},
	}))
	return errResume
}

func isAgentNotFound(err error) bool {
	details, ok := sdkErrorDetails(err)
	return ok && details.GetSdkErrorCode() == sdkv1.SdkErrorCode_SDK_ERROR_CODE_AGENT_NOT_FOUND
}

// consumeRunStream reads a Send stream to its terminal result.
//
// Every path that leaves without a terminal result cancels the run first, because losing the
// stream does not stop it: the bridge keeps it executing and Cursor keeps billing it. The
// cancellation is issued here rather than from a watcher goroutine so that it reaches the
// bridge before the agent teardown that follows this call.
func consumeRunStream(
	process *bridgeProcess,
	agentID string,
	stream *connect.ServerStreamForClient[sdkv1.RunStreamMessage],
	onDelta func(string) error,
) (generateResult, error) {
	var (
		text          strings.Builder
		result        *sdkv1.RunStreamResult
		statusMessage string
		runID         string
	)
	for stream.Receive() {
		message := stream.Msg()
		if observed := runIDFrom(message); observed != "" && runID == "" {
			runID = observed
		}
		switch {
		case message.GetSdkMessage() != nil:
			// A failed run reports its reason here rather than in the terminal envelope, whose
			// error_code is often empty, so the last status message is kept for the error path.
			if reason := statusMessageFrom(message.GetSdkMessage()); reason != "" {
				statusMessage = reason
			}
		case message.GetInteractionUpdate() != nil:
			delta := textDeltaFrom(message.GetInteractionUpdate())
			if delta == "" || onDelta == nil {
				continue
			}
			text.WriteString(delta)
			if errDelta := onDelta(delta); errDelta != nil {
				// Nobody is reading the output any more, so stop the run rather than let it
				// finish and bill for work that is being discarded.
				cancelRun(process, agentID, runID)
				return generateResult{}, errDelta
			}
		case message.GetResult() != nil:
			result = message.GetResult()
		case message.GetDone() != nil:
			// The last message on a healthy stream.
		default:
			// Keepalives arrive as an envelope with no case set, and sdk.v1 may grow cases this
			// build does not know. Both are no-ops.
		}
	}
	if errStream := stream.Err(); errStream != nil {
		// A cancelled request is the common case here, but a broken stream is no different:
		// either way the answer is lost, so the run should not keep running.
		if result == nil {
			cancelRun(process, agentID, runID)
		}
		return generateResult{}, errStream
	}
	if result == nil {
		return generateResult{}, errors.New("cursor sdk bridge closed the run stream before it completed")
	}
	if result.GetStatus() != sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED {
		return generateResult{}, &upstreamError{failure: runFailureFromResult(result, statusMessage)}
	}
	final := result.GetResult().GetResult()
	if final == "" {
		final = text.String()
	}
	return generateResult{text: final, usage: result.GetResult().GetUsage()}, nil
}

// runFailureMessage picks the most specific description of a failed run.
func runFailureMessage(result *sdkv1.RunStreamResult, statusMessage string) string {
	if statusMessage != "" {
		return statusMessage
	}
	if code := strings.TrimSpace(result.GetErrorCode()); code != "" {
		return fmt.Sprintf("cursor run %s: %s", strings.ToLower(strings.TrimPrefix(result.GetStatus().String(), "RUN_LIFECYCLE_STATUS_")), code)
	}
	return fmt.Sprintf("cursor run ended with status %s", result.GetStatus().String())
}

// textDeltaFrom extracts the incremental text from a streaming update, ignoring every other kind.
func textDeltaFrom(update *sdkv1.InteractionUpdate) string {
	if update.GetType() != textDeltaUpdate {
		return ""
	}
	return structString(update.GetUpdate(), "text")
}

// statusMessageFrom reads the human-readable reason from a status message payload.
func statusMessageFrom(message *sdkv1.SdkMessage) string {
	if message.GetType() != "status" {
		return ""
	}
	return strings.TrimSpace(structString(message.GetMessage(), "message"))
}

// runIDFrom finds the run id wherever the stream happens to carry it. The run typically opens
// with a system/init sdk_message whose payload holds it, and the terminal envelopes repeat it.
func runIDFrom(message *sdkv1.RunStreamMessage) string {
	if result := message.GetResult(); result != nil {
		if runID := strings.TrimSpace(result.GetRunId()); runID != "" {
			return runID
		}
	}
	if done := message.GetDone(); done != nil {
		if runID := strings.TrimSpace(done.GetRunId()); runID != "" {
			return runID
		}
	}
	if sdkMessage := message.GetSdkMessage(); sdkMessage != nil {
		for _, field := range []string{"run_id", "runId"} {
			if runID := strings.TrimSpace(structString(sdkMessage.GetMessage(), field)); runID != "" {
				return runID
			}
		}
	}
	return ""
}

func structString(value *structpb.Struct, field string) string {
	if value == nil {
		return ""
	}
	entry, present := value.GetFields()[field]
	if !present {
		return ""
	}
	if text, ok := entry.GetKind().(*structpb.Value_StringValue); ok {
		return text.StringValue
	}
	return ""
}

// cancelRun stops a run nobody is going to read.
//
// It is best effort and runs on its own context: the request context is usually what caused the
// cancellation in the first place, and the request is over either way.
func cancelRun(process *bridgeProcess, agentID, runID string) {
	if runID == "" {
		// The run id arrives on the stream, so a run abandoned before its first message cannot
		// be cancelled by id. The agent teardown that follows is the only stop signal left.
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), agentCleanupTimeout)
	defer cancel()
	if _, errCancel := process.agent.CancelRun(ctx, connect.NewRequest(&sdkv1.CancelRunRequest{
		RunId:   runID,
		AgentId: &agentID,
	})); errCancel != nil {
		hostLog("warn", "cursor run could not be cancelled", map[string]any{
			"run_id": runID,
			"error":  errCancel.Error(),
		})
	}
}

// releaseAgent tears down a one-shot agent. Both calls are best effort: the request has already
// been answered by the time they run, and a failure here must not change its outcome.
func releaseAgent(process *bridgeProcess, agentID, apiKey string) {
	ctx, cancel := context.WithTimeout(context.Background(), agentCleanupTimeout)
	defer cancel()
	if _, errClose := process.agent.CloseAgent(ctx, connect.NewRequest(&sdkv1.CloseAgentRequest{AgentId: agentID})); errClose != nil {
		hostLog("debug", "cursor agent could not be closed", map[string]any{"error": errClose.Error()})
	}
	if _, errDelete := process.agent.DeleteAgent(ctx, connect.NewRequest(&sdkv1.DeleteAgentRequest{
		AgentId: agentID,
		Options: &sdkv1.AgentOperationOptions{Cwd: process.workspace, ApiKey: apiKey},
	})); errDelete != nil {
		hostLog("debug", "cursor agent state could not be deleted", map[string]any{"error": errDelete.Error()})
	}
}
