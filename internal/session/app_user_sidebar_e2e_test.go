package session

import (
	"context"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// e2e for commit 05ed1b1 "include app_user sessions in sidebar and fix
// message preview lookup" — the app_user sidebar half.
//
// Mirrors the Cloud call path for the web dashboard sidebar: the Cloud
// proxy forwards GET /api/chat/sessions?agentId= for a logged-in user
// whose agent is bound to a StoreAdapter scoped to that user. #41 makes
// two changes on that path:
//
//   1. store.ListSessions now lists sessions owned by the caller OR by
//      any child app_user (owner_user_id = caller) — so IM conversations
//      routed through an API-key-provisioned app_user's channel binding
//      surface in the parent's dashboard sidebar.
//   2. session.ListWebSessions uses each session's REAL owner (m.UserID)
//      for the message/archive preview lookup — so a child-owned session
//      whose user_id differs from the listing caller renders its preview
//      instead of empty history.
//
// The handler (setup.handleChatSessions) is a thin JSON wrapper around
// resolveAgent → WebChatSessions → ListWebSessions; the owner-scoping and
// preview-resolution logic under test lives in the StoreAdapter + DBStore
// exercised here against a real sqlite store.

// TestSessions_AppUserSidebarE2E_ListIncludesChildren drives the new
// ListSessions owner-set: sessions owned by the caller AND by any child
// app_user (owner_user_id) appear; sessions owned by an unrelated top-level
// user stay hidden.
func TestSessions_AppUserSidebarE2E_ListIncludesChildren(t *testing.T) {
	db := newSessionE2EDB(t)
	defer db.Close()
	ctx := context.Background()

	const (
		parentID   = "sidebar_e2e_parent"
		childID1   = "sidebar_e2e_child1"
		childID2   = "sidebar_e2e_child2"
		strangerID = "sidebar_e2e_stranger"
		agentID    = "sidebar_e2e_agent"
	)
	createSessionE2EUser(t, db, parentID, "user", "")
	createSessionE2EUser(t, db, childID1, "app_user", parentID)
	createSessionE2EUser(t, db, childID2, "app_user", parentID)
	createSessionE2EUser(t, db, strangerID, "user", "")

	// Sessions owned by the parent itself.
	saveSidebarSession(t, db, parentID, agentID, "sbk_parent", "parent opening")
	// Sessions owned by the parent's child app_users (IM-routed via
	// api-key-provisioned channel binding).
	saveSidebarSession(t, db, childID1, agentID, "sbk_child1", "child one opening")
	saveSidebarSession(t, db, childID2, agentID, "sbk_child2", "child two opening")
	// An unrelated top-level user's session must stay out of the listing.
	saveSidebarSession(t, db, strangerID, agentID, "sbk_stranger", "stranger opening")

	metas, err := db.ListSessions(ctx, parentID, agentID)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	got := map[string]bool{}
	for _, m := range metas {
		got[m.Key] = true
	}
	for _, want := range []string{"sbk_parent", "sbk_child1", "sbk_child2"} {
		if !got[want] {
			t.Errorf("ListSessions missing %q (owned by caller or child app_user)", want)
		}
	}
	if got["sbk_stranger"] {
		t.Errorf("ListSessions leaked stranger-owned session %q into parent listing", "sbk_stranger")
	}

	// Each listed meta must carry its real owner so the adapter can scope
	// message lookups per-session (m.UserID, #41's preview fix).
	ownerByKey := map[string]string{}
	for _, m := range metas {
		ownerByKey[m.Key] = m.UserID
	}
	if ownerByKey["sbk_child1"] != childID1 {
		t.Errorf("meta.UserID for sbk_child1 = %q, want %q (real owner surfaced)", ownerByKey["sbk_child1"], childID1)
	}
	if ownerByKey["sbk_parent"] != parentID {
		t.Errorf("meta.UserID for sbk_parent = %q, want %q", ownerByKey["sbk_parent"], parentID)
	}
}

// TestSessions_AppUserSidebarE2E_WebPreviewCrossOwner drives the parent's
// dashboard listing through the real StoreAdapter: a child-owned session
// must BOTH appear in the sidebar AND resolve its preview (the owner-scoped
// message lookup uses the session's real user_id, not the listing caller's).
func TestSessions_AppUserSidebarE2E_WebPreviewCrossOwner(t *testing.T) {
	db := newSessionE2EDB(t)
	defer db.Close()
	ctx := context.Background()

	const (
		parentID  = "sidebar_e2e_parent2"
		childID   = "sidebar_e2e_child2b"
		agentID   = "sidebar_e2e_agent2"
		childKey  = "sbk_web_child"
		parentKey = "sbk_web_parent"
	)
	createSessionE2EUser(t, db, parentID, "user", "")
	createSessionE2EUser(t, db, childID, "app_user", parentID)

	// Child-owned session with row + archive (the IM-routed conversation
	// the parent's sidebar should surface with a real preview). The
	// append-only archive is authoritative for previews — its first row
	// is always the user's original opening turn — so seed it in
	// chronological order (the gateway appends every turn).
	if err := db.SaveSession(ctx, childID, agentID, childKey, &store.SessionRecord{
		Channel: "web",
		ChatID:  "chat-child",
		Messages: []store.SessionMessage{
			{Role: "user", Content: "child asked about billing", Timestamp: time.Now().UTC()},
			{Role: "assistant", Content: "here is the plan", Timestamp: time.Now().UTC()},
		},
	}); err != nil {
		t.Fatalf("save child session: %v", err)
	}
	for _, m := range []store.SessionMessage{
		{Role: "user", Content: "child asked about billing", Timestamp: time.Now().UTC()},
		{Role: "assistant", Content: "here is the plan", Timestamp: time.Now().UTC()},
		{Role: "user", Content: "child follow-up", Timestamp: time.Now().UTC()},
	} {
		if err := db.AppendSessionMessage(ctx, childID, agentID, childKey, m); err != nil {
			t.Fatalf("append child archive: %v", err)
		}
	}

	// Parent-owned session alongside it.
	if err := db.SaveSession(ctx, parentID, agentID, parentKey, &store.SessionRecord{
		Channel: "web",
		ChatID:  "chat-parent",
		Messages: []store.SessionMessage{
			{Role: "user", Content: "parent opening", Timestamp: time.Now().UTC()},
		},
	}); err != nil {
		t.Fatalf("save parent session: %v", err)
	}

	parentAdapter := NewStoreAdapter(db, parentID)
	sessions, err := parentAdapter.ListWebSessions(ctx, agentID)
	if err != nil {
		t.Fatalf("ListWebSessions: %v", err)
	}

	byKey := map[string]WebSession{}
	for _, s := range sessions {
		byKey[s.ID] = s
	}

	child, ok := byKey[childKey]
	if !ok {
		t.Fatalf("sidebar missing child-owned session %q; got keys %v", childKey, keysOf(byKey))
	}
	// #41 preview fix: the child session's preview resolves via its REAL
	// owner (childID), not the listing caller (parentID).
	if child.Preview != "child asked about billing" {
		t.Errorf("child preview = %q, want %q (owner-scoped message lookup)", child.Preview, "child asked about billing")
	}

	parent, ok := byKey[parentKey]
	if !ok {
		t.Fatalf("sidebar missing parent-owned session %q", parentKey)
	}
	if parent.Preview != "parent opening" {
		t.Errorf("parent preview = %q, want %q", parent.Preview, "parent opening")
	}
}

// saveSidebarSession writes a session row under (owner, agent) with a
// user/assistant message pair plus its append-only archive row, mirroring
// how the gateway persists a real conversation.
func saveSidebarSession(t *testing.T, db *store.DBStore, ownerID, agentID, key, opening string) {
	t.Helper()
	ctx := context.Background()
	if err := db.SaveSession(ctx, ownerID, agentID, key, &store.SessionRecord{
		Channel: "web",
		ChatID:  "chat-" + key,
		Messages: []store.SessionMessage{
			{Role: "user", Content: opening, Timestamp: time.Now().UTC()},
			{Role: "assistant", Content: "ok", Timestamp: time.Now().UTC()},
		},
	}); err != nil {
		t.Fatalf("save session %s: %v", key, err)
	}
	if err := db.AppendSessionMessage(ctx, ownerID, agentID, key, store.SessionMessage{
		Role: "user", Content: opening, Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("append archive %s: %v", key, err)
	}
}

func keysOf(m map[string]WebSession) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
