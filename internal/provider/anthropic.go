package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// AnthropicProvider implements the Provider interface for Anthropic Messages API.
type AnthropicProvider struct {
	apiKey  string
	apiBase string
	client  *http.Client
}

// NewAnthropic creates a new Anthropic Messages API provider.
func NewAnthropic(apiKey, apiBase string) *AnthropicProvider {
	if strings.TrimSpace(apiBase) == "" {
		apiBase = "https://api.anthropic.com"
	}
	return &AnthropicProvider{
		apiKey:  apiKey,
		apiBase: NormalizeAPIBase(apiBase, "anthropic-messages"),
		client:  &http.Client{},
	}
}

// anthropicMessage is the wire format for Anthropic Messages API.
type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"input_schema"`
}

type anthropicRequest struct {
	Model       string             `json:"model"`
	Messages    []anthropicMessage `json:"messages"`
	System      string             `json:"system,omitempty"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature float64            `json:"temperature,omitempty"`
	Stream      bool               `json:"stream"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
}

// toAnthropicMessages converts provider Messages to Anthropic wire format.
// Extracts the system message and returns the rest.
func toAnthropicMessages(msgs []Message) (string, []anthropicMessage) {
	var system string
	var out []anthropicMessage

	for _, m := range msgs {
		if m.Role == "system" {
			system = m.Content
			continue
		}

		am := anthropicMessage{Role: m.Role}

		// Tool results become role "user" with tool_result content blocks.
		// Anthropic requires every tool_result for a parallel tool_use batch
		// to live in the SAME user message — separate user messages, even
		// back-to-back, get rejected with "tool_use ids ... without tool_result
		// blocks immediately after". So coalesce consecutive tool messages
		// into a single user message containing one tool_result block each.
		if m.Role == "tool" {
			block := map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": m.ToolCallID,
				"content":     m.Content,
			}
			if n := len(out); n > 0 && out[n-1].Role == "user" {
				var existing []interface{}
				if err := json.Unmarshal(out[n-1].Content, &existing); err == nil && len(existing) > 0 {
					allToolResults := true
					for _, eb := range existing {
						mp, ok := eb.(map[string]interface{})
						if !ok || mp["type"] != "tool_result" {
							allToolResults = false
							break
						}
					}
					if allToolResults {
						existing = append(existing, block)
						out[n-1].Content, _ = json.Marshal(existing)
						continue
					}
				}
			}
			am.Role = "user"
			am.Content, _ = json.Marshal([]interface{}{block})
			out = append(out, am)
			continue
		}

		// Assistant messages with tool calls
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			var blocks []interface{}
			if tb := thinkingBlockFor(m); tb != nil {
				blocks = append(blocks, tb)
			}
			if m.Content != "" {
				blocks = append(blocks, map[string]interface{}{
					"type": "text",
					"text": m.Content,
				})
			}
			for _, tc := range m.ToolCalls {
				var input interface{}
				json.Unmarshal([]byte(tc.Function.Arguments), &input)
				blocks = append(blocks, map[string]interface{}{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Function.Name,
					"input": input,
				})
			}
			am.Content, _ = json.Marshal(blocks)
			out = append(out, am)
			continue
		}

		// Regular text content
		if len(m.ContentParts) > 0 {
			var blocks []interface{}
			if m.Role == "assistant" {
				if tb := thinkingBlockFor(m); tb != nil {
					blocks = append(blocks, tb)
				}
			}
			for _, part := range m.ContentParts {
				if part.Type == "text" {
					blocks = append(blocks, map[string]interface{}{
						"type": "text",
						"text": part.Text,
					})
				} else if part.Type == "image_url" && part.ImageURL != nil {
					blocks = append(blocks, map[string]interface{}{
						"type": "image",
						"source": map[string]string{
							"type":       "url",
							"url":        part.ImageURL.URL,
						},
					})
				}
			}
			am.Content, _ = json.Marshal(blocks)
		} else if m.Role == "assistant" {
			var blocks []interface{}
			if tb := thinkingBlockFor(m); tb != nil {
				blocks = append(blocks, tb)
			}
			if m.Content != "" {
				blocks = append(blocks, map[string]interface{}{
					"type": "text",
					"text": m.Content,
				})
			}
			if len(blocks) > 0 {
				am.Content, _ = json.Marshal(blocks)
			} else if m.Content != "" {
				am.Content, _ = json.Marshal(m.Content)
			}
		} else if m.Content != "" {
			am.Content, _ = json.Marshal(m.Content)
		} else {
			// Defensive: a content-less user/system message would marshal
			// to a wire object missing the `content` field, which
			// Anthropic rejects with "expected a string or a list". Send
			// an empty string so it round-trips even if upstream callers
			// produce a degenerate message (e.g. legacy session load
			// pre-dating ContentParts persistence).
			am.Content, _ = json.Marshal("")
		}

		out = append(out, am)
	}

	return system, out
}

// thinkingBlockFor returns a content-block map for an assistant message's
// prior thinking, or nil if nothing to replay. Extended-thinking models
// (real Anthropic, DeepSeek's /anthropic compat) reject the next turn when
// the prior `content[].thinking` is dropped, so we echo it back verbatim.
func thinkingBlockFor(m Message) map[string]interface{} {
	if m.Role != "assistant" {
		return nil
	}
	// Prefer the raw thinking block we captured on the response — preserves
	// signature for real Anthropic. Falls back to the plain text we stored
	// on Message.Thinking for sessions written before signature capture.
	if len(m.RawAssistant) > 0 {
		var raw map[string]interface{}
		if err := json.Unmarshal(m.RawAssistant, &raw); err == nil {
			if t, _ := raw["type"].(string); t == "thinking" {
				return raw
			}
		}
	}
	if m.Thinking != "" {
		return map[string]interface{}{
			"type":     "thinking",
			"thinking": m.Thinking,
		}
	}
	return nil
}

func (p *AnthropicProvider) buildRequest(ctx context.Context, messages []Message, tools []Tool, model string, maxTokens int, temperature float64, stream bool) (*http.Request, error) {
	system, anthropicMsgs := toAnthropicMessages(messages)

	if maxTokens <= 0 {
		maxTokens = 4096
	}

	req := anthropicRequest{
		Model:       StripProviderPrefix(model),
		Messages:    anthropicMsgs,
		System:      system,
		MaxTokens:   maxTokens,
		Temperature: temperature,
		Stream:      stream,
	}

	if len(tools) > 0 {
		for _, t := range tools {
			req.Tools = append(req.Tools, anthropicTool{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				InputSchema: t.Function.Parameters,
			})
		}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := p.apiBase + "/v1/messages"
	slog.Info("anthropic request", "url", url, "model", req.Model)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	return httpReq, nil
}

// Anthropic SSE event types
type anthropicSSEEvent struct {
	Type string `json:"type"`
}

type anthropicContentBlockStart struct {
	Type         string                     `json:"type"`
	Index        int                        `json:"index"`
	ContentBlock anthropicContentBlockEntry `json:"content_block"`
}

type anthropicContentBlockEntry struct {
	Type  string `json:"type"` // "text" or "tool_use"
	ID    string `json:"id,omitempty"`
	Name  string `json:"name,omitempty"`
	Text  string `json:"text,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type anthropicContentBlockDelta struct {
	Type  string                `json:"type"`
	Index int                   `json:"index"`
	Delta anthropicDeltaContent `json:"delta"`
}

type anthropicDeltaContent struct {
	Type        string `json:"type"` // "text_delta" | "input_json_delta" | "thinking_delta" | "signature_delta"
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	Signature   string `json:"signature,omitempty"`
}

func (p *AnthropicProvider) Chat(ctx context.Context, messages []Message, tools []Tool, model string, maxTokens int, temperature float64) (*Response, error) {
	httpReq, err := p.buildRequest(ctx, messages, tools, model, maxTokens, temperature, true)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	return p.parseSSE(resp.Body)
}

func (p *AnthropicProvider) ChatStream(ctx context.Context, messages []Message, tools []Tool, model string, maxTokens int, temperature float64) (*StreamReader, error) {
	httpReq, err := p.buildRequest(ctx, messages, tools, model, maxTokens, temperature, true)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	ch := make(chan StreamChunk, 64)
	reader := NewStreamReader(ch)

	go func() {
		defer resp.Body.Close()
		defer close(ch)

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		type blockState struct {
			blockType string // "text" | "tool_use" | "thinking"
			id        string
			name      string
			argsJSON  strings.Builder
			thinking  strings.Builder
			signature string
		}
		blocks := make(map[int]*blockState)

		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")

			var event anthropicSSEEvent
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				continue
			}

			switch event.Type {
			case "content_block_start":
				var cbs anthropicContentBlockStart
				if json.Unmarshal([]byte(data), &cbs) == nil {
					blocks[cbs.Index] = &blockState{
						blockType: cbs.ContentBlock.Type,
						id:        cbs.ContentBlock.ID,
						name:      cbs.ContentBlock.Name,
					}
					if cbs.ContentBlock.Text != "" {
						select {
						case ch <- StreamChunk{Content: cbs.ContentBlock.Text}:
						case <-ctx.Done():
							return
						}
					}
				}

			case "content_block_delta":
				var cbd anthropicContentBlockDelta
				if json.Unmarshal([]byte(data), &cbd) == nil {
					switch cbd.Delta.Type {
					case "text_delta":
						if cbd.Delta.Text != "" {
							select {
							case ch <- StreamChunk{Content: cbd.Delta.Text}:
							case <-ctx.Done():
								return
							}
						}
					case "input_json_delta":
						if bs, ok := blocks[cbd.Index]; ok {
							bs.argsJSON.WriteString(cbd.Delta.PartialJSON)
						}
					case "thinking_delta":
						if bs, ok := blocks[cbd.Index]; ok {
							bs.thinking.WriteString(cbd.Delta.Thinking)
						}
					case "signature_delta":
						if bs, ok := blocks[cbd.Index]; ok {
							bs.signature = cbd.Delta.Signature
						}
					}
				}

			case "message_stop":
				var toolCalls []ToolCall
				var thinkingText, thinkingSig string
				for i := 0; i < len(blocks); i++ {
					bs, ok := blocks[i]
					if !ok {
						continue
					}
					switch bs.blockType {
					case "tool_use":
						toolCalls = append(toolCalls, ToolCall{
							ID:   bs.id,
							Type: "function",
							Function: FunctionCall{
								Name:      bs.name,
								Arguments: bs.argsJSON.String(),
							},
						})
					case "thinking":
						if t := bs.thinking.String(); t != "" {
							thinkingText = t
						}
						if bs.signature != "" {
							thinkingSig = bs.signature
						}
					}
				}
				select {
				case ch <- StreamChunk{
					ToolCalls:         toolCalls,
					Thinking:          thinkingText,
					ThinkingSignature: thinkingSig,
					Done:              true,
				}:
				case <-ctx.Done():
				}
				return
			}
		}

		if err := scanner.Err(); err != nil {
			reader.SetErr(fmt.Errorf("read stream: %w", err))
		}
	}()

	return reader, nil
}

func (p *AnthropicProvider) parseSSE(body io.Reader) (*Response, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var contentBuilder strings.Builder

	type blockState struct {
		blockType string
		id        string
		name      string
		argsJSON  strings.Builder
		thinking  strings.Builder
		signature string
	}
	blocks := make(map[int]*blockState)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		var event anthropicSSEEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			slog.Warn("parse anthropic SSE event", "error", err, "data", data)
			continue
		}

		switch event.Type {
		case "content_block_start":
			var cbs anthropicContentBlockStart
			if json.Unmarshal([]byte(data), &cbs) == nil {
				blocks[cbs.Index] = &blockState{
					blockType: cbs.ContentBlock.Type,
					id:        cbs.ContentBlock.ID,
					name:      cbs.ContentBlock.Name,
				}
				if cbs.ContentBlock.Text != "" {
					contentBuilder.WriteString(cbs.ContentBlock.Text)
				}
			}

		case "content_block_delta":
			var cbd anthropicContentBlockDelta
			if json.Unmarshal([]byte(data), &cbd) == nil {
				switch cbd.Delta.Type {
				case "text_delta":
					contentBuilder.WriteString(cbd.Delta.Text)
				case "input_json_delta":
					if bs, ok := blocks[cbd.Index]; ok {
						bs.argsJSON.WriteString(cbd.Delta.PartialJSON)
					}
				case "thinking_delta":
					if bs, ok := blocks[cbd.Index]; ok {
						bs.thinking.WriteString(cbd.Delta.Thinking)
					}
				case "signature_delta":
					if bs, ok := blocks[cbd.Index]; ok {
						bs.signature = cbd.Delta.Signature
					}
				}
			}

		case "message_stop":
			// Done
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read stream: %w", err)
	}

	result := &Response{
		Content: contentBuilder.String(),
	}
	var thinkingBuilder strings.Builder
	var thinkingSig string
	for i := 0; i < len(blocks); i++ {
		bs, ok := blocks[i]
		if !ok {
			continue
		}
		switch bs.blockType {
		case "tool_use":
			result.ToolCalls = append(result.ToolCalls, ToolCall{
				ID:   bs.id,
				Type: "function",
				Function: FunctionCall{
					Name:      bs.name,
					Arguments: bs.argsJSON.String(),
				},
			})
		case "thinking":
			if t := bs.thinking.String(); t != "" {
				thinkingBuilder.WriteString(t)
			}
			if bs.signature != "" {
				thinkingSig = bs.signature
			}
		}
	}
	if thinking := thinkingBuilder.String(); thinking != "" {
		result.Thinking = thinking
		// DeepSeek's Anthropic-compat endpoint (and real Anthropic extended
		// thinking) requires the thinking block to be echoed verbatim on the
		// next turn. Pack {thinking, signature} into RawAssistant so
		// toAnthropicMessages can replay it as a content block.
		type thinkingBlock struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature,omitempty"`
		}
		if raw, err := json.Marshal(thinkingBlock{
			Type:      "thinking",
			Thinking:  thinking,
			Signature: thinkingSig,
		}); err == nil {
			result.RawAssistant = raw
		}
	}

	return result, nil
}
