package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	sdkv1 "github.com/UNICKCHENG/cliproxyapi-plugins/auth-cursor/go/internal/sdk/v1"
)

var (
	toolBatchWindow   = 50 * time.Millisecond
	toolResultTimeout = 10 * time.Minute
)

const toolResultMissing = "tool result was not delivered"

var errToolResultTimeout = errors.New(toolResultMissing)

var toolRegistry = struct {
	mu              sync.Mutex
	byAgentID       map[string]*toolRun
	byExposedCallID map[string]*toolRun
}{
	byAgentID:       make(map[string]*toolRun),
	byExposedCallID: make(map[string]*toolRun),
}

type toolCallbackServer struct{}

type toolRun struct {
	mu sync.Mutex

	process     *bridgeProcess
	fingerprint string
	apiKey      string
	model       string
	agentID     string
	runID       string

	ctx    context.Context
	cancel context.CancelFunc
	stream *connect.ServerStreamForClient[sdkv1.RunStreamMessage]

	toolNames map[string]struct{}
	text      strings.Builder
	calls     []*toolCallRecord
	announced []*toolCallRecord

	batchTimer  *time.Timer
	resultTimer *time.Timer

	waiter *turnWaiter

	finishOnce sync.Once
	finished   bool
}

type toolCallRecord struct {
	exposedID string
	name      string
	arguments string
	reply     chan string
	announced bool
}

type turnWaiter struct {
	onDelta func(string) error
	done    chan toolTurnResult
}

type toolTurnResult struct {
	result generateResult
	err    error
}

func tenantFingerprint(apiKey, proxyURL string) string {
	sum := sha256.Sum256([]byte(apiKey + "\n" + proxyURL))
	return hex.EncodeToString(sum[:])
}

func newExposedCallID() string {
	buf := make([]byte, 16)
	if _, errRead := rand.Read(buf); errRead != nil {
		return fmt.Sprintf("call_%d", time.Now().UnixNano())
	}
	return "call_" + hex.EncodeToString(buf)
}

func toolRequestError(message string) error {
	return &upstreamError{failure: requestFault("invalid_tool_results", message)}
}

func startToolRun(ctx context.Context, req generateRequest, onDelta func(string) error) (generateResult, error) {
	process, errProcess := acquireBridge(req.proxyURL)
	if errProcess != nil {
		return generateResult{}, errProcess
	}
	images, errImages := buildSdkImages(ctx, req.images, downloadImage)
	if errImages != nil {
		return generateResult{}, errImages
	}
	agentID, errCreate := createAgent(ctx, process, req)
	if errCreate != nil {
		return generateResult{}, errCreate
	}

	runCtx, cancel := context.WithCancel(context.Background())
	run := &toolRun{
		process:     process,
		fingerprint: tenantFingerprint(req.apiKey, req.proxyURL),
		apiKey:      req.apiKey,
		model:       req.model,
		agentID:     agentID,
		ctx:         runCtx,
		cancel:      cancel,
		toolNames:   toolNameSet(req.tools),
	}
	registerToolRun(run)

	waiter, errWaiter := run.attachWaiter(onDelta)
	if errWaiter != nil {
		run.abort(errWaiter)
		return generateResult{}, errWaiter
	}

	stream, errSend := openSend(runCtx, process, agentID, req, images, true)
	if errSend != nil {
		run.abort(errSend)
		return generateResult{}, errSend
	}
	run.mu.Lock()
	run.stream = stream
	run.mu.Unlock()
	go run.readStream()
	return run.waitOn(ctx, waiter)
}
func resumeToolRun(ctx context.Context, req generateRequest, onDelta func(string) error) (generateResult, bool, error) {
	run, resumed, errLookup := lookupResumeRun(req)
	if !resumed {
		return generateResult{}, false, nil
	}
	if errLookup != nil {
		return generateResult{}, true, errLookup
	}
	waiter, errWaiter := run.attachWaiter(onDelta)
	if errWaiter != nil {
		return generateResult{}, true, errWaiter
	}
	if errDeliver := run.deliver(req.toolResults); errDeliver != nil {
		run.detachWaiter(waiter)
		return generateResult{}, true, errDeliver
	}
	result, errWait := run.waitOn(ctx, waiter)
	return result, true, errWait
}

func lookupResumeRun(req generateRequest) (*toolRun, bool, error) {
	ids := make([]string, 0, len(req.toolResults))
	seen := make(map[string]struct{}, len(req.toolResults))
	for _, result := range req.toolResults {
		id := strings.TrimSpace(result.CallID)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			return nil, true, toolRequestError("duplicate tool_call_id")
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, false, nil
	}

	fingerprint := tenantFingerprint(req.apiKey, req.proxyURL)
	toolRegistry.mu.Lock()
	var run *toolRun
	hits := 0
	for _, id := range ids {
		found := toolRegistry.byExposedCallID[id]
		if found == nil {
			continue
		}
		hits++
		if run == nil {
			run = found
			continue
		}
		if run != found {
			toolRegistry.mu.Unlock()
			return nil, true, toolRequestError("tool results mix multiple runs")
		}
	}
	toolRegistry.mu.Unlock()

	if hits == 0 {
		return nil, false, nil
	}
	if hits != len(ids) {
		return nil, true, toolRequestError("partial tool result resume")
	}
	if run.fingerprint != fingerprint {
		return nil, true, toolRequestError("tool results do not match this credential")
	}
	return run, true, nil
}

func registerToolRun(run *toolRun) {
	toolRegistry.mu.Lock()
	toolRegistry.byAgentID[run.agentID] = run
	toolRegistry.mu.Unlock()
}

func toolNameSet(tools map[string]*sdkv1.CustomToolDefinition) map[string]struct{} {
	names := make(map[string]struct{}, len(tools))
	for name := range tools {
		names[name] = struct{}{}
	}
	return names
}

func (r *toolRun) attachWaiter(onDelta func(string) error) (*turnWaiter, error) {
	waiter := &turnWaiter{onDelta: onDelta, done: make(chan toolTurnResult, 1)}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return nil, toolRequestError("tool run is no longer active")
	}
	if r.waiter != nil {
		return nil, toolRequestError("concurrent tool continuation")
	}
	r.waiter = waiter
	if len(r.announced) == 0 {
		r.scheduleAnnounceLocked()
	}
	return waiter, nil
}

func (r *toolRun) detachWaiter(waiter *turnWaiter) {
	r.mu.Lock()
	if r.waiter == waiter {
		r.waiter = nil
	}
	r.mu.Unlock()
}

func (r *toolRun) waitOn(ctx context.Context, waiter *turnWaiter) (generateResult, error) {
	select {
	case out := <-waiter.done:
		return out.result, out.err
	case <-ctx.Done():
		r.mu.Lock()
		if r.waiter != waiter {
			r.mu.Unlock()
			out := <-waiter.done
			return out.result, out.err
		}
		r.waiter = nil
		r.mu.Unlock()
		r.abort(ctx.Err())
		return generateResult{}, ctx.Err()
	}
}

func (r *toolRun) scheduleAnnounceLocked() {
	if r.finished || r.waiter == nil || len(r.announced) > 0 {
		return
	}
	hasUnannounced := false
	for _, rec := range r.calls {
		if !rec.announced {
			hasUnannounced = true
			break
		}
	}
	if !hasUnannounced {
		return
	}
	if r.batchTimer != nil {
		r.batchTimer.Stop()
	}
	r.batchTimer = time.AfterFunc(toolBatchWindow, r.announceBatch)
}

func (r *toolRun) announceBatch() {
	r.mu.Lock()
	if r.finished || r.waiter == nil || len(r.announced) > 0 {
		r.mu.Unlock()
		return
	}
	var batch []*toolCallRecord
	for _, rec := range r.calls {
		if rec.announced {
			continue
		}
		rec.announced = true
		batch = append(batch, rec)
	}
	if len(batch) == 0 {
		r.mu.Unlock()
		return
	}
	r.announced = batch
	text := r.text.String()
	r.text.Reset()
	waiter := r.waiter
	r.waiter = nil
	if r.batchTimer != nil {
		r.batchTimer.Stop()
		r.batchTimer = nil
	}
	if r.resultTimer != nil {
		r.resultTimer.Stop()
	}
	r.resultTimer = time.AfterFunc(toolResultTimeout, func() {
		r.abort(errToolResultTimeout)
	})
	r.mu.Unlock()

	toolRegistry.mu.Lock()
	for _, rec := range batch {
		toolRegistry.byExposedCallID[rec.exposedID] = r
	}
	toolRegistry.mu.Unlock()

	calls := make([]chatToolCall, len(batch))
	for i, rec := range batch {
		calls[i] = chatToolCall{
			ID:   rec.exposedID,
			Type: "function",
			Function: chatToolCallFunction{
				Name:      rec.name,
				Arguments: rec.arguments,
			},
		}
	}
	waiter.done <- toolTurnResult{result: generateResult{text: text, toolCalls: calls}}
}

func (r *toolRun) deliver(results []chatToolResult) error {
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return toolRequestError("tool run is no longer active")
	}
	if len(r.announced) == 0 {
		r.mu.Unlock()
		return toolRequestError("tool results are not currently announced")
	}
	byID := make(map[string]*toolCallRecord, len(r.announced))
	for _, rec := range r.announced {
		byID[rec.exposedID] = rec
	}
	if len(results) != len(r.announced) {
		r.mu.Unlock()
		return toolRequestError("incomplete tool result batch")
	}
	type delivery struct {
		ch      chan string
		content string
	}
	delivered := make([]delivery, 0, len(results))
	seen := make(map[string]struct{}, len(results))
	for _, result := range results {
		id := strings.TrimSpace(result.CallID)
		if _, dup := seen[id]; dup {
			r.mu.Unlock()
			return toolRequestError("duplicate tool_call_id")
		}
		seen[id] = struct{}{}
		rec := byID[id]
		if rec == nil {
			r.mu.Unlock()
			return toolRequestError("tool results are not currently announced")
		}
		delivered = append(delivered, delivery{ch: rec.reply, content: result.Content})
	}
	announced := r.announced
	r.announced = nil
	if r.resultTimer != nil {
		r.resultTimer.Stop()
		r.resultTimer = nil
	}
	r.scheduleAnnounceLocked()
	r.mu.Unlock()

	toolRegistry.mu.Lock()
	for _, rec := range announced {
		if toolRegistry.byExposedCallID[rec.exposedID] == r {
			delete(toolRegistry.byExposedCallID, rec.exposedID)
		}
	}
	toolRegistry.mu.Unlock()

	for _, item := range delivered {
		select {
		case item.ch <- item.content:
		default:
		}
	}
	return nil
}

func (r *toolRun) readStream() {
	var (
		statusMessage string
		result        *sdkv1.RunStreamResult
	)
	for r.stream.Receive() {
		message := r.stream.Msg()
		if observed := runIDFrom(message); observed != "" {
			r.mu.Lock()
			if r.runID == "" {
				r.runID = observed
			}
			r.mu.Unlock()
		}
		switch {
		case message.GetSdkMessage() != nil:
			if reason := statusMessageFrom(message.GetSdkMessage()); reason != "" {
				statusMessage = reason
			}
		case message.GetInteractionUpdate() != nil:
			delta := textDeltaFrom(message.GetInteractionUpdate())
			if delta == "" {
				continue
			}
			r.mu.Lock()
			r.text.WriteString(delta)
			var onDelta func(string) error
			if r.waiter != nil {
				onDelta = r.waiter.onDelta
			}
			r.mu.Unlock()
			if onDelta == nil {
				continue
			}
			if errDelta := onDelta(delta); errDelta != nil {
				r.abort(errDelta)
				return
			}
		case message.GetResult() != nil:
			result = message.GetResult()
		case message.GetDone() != nil:
		default:
		}
	}
	if errStream := r.stream.Err(); errStream != nil {
		if result == nil {
			r.abort(errStream)
			return
		}
	}
	if result == nil {
		r.abort(errors.New("cursor sdk bridge closed the run stream before it completed"))
		return
	}
	if result.GetStatus() != sdkv1.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED {
		r.abort(&upstreamError{failure: runFailureFromResult(result, statusMessage)})
		return
	}
	r.mu.Lock()
	turnText := r.text.String()
	r.mu.Unlock()
	final := result.GetResult().GetResult()
	if turnText != "" {
		final = turnText
	}
	r.complete(generateResult{text: final, usage: result.GetResult().GetUsage()})
}

func (r *toolRun) abort(err error) {
	r.finishOnce.Do(func() {
		r.teardown(true, generateResult{}, err)
	})
}

func (r *toolRun) complete(result generateResult) {
	r.finishOnce.Do(func() {
		r.teardown(false, result, nil)
	})
}

func (r *toolRun) teardown(abort bool, result generateResult, err error) {
	r.mu.Lock()
	r.finished = true
	waiter := r.waiter
	r.waiter = nil
	if r.batchTimer != nil {
		r.batchTimer.Stop()
		r.batchTimer = nil
	}
	if r.resultTimer != nil {
		r.resultTimer.Stop()
		r.resultTimer = nil
	}
	cancel := r.cancel
	stream := r.stream
	process := r.process
	agentID := r.agentID
	runID := r.runID
	apiKey := r.apiKey
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	r.unregister()
	if abort {
		cancelRun(process, agentID, runID)
	}
	if stream != nil {
		_ = stream.Close()
	}
	releaseAgent(process, agentID, apiKey)
	if waiter != nil {
		waiter.done <- toolTurnResult{result: result, err: err}
	}
}

func (r *toolRun) unregister() {
	r.mu.Lock()
	agentID := r.agentID
	ids := make([]string, 0, len(r.calls))
	for _, rec := range r.calls {
		ids = append(ids, rec.exposedID)
	}
	r.mu.Unlock()

	toolRegistry.mu.Lock()
	if toolRegistry.byAgentID[agentID] == r {
		delete(toolRegistry.byAgentID, agentID)
	}
	for _, id := range ids {
		if toolRegistry.byExposedCallID[id] == r {
			delete(toolRegistry.byExposedCallID, id)
		}
	}
	toolRegistry.mu.Unlock()
}

func abortParkedToolRuns(err error) {
	if err == nil {
		err = errors.New("tool run aborted")
	}
	var runs []*toolRun
	toolRegistry.mu.Lock()
	for _, run := range toolRegistry.byAgentID {
		runs = append(runs, run)
	}
	toolRegistry.mu.Unlock()
	for _, run := range runs {
		run.abort(err)
	}
}

func failToolRunsForProcess(p *bridgeProcess) {
	if p == nil {
		return
	}
	var runs []*toolRun
	toolRegistry.mu.Lock()
	for _, run := range toolRegistry.byAgentID {
		if run.process == p {
			runs = append(runs, run)
		}
	}
	toolRegistry.mu.Unlock()
	for _, run := range runs {
		run.abort(errors.New("cursor sdk bridge stopped"))
	}
}

func (toolCallbackServer) CallCustomTool(ctx context.Context, req *connect.Request[sdkv1.CallCustomToolRequest]) (*connect.Response[sdkv1.CallCustomToolResponse], error) {
	agentID := strings.TrimSpace(req.Msg.GetAgentId())
	name := strings.TrimSpace(req.Msg.GetToolName())

	toolRegistry.mu.Lock()
	run := toolRegistry.byAgentID[agentID]
	toolRegistry.mu.Unlock()
	if run == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("unknown agent_id"))
	}

	arguments, errArgs := marshalToolArgs(req.Msg.GetArgs())
	if errArgs != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errArgs)
	}

	rec := &toolCallRecord{
		exposedID: newExposedCallID(),
		name:      name,
		arguments: arguments,
		reply:     make(chan string, 1),
	}

	run.mu.Lock()
	if run.finished {
		run.mu.Unlock()
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("unknown agent_id"))
	}
	if _, ok := run.toolNames[name]; !ok {
		run.mu.Unlock()
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("undeclared tool %q", name))
	}
	run.calls = append(run.calls, rec)
	run.scheduleAnnounceLocked()
	run.mu.Unlock()

	select {
	case content := <-rec.reply:
		return connect.NewResponse(&sdkv1.CallCustomToolResponse{Result: customToolResult(content, false)}), nil
	case <-ctx.Done():
		run.abort(ctx.Err())
		return connect.NewResponse(&sdkv1.CallCustomToolResponse{Result: customToolResult(toolResultMissing, true)}), nil
	case <-run.ctx.Done():
		return connect.NewResponse(&sdkv1.CallCustomToolResponse{Result: customToolResult(toolResultMissing, true)}), nil
	}
}

func marshalToolArgs(args *structpb.Struct) (string, error) {
	if args == nil {
		return "{}", nil
	}
	raw, errMarshal := json.Marshal(args.AsMap())
	if errMarshal != nil {
		return "", errMarshal
	}
	if string(raw) == "null" {
		return "{}", nil
	}
	return string(raw), nil
}

func customToolResult(text string, isError bool) *structpb.Struct {
	fields := map[string]any{
		"content": []any{
			map[string]any{"type": "text", "text": text},
		},
	}
	if isError {
		fields["isError"] = true
	}
	value, errStruct := structpb.NewStruct(fields)
	if errStruct != nil {
		fallback, _ := structpb.NewStruct(map[string]any{"content": []any{}})
		return fallback
	}
	return value
}
