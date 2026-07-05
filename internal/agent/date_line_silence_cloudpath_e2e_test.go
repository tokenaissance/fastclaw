package agent

// e2e for commit 150dbb0 "Stop system prompt from nudging replies to open
// with the timestamp".
//
// Cloud zero-impact rationale: the change is a single sentence in
// buildDateLine, which renders inside the "Current date/time:" anchor of
// the system prompt assembled server-side by the FastAgent gateway
// (ContextBuilder.BuildSystemPromptAs) and injected into every model turn.
// Cloud (Next.js app) reaches the prompt only through the /api/fastagent
// proxy's session/chat handlers — it never assembles the prompt itself and
// this commit changed no endpoint, response shape, or auth. Only the
// textual content of one prompt sentence changed.
//
// What is pinned down (mirrors the Cloud call path — real DBStore + real
// MemoryStoreAdapter wired exactly as loop.go reloadWorkspaceFiles wires
// a.ctxBuilder, then BuildSystemPromptAs):
//   - The "Current date/time:" anchor is still rendered (the time anchor
//     itself is unchanged — #4 only adds silence guidance).
//   - The new sentence is present: the model is told the timestamp is
//     "silent background context ... not something to report", must not
//     open or pepper replies with the current date/time/day-of-week, with
//     the two carve-outs (chatter directly asked, or precise time
//     materially relevant).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

func TestDateLine_Silence_CloudPathE2E(t *testing.T) {
	db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("NewDBStore: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	now := time.Now().UTC()
	owner := &store.UserRecord{ID: "u_owner", Username: "owner", Email: "o@example.com",
		PasswordHash: "x", Role: "user", Status: "active", AgentQuota: -1,
		CreatedAt: now, UpdatedAt: now}
	if err := db.CreateUser(ctx, owner); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	ag := &store.AgentRecord{ID: "agt_dt", UserID: owner.ID, Name: "拽姐",
		IsPublic: true, CreatedAt: now, UpdatedAt: now}
	if err := db.SaveAgent(ctx, ag); err != nil {
		t.Fatalf("save agent: %v", err)
	}

	cb := NewContextBuilder("", NewMemory(""), "")
	cb.store = NewMemoryStoreAdapter(db)
	cb.agentID = "agt_dt"
	cb.userID = owner.ID
	cb.SetDisplayName("拽姐")

	p := cb.BuildSystemPromptAs(owner.ID, NewMemory(""))

	// The time anchor still renders.
	if !strings.Contains(p, "Current date/time:") {
		t.Errorf("date anchor missing from prompt")
	}
	// The new silence guidance is present, verbatim.
	for _, want := range []string{
		"silent background context for your own reasoning, not something to report",
		"do NOT open or pepper your reply with the",
		"unless the chatter directly asked what time/day it is",
		"or the precise time is materially relevant to the answer",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing silence-guidance %q", want)
		}
	}
}
