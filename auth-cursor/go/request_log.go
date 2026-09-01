package main

import (
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	sdkv1 "github.com/UNICKCHENG/cliproxyapi-plugins/auth-cursor/go/internal/sdk/v1"
)

// requestLogErrorLimit keeps a failure message readable in main.log without letting an upstream
// error body take over the line.
const requestLogErrorLimit = 300

// requestLogContext is the per-request identity the host cannot record for a C ABI executor.
// Cursor traffic reaches the SDK bridge over Connect rather than host.http, so the host has no
// upstream HTTP call to record and the request-log file keeps only the client side. Writing the
// identity here is what makes a Cursor request as searchable in main.log as a built-in
// channel's. Host usage statistics are unaffected: they are reported through the executor
// adapter with the provider key and the selected credential.
type requestLogContext struct {
	authID       string
	authProvider string
	authLabel    string
	model        string
	stream       bool
	source       string
	requestPath  string
	started      time.Time
	ttft         time.Duration
}

// requestLogOutcome is everything only known once the run has finished.
type requestLogOutcome struct {
	latency time.Duration
	usage   *sdkv1.TokenUsage
	err     string
}

// newRequestLogContext starts timing a request. The model is passed separately because the host
// leaves ExecutorRequest.Model empty when the client named the model only in the payload.
func newRequestLogContext(req pluginapi.ExecutorRequest, model string) requestLogContext {
	return requestLogContext{
		authID:       strings.TrimSpace(req.AuthID),
		authProvider: strings.TrimSpace(req.AuthProvider),
		authLabel:    authLabel(req.StorageJSON, req.AuthID),
		model:        model,
		stream:       req.Stream,
		source:       strings.TrimSpace(req.SourceFormat),
		requestPath:  requestPathMetadata(req.Metadata),
		started:      time.Now(),
	}
}

// markFirstDelta records the time to first token, which only exists on the streaming path.
func (c *requestLogContext) markFirstDelta() {
	if c.ttft == 0 {
		c.ttft = time.Since(c.started)
	}
}

func (c *requestLogContext) completed(usage *sdkv1.TokenUsage) {
	hostLog("info", "cursor request completed", requestLogFields(*c, requestLogOutcome{
		latency: time.Since(c.started),
		usage:   usage,
	}))
}

func (c *requestLogContext) failed(message string) {
	hostLog("warn", "cursor request failed", requestLogFields(*c, requestLogOutcome{
		latency: time.Since(c.started),
		err:     message,
	}))
}

// requestLogFields names its keys after the host's UsageRecord so that a Cursor line reads like
// the built-in channels' lines. It never carries the prompt, the response or the credential.
func requestLogFields(ctx requestLogContext, outcome requestLogOutcome) map[string]any {
	fields := map[string]any{
		"auth_id":    ctx.authID,
		"auth_label": ctx.authLabel,
		"model":      ctx.model,
		"stream":     ctx.stream,
		"latency_ms": outcome.latency.Milliseconds(),
	}
	if ctx.authProvider != "" {
		fields["auth_provider"] = ctx.authProvider
	}
	if ctx.source != "" {
		fields["source"] = ctx.source
	}
	if ctx.requestPath != "" {
		fields["request_path"] = ctx.requestPath
	}
	if ctx.ttft > 0 {
		fields["ttft_ms"] = ctx.ttft.Milliseconds()
	}
	if usage := convertUsage(outcome.usage); usage != nil {
		fields["input_tokens"] = usage.PromptTokens
		fields["output_tokens"] = usage.CompletionTokens
		fields["total_tokens"] = usage.TotalTokens
	}
	if outcome.err != "" {
		fields["failed"] = true
		fields["error"] = truncateForLog(outcome.err, requestLogErrorLimit)
	}
	return fields
}

// truncateForLog cuts on rune boundaries so a clipped non-ASCII message stays valid UTF-8.
func truncateForLog(message string, limit int) string {
	message = strings.TrimSpace(message)
	runes := []rune(message)
	if len(runes) <= limit {
		return message
	}
	return string(runes[:limit]) + "..."
}
