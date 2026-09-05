package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/config"
)

type failingEventSink struct {
	err error
}

func (f *failingEventSink) AppendSessionEvent(_ context.Context, _, _, _, _ string, _ []byte) (int64, error) {
	return 0, f.err
}

// TestEmitEventCheckedFailingSinkE2E drives the real persistence path with
// an injectable failing EventSink: the tool_result carrying the mcp undo
// marker must surface the persistence error (seq stays -1) while live
// delivery (hub + legacy channel) still happens — the loop can then
// fail loud without killing the stream.
func TestEmitEventCheckedFailingSinkE2E(t *testing.T) {
	hub := NewEventHub()
	envelopes, unsub := hub.Subscribe("u-owner", "a-agent", "s-chat")
	defer unsub()
	legacy := make(chan ChatEvent, 1)

	sinkErr := errors.New("session_events unavailable")
	ctx := ContextWithStream(context.Background(), legacy, &failingEventSink{err: sinkErr},
		hub, "u-owner", "a-agent", "s-chat")

	markerText, err := appendUndoMarker("quandora: registered.", mcpUndoPayload{
		Action: "remove", ServerName: "quandora",
	})
	if err != nil {
		t.Fatalf("marker: %v", err)
	}
	evt := ChatEvent{Type: "tool_result", Data: map[string]any{
		"id": "tc-1", "name": "mcp", "result": markerText,
	}}

	seq, persistErr := emitEventChecked(ctx, evt)
	if persistErr == nil || !errors.Is(persistErr, sinkErr) {
		t.Fatalf("persistErr = %v; want injected sink error", persistErr)
	}
	if seq != -1 {
		t.Fatalf("seq = %d; want -1 when persistence fails", seq)
	}

	// Live delivery survives: hub envelope arrives.
	select {
	case env := <-envelopes:
		if env.Event.Type != "tool_result" {
			t.Fatalf("hub event = %+v", env.Event)
		}
	default:
		t.Fatal("hub did not receive the event despite persistence failure")
	}
	// Legacy channel arrives too.
	select {
	case got := <-legacy:
		if got.Type != "tool_result" {
			t.Fatalf("legacy event = %+v", got)
		}
	default:
		t.Fatal("legacy channel did not receive the event despite persistence failure")
	}

	// The loop's fail-loud decision on this exact outcome appends the
	// warning to the model-visible result.
	updated, warned := applyUndoJournalWarning("mcp", markerText, seq, persistErr)
	if !warned || !strings.Contains(updated, mcpUndoJournalWarning) {
		t.Fatalf("fail-loud warning missing: warned=%v updated=%q", warned, updated)
	}
}

// TestApplyUndoJournalWarningDecision covers the decision matrix without a
// running agent: only mcp results carrying the undo marker are warned, and
// both a persistence error and a journal-less context (seq=-1) trigger it.
func TestApplyUndoJournalWarningDecision(t *testing.T) {
	markerText, err := appendUndoMarker("quandora: removed.", mcpUndoPayload{
		Action: "add", ServerName: "quandora",
		Config: &config.MCPServerConfig{Type: "http", URL: "https://mcp.quandora.ai/quant"},
	})
	if err != nil {
		t.Fatalf("marker: %v", err)
	}
	sinkErr := errors.New("sink down")

	cases := []struct {
		name     string
		toolName string
		result   string
		seq      int64
		jerr     error
		wantWarn bool
	}{
		{"persist error on mcp mutation", "mcp", markerText, 7, sinkErr, true},
		{"journal-less context (seq -1)", "mcp", markerText, -1, nil, true},
		{"persist ok", "mcp", markerText, 7, nil, false},
		{"non-mcp tool with marker", "mcp_quandora_quant", markerText, 7, sinkErr, false},
		{"mcp result without marker", "mcp", "quandora: registered.", 7, sinkErr, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			updated, warned := applyUndoJournalWarning(tc.toolName, tc.result, tc.seq, tc.jerr)
			if warned != tc.wantWarn {
				t.Fatalf("warned = %v; want %v", warned, tc.wantWarn)
			}
			if strings.Contains(updated, mcpUndoJournalWarning) != tc.wantWarn {
				t.Fatalf("updated result = %q", updated)
			}
		})
	}
}
