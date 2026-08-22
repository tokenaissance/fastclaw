package setup

// e2e coverage for five merged upstream skill/task hardening commits.
// Everything here runs through the real handlers / real auth middleware;
// no production code is touched and nothing that needs a network call is
// exercised without a fake transport or a purely-local path.
//
// Covered upstream commits (local equivalents on fastagent):
//   - 9bef515 / fc488ac  require auth for global skills list
//   - d6ab971 / c25fba7  fix global skill install authorization
//   - 601987b / 1ad25fa  protect tasks list endpoint
//   - 97c0edd / 3137f5f  fix(skills): keep gated skills visible
//   - 181165c / 91f44f6  feat: load skill bodies on demand

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/auth"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newSkillsE2EAuthServer builds on setupTestServer (in-memory SQLite +
// Migrate) and wires a real auth.Resolver plus one super_admin and one
// regular user. Route-level auth gates (skills list, tasks list) live in
// the middleware layer, and the 401 branch comes from real cookie
// resolution — stampAuth cannot express "unauthenticated" because it
// always stamps a valid identity — so these two tests go through the
// actual resolver middleware with real session cookies.
func newSkillsE2EAuthServer(t *testing.T) (*Server, *auth.Resolver, string, string) {
	t.Helper()
	s := setupTestServer(t)

	resolver, err := auth.NewResolver(s.dataStore)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	s.SetAuth(resolver)

	accts, err := users.NewAccounts(s.dataStore)
	if err != nil {
		t.Fatalf("NewAccounts: %v", err)
	}
	admin := createSkillsE2EUser(t, accts, "skills_e2e_admin", users.RoleSuperAdmin)
	reg := createSkillsE2EUser(t, accts, "skills_e2e_user", users.RoleUser)
	return s, resolver, admin.ID, reg.ID
}

func createSkillsE2EUser(t *testing.T, accts *users.Accounts, username, role string) *users.Account {
	t.Helper()
	acct, err := accts.Create(context.Background(), users.CreateInput{
		Username: username,
		Email:    username + "@example.test",
		Password: "password",
		Role:     role,
	})
	if err != nil {
		t.Fatalf("create user %s: %v", username, err)
	}
	return acct
}

// e2eSessionCookie issues a real session cookie for userID via the
// resolver so the composed auth middleware resolves it the way a browser
// would.
func e2eSessionCookie(t *testing.T, ctx context.Context, resolver *auth.Resolver, userID string) *http.Cookie {
	t.Helper()
	cookie, err := resolver.IssueSession(ctx, userID)
	if err != nil {
		t.Fatalf("IssueSession(%s): %v", userID, err)
	}
	return cookie
}

// stampSuperAdmin stamps a super_admin session identity. stampAuth only
// produces RoleUser, but the global-install gate keys on
// Identity.CanAdminPlatform(), which needs RoleSuperAdmin for sessions.
func stampSuperAdmin(r *http.Request, uid string) *http.Request {
	return r.WithContext(auth.WithIdentity(r.Context(), auth.Identity{
		UserID:     uid,
		Role:       users.RoleSuperAdmin,
		AuthMethod: "session",
	}))
}

// buildSkillZip packs a single skill folder (name/SKILL.md) into an
// in-memory zip — the shape handleUploadSkill expects (single common
// top-level dir → that dir becomes the skill name).
func buildSkillZip(t *testing.T, skillName, skillMD string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(skillName + "/SKILL.md")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	if _, err := io.WriteString(w, skillMD); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// multipartZipRequest builds a multipart POST carrying an in-memory zip
// under the `file` field (the handleUploadSkill contract).
func multipartZipRequest(t *testing.T, url, filename string, data []byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, url, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// writeSkill seeds a skill folder under dir/<name>/SKILL.md and returns
// the skill's absolute directory.
func writeSkill(t *testing.T, dir, name, skillMD string) string {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", skillDir, err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillMD), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	return skillDir
}

// ---------------------------------------------------------------------------
// 9bef515 / fc488ac — require auth for global skills list
// ---------------------------------------------------------------------------

func TestSkills_GlobalListRequiresAuth(t *testing.T) {
	ctx := context.Background()
	s, resolver, adminID, userID := newSkillsE2EAuthServer(t)
	home := t.TempDir()
	t.Setenv("FASTAGENT_HOME", home)

	// Route chain in server.go: GET /api/skills → auth(s.handleListSkills).
	handler := s.authMiddleware(s.handleListSkills)

	// 1. No credentials → resolver middleware 401s before the handler runs.
	rr := httptest.NewRecorder()
	handler(rr, httptest.NewRequest(http.MethodGet, "/api/skills", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated skills list status = %d, want 401; body=%s", rr.Code, rr.Body.String())
	}

	// 2. Any authenticated user (regular OR admin) may list — auth, not
	//    admin, is the gate for the global catalog.
	for _, tc := range []struct {
		name string
		uid  string
	}{
		{"regular user", userID},
		{"super admin", adminID},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/skills", nil)
		req.AddCookie(e2eSessionCookie(t, ctx, resolver, tc.uid))
		rr = httptest.NewRecorder()
		handler(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s skills list status = %d, want 200; body=%s", tc.name, rr.Code, rr.Body.String())
		}
		if got := strings.TrimSpace(rr.Body.String()); got != "[]" {
			t.Fatalf("%s skills list body = %q, want []", tc.name, got)
		}
	}

	// 3. Seed a real skill into the managed dir and re-list: the catalog
	//    handler should surface it for an authenticated regular user.
	writeSkill(t, filepath.Join(home, "skills"), "demo-skill",
		"---\ndescription: Demo e2e skill\n---\n# demo-skill\n\nBody.\n")

	req := httptest.NewRequest(http.MethodGet, "/api/skills", nil)
	req.AddCookie(e2eSessionCookie(t, ctx, resolver, userID))
	rr = httptest.NewRecorder()
	handler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("seeded skills list status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var listed []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode skills list: %v", err)
	}
	if len(listed) != 1 || listed[0]["name"] != "demo-skill" {
		t.Fatalf("skills list = %+v; want exactly [demo-skill]", listed)
	}
	if listed[0]["description"] != "Demo e2e skill" {
		t.Errorf("demo-skill description = %v, want %q", listed[0]["description"], "Demo e2e skill")
	}
}

// ---------------------------------------------------------------------------
// 601987b / 1ad25fa — protect tasks list endpoint
// ---------------------------------------------------------------------------

func TestSkills_TasksListRequiresPlatformAdmin(t *testing.T) {
	ctx := context.Background()
	s, resolver, adminID, userID := newSkillsE2EAuthServer(t)

	// Route chain in server.go: GET /api/tasks → admin(s.handleListTasks),
	// i.e. auth middleware (401) then RequirePlatformAdmin (403 for
	// non-admins), then the handler (200).
	handler := s.requireSuperAdmin(s.handleListTasks)

	// 1. Unauthenticated → 401 (auth layer).
	rr := httptest.NewRecorder()
	handler(rr, httptest.NewRequest(http.MethodGet, "/api/tasks", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated tasks status = %d, want 401; body=%s", rr.Code, rr.Body.String())
	}

	// 2. Regular user → 403 (RequirePlatformAdmin layer).
	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.AddCookie(e2eSessionCookie(t, ctx, resolver, userID))
	rr = httptest.NewRecorder()
	handler(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("regular user tasks status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}

	// 3. Super admin → 200 (empty queue in this harness).
	req = httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.AddCookie(e2eSessionCookie(t, ctx, resolver, adminID))
	rr = httptest.NewRecorder()
	handler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin tasks status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got := strings.TrimSpace(rr.Body.String()); got != "[]" {
		t.Fatalf("admin tasks body = %q, want []", got)
	}
}

// ---------------------------------------------------------------------------
// d6ab971 / c25fba7 — fix global skill install authorization
// ---------------------------------------------------------------------------

// TestSkills_InstallGlobalRequiresAdmin drives the JSON install handler
// (POST /api/skills/install) and asserts the gate added by
// authorizeSkillInstallTarget: global installs (no `agent`) are
// platform-admin-only. Non-admin and unauthenticated callers are rejected
// before any install logic (and therefore before any network call) runs.
func TestSkills_InstallGlobalRequiresAdmin(t *testing.T) {
	s := setupTestServer(t)
	home := t.TempDir()
	t.Setenv("FASTAGENT_HOME", home)

	newInstallReq := func(r *http.Request) *httptest.ResponseRecorder {
		r.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		s.handleInstallSkill(rr, r)
		return rr
	}
	body := `{"name":"demo-skill","source":"github","repo":"owner/repo"}`

	// 1. No identity at all → requireWritable 401s.
	rr := newInstallReq(httptest.NewRequest(http.MethodPost, "/api/skills/install", strings.NewReader(body)))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no-identity install status = %d, want 401; body=%s", rr.Code, rr.Body.String())
	}

	// 2. Regular user (stampAuth) → platform-admin gate 403s.
	rr = newInstallReq(stampAuth(httptest.NewRequest(http.MethodPost, "/api/skills/install", strings.NewReader(body)), "u_user", false))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("regular user install status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "platform admin required") {
		t.Errorf("regular user install error body = %q; want platform admin required", rr.Body.String())
	}

	// 3. super_admin acting as another user (read-only) → 403 before any
	//    write. ReadOnly() short-circuits in requireWritable.
	actAs := httptest.NewRequest(http.MethodPost, "/api/skills/install", strings.NewReader(body))
	actAs = actAs.WithContext(auth.WithIdentity(actAs.Context(), auth.Identity{
		UserID:      "u_admin",
		Role:        users.RoleSuperAdmin,
		AuthMethod:  "session",
		ActAsUserID: "u_other",
	}))
	rr = newInstallReq(actAs)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("read-only admin install status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}

	// Nothing may have been written into the global skills dir.
	if _, err := os.Stat(filepath.Join(home, "skills", "demo-skill")); err == nil {
		t.Fatal("install wrote a skill dir despite the auth gate rejecting it")
	}
}

// TestSkills_UploadGlobalRequiresAdmin covers the zip-upload sibling of
// the same gate (handleUploadSkill calls authorizeSkillInstallTarget too).
// The admin branch runs the FULL upload pipeline to completion (zip parse
// + extract to the global skills dir) with zero network — the object-store
// mirror is skipped because setupTestServer leaves workspaceStore nil.
func TestSkills_UploadGlobalRequiresAdmin(t *testing.T) {
	s := setupTestServer(t)
	home := t.TempDir()
	t.Setenv("FASTAGENT_HOME", home)

	skillMD := "---\ndescription: Uploaded e2e skill\n---\n# myzipskill\n\nBody.\n"
	zipData := buildSkillZip(t, "myzipskill", skillMD)

	// 1. Regular user → 403; gate runs before multipart parsing.
	rr := httptest.NewRecorder()
	s.handleUploadSkill(rr, stampAuth(multipartZipRequest(t, "/api/skills/upload", "myzipskill.zip", zipData), "u_user", false))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("regular user upload status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}

	// 2. Super admin → gate passes, full pipeline writes the skill locally.
	rr = httptest.NewRecorder()
	s.handleUploadSkill(rr, stampSuperAdmin(multipartZipRequest(t, "/api/skills/upload", "myzipskill.zip", zipData), "u_admin"))
	if rr.Code != http.StatusOK {
		t.Fatalf("admin upload status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		OK   bool   `json:"ok"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	if !resp.OK || resp.Name != "myzipskill" {
		t.Fatalf("upload response = %+v; want ok + name myzipskill", resp)
	}

	// 3. The skill actually landed in the global managed dir.
	written, err := os.ReadFile(filepath.Join(home, "skills", "myzipskill", "SKILL.md"))
	if err != nil {
		t.Fatalf("admin upload did not write SKILL.md: %v", err)
	}
	if !strings.Contains(string(written), "Uploaded e2e skill") {
		t.Errorf("written SKILL.md = %q; want uploaded frontmatter", string(written))
	}
}

// ---------------------------------------------------------------------------
// 97c0edd / 3137f5f — keep gated skills visible
// ---------------------------------------------------------------------------

// TestSkills_GatedSkillsStayVisible is a pure-function test of
// internal/agent/skills.go (LoadSkills + BuildSkillsSummary). This path
// has no HTTP handler and no session/workspace wiring — setupTestServer
// can't provide it — so we exercise the loader directly against temp skill
// dirs under a temporary FASTAGENT_HOME. It asserts the 3137f5f behavior:
// a skill whose gating requirement (missing env var) is unmet stays in the
// catalog, is labeled "(currently unavailable: …)", and is NOT inlined as
// always-loaded.
func TestSkills_GatedSkillsStayVisible(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FASTAGENT_HOME", home)
	// Force determinism: the missing var is absent regardless of the host.
	t.Setenv("SKILL_E2E_MISSING_VAR", "")

	writeSkill(t, filepath.Join(home, "skills"), "gatedtool",
		`---
name: gatedtool
description: Needs a secret
metadata:
  fastagent:
    requires:
      env:
        - SKILL_E2E_MISSING_VAR
---
# Gated Tool

Body.
`)

	loader := agent.NewSkillsLoader(home, filepath.Join(home, "agents", "e2e"), "", config.SkillsConfig{})
	skills := loader.LoadSkills()

	var found bool
	for _, sk := range skills {
		if sk.Name != "gatedtool" {
			continue
		}
		found = true
		if !sk.Gated {
			t.Fatal("gatedtool should be gated (required env var absent)")
		}
		if !strings.Contains(sk.GateReason, "SKILL_E2E_MISSING_VAR") {
			t.Errorf("GateReason = %q; want mention of the missing var", sk.GateReason)
		}
	}
	if !found {
		t.Fatal("gated skill was dropped from LoadSkills(); want it kept visible in the catalog")
	}

	summary := loader.BuildSkillsSummary(skills)
	if !strings.Contains(summary, "gatedtool") {
		t.Errorf("catalog summary missing gated skill name")
	}
	if !strings.Contains(summary, "(currently unavailable:") {
		t.Errorf("catalog summary does not label the gated skill as unavailable")
	}
	if strings.Contains(summary, "<always_loaded_skills>") {
		t.Errorf("gated skill must not be inlined into always_loaded_skills")
	}
}

// ---------------------------------------------------------------------------
// 181165c / 91f44f6 — load skill bodies on demand
// ---------------------------------------------------------------------------

// TestSkills_SkillBodyLazyLoad is a pure-function test of the progressive-
// disclosure change to internal/agent/skills.go BuildSkillsSummary: the
// prompt ships only the name+description catalog for ordinary skills (the
// model pulls the body later via load_skill), while explicit always-load
// skills are inlined and their {baseDir} placeholder is substituted.
// Same rationale as the gated test — no HTTP handler exists for this logic.
func TestSkills_SkillBodyLazyLoad(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FASTAGENT_HOME", home)

	// Ordinary skill: body must NOT appear in the prompt.
	writeSkill(t, filepath.Join(home, "skills"), "lazytool",
		"---\nname: lazytool\ndescription: Lazy tool\n---\n# Lazy\n\nLAZY_SECRET_BODY\n")

	// always:true skill: body IS inlined, with {baseDir} substituted.
	writeSkill(t, filepath.Join(home, "skills"), "inlinetool",
		"---\nname: inlinetool\ndescription: Inline tool\nmetadata:\n  fastagent:\n    always: true\n---\n# Inline\n\nINLINE_SECRET_BODY at {baseDir}\n")

	loader := agent.NewSkillsLoader(home, filepath.Join(home, "agents", "e2e"), "", config.SkillsConfig{})
	skills := loader.LoadSkills()
	if len(skills) != 2 {
		t.Fatalf("LoadSkills() = %d skills; want 2", len(skills))
	}

	summary := loader.BuildSkillsSummary(skills)
	if !strings.Contains(summary, "lazytool") || !strings.Contains(summary, "inlinetool") {
		t.Errorf("catalog summary missing skill names; got:\n%s", summary)
	}

	// Progressive disclosure: the lazy skill's body stays out of context.
	if strings.Contains(summary, "LAZY_SECRET_BODY") {
		t.Errorf("lazy skill body leaked into the prompt; want on-demand load")
	}

	// Always-load skill body is present and {baseDir} resolved.
	if !strings.Contains(summary, "INLINE_SECRET_BODY") {
		t.Errorf("always-load skill body missing from prompt; got:\n%s", summary)
	}
	if strings.Contains(summary, "{baseDir}") {
		t.Errorf("{baseDir} placeholder not substituted in inline body")
	}
	if !strings.Contains(summary, filepath.Join(home, "skills", "inlinetool")) {
		t.Errorf("inline body should reference the resolved absolute skill dir")
	}
}
