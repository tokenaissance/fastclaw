package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestToAnthropicMessagesOrphanToolUse covers the exact session shape
// that produced API error 400 "tool_use ids were found without
// tool_result blocks immediately after" — the agent's loop-detection
// path appended a system warning + a synthetic cap-reached assistant
// message between the orphan tool_use and the deferred tool_result
// pad, breaking Anthropic's "tool_result must follow immediately"
// invariant. toAnthropicMessages must strip the orphan tool_use AND
// the dangling tool_result so the wire request passes validation.
func TestToAnthropicMessagesOrphanToolUse(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "go"},
		// Orphan assistant: tool_call followed by a system message
		// instead of a tool reply — recreates the loop-detection
		// path's mid-loop break.
		{
			Role:    "assistant",
			Content: "trying tool",
			ToolCalls: []ToolCall{{
				ID:       "toolu_orphan",
				Type:     "function",
				Function: FunctionCall{Name: "write_file", Arguments: `{"path":"x"}`},
			}},
		},
		{Role: "system", Content: "Loop detected: stopping."},
		{Role: "assistant", Content: "I've reached the cap, here's what I have."},
		// Late tool_result for the orphan; would dangle on its own
		// (Anthropic 400's both "ids without results" AND "results
		// without ids" — strip them together).
		{Role: "tool", ToolCallID: "toolu_orphan", Content: "interrupted"},
	}

	_, out := toAnthropicMessages(msgs)

	// Verify: no tool_use blocks appear anywhere, AND no tool_result
	// blocks either.
	for _, am := range out {
		var blocks []any
		if json.Unmarshal(am.Content, &blocks) == nil {
			for _, b := range blocks {
				mp, ok := b.(map[string]any)
				if !ok {
					continue
				}
				if bt, _ := mp["type"].(string); bt == "tool_use" || bt == "tool_result" {
					t.Errorf("orphan %s survived wire build: %+v", bt, mp)
				}
			}
		}
	}

	// The orphan assistant's TEXT content must survive (we strip
	// tool_calls but keep "trying tool"), and the cap-reached
	// assistant must too.
	got := allText(out)
	if !strings.Contains(got, "trying tool") {
		t.Errorf("orphan assistant text dropped; out=%v", got)
	}
	if !strings.Contains(got, "reached the cap") {
		t.Errorf("post-orphan assistant text dropped; out=%v", got)
	}
}

// TestToAnthropicMessagesOrphanAssistantEmpty covers msg 127 from the
// reported session: an assistant message whose only payload was the
// orphan tool_use (content=""). Stripping the tool_use leaves nothing,
// so the whole message must be dropped — emitting an empty wire
// message trips "expected a string or a list".
func TestToAnthropicMessagesOrphanAssistantEmpty(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "go"},
		{
			Role: "assistant",
			// no Content, no Thinking, just an orphan tool_call
			ToolCalls: []ToolCall{{
				ID:       "toolu_empty_orphan",
				Type:     "function",
				Function: FunctionCall{Name: "write_file", Arguments: `{"path":"y"}`},
			}},
		},
		{Role: "system", Content: "Loop detected."},
		{Role: "assistant", Content: "bailing out"},
		{Role: "tool", ToolCallID: "toolu_empty_orphan", Content: "interrupted"},
	}

	_, out := toAnthropicMessages(msgs)
	// Should contain: user "go", assistant "bailing out". The
	// orphan-only assistant and dangling tool reply both vanish.
	if len(out) != 2 {
		t.Fatalf("expected 2 messages after orphan strip, got %d: %+v", len(out), out)
	}
	if out[0].Role != "user" || out[1].Role != "assistant" {
		t.Errorf("unexpected role sequence: %s, %s", out[0].Role, out[1].Role)
	}
}

func allText(out []anthropicMessage) string {
	var sb strings.Builder
	for _, am := range out {
		// Content can be either a bare JSON string or a block array.
		var s string
		if json.Unmarshal(am.Content, &s) == nil && s != "" {
			sb.WriteString(s)
			sb.WriteString("\n")
			continue
		}
		var blocks []any
		if json.Unmarshal(am.Content, &blocks) == nil {
			for _, b := range blocks {
				mp, ok := b.(map[string]any)
				if !ok {
					continue
				}
				if t, _ := mp["type"].(string); t == "text" {
					if txt, _ := mp["text"].(string); txt != "" {
						sb.WriteString(txt)
						sb.WriteString("\n")
					}
				}
			}
		}
	}
	return sb.String()
}
