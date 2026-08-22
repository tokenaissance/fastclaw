package setup

// E2E coverage for upstream commit 583a1a7 (local c488a8e)
// "feat(security): hard-block SKILL.md exfiltration via tools, sandbox,
// and downloads".
//
// This file covers the SETUP-layer download endpoint (b):
//
//	handleAgentFile must refuse to serve any workspace file whose name is
//	SKILL.md — the tail of the `cat /skills/foo/SKILL.md > /workspace/...`
//	exfil chain. The guard is name-level (EqualFold), defense-in-depth: the
//	model-cooperation rename case stays the domain of the soft
//	confidentiality directives.
//
// The tools-layer (a) and sandbox-layer (c) blocks live in their own
// package test files (internal/agent/tools/security_e2e_test.go and
// internal/sandbox/security_e2e_test.go) because those predicates are
// unexported and must be asserted in-package.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

// TestSecurity_AgentFileDownloadRefusesSkillManifest drives the real
// handleAgentFile handler via setupTestServer + stampAuthAndUserID, exactly
// like the channels e2e, and asserts the SKILL.md guard returns 403 for both
// the exact name and a case-insensitive variant, while a non-manifest
// workspace file still downloads (proving the guard is narrow).
func TestSecurity_AgentFileDownloadRefusesSkillManifest(t *testing.T) {
	s, uid, aid := setupFileUploadTest(t)
	ctx := context.Background()

	// Seed the exfil artifact: a SKILL.md that landed in the workspace (the
	// tail of the `cat /skills/foo/SKILL.md > /workspace/...` chain), plus a
	// decoy non-manifest file that must NOT be refused.
	if err := s.workspaceStore.Put(ctx, aid, "", "", "exfil/SKILL.md",
		strings.NewReader("secret skill manifest"), -1, "text/markdown"); err != nil {
		t.Fatalf("seed exfil/SKILL.md: %v", err)
	}
	if err := s.workspaceStore.Put(ctx, aid, "", "", "exfil/notes.md",
		strings.NewReader("boring notes"), -1, "text/markdown"); err != nil {
		t.Fatalf("seed exfil/notes.md: %v", err)
	}

	// 1. Exact name → 403 + the manifest refusal body.
	req := httptest.NewRequest(http.MethodGet, "/api/agents/"+aid+"/files/exfil/SKILL.md", nil)
	req.SetPathValue("id", aid)
	req.SetPathValue("path", "exfil/SKILL.md")
	req = stampAuthAndUserID(req, uid)
	rec := httptest.NewRecorder()
	s.handleAgentFile(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET exfil/SKILL.md status = %d, body=%s; want 403", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "skill manifests are not downloadable") {
		t.Errorf("SKILL.md refusal body = %q", rec.Body.String())
	}

	// 2. Case-insensitive variant (EqualFold guard) → still 403.
	req = httptest.NewRequest(http.MethodGet, "/api/agents/"+aid+"/files/exfil/skill.md", nil)
	req.SetPathValue("id", aid)
	req.SetPathValue("path", "exfil/skill.md")
	req = stampAuthAndUserID(req, uid)
	rec = httptest.NewRecorder()
	s.handleAgentFile(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET exfil/skill.md (lowercase) status = %d, body=%s; want 403", rec.Code, rec.Body.String())
	}

	// 3. A workspace file that merely LOOKS like a manifest at the agent
	//    root is refused too.
	if err := s.workspaceStore.Put(ctx, aid, "", "", "SKILL.md",
		strings.NewReader("root manifest"), -1, "text/markdown"); err != nil {
		t.Fatalf("seed root SKILL.md: %v", err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/agents/"+aid+"/files/SKILL.md", nil)
	req.SetPathValue("id", aid)
	req.SetPathValue("path", "SKILL.md")
	req = stampAuthAndUserID(req, uid)
	rec = httptest.NewRecorder()
	s.handleAgentFile(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET root SKILL.md status = %d, body=%s; want 403", rec.Code, rec.Body.String())
	}

	// 4. The guard fires even when the file doesn't exist in the store —
	//    it is name-level, so it can't be probed around by requesting a
	//    path the exfil just created.
	req = httptest.NewRequest(http.MethodGet, "/api/agents/"+aid+"/files/ghost/SKILL.md", nil)
	req.SetPathValue("id", aid)
	req.SetPathValue("path", "ghost/SKILL.md")
	req = stampAuthAndUserID(req, uid)
	rec = httptest.NewRecorder()
	s.handleAgentFile(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET ghost/SKILL.md (unseeded) status = %d, body=%s; want 403 (name-level guard)", rec.Code, rec.Body.String())
	}

	// 5. Positive control: a non-manifest file is served normally.
	req = httptest.NewRequest(http.MethodGet, "/api/agents/"+aid+"/files/exfil/notes.md", nil)
	req.SetPathValue("id", aid)
	req.SetPathValue("path", "exfil/notes.md")
	req = stampAuthAndUserID(req, uid)
	rec = httptest.NewRecorder()
	s.handleAgentFile(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET exfil/notes.md status = %d, body=%s; want 200", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "boring notes" {
		t.Errorf("exfil/notes.md body = %q, want %q", got, "boring notes")
	}
}

// TestSecurity_AgentFileDownloadRequiresOwnership ensures the SKILL.md
// guard isn't the only gate on the download path: a caller who doesn't own
// the agent is rejected with 403 before the manifest check, so the guard
// isn't reachable cross-tenant even for a non-manifest filename.
func TestSecurity_AgentFileDownloadRequiresOwnership(t *testing.T) {
	s, ctx, _, aid := setupFileDeleteTest(t)

	otherUID := "user_intruder_security"
	if err := s.dataStore.CreateUser(ctx, &store.UserRecord{
		ID: otherUID, Username: "intruder_sec", Email: "intruder_sec@test.com",
		PasswordHash: "x", Role: users.RoleUser, Status: "active",
	}); err != nil {
		t.Fatalf("create intruder: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/agents/"+aid+"/files/SKILL.md", nil)
	req.SetPathValue("id", aid)
	req.SetPathValue("path", "SKILL.md")
	req = stampAuthAndUserID(req, otherUID)
	rec := httptest.NewRecorder()
	s.handleAgentFile(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("intruder GET SKILL.md status = %d, body=%s; want 403", rec.Code, rec.Body.String())
	}
}
