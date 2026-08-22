package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestStoreLookupSessionOwnerE2E covers the store-side half of upstream
// commit 84a179f (local 35b8bb4): LookupSessionOwner + GetSessionByKey let
// a caller resolve and read a session row regardless of its user_id. The
// user-scoped GetSession is the pre-fix behavior — it misses rows owned by
// a child app_user, which is exactly the empty-dashboard bug the commit
// fixed.
func TestStoreLookupSessionOwnerE2E(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	const (
		parentID   = "store_e2e_parent"
		childID    = "store_e2e_child"
		agentID    = "store_e2e_agent"
		sessionKey = "store_e2e_key"
	)
	for _, u := range []*UserRecord{
		{ID: parentID, Username: parentID, Email: parentID + "@example.com", Role: "user", Status: "active", CreatedAt: time.Now().UTC()},
		{ID: childID, Username: childID, Email: childID + "@example.com", Role: "app_user", Status: "active", OwnerUserID: parentID, CreatedAt: time.Now().UTC()},
	} {
		if err := db.CreateUser(ctx, u); err != nil {
			t.Fatalf("create user %s: %v", u.ID, err)
		}
	}

	// Session row lives under the CHILD's user_id.
	if err := db.SaveSession(ctx, childID, agentID, sessionKey, &SessionRecord{
		Channel:  "web",
		ChatID:   "chat-1",
		Messages: []SessionMessage{{Role: "user", Content: "hi", Timestamp: time.Now().UTC()}},
	}); err != nil {
		t.Fatalf("save session: %v", err)
	}

	// LookupSessionOwner resolves the real row owner (the child).
	if got, err := db.LookupSessionOwner(ctx, agentID, sessionKey); err != nil || got != childID {
		t.Fatalf("LookupSessionOwner = %q err=%v, want %q", got, err, childID)
	}

	// GetSessionByKey reads the row without user_id scoping — this is the
	// primitive the session adapter's resolveSessionOwner uses indirectly.
	rec, err := db.GetSessionByKey(ctx, agentID, sessionKey)
	if err != nil || rec == nil {
		t.Fatalf("GetSessionByKey: rec=%v err=%v", rec, err)
	}
	if len(rec.Messages) != 1 || rec.Messages[0].Content != "hi" {
		t.Fatalf("GetSessionByKey messages = %+v, want the saved turn", rec.Messages)
	}

	// The user-scoped GetSession with the PARENT's user_id misses the
	// child-owned row — the empty-history bug the commit fixed.
	if rec, err := db.GetSession(ctx, parentID, agentID, sessionKey); err == nil || rec != nil {
		t.Fatalf("GetSession(parent) = %+v err=%v; want ErrNotFound (pre-fix behavior)", rec, err)
	}

	// The user-scoped GetSession with the CHILD's user_id (the resolved
	// owner) hits the row.
	if rec, err := db.GetSession(ctx, childID, agentID, sessionKey); err != nil || rec == nil {
		t.Fatalf("GetSession(child) = %+v err=%v, want the child-owned row", rec, err)
	}

	// Unknown key → ErrNotFound from both owner-resolution primitives.
	if _, err := db.LookupSessionOwner(ctx, agentID, "store_e2e_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LookupSessionOwner(missing) err = %v, want ErrNotFound", err)
	}
	if _, err := db.GetSessionByKey(ctx, agentID, "store_e2e_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetSessionByKey(missing) err = %v, want ErrNotFound", err)
	}
}
