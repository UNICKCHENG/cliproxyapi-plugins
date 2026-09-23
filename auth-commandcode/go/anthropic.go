package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

type anthropicRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	Messages      []anthropicMsg  `json:"messages"`
	System        any             `json:"system,omitempty"`
	Tools         []anthropicTool `json:"tools,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
}

type anthropicMsg struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type anthropicTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema"`
}

type anthropicContent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   any             `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Source    *anthropicImage `json:"source,omitempty"`
}

type anthropicImage struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
}

func executeAnthropic(ctx context.Context, call executorCall) ([]byte, *chatCompletionUsage, error) {
	body, errConvert := chatToAnthropic(ctx, call.payload, call.model, call.proxyURL, false)
	if errConvert != nil {
		return nil, nil, errConvert
	}
	endpoint, errURL := providerEndpoint(loadedConfig().APIBase, "messages")
	if errURL != nil {
		return nil, nil, errURL
	}
	status, _, raw, errDo := doJSON(ctx, upstreamCall{
		Method:    http.MethodPost,
		URL:       endpoint,
		APIKey:    call.apiKey,
		ProxyURL:  call.proxyURL,
		Body:      body,
		Timeout:   nonStreamTimeout,
		ZDR:       call.zdr,
		ClientZDR: call.clientZDR,
	})
	if errDo != nil {
		return nil, nil, errDo
	}
	if status < 200 || status > 299 {
		return nil, nil, &upstreamError{failure: classifyHTTPStatus(status, raw)}
	}
	payload, usage, errResp := anthropicToChatCompletion(raw, call.model)
	if errResp != nil {
		return nil, nil, errResp
	}
	return payload, usage, nil
}

func streamAnthropic(ctx context.Context, call executorCall, emit func([]byte) error) (*chatCompletionUsage, error) {
	body, errConvert := chatToAnthropic(ctx, call.payload, call.model, call.proxyURL, true)
	if errConvert != nil {
		return nil, errConvert
	}
	endpoint, errURL := providerEndpoint(loadedConfig().APIBase, "messages")
	if errURL != nil {
		return nil, errURL
	}
	resp, errDo := doHTTP(ctx, upstreamCall{
		Method:    http.MethodPost,
		URL:       endpoint,
		APIKey:    call.apiKey,
		ProxyURL:  call.proxyURL,
		Body:      body,
		Accept:    "text/event-stream",
		ZDR:       call.zdr,
		ClientZDR: call.clientZDR,
		Stream:    true,
	})
	if errDo != nil {
		return nil, errDo
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, errorSummaryLimit+1))
		return nil, &upstreamError{failure: classifyHTTPStatus(resp.StatusCode, raw)}
	}
	return forwardAnthropicSSE(resp.Body, call.model, call.framing, emit)
}

func chatToAnthropic(ctx context.Context, payload []byte, model, proxyURL string, stream bool) ([]byte, error) {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return nil, requestFault("request payload is not valid JSON")
	}
	messagesNode := gjson.GetBytes(payload, "messages")
	if !messagesNode.IsArray() {
		return nil, requestFault("request must include a messages array")
	}
	maxTokens, errTokens := anthropicMaxTokens(payload)
	if errTokens != nil {
		return nil, errTokens
	}
	req := anthropicRequest{
		Model:     model,
		MaxTokens: maxTokens,
		Stream:    stream,
	}
	if node := gjson.GetBytes(payload, "temperature"); node.Exists() && node.Type == gjson.Number {
		value := node.Float()
		req.Temperature = &value
	}
	if node := gjson.GetBytes(payload, "top_p"); node.Exists() && node.Type == gjson.Number {
		value := node.Float()
		req.TopP = &value
	}
	if stops := parseStopSequences(payload); len(stops) > 0 {
		req.StopSequences = stops
	}
	tools, errTools := convertTools(payload)
	if errTools != nil {
		return nil, errTools
	}
	req.Tools = tools

	var system []anthropicContent
	var out []anthropicMsg
	for _, item := range messagesNode.Array() {
		role := strings.ToLower(strings.TrimSpace(item.Get("role").String()))
		switch role {
		case "system":
			blocks, errBlocks := convertUserContent(ctx, item.Get("content"), proxyURL)
			if errBlocks != nil {
				return nil, errBlocks
			}
			for _, block := range blocks {
				if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
					system = append(system, anthropicContent{Type: "text", Text: block.Text})
				}
			}
		case "user":
			blocks, errBlocks := convertUserContent(ctx, item.Get("content"), proxyURL)
			if errBlocks != nil {
				return nil, errBlocks
			}
			out = append(out, anthropicMsg{Role: "user", Content: blocks})
		case "assistant":
			blocks, errBlocks := convertAssistantContent(item)
			if errBlocks != nil {
				return nil, errBlocks
			}
			out = append(out, anthropicMsg{Role: "assistant", Content: blocks})
		case "tool":
			block, errBlock := convertToolResult(item)
			if errBlock != nil {
				return nil, errBlock
			}
			if len(out) > 0 && out[len(out)-1].Role == "user" {
				if existing, ok := out[len(out)-1].Content.([]anthropicContent); ok {
					out[len(out)-1].Content = append(existing, block)
					continue
				}
			}
			out = append(out, anthropicMsg{Role: "user", Content: []anthropicContent{block}})
		default:
			return nil, requestFault("unsupported chat message role " + role)
		}
	}
	if len(system) == 1 {
		req.System = system[0].Text
	} else if len(system) > 1 {
		req.System = system
	}
	req.Messages = out
	encoded, errMarshal := json.Marshal(req)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return encoded, nil
}

func anthropicMaxTokens(payload []byte) (int, error) {
	node := gjson.GetBytes(payload, "max_tokens")
	if !node.Exists() {
		node = gjson.GetBytes(payload, "max_completion_tokens")
	}
	if !node.Exists() {
		return defaultAnthropicMaxTokens, nil
	}
	if node.Type != gjson.Number {
		return 0, requestFault("max_tokens must be a positive integer")
	}
	value := int(node.Int())
	if value <= 0 {
		return 0, requestFault("max_tokens must be greater than zero")
	}
	if value > maxAnthropicMaxTokens {
		return 0, requestFault(fmt.Sprintf("max_tokens exceeds %d", maxAnthropicMaxTokens))
	}
	return value, nil
}

func parseStopSequences(payload []byte) []string {
	node := gjson.GetBytes(payload, "stop")
	if !node.Exists() {
		return nil
	}
	if node.Type == gjson.String {
		if trimmed := strings.TrimSpace(node.String()); trimmed != "" {
			return []string{trimmed}
		}
		return nil
	}
	if !node.IsArray() {
		return nil
	}
	out := make([]string, 0, len(node.Array()))
	for _, item := range node.Array() {
		if trimmed := strings.TrimSpace(item.String()); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func convertTools(payload []byte) ([]anthropicTool, error) {
	node := gjson.GetBytes(payload, "tools")
	if !node.Exists() {
		return nil, nil
	}
	if !node.IsArray() {
		return nil, requestFault("tools must be an array")
	}
	out := make([]anthropicTool, 0, len(node.Array()))
	for _, item := range node.Array() {
		toolType := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
		if toolType != "" && toolType != "function" {
			return nil, requestFault("only function tools can be sent to Command Code /messages")
		}
		fn := item.Get("function")
		if !fn.Exists() {
			fn = item
		}
		name := strings.TrimSpace(fn.Get("name").String())
		if name == "" {
			return nil, requestFault("tool function name is required")
		}
		schema := json.RawMessage(fn.Get("parameters").Raw)
		if len(strings.TrimSpace(string(schema))) == 0 || schema == nil || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, anthropicTool{
			Name:        name,
			Description: fn.Get("description").String(),
			InputSchema: json.RawMessage(schema),
		})
	}
	return out, nil
}

func convertUserContent(ctx context.Context, content gjson.Result, proxyURL string) ([]anthropicContent, error) {
	if content.Type == gjson.String {
		return []anthropicContent{{Type: "text", Text: content.String()}}, nil
	}
	if content.Type == gjson.Null || !content.Exists() {
		return []anthropicContent{{Type: "text", Text: ""}}, nil
	}
	if !content.IsArray() {
		return nil, requestFault("message content must be a string or array")
	}
	out := make([]anthropicContent, 0, len(content.Array()))
	for _, part := range content.Array() {
		partType := strings.ToLower(strings.TrimSpace(part.Get("type").String()))
		switch partType {
		case "", "text":
			out = append(out, anthropicContent{Type: "text", Text: part.Get("text").String()})
		case "image_url":
			block, errImage := convertImageURL(ctx, part.Get("image_url"), proxyURL)
			if errImage != nil {
				return nil, errImage
			}
			out = append(out, block)
		case "image":
			return nil, requestFault("OpenAI image parts must use type image_url")
		default:
			return nil, requestFault("unsupported content part type " + partType)
		}
	}
	if len(out) == 0 {
		return []anthropicContent{{Type: "text", Text: ""}}, nil
	}
	return out, nil
}

func convertImageURL(ctx context.Context, imageURL gjson.Result, proxyURL string) (anthropicContent, error) {
	reference := strings.TrimSpace(imageURL.Get("url").String())
	if reference == "" && imageURL.Type == gjson.String {
		reference = strings.TrimSpace(imageURL.String())
	}
	if reference == "" {
		return anthropicContent{}, requestFault("image_url is missing a url")
	}
	if data, mimeType, isData, errData := parseDataURL(reference); isData {
		if errData != nil {
			return anthropicContent{}, requestFault(errData.Error())
		}
		return anthropicContent{
			Type: "image",
			Source: &anthropicImage{
				Type:      "base64",
				MediaType: mimeType,
				Data:      data,
			},
		}, nil
	}
	data, mimeType, errFetch := downloadImage(ctx, proxyURL, reference)
	if errFetch != nil {
		return anthropicContent{}, requestFault(errFetch.Error())
	}
	return anthropicContent{
		Type: "image",
		Source: &anthropicImage{
			Type:      "base64",
			MediaType: mimeType,
			Data:      data,
		},
	}, nil
}

func convertAssistantContent(item gjson.Result) ([]anthropicContent, error) {
	var out []anthropicContent
	content := item.Get("content")
	if content.Type == gjson.String {
		if text := content.String(); text != "" {
			out = append(out, anthropicContent{Type: "text", Text: text})
		}
	} else if content.IsArray() {
		blocks, errBlocks := convertUserContent(context.Background(), content, "")
		if errBlocks != nil {
			return nil, errBlocks
		}
		out = append(out, blocks...)
	}
	toolCalls := item.Get("tool_calls")
	if !toolCalls.Exists() {
		if len(out) == 0 {
			out = append(out, anthropicContent{Type: "text", Text: ""})
		}
		return out, nil
	}
	if !toolCalls.IsArray() {
		return nil, requestFault("tool_calls must be an array")
	}
	for _, call := range toolCalls.Array() {
		callType := strings.ToLower(strings.TrimSpace(call.Get("type").String()))
		if callType != "" && callType != "function" {
			return nil, requestFault("only function tool_calls can be sent to Command Code /messages")
		}
		id := strings.TrimSpace(call.Get("id").String())
		name := strings.TrimSpace(call.Get("function.name").String())
		if id == "" || name == "" {
			return nil, requestFault("tool_calls require id and function.name")
		}
		arguments := strings.TrimSpace(call.Get("function.arguments").String())
		if arguments == "" {
			arguments = "{}"
		}
		if !json.Valid([]byte(arguments)) {
			return nil, requestFault("tool call arguments must be valid JSON")
		}
		out = append(out, anthropicContent{
			Type:  "tool_use",
			ID:    id,
			Name:  name,
			Input: json.RawMessage(arguments),
		})
	}
	return out, nil
}

func convertToolResult(item gjson.Result) (anthropicContent, error) {
	callID := strings.TrimSpace(item.Get("tool_call_id").String())
	if callID == "" {
		return anthropicContent{}, requestFault("tool messages require tool_call_id")
	}
	content := item.Get("content")
	text := ""
	if content.Type == gjson.String {
		text = content.String()
	} else if content.Exists() {
		text = content.Raw
	}
	return anthropicContent{
		Type:      "tool_result",
		ToolUseID: callID,
		Content:   text,
	}, nil
}

func anthropicToChatCompletion(raw []byte, model string) ([]byte, *chatCompletionUsage, error) {
	if !gjson.ValidBytes(raw) {
		return nil, nil, fmt.Errorf("upstream returned invalid JSON")
	}
	contentNode := gjson.GetBytes(raw, "content")
	var text strings.Builder
	var toolCalls []chatToolCall
	if contentNode.IsArray() {
		for _, part := range contentNode.Array() {
			switch part.Get("type").String() {
			case "text":
				text.WriteString(part.Get("text").String())
			case "tool_use":
				input := part.Get("input").Raw
				if strings.TrimSpace(input) == "" {
					input = "{}"
				}
				toolCalls = append(toolCalls, chatToolCall{
					ID:   part.Get("id").String(),
					Type: "function",
					Function: chatToolCallFunction{
						Name:      part.Get("name").String(),
						Arguments: compactJSON(input),
					},
				})
			}
		}
	}
	finish := mapStopReason(gjson.GetBytes(raw, "stop_reason").String())
	if len(toolCalls) > 0 {
		finish = "tool_calls"
	}
	usage := anthropicUsage(raw)
	contentRaw, _ := json.Marshal(text.String())
	message := &chatCompletionMessage{Role: "assistant", Content: contentRaw, ToolCalls: toolCalls}
	payload, errMarshal := json.Marshal(chatCompletion{
		ID:      firstNonEmpty(gjson.GetBytes(raw, "id").String(), newCompletionID()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []chatCompletionChoice{{
			Index:        0,
			Message:      message,
			FinishReason: &finish,
		}},
		Usage: usage,
	})
	if errMarshal != nil {
		return nil, nil, errMarshal
	}
	return payload, usage, nil
}

func anthropicUsage(raw []byte) *chatCompletionUsage {
	node := gjson.GetBytes(raw, "usage")
	if !node.Exists() {
		return nil
	}
	prompt := node.Get("input_tokens").Int()
	completion := node.Get("output_tokens").Int()
	if prompt == 0 && completion == 0 {
		return nil
	}
	return &chatCompletionUsage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
	}
}

func mapStopReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

func compactJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "{}"
	}
	var compact json.RawMessage
	if errUnmarshal := json.Unmarshal([]byte(raw), &compact); errUnmarshal != nil {
		return raw
	}
	return string(compact)
}

func forwardAnthropicSSE(body io.Reader, model string, framing streamFraming, emit func([]byte) error) (*chatCompletionUsage, error) {
	reader := newSSEReader(body)
	id := newCompletionID()
	roleSent := false
	var usage *chatCompletionUsage
	var finish *string
	toolIndex := -1
	emitDelta := func(delta chatCompletionDelta, stop *string, chunkUsage *chatCompletionUsage) error {
		if !roleSent {
			delta.Role = "assistant"
			roleSent = true
		}
		chunk := buildStreamChunk(id, model, delta, stop, chunkUsage)
		if len(chunk) == 0 {
			return fmt.Errorf("marshal stream chunk")
		}
		return emit(framing.frame(chunk))
	}
	for {
		event, errRead := readSSEEvent(reader)
		if errRead != nil {
			if errRead == io.EOF {
				return usage, incompleteStreamError()
			}
			return usage, errRead
		}
		name := strings.TrimSpace(event.Event)
		if name == "" {
			name = gjson.Get(event.Data, "type").String()
		}
		switch name {
		case "error":
			return usage, &upstreamError{failure: classifyHTTPStatus(http.StatusBadGateway, []byte(event.Data))}
		case "content_block_delta":
			delta := gjson.Get(event.Data, "delta")
			switch delta.Get("type").String() {
			case "text_delta":
				if errEmit := emitDelta(chatCompletionDelta{Content: delta.Get("text").String()}, nil, nil); errEmit != nil {
					return usage, errEmit
				}
			case "input_json_delta":
				if toolIndex < 0 {
					continue
				}
				idx := toolIndex
				if errEmit := emitDelta(chatCompletionDelta{ToolCalls: []chatToolCall{{
					Index:    &idx,
					Type:     "function",
					Function: chatToolCallFunction{Arguments: delta.Get("partial_json").String()},
				}}}, nil, nil); errEmit != nil {
					return usage, errEmit
				}
			}
		case "content_block_start":
			block := gjson.Get(event.Data, "content_block")
			if block.Get("type").String() != "tool_use" {
				continue
			}
			toolIndex++
			idx := toolIndex
			if errEmit := emitDelta(chatCompletionDelta{ToolCalls: []chatToolCall{{
				Index: &idx,
				ID:    block.Get("id").String(),
				Type:  "function",
				Function: chatToolCallFunction{
					Name:      block.Get("name").String(),
					Arguments: "",
				},
			}}}, nil, nil); errEmit != nil {
				return usage, errEmit
			}
		case "message_delta":
			if reason := gjson.Get(event.Data, "delta.stop_reason").String(); reason != "" {
				mapped := mapStopReason(reason)
				finish = &mapped
			}
			if parsed := anthropicUsage([]byte(event.Data)); parsed != nil {
				usage = parsed
			}
		case "message_start":
			if parsed := anthropicUsage([]byte(gjson.Get(event.Data, "message").Raw)); parsed != nil {
				usage = parsed
			}
		case "message_stop":
			if finish == nil {
				stop := "stop"
				finish = &stop
			}
			if errEmit := emitDelta(chatCompletionDelta{}, finish, usage); errEmit != nil {
				return usage, errEmit
			}
			if framed := framing.terminator(); len(framed) > 0 {
				if errEmit := emit(framed); errEmit != nil {
					return usage, errEmit
				}
			}
			return usage, nil
		}
	}
}
