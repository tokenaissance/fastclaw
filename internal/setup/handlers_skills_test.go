package setup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/auth"
	"github.com/fastclaw-ai/fastclaw/internal/skills"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

// writeSidecar is a test helper that writes a .fastagent-install.json
// sidecar file into dir, matching what writeInstallMetadata does in prod.
func writeSidecar(t *testing.T, dir, repo string) {
	t.Helper()
	if repo == "" {
		return
	}
	data := []byte(`{"repo":"` + repo + `"}`)
	if err := os.WriteFile(filepath.Join(dir, ".fastagent-install.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadInstallRepo_ReadsSidecarFile(t *testing.T) {
	dir := t.TempDir()

	// No sidecar → ("", nil)
	repo, err := skills.ReadInstallRepo(dir)
	if err != nil {
		t.Fatalf("unexpected error for dir with no sidecar: %v", err)
	}
	if repo != "" {
		t.Fatalf("expected empty repo for dir with no sidecar, got %q", repo)
	}

	writeSidecar(t, dir, "tokenaissance/skills")

	repo, err = skills.ReadInstallRepo(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repo != "tokenaissance/skills" {
		t.Fatalf("expected %q, got %q", "tokenaissance/skills", repo)
	}
}

func TestReadInstallRepo_BadJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(dir, ".fastagent-install.json"),
		[]byte("not json"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	_, err := skills.ReadInstallRepo(dir)
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}

func TestScanSkillsDir_PopulatesSourceFromSidecar(t *testing.T) {
	dir := t.TempDir()

	skillDir := filepath.Join(dir, "my-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\ndescription: A test skill\n---\n# my-skill\n\nDoes things."),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	writeSidecar(t, skillDir, "owner/repo")

	results := scanSkillsDir(dir)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	r := results[0]
	if r["name"] != "my-skill" {
		t.Errorf("name: expected %q, got %q", "my-skill", r["name"])
	}
	if r["description"] != "A test skill" {
		t.Errorf("description: expected %q, got %q", "A test skill", r["description"])
	}
	if r["source"] != "owner/repo" {
		t.Errorf("source: expected %q, got %q", "owner/repo", r["source"])
	}
}

func TestScanSkillsDir_SourceAbsentWithoutSidecar(t *testing.T) {
	dir := t.TempDir()

	skillDir := filepath.Join(dir, "no-source-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(skillDir, "SKILL.md"),
		[]byte("# no-source-skill\n\nNo sidecar here."),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	results := scanSkillsDir(dir)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	r := results[0]
	if _, ok := r["source"]; ok {
		t.Errorf("source key should be absent when sidecar is missing, got %v", r["source"])
	}
}

func TestScanSkillsDir_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	results := scanSkillsDir(dir)
	if results != nil {
		t.Fatalf("expected nil for empty dir, got %v", results)
	}
}

func TestScanSkillsDir_IgnoresFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	results := scanSkillsDir(dir)
	if results != nil {
		t.Fatalf("expected nil when dir has only files, got %v", results)
	}
}
func TestListSkillsRequiresAuth(t *testing.T) {
	ctx := context.Background()
	s, resolver, adminUser, regularUser := newSkillsAuthTestServer(t, ctx)
	t.Setenv("FASTAGENT_HOME", t.TempDir())

	handler := s.authMiddleware(s.handleListSkills)

	t.Run("unauthenticated request is rejected", func(t *testing.T) {
		rr := httptest.NewRecorder()
		handler(rr, httptest.NewRequest(http.MethodGet, "/api/skills", nil))
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
		}
	})

	t.Run("regular user is allowed", func(t *testing.T) {
		rr := httptest.NewRecorder()
		handler(rr, skillsListRequest(t, ctx, resolver, regularUser.ID))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
		}
		if got := strings.TrimSpace(rr.Body.String()); got != "[]" {
			t.Fatalf("body = %q, want []", got)
		}
	})

	t.Run("super admin is allowed", func(t *testing.T) {
		rr := httptest.NewRecorder()
		handler(rr, skillsListRequest(t, ctx, resolver, adminUser.ID))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
		}
		if got := strings.TrimSpace(rr.Body.String()); got != "[]" {
			t.Fatalf("body = %q, want []", got)
		}
	})
}

func newSkillsAuthTestServer(t *testing.T, ctx context.Context) (*Server, *auth.Resolver, *users.Account, *users.Account) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "fastclaw.db")
	st, err := store.NewDBStore("sqlite", "file:"+dbPath+"?cache=shared")
	if err != nil {
		t.Fatalf("NewDBStore: %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
	})
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	accts, err := users.NewAccounts(st)
	if err != nil {
		t.Fatalf("NewAccounts: %v", err)
	}
	adminUser := createSkillsTestUser(t, ctx, accts, "admin", users.RoleSuperAdmin)
	regularUser := createSkillsTestUser(t, ctx, accts, "user", users.RoleUser)
	resolver, err := auth.NewResolver(st)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	s := NewServer(0)
	s.SetStore(st)
	s.SetAuth(resolver)
	return s, resolver, adminUser, regularUser
}

func createSkillsTestUser(t *testing.T, ctx context.Context, accts *users.Accounts, username, role string) *users.Account {
	t.Helper()

	acct, err := accts.Create(ctx, users.CreateInput{
		Username: username,
		Email:    username + "@example.test",
		Password: "password",
		Role:     role,
	})
	if err != nil {
		t.Fatalf("Create(%s): %v", username, err)
	}
	return acct
}

func skillsListRequest(t *testing.T, ctx context.Context, resolver *auth.Resolver, userID string) *http.Request {
	t.Helper()

	cookie, err := resolver.IssueSession(ctx, userID)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/skills", nil)
	req.AddCookie(cookie)
	return req
}
