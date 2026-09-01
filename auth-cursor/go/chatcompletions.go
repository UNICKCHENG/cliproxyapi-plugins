package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/types/known/structpb"

	sdkv1 "github.com/UNICKCHENG/cliproxyapi-plugins/auth-cursor/go/internal/sdk/v1"
)

// chatRequest is the Cursor-facing view of an OpenAI chat completion request. An agent turn takes
// a single prompt string, so the message list is flattened into a transcript.
type chatRequest struct {
	Prompt           string
	Images           []chatImage
	Params           []modelParam
	Tools            map[string]*sdkv1.CustomToolDefinition
	ToolNames        []string
	ToolResults      []chatToolResult
	SessionPrefix    string
	SessionIncrement string
	IncrementImages  []chatImage
}

// modelParam is one model tuning hint as the client supplied it, before it has been checked
// against what the selected model actually exposes.
type modelParam struct {
	ID    string
	Value string
}

type chatToolCall struct {
	Index    *int                 `json:"index,omitempty"`
	ID       string               `json:"id"`
	Type     string               `json:"type"`
	Function chatToolCallFunction `json:"function"`
}

type chatToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatToolResult struct {
	CallID  string
	Content string
}

type chatCompletionMessage struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	ToolCalls []chatToolCall `json:"tool_calls,omitempty"`
}

type chatCompletionDelta struct {
	Role      string         `json:"role,omitempty"`
	Content   string         `json:"content,omitempty"`
	ToolCalls []chatToolCall `json:"tool_calls,omitempty"`
}

type chatCompletionChoice struct {
	Index        int                    `json:"index"`
	Message      *chatCompletionMessage `json:"message,omitempty"`
	Delta        *chatCompletionDelta   `json:"delta,omitempty"`
	FinishReason *string                `json:"finish_reason"`
}

type chatCompletionUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

type chatCompletion struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []chatCompletionChoice `json:"choices"`
	Usage   *chatCompletionUsage   `json:"usage,omitempty"`
}

func parseChatRequest(payload []byte) (chatRequest, error) {
	if !gjson.ValidBytes(payload) {
		return chatRequest{}, fmt.Errorf("request payload is not valid JSON")
	}
	messages := gjson.GetBytes(payload, "messages").Array()
	if len(messages) == 0 {
		return chatRequest{}, fmt.Errorf("request payload contains no messages")
	}

	tools, names, errTools := parseChatTools(payload)
	if errTools != nil {
		return chatRequest{}, errTools
	}
	tools, names, errChoice := applyToolChoice(payload, tools, names)
	if errChoice != nil {
		return chatRequest{}, errChoice
	}

	var systemParts []string
	var turns []string
	var images []chatImage
	userTurns := 0

	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Get("role").String()))
		text, messageImages := messageContent(message.Get("content"))
		images = append(images, messageImages...)
		switch role {
		case "system", "developer":
			if strings.TrimSpace(text) == "" {
				continue
			}
			systemParts = append(systemParts, text)
		case "assistant":
			toolCalls := message.Get("tool_calls").Array()
			if strings.TrimSpace(text) == "" && len(toolCalls) == 0 {
				continue
			}
			if strings.TrimSpace(text) != "" {
				turns = append(turns, "Assistant: "+text)
			}
			for _, call := range toolCalls {
				name := strings.TrimSpace(call.Get("function.name").String())
				args := toolCallArgumentsJSON(call.Get("function.arguments"))
				id := strings.TrimSpace(call.Get("id").String())
				line := fmt.Sprintf("Assistant requested tool %s with arguments %s", name, args)
				if id != "" {
					line += " [id " + id + "]"
				}
				turns = append(turns, line)
			}
		case "tool":
			id := strings.TrimSpace(message.Get("tool_call_id").String())
			turns = append(turns, fmt.Sprintf("Tool result for %s: %s", id, text))
		case "function":
			if strings.TrimSpace(text) == "" {
				continue
			}
			turns = append(turns, "Tool result: "+text)
		default:
			if strings.TrimSpace(text) == "" {
				continue
			}
			userTurns++
			turns = append(turns, "User: "+text)
		}
	}

	toolResults := finalToolResults(messages)
	prompt := buildPrompt(systemParts, turns, userTurns)
	if strings.TrimSpace(prompt) == "" && len(toolResults) == 0 {
		return chatRequest{}, fmt.Errorf("request payload contains no textual content")
	}
	prefix, increment, incrementImages := sessionSplit(messages)
	return chatRequest{
		Prompt:           prompt,
		Images:           images,
		Params:           chatParams(payload),
		Tools:            tools,
		ToolNames:        names,
		ToolResults:      toolResults,
		SessionPrefix:    prefix,
		SessionIncrement: increment,
		IncrementImages:  incrementImages,
	}, nil
}

// buildPrompt renders the transcript. A lone user message is forwarded verbatim so simple
// completions are not wrapped in scaffolding the model would otherwise echo.
func buildPrompt(systemParts, turns []string, userTurns int) string {
	if len(systemParts) == 0 && userTurns == 1 && len(turns) == 1 {
		return strings.TrimPrefix(turns[0], "User: ")
	}
	var builder strings.Builder
	if len(systemParts) > 0 {
		builder.WriteString(strings.Join(systemParts, "\n\n"))
		builder.WriteString("\n\n")
	}
	builder.WriteString(strings.Join(turns, "\n\n"))
	return strings.TrimSpace(builder.String())
}

// sessionSplit cuts the transcript after the last assistant turn. The prefix is what a reused
// agent has already seen; the increment is what the next Send should carry. No assistant turn
// means this is the first request of a conversation and reuse does not apply.
func sessionSplit(messages []gjson.Result) (prefix, increment string, incrementImages []chatImage) {
	lastAssistant := -1
	for i, message := range messages {
		if strings.ToLower(strings.TrimSpace(message.Get("role").String())) == "assistant" {
			lastAssistant = i
		}
	}
	if lastAssistant < 0 {
		return "", "", nil
	}
	prefix = strings.TrimSpace(renderTranscript(messages[:lastAssistant+1], false))
	increment = strings.TrimSpace(renderTranscript(messages[lastAssistant+1:], true))
	for _, message := range messages[lastAssistant+1:] {
		_, images := messageContent(message.Get("content"))
		incrementImages = append(incrementImages, images...)
	}
	return prefix, increment, incrementImages
}

func renderTranscript(messages []gjson.Result, increment bool) string {
	var systemParts []string
	var turns []string
	userTurns := 0
	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Get("role").String()))
		text, _ := messageContent(message.Get("content"))
		switch role {
		case "system", "developer":
			if increment {
				continue
			}
			if strings.TrimSpace(text) != "" {
				systemParts = append(systemParts, text)
			}
		case "assistant":
			toolCalls := message.Get("tool_calls").Array()
			if strings.TrimSpace(text) == "" && len(toolCalls) == 0 {
				continue
			}
			if strings.TrimSpace(text) != "" {
				turns = append(turns, "Assistant: "+text)
			}
			for _, call := range toolCalls {
				name := strings.TrimSpace(call.Get("function.name").String())
				args := toolCallArgumentsJSON(call.Get("function.arguments"))
				id := strings.TrimSpace(call.Get("id").String())
				line := fmt.Sprintf("Assistant requested tool %s with arguments %s", name, args)
				if id != "" {
					line += " [id " + id + "]"
				}
				turns = append(turns, line)
			}
		case "tool":
			id := strings.TrimSpace(message.Get("tool_call_id").String())
			turns = append(turns, fmt.Sprintf("Tool result for %s: %s", id, text))
		case "function":
			if strings.TrimSpace(text) != "" {
				turns = append(turns, "Tool result: "+text)
			}
		default:
			if strings.TrimSpace(text) == "" {
				continue
			}
			userTurns++
			turns = append(turns, "User: "+text)
		}
	}
	if increment {
		return strings.Join(turns, "\n\n")
	}
	return buildPrompt(systemParts, turns, userTurns)
}

// messageContent flattens a chat message body, which may be a plain string or an array of
// typed parts, into text plus any attached images.
func messageContent(content gjson.Result) (string, []chatImage) {
	if content.Type == gjson.String {
		return content.String(), nil
	}
	if !content.IsArray() {
		return "", nil
	}
	var texts []string
	var images []chatImage
	for _, part := range content.Array() {
		switch strings.TrimSpace(part.Get("type").String()) {
		case "text", "input_text":
			if text := part.Get("text").String(); text != "" {
				texts = append(texts, text)
			}
		case "image_url", "input_image":
			url := part.Get("image_url.url").String()
			if url == "" {
				url = part.Get("image_url").String()
			}
			if url == "" {
				url = part.Get("image").String()
			}
			if image, ok := parseImage(url); ok {
				images = append(images, image)
			}
		}
	}
	return strings.Join(texts, "\n"), images
}

// parseImage separates inline data URLs, which already carry their bytes, from remote references
// the plugin still has to fetch before a local agent will accept them.
func parseImage(reference string) (chatImage, bool) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return chatImage{}, false
	}
	if !strings.HasPrefix(reference, "data:") {
		return chatImage{URL: reference}, true
	}
	comma := strings.Index(reference, ",")
	if comma < 0 {
		return chatImage{}, false
	}
	header := reference[len("data:"):comma]
	if !strings.Contains(header, ";base64") {
		return chatImage{}, false
	}
	mimeType := strings.TrimSpace(strings.Split(header, ";")[0])
	if mimeType == "" {
		mimeType = defaultImageMimeType
	}
	return chatImage{Data: reference[comma+1:], MimeType: mimeType}, true
}

// chatParams forwards model tuning hints. They are checked against the account's model catalog
// during model resolution, which silently drops anything the selected model does not expose.
func chatParams(payload []byte) []modelParam {
	var params []modelParam
	seen := map[string]struct{}{}
	add := func(id, value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		params = append(params, modelParam{ID: id, Value: value})
	}
	add("reasoning_effort", gjson.GetBytes(payload, "reasoning_effort").String())
	add("reasoning_effort", gjson.GetBytes(payload, "reasoning.effort").String())
	return params
}

func newCompletionID() string {
	buf := make([]byte, 12)
	if _, errRead := rand.Read(buf); errRead != nil {
		return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	return "chatcmpl-" + hex.EncodeToString(buf)
}

// convertUsage maps Cursor token accounting onto OpenAI's prompt/completion split. Cache
// reads are prompt tokens that were served from cache, so they belong on the prompt side.
func convertUsage(usage *sdkv1.TokenUsage) *chatCompletionUsage {
	if usage == nil {
		return nil
	}
	prompt := usage.GetInputTokens() + usage.GetCacheReadTokens()
	completion := usage.GetOutputTokens()
	return &chatCompletionUsage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
	}
}

func buildCompletion(id, model, text string, usage *sdkv1.TokenUsage) ([]byte, error) {
	finish := "stop"
	return json.Marshal(chatCompletion{
		ID:      id,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []chatCompletionChoice{{
			Index:        0,
			Message:      &chatCompletionMessage{Role: "assistant", Content: text},
			FinishReason: &finish,
		}},
		Usage: convertUsage(usage),
	})
}

func buildStreamChunk(id, model string, delta chatCompletionDelta, finishReason *string, usage *sdkv1.TokenUsage) []byte {
	chunk := chatCompletion{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []chatCompletionChoice{{
			Index:        0,
			Delta:        &delta,
			FinishReason: finishReason,
		}},
		Usage: convertUsage(usage),
	}
	raw, errMarshal := json.Marshal(chunk)
	if errMarshal != nil {
		return nil
	}
	return raw
}

func parseChatTools(payload []byte) (map[string]*sdkv1.CustomToolDefinition, []string, error) {
	toolsJSON := gjson.GetBytes(payload, "tools")
	if !toolsJSON.Exists() || !toolsJSON.IsArray() {
		return nil, nil, nil
	}
	tools := make(map[string]*sdkv1.CustomToolDefinition)
	var names []string
	for _, tool := range toolsJSON.Array() {
		typ := strings.TrimSpace(tool.Get("type").String())
		name := strings.TrimSpace(tool.Get("function.name").String())
		if typ != "" && typ != "function" {
			continue
		}
		if typ == "" && name == "" {
			continue
		}
		if name == "" {
			return nil, nil, fmt.Errorf("tool function name is empty")
		}
		if _, exists := tools[name]; exists {
			return nil, nil, fmt.Errorf("duplicate tool name %q", name)
		}
		def, errDef := parseFunctionTool(name, tool)
		if errDef != nil {
			return nil, nil, errDef
		}
		tools[name] = def
		names = append(names, name)
	}
	if len(tools) == 0 {
		return nil, nil, nil
	}
	return tools, names, nil
}

func parseFunctionTool(name string, tool gjson.Result) (*sdkv1.CustomToolDefinition, error) {
	params := tool.Get("function.parameters")
	var schema map[string]any
	switch {
	case !params.Exists() || params.Type == gjson.Null:
		schema = map[string]any{"type": "object"}
	case !params.IsObject():
		return nil, fmt.Errorf("tool %q parameters must be a JSON object", name)
	default:
		if errUnmarshal := json.Unmarshal([]byte(params.Raw), &schema); errUnmarshal != nil {
			return nil, fmt.Errorf("tool %q parameters: %w", name, errUnmarshal)
		}
	}
	input, errStruct := structpb.NewStruct(schema)
	if errStruct != nil {
		return nil, fmt.Errorf("tool %q parameters: %w", name, errStruct)
	}
	def := &sdkv1.CustomToolDefinition{InputSchema: input}
	if desc := strings.TrimSpace(tool.Get("function.description").String()); desc != "" {
		def.Description = &desc
	}
	return def, nil
}

func applyToolChoice(payload []byte, tools map[string]*sdkv1.CustomToolDefinition, names []string) (map[string]*sdkv1.CustomToolDefinition, []string, error) {
	choice := gjson.GetBytes(payload, "tool_choice")
	if !choice.Exists() {
		return tools, names, nil
	}
	if choice.Type == gjson.String {
		if choice.String() == "none" {
			return nil, nil, nil
		}
		return tools, names, nil
	}
	if !choice.IsObject() {
		return tools, names, nil
	}
	name := strings.TrimSpace(choice.Get("function.name").String())
	if _, ok := tools[name]; !ok {
		return nil, nil, fmt.Errorf("tool_choice names unknown tool %q", name)
	}
	return map[string]*sdkv1.CustomToolDefinition{name: tools[name]}, []string{name}, nil
}

func finalToolResults(messages []gjson.Result) []chatToolResult {
	var block []chatToolResult
	for i := len(messages) - 1; i >= 0; i-- {
		role := strings.ToLower(strings.TrimSpace(messages[i].Get("role").String()))
		if role != "tool" {
			break
		}
		id := strings.TrimSpace(messages[i].Get("tool_call_id").String())
		if id == "" {
			break
		}
		text, _ := messageContent(messages[i].Get("content"))
		block = append(block, chatToolResult{CallID: id, Content: text})
	}
	for i, j := 0, len(block)-1; i < j; i, j = i+1, j-1 {
		block[i], block[j] = block[j], block[i]
	}
	return block
}

func toolCallArgumentsJSON(value gjson.Result) string {
	if !value.Exists() || value.Type == gjson.Null {
		return "{}"
	}
	if value.Type == gjson.String {
		if text := value.String(); text != "" {
			return text
		}
		return "{}"
	}
	if raw := strings.TrimSpace(value.Raw); raw != "" {
		return raw
	}
	return "{}"
}

func buildToolCallCompletion(id, model, text string, calls []chatToolCall) ([]byte, error) {
	finish := "tool_calls"
	return json.Marshal(chatCompletion{
		ID:      id,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []chatCompletionChoice{{
			Index: 0,
			Message: &chatCompletionMessage{
				Role:      "assistant",
				Content:   text,
				ToolCalls: calls,
			},
			FinishReason: &finish,
		}},
	})
}
