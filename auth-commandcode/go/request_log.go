package main

import (
	"context"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const requestLogErrorLimit = 300

type requestLogContext struct {
	ctx          context.Context
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

func newRequestLogContext(ctx context.Context, req pluginapi.ExecutorRequest, model string) requestLogContext {
	return requestLogContext{
		ctx:          ctx,
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

func (c *requestLogContext) markFirstDelta() {
	if c.ttft == 0 {
		c.ttft = time.Since(c.started)
	}
}

func (c *requestLogContext) completed(usage *chatCompletionUsage) {
	hostLogContext(c.ctx, "info", "commandcode request completed", requestLogFields(*c, usage, ""))
}

func (c *requestLogContext) failed(message string) {
	hostLogContext(c.ctx, "warn", "commandcode request failed", requestLogFields(*c, nil, message))
}

func requestLogFields(ctx requestLogContext, usage *chatCompletionUsage, err string) map[string]any {
	fields := map[string]any{
		"auth_id":    ctx.authID,
		"auth_label": ctx.authLabel,
		"model":      ctx.model,
		"stream":     ctx.stream,
		"latency_ms": time.Since(ctx.started).Milliseconds(),
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
	if usage != nil {
		fields["input_tokens"] = usage.PromptTokens
		fields["output_tokens"] = usage.CompletionTokens
		fields["total_tokens"] = usage.TotalTokens
	}
	if err != "" {
		fields["failed"] = true
		fields["error"] = truncateForLog(err, requestLogErrorLimit)
	}
	return fields
}

func truncateForLog(message string, limit int) string {
	message = strings.TrimSpace(message)
	runes := []rune(message)
	if len(runes) <= limit {
		return message
	}
	return string(runes[:limit]) + "..."
}
