package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestToAPIMessagesDropsDanglingToolReplies covers the inverse orphan:
// "Messages with role 'tool' must be a response to a preceding message
// with 'tool_calls'". This happens when the assistant half of a pair is
// dropped (compaction/truncation) or a hung exec's late tool result lands
// out of order. The wire build must drop the dangling tool reply instead
// of shipping a request the provider rejects.
func TestToAPIMessagesDropsDanglingToolReplies(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "run it"},
		// No preceding assistant declared this id — dangling tool reply.
		{Role: "tool", ToolCallID: "call_dangling", Content: "late result"},
		{Role: "assistant", Content: "ok"},
	}
	wire := toAPIMessages(msgs)
	if len(wire) != 2 {
		t.Fatalf("wire len = %d; want 2 (dangling tool dropped)\n%+v", len(wire), wire)
	}
	var roles []string
	for _, raw := range wire {
		var am apiMessage
		if err := json.Unmarshal(raw, &am); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		roles = append(roles, am.Role)
	}
	if strings.Join(roles, ",") != "user,assistant" {
		t.Fatalf("roles after strip = %v; want user,assistant", roles)
	}
}

// TestToAPIMessagesKeepsAnsweredPairAndDropsStrayReply ensures the valid
// assistant(tool_calls)→tool pair survives while a stray tool reply after
// a later user turn is removed.
func TestToAPIMessagesKeepsAnsweredPairAndDropsStrayReply(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "run"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_ok", Type: "function", Function: FunctionCall{Name: "exec", Arguments: `{"command":"sleep 1"}`}}}},
		{Role: "tool", ToolCallID: "call_ok", Content: "done"},
		{Role: "user", Content: "again"},
		{Role: "tool", ToolCallID: "call_stray", Content: "too late"},
	}
	_, orphanTool := findOrphanToolCalls(msgs)
	if orphanTool[2] {
		t.Fatal("answered tool reply must not be flagged")
	}
	if !orphanTool[4] {
		t.Fatal("stray tool reply after user turn must be flagged")
	}
}
