package agent

// e2e for commit 8e3625c "fix(context): use agent ID not name for
// GetAgentFile fallback lookup".
//
// Cloud zero-impact rationale: ctxBuilder.agentID feeds the store reads of
// the identity files (SOUL.md / IDENTITY.md / AGENTS / BOOTSTRAP / HEARTBEAT
// / TOOLS) that the gateway's agent runtime injects into the system prompt.
// Cloud (Next.js app) reaches it only through the /api/fastagent proxy's
// session/chat handlers, never directly. The bug: reloadWorkspaceFiles
// (loop.go) set ctxBuilder.agentID to a.name (the operator-given display
// name, e.g. "拽姐") instead of a.agentID (the DB id, e.g. "agt_xxx").
// GetAgentFile's owner-fallback subquery `SELECT user_id FROM agents WHERE
// id = ?` then matched nothing, so when the UserSpace owner differs from
// the agent owner (app_user channels) the owner's customized SOUL.md /
// IDENTITY.md were silently dropped from the system prompt. The fix wires
// the DB id, restoring the owner-fallback overlay. No endpoint, response
// shape, or auth change.
//
// What is pinned down:
//   - With ctxBuilder.agentID = the DB id, a chatter who has no row of
//     their own still inherits the agent owner's SOUL.md (owner-fallback
//     overlay works via the agents.id subquery).
//   - With ctxBuilder.agentID = the display name (the bug), that same
//     chatter gets an empty identity file (owner-fallback misses).
//   - A chatter's own SOUL.md row still wins over the owner's (sort key 0).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

func TestAgentFile_AgentIDFallback_CloudPathE2E(t *testing.T) {
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
	// Agent owner — the web user who created the agent.
	owner := &store.UserRecord{ID: "u_owner", Username: "owner", Email: "o@example.com",
		PasswordHash: "x", Role: "user", Status: "active", AgentQuota: -1,
		CreatedAt: now, UpdatedAt: now}
	if err := db.CreateUser(ctx, owner); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	// Chatter app_user — different UserSpace owner than the agent owner.
	chatter := &store.UserRecord{ID: "u_chatter", Username: "chatter", Email: "c@example.com",
		PasswordHash: "x", Role: "app_user", Status: "active",
		OwnerUserID: owner.ID, CreatedAt: now, UpdatedAt: now}
	if err := db.CreateUser(ctx, chatter); err != nil {
		t.Fatalf("create chatter: %v", err)
	}

	// Agent row: DB id "agt_1", operator-given name "拽姐".
	ag := &store.AgentRecord{ID: "agt_1", UserID: owner.ID, Name: "拽姐",
		IsPublic: true, CreatedAt: now, UpdatedAt: now}
	if err := db.SaveAgent(ctx, ag); err != nil {
		t.Fatalf("save agent: %v", err)
	}

	// Owner's customized identity file, stored at (agent_id, owner_user_id).
	soul := "我是拽姐，一个帮你处理事务的智能体。"
	if err := db.SaveAgentFile(ctx, "agt_1", owner.ID, "SOUL.md", []byte(soul)); err != nil {
		t.Fatalf("save owner SOUL.md: %v", err)
	}

	// --- Fixed path: ctxBuilder.agentID = the DB id (a.agentID) ---
	// Wired exactly as reloadWorkspaceFiles does (loop.go): store + agentID
	// + userID all pinned to the DB-backed identity store.
	cbFixed := NewContextBuilder("", nil, "")
	cbFixed.store = NewMemoryStoreAdapter(db)
	cbFixed.agentID = "agt_1"
	cbFixed.userID = owner.ID
	got := cbFixed.loadFileForUser("SOUL.md", chatter.ID)
	if strings.TrimSpace(got) != soul {
		t.Errorf("fixed path: SOUL.md for chatter = %q, want owner content %q", got, soul)
	}

	// --- Buggy path: ctxBuilder.agentID = the display name (a.name) ---
	// Same chatter, but the owner-fallback subquery now misses (no agents
	// row has id = '拽姐'), so the owner's SOUL.md is silently dropped.
	cbBuggy := NewContextBuilder("", nil, "")
	cbBuggy.store = NewMemoryStoreAdapter(db)
	cbBuggy.agentID = "拽姐"
	cbBuggy.userID = owner.ID
	gotBug := cbBuggy.loadFileForUser("SOUL.md", chatter.ID)
	if strings.TrimSpace(gotBug) != "" {
		t.Errorf("buggy path: SOUL.md for chatter = %q, want empty (owner-fallback miss)", gotBug)
	}

	// --- Chatter's own row still wins over the owner's (sort key 0) ---
	chatterSoul := "我是这位访客自己的 SOUL。"
	if err := db.SaveAgentFile(ctx, "agt_1", chatter.ID, "SOUL.md", []byte(chatterSoul)); err != nil {
		t.Fatalf("save chatter SOUL.md: %v", err)
	}
	gotOwn := cbFixed.loadFileForUser("SOUL.md", chatter.ID)
	if strings.TrimSpace(gotOwn) != chatterSoul {
		t.Errorf("chatter own row = %q, want %q", gotOwn, chatterSoul)
	}

	// --- Store-level contract: GetAgentFile owner-fallback by DB id ---
	if data, err := db.GetAgentFile(ctx, "agt_1", chatter.ID, "SOUL.md"); err != nil {
		t.Errorf("GetAgentFile(DB id) err = %v", err)
	} else if strings.TrimSpace(string(data)) != chatterSoul {
		t.Errorf("GetAgentFile(DB id) = %q, want chatter own row %q", data, chatterSoul)
	}
}
