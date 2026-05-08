package provider

import (
	"context"
	"encoding/json"
	"strings"
)

// Message represents a chat message.
// When storing in session, keep ALL fields exactly as returned by the LLM
// to ensure prompt cache hits on subsequent turns.
type Message struct {
	Role         string        `json:"role"`
	Content      string        `json:"content,omitempty"`
	ContentParts []ContentPart `json:"content_parts,omitempty"` // multimodal input (user messages)
	ToolCalls    []ToolCall    `json:"tool_calls,omitempty"`
	ToolCallID   string        `json:"tool_call_id,omitempty"`
	Name         string        `json:"name,omitempty"`
	Thinking     string        `json:"thinking,omitempty"`  // model's reasoning (for memory extraction)
	Timestamp    int64         `json:"timestamp,omitempty"` // unix ms, for memory timeline

	// Metadata is UI-only state attached to a tool-role message (e.g.
	// { "sandbox": true } so the chat UI can badge it). Not sent to the
	// LLM — provider.toLLMMessages / anthropic / openai serializers
	// ignore it.
	Metadata map[string]any `json:"metadata,omitempty"`

	// RawAssistant preserves the exact assistant message JSON as returned by the API.
	// When sending history back to the LLM, use this instead of re-serializing from
	// parsed fields — guarantees prompt cache hits by maintaining byte-identical prefix.
	// Only set on role="assistant" messages.
	RawAssistant json.RawMessage `json:"_raw,omitempty"`
}

// TextContent returns the message's user-visible text. Falls back to
// joining the `text` parts of ContentParts when Content is empty —
// which is the shape we store for user messages that arrive with
// attachments (Content="" + ContentParts=[text, image_url, ...]).
// Without this fallback, history/preview/title code that gates on
// `Content != ""` silently drops every multimodal turn.
//
// Also strips the legacy "[Attached: <filename>]\n" breadcrumb that
// older client versions prepended to outgoing chat text. New sends no
// longer add it, but historical sessions still carry it baked into
// stored content; without stripping here the prefix shows up in chat
// bubbles, page titles, and sidebar entries.
func (m Message) TextContent() string {
	if m.Content != "" {
		return StripAttachedPrefix(m.Content)
	}
	var parts []string
	for _, p := range m.ContentParts {
		if p.Type == "text" && p.Text != "" {
			parts = append(parts, p.Text)
		}
	}
	return StripAttachedPrefix(strings.Join(parts, "\n"))
}

// StripAttachedPrefix removes one or more leading "[Attached: …]" tags
// (followed by optional whitespace / newline) from a string. Exposed
// so non-Message callers (e.g. raw store rows in session adapters)
// can apply the same cleanup.
func StripAttachedPrefix(s string) string {
	for {
		if !strings.HasPrefix(s, "[Attached:") {
			break
		}
		end := strings.IndexByte(s, ']')
		if end < 0 {
			break
		}
		s = strings.TrimLeft(s[end+1:], " \t\r\n")
	}
	return s
}

// ContentPart represents a part of multimodal content.
type ContentPart struct {
	Type     string    `json:"type"`                // "text" or "image_url"
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL represents an image URL for vision messages.
type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"` // "auto", "low", "high"
}

// ToolCall represents a function call requested by the model.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall contains the function name and arguments.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool describes a tool available to the model.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction describes a function tool.
type ToolFunction struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters"`
}

// Response is the result of a Chat call.
type Response struct {
	Content      string
	ToolCalls    []ToolCall
	Thinking     string          // model's reasoning/thinking content (extracted for memory)
	RawAssistant json.RawMessage // exact API response message JSON (for cache-safe replay)
}

// HasToolCalls returns true if the response contains tool calls.
func (r *Response) HasToolCalls() bool {
	return len(r.ToolCalls) > 0
}

// StreamChunk represents a single chunk from a streaming response.
type StreamChunk struct {
	Content   string
	ToolCalls []ToolCall
	Done      bool
	// Thinking is emitted once at message_stop (if the model produced any)
	// so callers can persist it alongside the final assistant message —
	// required so the next turn can echo content[].thinking back to
	// extended-thinking providers (Anthropic + DeepSeek /anthropic compat).
	Thinking          string
	ThinkingSignature string
	// RawAssistant is the fully-serialized assistant message in the
	// provider's wire format, emitted on the final (Done) chunk. When
	// set, callers should persist it verbatim onto Message.RawAssistant
	// instead of reconstructing — required so DeepSeek (OpenAI-compat
	// thinking mode) sees the correct top-level `reasoning_content` on
	// replay, which it does not auto-generate.
	RawAssistant json.RawMessage
}

// StreamReader reads streaming chunks from an LLM response.
type StreamReader struct {
	ch  chan StreamChunk
	err error
}

// NewStreamReader creates a new StreamReader with the given channel.
func NewStreamReader(ch chan StreamChunk) *StreamReader {
	return &StreamReader{ch: ch}
}

// Next returns the next chunk and whether more chunks are available.
func (r *StreamReader) Next() (StreamChunk, bool) {
	chunk, ok := <-r.ch
	return chunk, ok
}

// Err returns any error that occurred during streaming.
func (r *StreamReader) Err() error {
	return r.err
}

// SetErr sets the error on the stream reader.
func (r *StreamReader) SetErr(err error) {
	r.err = err
}

// Provider is the LLM provider interface.
type Provider interface {
	Chat(ctx context.Context, messages []Message, tools []Tool, model string, maxTokens int, temperature float64) (*Response, error)
	ChatStream(ctx context.Context, messages []Message, tools []Tool, model string, maxTokens int, temperature float64) (*StreamReader, error)
}

// StripProviderPrefix removes the "provider/" prefix from a model string.
// e.g. "minimax-coding-plan/MiniMax-M2.7" -> "MiniMax-M2.7"
func StripProviderPrefix(model string) string {
	if idx := strings.Index(model, "/"); idx >= 0 {
		return model[idx+1:]
	}
	return model
}

// NewProvider creates a Provider based on apiType.
// "anthropic-messages" creates an Anthropic provider, anything else creates OpenAI-compatible.
func NewProvider(apiKey, apiBase, apiType string) Provider {
	if apiType == "anthropic-messages" {
		return NewAnthropic(apiKey, apiBase)
	}
	return NewOpenAI(apiKey, apiBase)
}
