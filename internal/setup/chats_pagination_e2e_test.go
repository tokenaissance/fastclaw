package setup

// e2e for commit c3a2510 "feat(api): pagination /api/chats + /api/admin/chats".
//
// Cloud zero-impact rationale: the Cloud proxy's ROUTE_ACCESS
// (src/app/api/fastagent/route-access.ts) only covers per-agent chat
// endpoints — chat/history, chat/sessions, chat/todo, chat/stream,
// chat/steer, chat/subscribe, chat/sessions/{id} — and there is NO
// /api/chats (plural) entry nor any client reference to it. This commit's
// pagination params (?page / ?pageSize), response metadata
// (page/pageSize/total/totalPages), and reverse-chronological ordering
// therefore have zero Cloud consumers; the tests below drive the real
// handlers (handleChats via session identity, handleAdminChats via the
// super-admin list) along the exact call path the Cloud proxy would
// exercise if it proxied these endpoints.
//
// What is pinned down:
//   - ListSessionsPaginated store layer: COUNT + LIMIT/OFFSET pages
//     ordered by updated_at DESC, and the agentIDs nil (all) / scoped /
//     empty (no rows) semantics.
//   - handleChats: ?page / ?pageSize parsing + clamps (page<1→1,
//     pageSize<1||>100→30), per-page metas enriched through
//     BuildWebSession, response carries pagination metadata.
//   - handleAdminChats: the same pagination through the fork's hybrid
//     batch path (ListSessionsPaginated meta source + batch preview /
//     owner maps).
//   - BuildWebSession: the extraction of single-meta → WebSession, and
//     nil for empty-preview sessions.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/session"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// chatsPage is the full /api/chats response: per-session entries plus the
// pagination metadata this commit adds.
type chatsPage struct {
	Sessions   []chatEntry `json:"sessions"`
	Page       int         `json:"page"`
	PageSize   int         `json:"pageSize"`
	Total      int         `json:"total"`
	TotalPages int         `json:"totalPages"`
}

func decodeChatsPage(t *testing.T, body []byte) chatsPage {
	t.Helper()
	var p chatsPage
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("decode /api/chats page: %v\nbody: %s", err, body)
	}
	return p
}

// sessionKeys flattens a page to its ordered session ids for compact
// ordering assertions.
func sessionKeys(entries []chatEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i], _ = e["id"].(string)
	}
	return out
}

// ---------------------------------------------------------------------------
// Store layer — ListSessionsPaginated
// ---------------------------------------------------------------------------

func TestListSessionsPaginated_StoreLayer(t *testing.T) {
	s := setupTestServer(t)
	ctx := context.Background()

	if err := s.dataStore.SaveAgent(ctx, &store.AgentRecord{ID: "agent_page_a", UserID: "owner_page", Name: "A"}); err != nil {
		t.Fatalf("seed agent A: %v", err)
	}
	if err := s.dataStore.SaveAgent(ctx, &store.AgentRecord{ID: "agent_page_b", UserID: "owner_page", Name: "B"}); err != nil {
		t.Fatalf("seed agent B: %v", err)
	}

	msgs := []store.SessionMessage{{Role: "user", Content: "hello"}}
	seedSession := func(agent, key string) {
		t.Helper()
		if err := s.dataStore.SaveSession(ctx, "owner_page", agent, key, &store.SessionRecord{
			Channel:  "web",
			ChatID:   key,
			Messages: msgs,
		}); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
		// Stagger updated_at so ORDER BY updated_at DESC is deterministic.
		time.Sleep(2 * time.Millisecond)
	}

	// Seed oldest → newest; the page query must return newest first.
	seedSession("agent_page_a", "a1")
	seedSession("agent_page_a", "a2")
	seedSession("agent_page_a", "a3")
	seedSession("agent_page_b", "b1")

	// nil agentIDs = admin view: every session, newest first.
	metas, total, err := s.dataStore.ListSessionsPaginated(ctx, nil, 0, 100)
	if err != nil {
		t.Fatalf("ListSessionsPaginated(nil): %v", err)
	}
	if total != 4 {
		t.Errorf("nil total = %d, want 4", total)
	}
	wantOrder := []string{"b1", "a3", "a2", "a1"}
	if len(metas) != len(wantOrder) {
		t.Fatalf("nil len = %d, want %d", len(metas), len(wantOrder))
	}
	for i, want := range wantOrder {
		if metas[i].Key != want {
			t.Errorf("metas[%d].Key = %q, want %q (updated_at DESC)", i, metas[i].Key, want)
		}
	}
	// AgentID is populated on the meta row by ListSessionsPaginated.
	if metas[0].AgentID != "agent_page_b" {
		t.Errorf("metas[0].AgentID = %q, want agent_page_b", metas[0].AgentID)
	}

	// Scoped to agent A, page 1 of 2.
	scoped, totalA, err := s.dataStore.ListSessionsPaginated(ctx, []string{"agent_page_a"}, 0, 2)
	if err != nil {
		t.Fatalf("ListSessionsPaginated(scoped): %v", err)
	}
	if totalA != 3 {
		t.Errorf("scoped total = %d, want 3", totalA)
	}
	if len(scoped) != 2 {
		t.Fatalf("scoped len = %d, want 2 (pageSize=2)", len(scoped))
	}
	if scoped[0].Key != "a3" || scoped[1].Key != "a2" {
		t.Errorf("page1 keys = [%s %s], want [a3 a2]", scoped[0].Key, scoped[1].Key)
	}

	// Second page of the scoped query.
	page2, _, err := s.dataStore.ListSessionsPaginated(ctx, []string{"agent_page_a"}, 2, 2)
	if err != nil {
		t.Fatalf("ListSessionsPaginated(page2): %v", err)
	}
	if len(page2) != 1 || page2[0].Key != "a1" {
		t.Errorf("page2 = %+v, want single a1", page2)
	}

	// Empty (non-nil) agentIDs = no sessions.
	empty, emptyTotal, err := s.dataStore.ListSessionsPaginated(ctx, []string{}, 0, 100)
	if err != nil {
		t.Fatalf("ListSessionsPaginated(empty): %v", err)
	}
	if empty != nil || emptyTotal != 0 {
		t.Errorf("empty = (%v, %d), want (nil, 0)", empty, emptyTotal)
	}
}

// ---------------------------------------------------------------------------
// User path — handleChats (GET /api/chats, session identity)
// ---------------------------------------------------------------------------

func TestChats_CloudPathE2E_Pagination(t *testing.T) {
	s, accts := chatsOwnerE2EServer(t)
	owner := createChatsUser(t, accts, "page_owner", "page_ext", "Page Owner")
	ctx := context.Background()
	if err := s.dataStore.SaveAgent(ctx, &store.AgentRecord{ID: "agent_page_e2e", UserID: owner.ID, Name: "Page Agent"}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	// Each session needs a real first user message — BuildWebSession
	// drops sessions whose preview is empty.
	msgs := []store.SessionMessage{{Role: "user", Content: "page preview"}}
	seedOwnerSession(t, s, owner.ID, "agent_page_e2e", "sess_p1", owner.ID, msgs)
	time.Sleep(2 * time.Millisecond)
	seedOwnerSession(t, s, owner.ID, "agent_page_e2e", "sess_p2", owner.ID, msgs)
	time.Sleep(2 * time.Millisecond)
	seedOwnerSession(t, s, owner.ID, "agent_page_e2e", "sess_p3", owner.ID, msgs)

	get := func(query string) chatsPage {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/chats"+query, nil)
		req = stampAuth(req, owner.ID, false)
		w := httptest.NewRecorder()
		s.handleChats(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /api/chats%s status = %d; body = %s", query, w.Code, w.Body.String())
		}
		return decodeChatsPage(t, w.Body.Bytes())
	}

	page1 := get("?page=1&pageSize=2")
	if page1.Total != 3 || page1.Page != 1 || page1.PageSize != 2 || page1.TotalPages != 2 {
		t.Errorf("page1 meta = %+v, want {total:3 page:1 pageSize:2 totalPages:2}", page1)
	}
	if got := sessionKeys(page1.Sessions); len(got) != 2 || got[0] != "sess_p3" || got[1] != "sess_p2" {
		t.Errorf("page1 ids = %v, want [sess_p3 sess_p2]", got)
	}
	if page1.Sessions[0]["preview"] != "page preview" {
		t.Errorf("page1 preview = %v, want %q", page1.Sessions[0]["preview"], "page preview")
	}

	page2 := get("?page=2&pageSize=2")
	if got := sessionKeys(page2.Sessions); len(got) != 1 || got[0] != "sess_p1" {
		t.Errorf("page2 ids = %v, want [sess_p1]", got)
	}
	if page2.Total != 3 || page2.TotalPages != 2 {
		t.Errorf("page2 meta = %+v, want total 3 totalPages 2", page2)
	}

	// pageSize > 100 clamps to the default 30 → all rows on one page.
	big := get("?pageSize=999")
	if big.PageSize != 30 {
		t.Errorf("pageSize clamp = %d, want 30", big.PageSize)
	}
	if len(big.Sessions) != 3 || big.TotalPages != 1 {
		t.Errorf("big = %+v, want all 3 sessions, totalPages 1", big)
	}

	// page < 1 clamps to page 1.
	zero := get("?page=0")
	if zero.Page != 1 {
		t.Errorf("page=0 → page %d, want 1", zero.Page)
	}
}

// ---------------------------------------------------------------------------
// Admin path — handleAdminChats (super-admin list)
// ---------------------------------------------------------------------------

func TestAdminChats_CloudPathE2E_Pagination(t *testing.T) {
	s, accts := chatsOwnerE2EServer(t)
	owner := createChatsUser(t, accts, "admin_page_owner", "admin_page_ext", "Admin Page Owner")
	ctx := context.Background()
	if err := s.dataStore.SaveAgent(ctx, &store.AgentRecord{ID: "agent_admin_page", UserID: owner.ID, Name: "Admin Page Agent"}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	msgs := []store.SessionMessage{{Role: "user", Content: "admin page preview"}}
	seedOwnerSession(t, s, owner.ID, "agent_admin_page", "adm_p1", owner.ID, msgs)
	time.Sleep(2 * time.Millisecond)
	seedOwnerSession(t, s, owner.ID, "agent_admin_page", "adm_p2", owner.ID, msgs)
	time.Sleep(2 * time.Millisecond)
	seedOwnerSession(t, s, owner.ID, "agent_admin_page", "adm_p3", owner.ID, msgs)

	get := func(query string) chatsPage {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/admin/chats"+query, nil)
		req = stampSuperAdmin(req, owner.ID)
		w := httptest.NewRecorder()
		s.handleAdminChats(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /api/admin/chats%s status = %d; body = %s", query, w.Code, w.Body.String())
		}
		return decodeChatsPage(t, w.Body.Bytes())
	}

	// The hybrid path paginates the meta source via ListSessionsPaginated
	// (nil agentIDs = admin sees everything) while enriching each page
	// with the fork's batch preview/owner maps.
	page1 := get("?page=1&pageSize=2")
	if page1.Total != 3 || page1.Page != 1 || page1.PageSize != 2 || page1.TotalPages != 2 {
		t.Errorf("page1 meta = %+v, want {total:3 page:1 pageSize:2 totalPages:2}", page1)
	}
	if got := sessionKeys(page1.Sessions); len(got) != 2 || got[0] != "adm_p3" || got[1] != "adm_p2" {
		t.Errorf("page1 ids = %v, want [adm_p3 adm_p2]", got)
	}
	if page1.Sessions[0]["preview"] != "admin page preview" {
		t.Errorf("page1 preview = %v, want %q", page1.Sessions[0]["preview"], "admin page preview")
	}

	page2 := get("?page=2&pageSize=2")
	if got := sessionKeys(page2.Sessions); len(got) != 1 || got[0] != "adm_p1" {
		t.Errorf("page2 ids = %v, want [adm_p1]", got)
	}
}

// ---------------------------------------------------------------------------
// BuildWebSession extraction (the #38 refactor pulled this out of
// ListWebSessions) — nil for sessions with no displayable user turn.
// ---------------------------------------------------------------------------

func TestBuildWebSession_EmptyPreviewNil(t *testing.T) {
	s := setupTestServer(t)
	ctx := context.Background()
	agentID := "agent_bw_s"
	if err := s.dataStore.SaveAgent(ctx, &store.AgentRecord{ID: agentID, UserID: "bw_owner", Name: "BW"}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	noUser := "bw_no_user"
	if err := s.dataStore.SaveSession(ctx, "bw_owner", agentID, noUser, &store.SessionRecord{
		Channel: "web",
		ChatID:  noUser,
		Messages: []store.SessionMessage{
			{Role: "assistant", Content: "assistant only"},
		},
	}); err != nil {
		t.Fatalf("seed %s: %v", noUser, err)
	}

	adapter := session.NewStoreAdapter(s.dataStore, "bw_owner")
	metas, _, err := s.dataStore.ListSessionsPaginated(ctx, []string{agentID}, 0, 10)
	if err != nil {
		t.Fatalf("ListSessionsPaginated: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("metas = %d, want 1", len(metas))
	}
	if ws := adapter.BuildWebSession(ctx, metas[0]); ws != nil {
		t.Errorf("BuildWebSession = %+v, want nil (no displayable user turn)", ws)
	}
}
