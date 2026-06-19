package session

import (
	"context"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/provider"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// newSessionE2EDB opens a fresh sqlite in-memory store with the schema
// migrated. The DSN mirrors the store package's openTestDB helper; no
// other test in this package opens a DBStore, so the shared in-memory DB
// is effectively private to these e2e tests.
func newSessionE2EDB(t *testing.T) *store.DBStore {
	t.Helper()
	db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		db.Close()
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func createSessionE2EUser(t *testing.T, db *store.DBStore, id, role, ownerID string) {
	t.Helper()
	err := db.CreateUser(context.Background(), &store.UserRecord{
		ID:           id,
		Username:     id,
		Email:        id + "@example.com",
		PasswordHash: "x",
		Role:         role, // "user" (top-level) or "app_user" (child)
		Status:       "active",
		OwnerUserID:  ownerID,
		CreatedAt:    time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create user %s: %v", id, err)
	}
}

// TestSessions_CrossUserOwnerResolutionE2E covers upstream commit 84a179f
// (local 35b8bb4): GetSession / ListMessages now resolve the real session
// owner via LookupSessionOwner, so a parent user's dashboard can read
// history for sessions owned by its child app_user. Previously the
// parent-scoped query (WHERE user_id = <parent>) missed the child-owned
// row and the dashboard showed empty chat history.
func TestSessions_CrossUserOwnerResolutionE2E(t *testing.T) {
	db := newSessionE2EDB(t)
	defer db.Close()
	ctx := context.Background()

	const (
		parentID   = "sess_e2e_parent"
		childID    = "sess_e2e_child"
		agentID    = "sess_e2e_agent"
		sessionKey = "sess_e2e_key_cross"
	)
	createSessionE2EUser(t, db, parentID, "user", "")
	createSessionE2EUser(t, db, childID, "app_user", parentID) // child of the parent

	// Build the session row AND its append-only archive under the CHILD's
	// user_id — the "child app_user owns this chat" shape the dashboard
	// renders for the parent.
	if err := db.SaveSession(ctx, childID, agentID, sessionKey, &store.SessionRecord{
		Channel: "web",
		ChatID:  "chat-1",
		Messages: []store.SessionMessage{
			{Role: "user", Content: "hi from child", Timestamp: time.Now().UTC()},
			{Role: "assistant", Content: "hello", Timestamp: time.Now().UTC()},
		},
	}); err != nil {
		t.Fatalf("save session: %v", err)
	}
	if err := db.AppendSessionMessage(ctx, childID, agentID, sessionKey, store.SessionMessage{
		Role: "user", Content: "archive turn", Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("append archive: %v", err)
	}

	// The history reader is the PARENT (UserSpace owner bound to a
	// StoreAdapter); the session row belongs to its child.
	parentAdapter := NewStoreAdapter(db, parentID)

	// resolveSessionOwner must resolve the REAL row owner (the child),
	// not fall back to the adapter's bound userID (the parent).
	if got := parentAdapter.resolveSessionOwner(ctx, agentID, sessionKey); got != childID {
		t.Fatalf("resolveSessionOwner = %q, want %q (real owner, not bound user)", got, childID)
	}

	// Cross-user working-set read: the parent-bound adapter must surface
	// the child-owned messages instead of empty history.
	msgs, err := parentAdapter.GetSession(ctx, agentID, sessionKey)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("GetSession messages = %d, want 2 (cross-user read resolved the child owner)", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Content != "hi from child" {
		t.Errorf("first message = %+v, want the child's opening turn", msgs[0])
	}

	// Cross-user archive read: same resolution on the append-only path.
	arch, err := parentAdapter.ListMessages(ctx, agentID, sessionKey)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(arch) != 1 || arch[0].Content != "archive turn" {
		t.Fatalf("ListMessages = %+v, want the child's archived turn", arch)
	}

	// #39 (f94119f) security: an unrelated caller must NOT resolve the
	// row's true owner — knowing a session_key on a shared/public agent
	// must not read another user's chat. resolveSessionOwner now returns
	// the caller's own ID (deny), so the downstream user_id-scoped query
	// lands on zero rows (fail closed).
	createSessionE2EUser(t, db, "sess_e2e_stranger", "user", "")
	strangerAdapter := NewStoreAdapter(db, "sess_e2e_stranger")
	if got := strangerAdapter.resolveSessionOwner(ctx, agentID, sessionKey); got != "sess_e2e_stranger" {
		t.Fatalf("stranger resolveSessionOwner = %q, want %q (denied: not caller, not caller's child)", got, "sess_e2e_stranger")
	}
	// The denied read surfaces as empty history (or not-found), never
	// another user's chat — fail closed.
	if msgs, err := strangerAdapter.GetSession(ctx, agentID, sessionKey); err == nil && len(msgs) != 0 {
		t.Fatalf("stranger GetSession leaked %d msgs, want 0 (fail closed)", len(msgs))
	}

	// Fallback: an unknown key (new session, not yet stored) falls back to
	// the adapter's bound userID so the caller's own reads keep working.
	if got := parentAdapter.resolveSessionOwner(ctx, agentID, "sess_e2e_missing"); got != parentID {
		t.Fatalf("missing-key resolveSessionOwner = %q, want fallback to bound user %q", got, parentID)
	}
}

// TestSessions_EmptyUserIDFailClosedE2E covers upstream commit 917b206
// (local 7cfe667): NewManagerWithStoreForUser no longer panics on an
// empty userID — it logs and keeps the Manager alive so a bad request
// cannot crash the whole gateway. Against a real DB store the empty owner
// must fail closed: nothing is persisted that any real user could read.
func TestSessions_EmptyUserIDFailClosedE2E(t *testing.T) {
	db := newSessionE2EDB(t)
	defer db.Close()
	ctx := context.Background()

	const (
		agentID = "sess_e2e_empty_agent"
		realID  = "sess_e2e_real"
	)
	createSessionE2EUser(t, db, realID, "user", "")

	// Baseline: a manager bound to a real user persists + reads back,
	// proving the store-backed path works (so the empty-owner behavior
	// below is the intended fail-closed, not a broken harness). The
	// manager takes a SessionStore — the StoreAdapter is that adapter.
	realMgr := NewManagerWithStoreForUser(t.TempDir(), NewStoreAdapter(db, realID), realID, agentID)
	realS := realMgr.Get("web", "", "chat-real", "")
	realS.Append(provider.Message{Role: "user", Content: "real turn"})
	if _, err := db.LookupSessionOwner(ctx, agentID, realS.SessionKey()); err != nil {
		t.Fatalf("baseline: real-user session row missing: %v", err)
	}

	// Empty userID: the constructor must NOT panic, and must keep the
	// Manager alive.
	mgr := NewManagerWithStoreForUser(t.TempDir(), NewStoreAdapter(db, ""), "", agentID)
	if mgr == nil {
		t.Fatal("expected manager for empty userID")
	}

	// Driving a session through the empty owner must not crash.
	emptyS := mgr.Get("web", "", "chat-empty", "")
	if emptyS == nil {
		t.Fatal("expected session for empty-owner manager")
	}
	emptyS.Append(provider.Message{Role: "user", Content: "orphan turn"})

	// Fail-closed: the orphan write must NOT be readable by the real user.
	if _, err := db.GetSession(ctx, realID, agentID, emptyS.SessionKey()); err == nil {
		t.Fatal("empty-owner session leaked into real user's history")
	}
	// And no session row was created for the orphan key at all — SaveSession
	// rejects the empty user_id.
	if owner, err := db.LookupSessionOwner(ctx, agentID, emptyS.SessionKey()); err == nil {
		t.Fatalf("empty-owner session persisted under owner %q, want fail-closed", owner)
	}
}
