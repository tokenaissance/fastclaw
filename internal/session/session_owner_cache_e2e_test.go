package session

import (
	"context"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// e2e for commit e18620e "perf(sessions): cache resolveSessionOwner".
//
// Cloud zero-impact rationale: the cache lives entirely inside
// StoreAdapter (internal/session), which backs the Cloud dashboard's
// GET /chat/history → ListMessages and the agent loop's GetSession.
// It is a transparent in-memory optimization — same return values, only
// fewer DB lookups (LookupSessionOwner + GetUser). No response shape,
// endpoint, or auth semantics change, so Cloud has zero consumers of the
// internal caching behavior.
//
// What is pinned down:
//   - The cache is keyed by session_key and is per-StoreAdapter (per
//     owning user; the adapter serves one agent inside a UserSpace, so
//     session_key-only keying is correct — documented limitation, same
//     tradeoff as upstream).
//   - Resolving the same key twice on one adapter returns the same
//     result (cache hit) for BOTH the allow (direct child) and deny
//     (unrelated owner) cases, and GetSession + ListMessages both keep
//     working through the cached resolution.
//   - Populating the cache for one key does not contaminate a different
//     key (fresh lookup still happens) — lazy-init + per-key map.
//   - The cache is NOT shared across adapters: a stranger adapter
//     independently denies a child-owned session that a caller adapter
//     had already allowed.
//   - Missing keys keep the fail-closed fallback (caller ID) on cache
//     hits too.

func TestResolveSessionOwner_Cache_CloudPathE2E(t *testing.T) {
	db := newSessionE2EDB(t)
	defer db.Close()
	ctx := context.Background()

	const (
		callerID   = "cache_e2e_caller"
		childID    = "cache_e2e_child"
		strangerID = "cache_e2e_stranger"
		agentID    = "cache_e2e_agent"
	)
	createSessionE2EUser(t, db, callerID, "user", "")
	createSessionE2EUser(t, db, childID, "app_user", callerID)
	createSessionE2EUser(t, db, strangerID, "user", "")

	// child_key: child-owned (caller may read). child_key2: a second
	// child session to prove per-key cache isolation. stranger_key:
	// unrelated owner (must be denied).
	saveCacheSession(t, db, childID, agentID, "cache_key_child", "child turn")
	saveCacheSession(t, db, childID, agentID, "cache_key_child2", "child turn 2")
	saveCacheSession(t, db, strangerID, agentID, "cache_key_stranger", "stranger turn")

	callerAdapter := NewStoreAdapter(db, callerID)

	// 1. Allow case, twice (miss then hit): direct child resolves to the
	//    child both times, and GetSession + ListMessages both return the
	//    child's conversation through the cached owner.
	if got := callerAdapter.resolveSessionOwner(ctx, agentID, "cache_key_child"); got != childID {
		t.Fatalf("resolve child (miss) = %q, want %q", got, childID)
	}
	if got := callerAdapter.resolveSessionOwner(ctx, agentID, "cache_key_child"); got != childID {
		t.Fatalf("resolve child (cache hit) = %q, want %q", got, childID)
	}
	msgs, err := callerAdapter.GetSession(ctx, agentID, "cache_key_child")
	if err != nil || len(msgs) != 1 || msgs[0].Content != "child turn" {
		t.Fatalf("GetSession(child) after cache = %d msgs (err=%v), want child turn", len(msgs), err)
	}
	arch, err := callerAdapter.ListMessages(ctx, agentID, "cache_key_child")
	if err != nil || len(arch) != 1 || arch[0].Content != "child turn" {
		t.Fatalf("ListMessages(child) after cache = %d msgs (err=%v), want child turn", len(arch), err)
	}

	// 2. Deny case, twice (miss then hit): unrelated owner stays denied,
	//    and the read fails closed on cache hits too.
	if got := callerAdapter.resolveSessionOwner(ctx, agentID, "cache_key_stranger"); got != callerID {
		t.Fatalf("resolve stranger (miss) = %q, want deny to caller %q", got, callerID)
	}
	if got := callerAdapter.resolveSessionOwner(ctx, agentID, "cache_key_stranger"); got != callerID {
		t.Fatalf("resolve stranger (cache hit) = %q, want deny to caller %q", got, callerID)
	}
	if msgs, err := callerAdapter.GetSession(ctx, agentID, "cache_key_stranger"); err == nil && len(msgs) != 0 {
		t.Fatalf("GetSession(stranger) after cache = %d msgs, want 0 (fail closed)", len(msgs))
	}
	if arch, err := callerAdapter.ListMessages(ctx, agentID, "cache_key_stranger"); err == nil && len(arch) != 0 {
		t.Fatalf("ListMessages(stranger) after cache = %d msgs, want 0 (fail closed)", len(arch))
	}

	// 3. Per-key isolation: after child_key is cached, a DIFFERENT child
	//    key still does a fresh lookup and resolves correctly.
	if got := callerAdapter.resolveSessionOwner(ctx, agentID, "cache_key_child2"); got != childID {
		t.Fatalf("resolve child_key2 = %q, want %q (no cross-key contamination)", got, childID)
	}

	// 4. Per-adapter isolation: a stranger adapter independently denies a
	//    child-owned session even though the caller adapter allowed it.
	strangerAdapter := NewStoreAdapter(db, strangerID)
	if got := strangerAdapter.resolveSessionOwner(ctx, agentID, "cache_key_child"); got != strangerID {
		t.Fatalf("stranger resolve child = %q, want deny to stranger %q (no cross-adapter cache)", got, strangerID)
	}

	// 5. Missing key: cached fallback stays on the caller (own reads keep
	//    working on cache hits).
	if got := callerAdapter.resolveSessionOwner(ctx, agentID, "cache_key_missing"); got != callerID {
		t.Fatalf("resolve missing (miss) = %q, want caller %q", got, callerID)
	}
	if got := callerAdapter.resolveSessionOwner(ctx, agentID, "cache_key_missing"); got != callerID {
		t.Fatalf("resolve missing (cache hit) = %q, want caller %q", got, callerID)
	}
}

// saveCacheSession writes a session to both the sessions blob AND the
// session_messages archive, so GetSession (blob) and ListMessages
// (archive) both have the turn to return.
func saveCacheSession(t *testing.T, db *store.DBStore, ownerID, agentID, key, opening string) {
	t.Helper()
	ctx := context.Background()
	msg := store.SessionMessage{Role: "user", Content: opening, Timestamp: time.Now().UTC()}
	if err := db.SaveSession(ctx, ownerID, agentID, key, &store.SessionRecord{
		Channel:  "web",
		ChatID:   "chat-" + key,
		Messages: []store.SessionMessage{msg},
	}); err != nil {
		t.Fatalf("save session %s: %v", key, err)
	}
	if err := db.AppendSessionMessage(ctx, ownerID, agentID, key, msg); err != nil {
		t.Fatalf("append archive message for %s: %v", key, err)
	}
}
