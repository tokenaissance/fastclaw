package session

import (
	"context"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// e2e for commit f94119f "restrict resolveSessionOwner to caller's own
// children" — the security half.
//
// Mirrors the Cloud call path for a cross-user session read: the dashboard
// chat page (or a crafted URL on a shared/public agent) calls
// GetSession/ListMessages, which route through StoreAdapter.resolveSessionOwner.
// #39 closes the hole that #40 opened — before it, ANY caller who knew a
// session_key could resolve the row's real owner and read its messages.
// Now the owner must be the caller OR a DIRECT child (app_user whose
// owner_user_id == caller); anything else fails closed.
//
// The allow/deny matrix below exercises the exact seam on a real DBStore.

// TestSessionOwner_CloudPathE2E_AllowDenyMatrix drives every branch of the
// #39 gate:
//   - caller owns the session        → allowed
//   - direct child app_user owns it  → allowed (parent dashboard reads child)
//   - unrelated top-level user owns  → denied (fail closed, empty read)
//   - grandchild app_user owns it    → denied (only DIRECT children)
//   - unknown session_key            → fallback to caller (own reads work)
func TestSessionOwner_CloudPathE2E_AllowDenyMatrix(t *testing.T) {
	db := newSessionE2EDB(t)
	defer db.Close()
	ctx := context.Background()

	const (
		callerID   = "deny_e2e_caller"
		childID    = "deny_e2e_child"
		strangerID = "deny_e2e_stranger"
		agentID    = "deny_e2e_agent"
	)
	createSessionE2EUser(t, db, callerID, "user", "")
	createSessionE2EUser(t, db, childID, "app_user", callerID)
	createSessionE2EUser(t, db, strangerID, "user", "")

	// Child-owned session (the IM-routed conversation the parent's
	// dashboard should read) and stranger-owned session (must be denied).
	saveOwnerDenySession(t, db, childID, agentID, "deny_key_child", "child turn")
	saveOwnerDenySession(t, db, strangerID, agentID, "deny_key_stranger", "stranger turn")

	callerAdapter := NewStoreAdapter(db, callerID)

	// 1. Caller's own session: resolution stays on the caller.
	if got := callerAdapter.resolveSessionOwner(ctx, agentID, "deny_key_child"); got != childID {
		t.Fatalf("caller→child resolveSessionOwner = %q, want %q (direct child allowed)", got, childID)
	}

	// 2. Direct child's session: parent resolves the child's real owner
	//    and reads its messages.
	if got := callerAdapter.resolveSessionOwner(ctx, agentID, "deny_key_child"); got != childID {
		t.Fatalf("child resolveSessionOwner = %q, want %q", got, childID)
	}
	msgs, err := callerAdapter.GetSession(ctx, agentID, "deny_key_child")
	if err != nil || len(msgs) != 1 || msgs[0].Content != "child turn" {
		t.Fatalf("caller GetSession(child) = %d msgs (err=%v), want the child's turn", len(msgs), err)
	}

	// 3. Unrelated top-level user's session: denied. resolveSessionOwner
	//    returns the caller's own ID so the scoped read lands on zero rows.
	strangerAdapter := NewStoreAdapter(db, strangerID)
	if got := strangerAdapter.resolveSessionOwner(ctx, agentID, "deny_key_stranger"); got != strangerID {
		t.Fatalf("owner-owns-own resolveSessionOwner = %q, want %q", got, strangerID)
	}
	if got := callerAdapter.resolveSessionOwner(ctx, agentID, "deny_key_stranger"); got != callerID {
		t.Fatalf("stranger-owner resolveSessionOwner = %q, want %q (denied: owner is not caller or caller's child)", got, callerID)
	}
	if msgs, err := callerAdapter.GetSession(ctx, agentID, "deny_key_stranger"); err == nil && len(msgs) != 0 {
		t.Fatalf("caller read stranger session = %d msgs, want 0 (fail closed)", len(msgs))
	}

	// 4. Grandchild app_user (child of the child, owner_user_id = child ≠
	//    caller): denied — only DIRECT children are permitted.
	grandchildID := "deny_e2e_grandchild"
	createSessionE2EUser(t, db, grandchildID, "app_user", childID)
	saveOwnerDenySession(t, db, grandchildID, agentID, "deny_key_grandchild", "grandchild turn")
	if got := callerAdapter.resolveSessionOwner(ctx, agentID, "deny_key_grandchild"); got != callerID {
		t.Fatalf("grandchild-owner resolveSessionOwner = %q, want %q (denied: not a direct child)", got, callerID)
	}
	if msgs, err := callerAdapter.GetSession(ctx, agentID, "deny_key_grandchild"); err == nil && len(msgs) != 0 {
		t.Fatalf("caller read grandchild session = %d msgs, want 0 (fail closed)", len(msgs))
	}

	// 5. Unknown key: falls back to the caller's own ID so the caller's
	//    fresh/new session reads keep working.
	if got := callerAdapter.resolveSessionOwner(ctx, agentID, "deny_key_missing"); got != callerID {
		t.Fatalf("missing-key resolveSessionOwner = %q, want fallback to caller %q", got, callerID)
	}
}

func saveOwnerDenySession(t *testing.T, db *store.DBStore, ownerID, agentID, key, opening string) {
	t.Helper()
	ctx := context.Background()
	if err := db.SaveSession(ctx, ownerID, agentID, key, &store.SessionRecord{
		Channel:  "web",
		ChatID:   "chat-" + key,
		Messages: []store.SessionMessage{{Role: "user", Content: opening, Timestamp: time.Now().UTC()}},
	}); err != nil {
		t.Fatalf("save session %s: %v", key, err)
	}
}
