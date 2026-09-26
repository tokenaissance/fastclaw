package agent

import (
	"context"
	"testing"
)

// The one fact this file owns: the client's id for a POST reaches the code that has
// to answer about that submission, and an absent id is a no-op (not an empty string
// someone could mistake for one).
//
// Kept after the 2026-09-26 audit: the other half of the original change (stamping
// the id onto the stored user message) was removed because its only reader was an
// MCP read contract that is still being designed. This half has a live reader — the
// `queued` event names whose submission is waiting, and the cloud web client reads
// that field to withdraw it (tokenaissance-cloud `use-chat-subscription.ts`).
func TestTheClientsTurnIDTravelsOnTheContext(t *testing.T) {
	ctx := ContextWithTurnID(context.Background(), "turn-abc")
	if got := TurnIDFromContext(ctx); got != "turn-abc" {
		t.Fatalf("TurnIDFromContext = %q, want turn-abc", got)
	}
}

func TestAnEmptyTurnIDAddsNothingToTheContext(t *testing.T) {
	base := context.Background()
	ctx := ContextWithTurnID(base, "")
	if ctx != base {
		t.Fatal("an empty id must be a no-op — the same context, not a wrapped one")
	}
	if got := TurnIDFromContext(ctx); got != "" {
		t.Fatalf("TurnIDFromContext on an untouched context = %q, want empty", got)
	}
}
