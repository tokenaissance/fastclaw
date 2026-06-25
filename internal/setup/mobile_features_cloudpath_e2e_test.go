package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

// ---------------------------------------------------------------------------
// Cloud-path E2E for commit #11 (0593999) "feat(mobile): support public
// discovery and team chat routing".
//
// These mirror the Cloud call path exercised by the mobile app: real handlers
// + real SQLite-in-memory DBStore, no mocks for the store boundary. The
// /api/public/agents and /api/push/devices endpoints are reachable through the
// Cloud proxy only if a client explicitly requests them (no Cloud client
// does today), so these pin behavior against the fork's actual schema.
// ---------------------------------------------------------------------------

func TestPublicAgents_CloudPathE2E(t *testing.T) {
	s := setupTestServer(t)
	ctx := context.Background()

	uid := "user_public_e2e"
	if err := s.dataStore.CreateUser(ctx, &store.UserRecord{
		ID: uid, Username: "publice2e", Email: "public@test.com",
		PasswordHash: "x", Role: users.RoleUser, Status: "active",
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Public agent with full metadata.
	if err := s.dataStore.SaveAgent(ctx, &store.AgentRecord{
		ID: "agt_pub_full", UserID: uid, Name: "Public Helper", IsPublic: true,
		Config: map[string]any{
			"description": "A public helper agent",
			"category":    "utility",
		},
	}); err != nil {
		t.Fatalf("save public agent: %v", err)
	}
	// Public agent with specialty fallback (category empty → specialty).
	if err := s.dataStore.SaveAgent(ctx, &store.AgentRecord{
		ID: "agt_pub_spec", UserID: uid, Name: "Specialty Agent", IsPublic: true,
		Config: map[string]any{
			"description": "Specialty fallback",
			"specialty":   "research",
		},
	}); err != nil {
		t.Fatalf("save specialty agent: %v", err)
	}
	// Private agent — must be excluded.
	if err := s.dataStore.SaveAgent(ctx, &store.AgentRecord{
		ID: "agt_private", UserID: uid, Name: "Private Agent",
		Config: map[string]any{"description": "hidden"},
	}); err != nil {
		t.Fatalf("save private agent: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/public/agents", nil)
	w := httptest.NewRecorder()
	s.handlePublicAgents(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Agents []publicAgentResponse `json:"agents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Agents) != 2 {
		t.Fatalf("agents = %d, want 2 (private excluded); body = %s", len(resp.Agents), w.Body.String())
	}
	byID := map[string]publicAgentResponse{}
	for _, a := range resp.Agents {
		byID[a.ID] = a
	}
	full, ok := byID["agt_pub_full"]
	if !ok {
		t.Fatalf("agt_pub_full missing: %+v", resp.Agents)
	}
	if full.Description != "A public helper agent" {
		t.Errorf("description = %q, want %q", full.Description, "A public helper agent")
	}
	if full.Category != "utility" {
		t.Errorf("category = %q, want %q", full.Category, "utility")
	}
	if full.AvatarURL != "/api/agents/agt_pub_full/files/avatar.png" {
		t.Errorf("avatarUrl = %q, want %q", full.AvatarURL, "/api/agents/agt_pub_full/files/avatar.png")
	}
	spec, ok := byID["agt_pub_spec"]
	if !ok {
		t.Fatalf("agt_pub_spec missing: %+v", resp.Agents)
	}
	if spec.Category != "research" {
		t.Errorf("specialty fallback category = %q, want %q", spec.Category, "research")
	}
	if _, ok := byID["agt_private"]; ok {
		t.Errorf("private agent leaked into public listing")
	}
}

func TestPushDevices_CloudPathE2E(t *testing.T) {
	s := setupTestServer(t)
	ctx := context.Background()

	uid := "user_push_e2e"
	if err := s.dataStore.CreateUser(ctx, &store.UserRecord{
		ID: uid, Username: "pushe2e", Email: "push@test.com",
		PasswordHash: "x", Role: users.RoleUser, Status: "active",
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	saveBody := func(tok string) *http.Request {
		body, _ := json.Marshal(map[string]any{
			"token": tok, "platform": "ios", "environment": "production", "bundleId": "com.fastagent.app",
		})
		req := httptest.NewRequest(http.MethodPost, "/api/push/devices", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		return req
	}

	// Save succeeds.
	req := stampAuth(saveBody("push_token_1"), uid, false)
	w := httptest.NewRecorder()
	s.handleSavePushDevice(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("save status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	devices, err := s.dataStore.ListPushDevices(ctx, uid)
	if err != nil || len(devices) != 1 || devices[0].Token != "push_token_1" || devices[0].Platform != "ios" {
		t.Fatalf("after save: devices = %+v err = %v, want 1 ios device", devices, err)
	}

	// Upsert same token → still one row, platform/env updated.
	req = stampAuth(saveBody("push_token_1"), uid, false)
	w = httptest.NewRecorder()
	s.handleSavePushDevice(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("upsert status = %d; body = %s", w.Code, w.Body.String())
	}
	devices, _ = s.dataStore.ListPushDevices(ctx, uid)
	if len(devices) != 1 {
		t.Fatalf("after upsert: devices = %d, want 1", len(devices))
	}

	// Empty token → 400.
	req = stampAuth(saveBody(""), uid, false)
	w = httptest.NewRecorder()
	s.handleSavePushDevice(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty-token status = %d, want 400", w.Code)
	}

	// Unsupported platform → 400.
	body, _ := json.Marshal(map[string]any{"token": "push_token_2", "platform": "android"})
	req = stampAuth(httptest.NewRequest(http.MethodPost, "/api/push/devices", bytes.NewReader(body)), uid, false)
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	s.handleSavePushDevice(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("android platform status = %d, want 400", w.Code)
	}

	// No auth → 403 (read-only gate).
	req = httptest.NewRequest(http.MethodPost, "/api/push/devices", bytes.NewReader([]byte(`{"token":"x"}`)))
	w = httptest.NewRecorder()
	s.handleSavePushDevice(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("no-auth status = %d, want 403", w.Code)
	}

	// Delete round-trip via PathValue.
	del := httptest.NewRequest(http.MethodDelete, "/api/push/devices/push_token_1", nil)
	del.SetPathValue("token", "push_token_1")
	del = stampAuth(del, uid, false)
	w = httptest.NewRecorder()
	s.handleDeletePushDevice(w, del)
	if w.Code != http.StatusOK {
		t.Fatalf("delete status = %d; body = %s", w.Code, w.Body.String())
	}
	devices, _ = s.dataStore.ListPushDevices(ctx, uid)
	if len(devices) != 0 {
		t.Fatalf("after delete: devices = %d, want 0", len(devices))
	}
}

// avatarMultipart builds a multipart POST whose file part carries an image
// Content-Type. handleUploadMyAvatar trusts the part's Content-Type header
// (only falling back to http.DetectContentType when it is empty), so the
// request must mirror a real mobile client, not multipartRequest's default
// application/octet-stream.
func avatarMultipart(t *testing.T, filename, contentType, content string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, filename))
	h.Set("Content-Type", contentType)
	fw, err := mw.CreatePart(h)
	if err != nil {
		t.Fatalf("create avatar part: %v", err)
	}
	if _, err := io.WriteString(fw, content); err != nil {
		t.Fatalf("write avatar part: %v", err)
	}
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/me/avatar", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func TestAvatarUpload_CloudPathE2E(t *testing.T) {
	s := setupTestServer(t)
	ctx := context.Background()

	var err error
	if s.accounts, err = users.NewAccounts(s.dataStore); err != nil {
		t.Fatalf("new accounts: %v", err)
	}

	uid := "user_avatar_e2e"
	if err := s.dataStore.CreateUser(ctx, &store.UserRecord{
		ID: uid, Username: "avatare2e", Email: "avatar@test.com",
		PasswordHash: "x", Role: users.RoleUser, Status: "active",
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Minimal valid PNG signature (image/png content type mirrors a real
	// mobile client upload).
	png := "\x89PNG\r\n\x1a\n" + "IHDR" + strings.Repeat("0", 24)
	req := avatarMultipart(t, "avatar.png", "image/png", png)
	req = stampAuth(req, uid, false)

	w := httptest.NewRecorder()
	s.handleUploadMyAvatar(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		OK   bool `json:"ok"`
		User struct {
			AvatarURL string `json:"avatarUrl"`
		} `json:"user"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.OK {
		t.Fatalf("ok = false; body = %s", w.Body.String())
	}
	if !strings.HasPrefix(resp.User.AvatarURL, "data:image/png;base64,") {
		t.Errorf("avatarUrl = %q, want data:image/png;base64,...", resp.User.AvatarURL)
	}

	// Persisted in the accounts registry (same row the /api/me reads).
	acct, err := s.accounts.Get(ctx, uid)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if !strings.HasPrefix(acct.AvatarURL, "data:image/png;base64,") {
		t.Errorf("persisted avatarUrl = %q, want data:image/png;base64,...", acct.AvatarURL)
	}

	// Read-only identity rejected.
	req = avatarMultipart(t, "avatar.png", "image/png", png)
	req = stampAuth(req, uid, true)
	w = httptest.NewRecorder()
	s.handleUploadMyAvatar(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("read-only status = %d, want 403", w.Code)
	}
}

func TestTeamChat_CloudPathE2E(t *testing.T) {
	s := setupTestServer(t)
	ctx := context.Background()

	uid := "user_team_e2e"
	if err := s.dataStore.CreateUser(ctx, &store.UserRecord{
		ID: uid, Username: "teame2e", Email: "team@test.com",
		PasswordHash: "x", Role: users.RoleUser, Status: "active",
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	post := func(body string, authed bool) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/api/chat/team/stream", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if authed {
			req = stampAuth(req, uid, false)
		}
		return req
	}

	// Malformed JSON → 400.
	w := httptest.NewRecorder()
	s.handleTeamChatStream(w, post(`{"teamId":`, true))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed body status = %d, want 400", w.Code)
	}

	// Valid body but empty members → 400 (before auth gate).
	validBody := `{"teamId":"t1","members":[]}`
	w = httptest.NewRecorder()
	s.handleTeamChatStream(w, post(validBody, false))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty members status = %d, want 400", w.Code)
	}

	// Valid teamId + members but no auth → 401.
	memberBody := `{"teamId":"t1","message":"hi","members":[{"agentId":"agt_a"}]}`
	w = httptest.NewRecorder()
	s.handleTeamChatStream(w, post(memberBody, false))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no-auth status = %d, want 401; body = %s", w.Code, w.Body.String())
	}

	// Pure-function pins: routing without a live AgentHandle.
	alice := resolvedTeamMember{AgentID: "agt_alice", SessionID: "s1", Name: "Alice"}
	bob := resolvedTeamMember{AgentID: "agt_bob", SessionID: "s2", Name: "Bob"}
	everyone := []resolvedTeamMember{alice, bob}

	// @all / 大家 → all members.
	if got := s.selectTeamMembers("大家好 帮我看下", everyone); len(got) != 2 {
		t.Errorf("@all routing = %d members, want 2", len(got))
	}
	// @name mention → only that member.
	if got := s.selectTeamMembers("请 @bob 处理", everyone); len(got) != 1 || got[0].AgentID != "agt_bob" {
		t.Errorf("mention routing = %+v, want [agt_bob]", got)
	}
	// Explicit id mention → routed to id.
	if got := s.selectTeamMembers("@agt_alice 来", everyone); len(got) != 1 || got[0].AgentID != "agt_alice" {
		t.Errorf("id mention routing = %+v, want [agt_alice]", got)
	}
	// No mention → best teamRouteScore.
	analyst := resolvedTeamMember{AgentID: "agt_data", SessionID: "s3", Name: "Data Analyst"}
	ops := resolvedTeamMember{AgentID: "agt_ops", SessionID: "s4", Name: "Ops"}
	if got := s.selectTeamMembers("run data analysis please", []resolvedTeamMember{ops, analyst}); len(got) != 1 || got[0].AgentID != "agt_data" {
		t.Errorf("score routing = %+v, want [agt_data]", got)
	}

	// teamAgentSessionID composition.
	if got := teamAgentSessionID("team-t1", "t1", "agt_x"); got != "team-t1-agent-agt_x" {
		t.Errorf("teamAgentSessionID = %q, want %q", got, "team-t1-agent-agt_x")
	}
	if got := teamAgentSessionID("team-t1-agent-agt_x", "t1", "agt_x"); got != "team-t1-agent-agt_x" {
		t.Errorf("teamAgentSessionID idempotent = %q, want %q", got, "team-t1-agent-agt_x")
	}
	if got := teamAgentSessionID("", "t1", "agt_x"); got != "team-t1-agent-agt_x" {
		t.Errorf("teamAgentSessionID empty base = %q, want %q", got, "team-t1-agent-agt_x")
	}
}
