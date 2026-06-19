package store

// e2e for commit 1131744 "fix(sessions): fallback to (channel, chatID)
// when accountID changes on re-scan".
//
// Cloud zero-impact rationale: ResolveActiveSessionKey is the store read
// side of the gateway's IM session resolution — session.Manager calls it
// (manager.go:242) with the agentID + channel triple whenever an inbound
// IM message needs its existing conversation. Cloud (Next.js app) reaches
// it only through the /api/fastagent proxy's chat handlers, never
// directly. This commit only widens the session lookup: on an exact
// (user_id, agent_id, channel, account_id, chat_id) miss it falls back for
// IM channels to (channel, chat_id) ignoring account_id and widening
// user_id to include child app_users (owner_user_id) — so a bot re-scan
// (accountID change) continues the existing conversation instead of
// minting a fresh session. No endpoint, response shape, or auth change.
//
// What is pinned down:
//   - Exact (channel, accountID, chatID) match still wins.
//   - On accountID change (same channel + chatID) the session is found
//     via the fallback (bot re-scan continuity).
//   - A session created under a child app_user is found when resolving
//     with the web user (user_id IN (id OR owner_user_id)).
//   - Non-IM channels (web/api/shared) skip the fallback → exact miss
//     returns ErrNotFound.
//   - No matching session → ErrNotFound.

import (
	"context"
	"testing"
	"time"
)

func TestResolveActiveSessionKey_ChannelChatIDFallback_CloudPathE2E(t *testing.T) {
	db, err := NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("NewDBStore: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	user := &UserRecord{ID: "u_user", Username: "u", Email: "u@example.com",
		PasswordHash: "x", Role: "user", Status: "active", AgentQuota: -1,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := db.CreateUser(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	// Child app_user for the cross-owner widening case.
	child := &UserRecord{ID: "u_child", Username: "child", Email: "c@example.com",
		PasswordHash: "x", Role: "app_user", Status: "active",
		OwnerUserID: user.ID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := db.CreateUser(ctx, child); err != nil {
		t.Fatalf("create child app_user: %v", err)
	}

	now := time.Now().UTC()
	// Session 1: telegram, bot-a, openid 111 under the web user.
	if err := db.SaveSession(ctx, user.ID, "agt", "s1", &SessionRecord{
		Channel: "telegram", AccountID: "bot-a", ChatID: "111", UpdatedAt: now,
	}); err != nil {
		t.Fatalf("save session s1: %v", err)
	}
	// Session 2: wechat session created under the child app_user.
	if err := db.SaveSession(ctx, child.ID, "agt", "s2", &SessionRecord{
		Channel: "wechat", AccountID: "wx1", ChatID: "222", UpdatedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatalf("save session s2: %v", err)
	}
	// Session 3: web session (exact-key lookup path).
	if err := db.SaveSession(ctx, user.ID, "agt", "s3", &SessionRecord{
		Channel: "web", ChatID: "sess-web", UpdatedAt: now.Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("save session s3: %v", err)
	}

	// Exact (channel, accountID, chatID) match still wins.
	if k, err := db.ResolveActiveSessionKey(ctx, user.ID, "agt", "telegram", "bot-a", "111"); err != nil || k != "s1" {
		t.Fatalf("exact match = (%q, %v), want (s1, nil)", k, err)
	}

	// accountID changed (bot re-scan) — same channel + chatID → fallback.
	if k, err := db.ResolveActiveSessionKey(ctx, user.ID, "agt", "telegram", "bot-b", "111"); err != nil || k != "s1" {
		t.Fatalf("accountID-change fallback = (%q, %v), want (s1, nil)", k, err)
	}

	// Session under a child app_user found when resolving with the web
	// user (user_id IN (id OR owner_user_id)), even across accountID.
	if k, err := db.ResolveActiveSessionKey(ctx, user.ID, "agt", "wechat", "wx9", "222"); err != nil || k != "s2" {
		t.Fatalf("cross-owner fallback = (%q, %v), want (s2, nil)", k, err)
	}

	// Non-IM channels skip the fallback: web exact miss → ErrNotFound.
	if k, err := db.ResolveActiveSessionKey(ctx, user.ID, "agt", "web", "", "missing"); err != ErrNotFound {
		t.Fatalf("web exact miss = (%q, %v), want ErrNotFound (fallback skipped)", k, err)
	}

	// No matching session anywhere → ErrNotFound.
	if k, err := db.ResolveActiveSessionKey(ctx, user.ID, "agt", "telegram", "bot-z", "999"); err != ErrNotFound {
		t.Fatalf("no-session miss = (%q, %v), want ErrNotFound", k, err)
	}
}
