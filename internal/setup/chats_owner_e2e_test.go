package setup

// e2e for commit 00dc3ff "chats owner externalId".
//
// The upstream delta this commit carries is small and precise: the
// /api/chats (user) and /api/admin/chats (admin) responses gain an
// `ownerExternalId` field, and the previously-unguarded
// `chatterExternalId` now only appears when the chatter account actually
// has an ExternalID. Both are JSON-omit-when-empty guards — the tests
// below drive the real handlers through the same call path the Cloud
// proxy exercises (handleChats via session identity, handleAdminChats
// via the admin list), and assert exactly those two behaviors.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

// chatsOwnerE2EServer builds a Server with accounts wired (handleChats /
// handleAdminChats call resolveOwner / accounts.List, so the test must
// mirror NewServer's SetStore path instead of relying on the bare
// setupTestServer).
func chatsOwnerE2EServer(t *testing.T) (*Server, *users.Accounts) {
	t.Helper()
	s := setupTestServer(t)
	accts, err := users.NewAccounts(s.dataStore)
	if err != nil {
		t.Fatalf("NewAccounts: %v", err)
	}
	s.accounts = accts
	return s, accts
}

// createChatsUser creates a user with the given identity fields so the
// owner/chatter resolution has real rows to read.
func createChatsUser(t *testing.T, accts *users.Accounts, username, extID, display string) *users.Account {
	t.Helper()
	acct, err := accts.Create(context.Background(), users.CreateInput{
		Username:    username,
		Email:       username + "@example.test",
		Password:    "password",
		ExternalID:  extID,
		DisplayName: display,
		Role:        users.RoleUser,
	})
	if err != nil {
		t.Fatalf("create user %s: %v", username, err)
	}
	return acct
}

// seedOwnerSession writes a sessions row for (owner, agent) whose
// chatter_user_id resolves to chatter, tagging ctx the way the agent
// loop does.
func seedOwnerSession(t *testing.T, s *Server, owner, agent, key, chatter string, msgs []store.SessionMessage) {
	t.Helper()
	ctx := store.WithChatterUserID(context.Background(), chatter)
	if err := s.dataStore.SaveSession(ctx, owner, agent, key, &store.SessionRecord{
		Channel:  "web",
		ChatID:   key,
		Messages: msgs,
	}); err != nil {
		t.Fatalf("seed session %s: %v", key, err)
	}
	if len(msgs) > 0 {
		// Admin list derives preview from the session_messages archive.
		if err := s.dataStore.AppendSessionMessage(ctx, owner, agent, key, msgs[0]); err != nil {
			t.Fatalf("append message for %s: %v", key, err)
		}
	}
}

// chatEntry is the shape both handlers emit per session.
type chatEntry map[string]any

func decodeChats(t *testing.T, body []byte) []chatEntry {
	t.Helper()
	var resp struct {
		Sessions []chatEntry `json:"sessions"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode /api/chats response: %v\nbody: %s", err, body)
	}
	return resp.Sessions
}

func findByID(t *testing.T, entries []chatEntry, id string) chatEntry {
	t.Helper()
	for _, e := range entries {
		if e["id"] == id {
			return e
		}
	}
	t.Fatalf("session %s not in response: %+v", id, entries)
	return nil
}

// ---------------------------------------------------------------------------
// User path — handleChats (GET /api/chats, session identity)
// ---------------------------------------------------------------------------

func TestChats_CloudPathE2E_OwnerExternalID(t *testing.T) {
	s, accts := chatsOwnerE2EServer(t)

	owner := createChatsUser(t, accts, "chats_owner", "owner_ext_1", "Owner One")
	chatterExt := createChatsUser(t, accts, "chatter_ext", "chatter_ext_1", "Chatter Ext")
	chatterNoExt := createChatsUser(t, accts, "chatter_noext", "", "Chatter NoExt")

	ctx := context.Background()
	if err := s.dataStore.SaveAgent(ctx, &store.AgentRecord{
		ID:     "agent_chats_e2e",
		UserID: owner.ID,
		Name:   "Chats E2E Agent",
	}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	// ListWebSessions skips empty-preview sessions, so every session gets
	// a real first user message (same as the admin path below).
	msgs := []store.SessionMessage{{Role: "user", Content: "hello world"}}
	// Session with a chatter that has an ExternalID.
	seedOwnerSession(t, s, owner.ID, "agent_chats_e2e", "sess_ext", chatterExt.ID, msgs)
	// Session whose chatter has no ExternalID — the guard this commit adds.
	seedOwnerSession(t, s, owner.ID, "agent_chats_e2e", "sess_noext", chatterNoExt.ID, msgs)

	req := httptest.NewRequest(http.MethodGet, "/api/chats", nil)
	req = stampAuth(req, owner.ID, false)

	w := httptest.NewRecorder()
	s.handleChats(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	ext := findByID(t, decodeChats(t, w.Body.Bytes()), "sess_ext")
	if ext["chatterUserId"] != chatterExt.ID {
		t.Errorf("sess_ext chatterUserId = %v, want %s", ext["chatterUserId"], chatterExt.ID)
	}
	if ext["chatterExternalId"] != "chatter_ext_1" {
		t.Errorf("sess_ext chatterExternalId = %v, want %q", ext["chatterExternalId"], "chatter_ext_1")
	}
	if ext["chatterDisplayName"] != "Chatter Ext" {
		t.Errorf("sess_ext chatterDisplayName = %v, want %q", ext["chatterDisplayName"], "Chatter Ext")
	}
	if ext["ownerExternalId"] != "owner_ext_1" {
		t.Errorf("sess_ext ownerExternalId = %v, want %q", ext["ownerExternalId"], "owner_ext_1")
	}
	if ext["ownerDisplayName"] != "Owner One" {
		t.Errorf("sess_ext ownerDisplayName = %v, want %q", ext["ownerDisplayName"], "Owner One")
	}
	if ext["ownerUsername"] != "chats_owner" {
		t.Errorf("sess_ext ownerUsername = %v, want %q", ext["ownerUsername"], "chats_owner")
	}

	noext := findByID(t, decodeChats(t, w.Body.Bytes()), "sess_noext")
	if noext["chatterUserId"] != chatterNoExt.ID {
		t.Errorf("sess_noext chatterUserId = %v, want %s", noext["chatterUserId"], chatterNoExt.ID)
	}
	if _, present := noext["chatterExternalId"]; present {
		t.Errorf("sess_noext chatterExternalId present (%v), want absent (ExternalID == \"\")", noext["chatterExternalId"])
	}
	if noext["chatterDisplayName"] != "Chatter NoExt" {
		t.Errorf("sess_noext chatterDisplayName = %v, want %q", noext["chatterDisplayName"], "Chatter NoExt")
	}
}

// ---------------------------------------------------------------------------
// Admin path — handleAdminChats (admin chats list)
// ---------------------------------------------------------------------------

func TestAdminChats_CloudPathE2E_OwnerExternalID(t *testing.T) {
	s, accts := chatsOwnerE2EServer(t)

	owner := createChatsUser(t, accts, "admin_chats_owner", "owner_ext_2", "Admin Owner")
	chatterExt := createChatsUser(t, accts, "admin_chatter_ext", "chatter_ext_2", "Admin Chatter")
	chatterNoExt := createChatsUser(t, accts, "admin_chatter_noext", "", "Admin Chatter NoExt")

	ctx := context.Background()
	if err := s.dataStore.SaveAgent(ctx, &store.AgentRecord{
		ID:     "agent_admin_chats_e2e",
		UserID: owner.ID,
		Name:   "Admin Chats E2E Agent",
	}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	// Both sessions need a real first user message — handleAdminChats
	// skips sessions whose preview is empty.
	msgs := []store.SessionMessage{{Role: "user", Content: "hello from chatter"}}
	seedOwnerSession(t, s, owner.ID, "agent_admin_chats_e2e", "admin_sess_ext", chatterExt.ID, msgs)
	seedOwnerSession(t, s, owner.ID, "agent_admin_chats_e2e", "admin_sess_noext", chatterNoExt.ID, msgs)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/chats", nil)
	req = stampSuperAdmin(req, owner.ID)

	w := httptest.NewRecorder()
	s.handleAdminChats(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	ext := findByID(t, decodeChats(t, w.Body.Bytes()), "admin_sess_ext")
	if ext["chatterExternalId"] != "chatter_ext_2" {
		t.Errorf("admin_sess_ext chatterExternalId = %v, want %q", ext["chatterExternalId"], "chatter_ext_2")
	}
	if ext["ownerExternalId"] != "owner_ext_2" {
		t.Errorf("admin_sess_ext ownerExternalId = %v, want %q", ext["ownerExternalId"], "owner_ext_2")
	}

	noext := findByID(t, decodeChats(t, w.Body.Bytes()), "admin_sess_noext")
	if _, present := noext["chatterExternalId"]; present {
		t.Errorf("admin_sess_noext chatterExternalId present (%v), want absent", noext["chatterExternalId"])
	}
	if _, present := noext["ownerExternalId"]; !present {
		t.Errorf("admin_sess_noext ownerExternalId absent, want %q", "owner_ext_2")
	}
}
