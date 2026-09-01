package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	sdkv1 "github.com/UNICKCHENG/cliproxyapi-plugins/auth-cursor/go/internal/sdk/v1"
)

// chatRequest is the Cursor-facing view of an OpenAI chat completion request. An agent turn takes
// a single prompt string, so the message list is flattened into a transcript.
type chatRequest struct {
	Prompt string
	Images []chatImage
	Params []modelParam
}

// modelParam is one model tuning hint as the client supplied it, before it has been checked
// against what the selected model actually exposes.
type modelParam struct {
	ID    string
	Value string
}

type chatCompletionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatCompletionDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
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

	var systemParts []string
	var turns []string
	var images []chatImage
	userTurns := 0

	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Get("role").String()))
		text, messageImages := messageContent(message.Get("content"))
		images = append(images, messageImages...)
		if strings.TrimSpace(text) == "" {
			continue
		}
		switch role {
		case "system", "developer":
			systemParts = append(systemParts, text)
		case "assistant":
			turns = append(turns, "Assistant: "+text)
		case "tool", "function":
			turns = append(turns, "Tool result: "+text)
		default:
			userTurns++
			turns = append(turns, "User: "+text)
		}
	}

	prompt := buildPrompt(systemParts, turns, userTurns, messages)
	if strings.TrimSpace(prompt) == "" {
		return chatRequest{}, fmt.Errorf("request payload contains no textual content")
	}
	return chatRequest{Prompt: prompt, Images: images, Params: chatParams(payload)}, nil
}

// buildPrompt renders the transcript. A lone user message is forwarded verbatim so simple
// completions are not wrapped in scaffolding the model would otherwise echo.
func buildPrompt(systemParts, turns []string, userTurns int, messages []gjson.Result) string {
	if len(systemParts) == 0 && userTurns == 1 && len(turns) == 1 {
		return strings.TrimPrefix(turns[0], "User: ")
	}
	var builder strings.Builder
	if len(systemParts) > 0 {
		builder.WriteString(strings.Join(systemParts, "\n\n"))
		builder.WriteString("\n\n")
	}
	builder.WriteString(strings.Join(turns, "\n\n"))
	_ = messages
	return strings.TrimSpace(builder.String())
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
	if effort := strings.TrimSpace(gjson.GetBytes(payload, "reasoning_effort").String()); effort != "" {
		params = append(params, modelParam{ID: "reasoning_effort", Value: effort})
	}
	if effort := strings.TrimSpace(gjson.GetBytes(payload, "reasoning.effort").String()); effort != "" {
		params = append(params, modelParam{ID: "reasoning_effort", Value: effort})
	}
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
