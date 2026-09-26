package setup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

// The owner's private memory, as bytes. Both cases below assert on its
// absence from another caller's response, so it is spelled once.
const ownerMemorySecret = "OWNER-SECRET: the operator's accumulated memory of this chatter"

// GET /api/agents/{id}/system-files/{name} is the Customize page's read.
// Its per-user half (USER.md / MEMORY.md) must draw the line the runtime
// read path already draws: internal/agent/memory_store_adapter.go reads
// MEMORY.md through the *Exact* lookup precisely because "a public-link
// visitor must not inherit the agent owner's accumulated memories".
//
// The write half draws it too — resolveSystemFileTarget sends per-user
// writes to the CALLER's row and refuses identity-file writes from a
// non-owner. These cases pin the read half: a caller who is not the
// owner gets their own row, or nothing. Never the owner's bytes, and
// never as the `baseContent` the diff view uses.
func setupSystemFilesFixture(t *testing.T) (s *Server, owner, visitor, agentID string) {
	t.Helper()
	s = setupTestServer(t)
	ctx := context.Background()
	owner, visitor = "u_owner_sysfiles", "u_visitor_sysfiles"
	for _, u := range []struct{ id, name, email string }{
		{owner, "sysfilesowner", "sysfiles-owner@test.com"},
		{visitor, "sysfilesvisitor", "sysfiles-visitor@test.com"},
	} {
		if err := s.dataStore.CreateUser(ctx, &store.UserRecord{
			ID:           u.id,
			Username:     u.name,
			Email:        u.email,
			PasswordHash: "nope",
			Role:         users.RoleUser,
			Status:       "active",
		}); err != nil {
			t.Fatalf("create user %s: %v", u.id, err)
		}
	}
	agentID = "agt_sysfiles"
	// Public: any signed-in caller reaches the endpoint (requireAgentReadable
	// takes the IsPublic branch), which is exactly the visitor case.
	if err := s.dataStore.SaveAgent(ctx, &store.AgentRecord{
		ID: agentID, UserID: owner, Name: "Sysfiles Agent", IsPublic: true,
	}); err != nil {
		t.Fatalf("save agent: %v", err)
	}
	return s, owner, visitor, agentID
}

func readSystemFile(t *testing.T, s *Server, caller, agentID, name string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetPathValue("id", agentID)
	req.SetPathValue("name", name)
	req = stampAuth(req, caller, false)
	w := httptest.NewRecorder()
	s.handleGetAgentSystemFile(w, req)
	return w.Code, w.Body.String()
}

func TestSystemFilesReadANonOwnerGetsAnEmptyMemoryNotTheOwners(t *testing.T) {
	s, owner, visitor, agentID := setupSystemFilesFixture(t)
	ctx := context.Background()
	if err := s.dataStore.SaveAgentFile(ctx, agentID, owner, "MEMORY.md", []byte(ownerMemorySecret)); err != nil {
		t.Fatalf("seed owner MEMORY.md: %v", err)
	}

	// Positive control: the owner's read is unchanged — their own row.
	code, body := readSystemFile(t, s, owner, agentID, "MEMORY.md")
	if code != http.StatusOK {
		t.Fatalf("owner read status = %d, want 200; body = %s", code, body)
	}
	var ownerResp map[string]any
	if err := json.Unmarshal([]byte(body), &ownerResp); err != nil {
		t.Fatalf("owner json: %v; body = %s", err, body)
	}
	if got, _ := ownerResp["content"].(string); got != ownerMemorySecret {
		t.Fatalf("owner content = %q, want their own row", got)
	}

	// The visitor has no row of their own: an empty memory, not the owner's.
	code, body = readSystemFile(t, s, visitor, agentID, "MEMORY.md")
	if code != http.StatusOK {
		t.Fatalf("visitor read status = %d, want 200 (an absent row is an empty file, not an error); body = %s", code, body)
	}
	var visitorResp map[string]any
	if err := json.Unmarshal([]byte(body), &visitorResp); err != nil {
		t.Fatalf("visitor json: %v; body = %s", err, body)
	}
	if strings.Contains(body, "OWNER-SECRET") {
		t.Errorf("the visitor's response carries the owner's MEMORY.md: %s", body)
	}
	if got, _ := visitorResp["content"].(string); got != "" {
		t.Errorf("visitor content = %q, want empty (they have no row)", got)
	}
	if src, _ := visitorResp["source"].(string); src != "default" {
		t.Errorf("visitor source = %q, want %q", src, "default")
	}
}

func TestSystemFilesReadANonOwnersOverrideDoesNotCarryTheOwnersBase(t *testing.T) {
	s, owner, visitor, agentID := setupSystemFilesFixture(t)
	ctx := context.Background()
	if err := s.dataStore.SaveAgentFile(ctx, agentID, owner, "MEMORY.md", []byte(ownerMemorySecret)); err != nil {
		t.Fatalf("seed owner MEMORY.md: %v", err)
	}
	const visitorMemory = "the visitor's own notes"
	if err := s.dataStore.SaveAgentFile(ctx, agentID, visitor, "MEMORY.md", []byte(visitorMemory)); err != nil {
		t.Fatalf("seed visitor MEMORY.md: %v", err)
	}

	code, body := readSystemFile(t, s, visitor, agentID, "MEMORY.md")
	if code != http.StatusOK {
		t.Fatalf("visitor read status = %d, want 200; body = %s", code, body)
	}
	if strings.Contains(body, "OWNER-SECRET") {
		t.Errorf("the visitor's own read also carries the owner's row (baseContent): %s", body)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("visitor json: %v; body = %s", err, body)
	}
	if got, _ := resp["content"].(string); got != visitorMemory {
		t.Errorf("visitor content = %q, want their own row", got)
	}
	if src, _ := resp["source"].(string); src != "db" {
		t.Errorf("visitor source = %q, want %q (their own override)", src, "db")
	}
}
