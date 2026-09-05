package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/runtime"
	"github.com/fastclaw-ai/fastclaw/internal/sandbox"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// These e2e tests cover the coding-agent project runtime HTTP surface
// (internal/setup/handlers_runtime.go) that was added upstream in:
//
//   - ff7ed19 feat: project runtime (coding-agent preview) + dashboard UI refinements
//   - 418ce82 feat: backend-agnostic project runtime + multi-template + preview UX
//
// They drive the real handlers through setupTestServer + direct handler
// calls + stampAuthAndUserID (the repo's e2e pattern) exactly like
// channels_e2e_test.go. The runtime manager is wired to a FAKE pooled
// executor so the full create→status→start→sleep→wake lifecycle runs with
// zero real docker / E2B / network.

// --- fakes (no real sandbox) ---

// fakeRuntimeExecutor scripts the Executor responses the pooled preview
// boot path (runtime.Manager.upViaPool) probes for, so Up() completes
// end-to-end without a real backend:
//
//   - "test -f .../package.json" → MISSING  (needsScaffoldExec → scaffold)
//   - curl --max-time 5          → 000     (startDevServerExec probe: not up)
//   - "... & echo started"       → started (startDevServerExec boot)
//   - curl --max-time 60         → 200     (waitForDevServerExec probe: up)
type fakeRuntimeExecutor struct {
	exposeURL string
}

func (f *fakeRuntimeExecutor) Exec(_ context.Context, command string, _ time.Duration) (string, error) {
	switch {
	case strings.Contains(command, "package.json"):
		return "MISSING", nil
	case strings.Contains(command, "--max-time 60"):
		return "200", nil
	case strings.Contains(command, "--max-time 5"):
		return "000", nil
	case strings.Contains(command, "echo started"):
		return "started", nil
	}
	return "", nil
}

func (f *fakeRuntimeExecutor) ReadFile(context.Context, string) (string, error) { return "", nil }
func (f *fakeRuntimeExecutor) WriteFile(context.Context, string, string) (string, error) {
	return "", nil
}
func (f *fakeRuntimeExecutor) ListDir(context.Context, string) (string, error) { return "", nil }
func (f *fakeRuntimeExecutor) Backend() string                                 { return "e2b" }
func (f *fakeRuntimeExecutor) Close() error                                    { return nil }

// ExposePort satisfies sandbox.PortExposer — the backend-agnostic runtime
// type-asserts this to mint the preview URL.
func (f *fakeRuntimeExecutor) ExposePort(_ context.Context, port int) (string, error) {
	if f.exposeURL != "" {
		return f.exposeURL, nil
	}
	return fmt.Sprintf("https://%d-fake.e2b.app", port), nil
}

type fakeRuntimePool struct {
	ex sandbox.Executor
}

func (p *fakeRuntimePool) Get(context.Context, string, string, string) (sandbox.Executor, error) {
	return p.ex, nil
}
func (p *fakeRuntimePool) Release(string, string, string) error { return nil }
func (p *fakeRuntimePool) CloseAll()                            {}
func (p *fakeRuntimePool) Backend() string                      { return "e2b" }

// newRuntimeTestServer wires a Server the way main.go does — a pooled
// runtime manager (backend "e2b" + a shared executor pool) — but backed by
// fakes. Callers seed projects via s.dataStore.SaveProject before /up.
func newRuntimeTestServer(t *testing.T) (*Server, string /*uid*/, string /*aid*/) {
	t.Helper()
	s, uid, aid := setupFileUploadTest(t)

	mgr := runtime.NewManager(s.dataStore, t.TempDir(), "img", &sandbox.Policy{}, "", "e2b", &fakeRuntimePool{ex: &fakeRuntimeExecutor{}})
	mgr.RegisterTemplate("shipany-tanstack", runtime.TemplateSpec{
		DevPort: 3000, ScaffoldCmd: "echo scaffold", DevCmd: "echo dev",
	})
	mgr.RegisterTemplate("vite-react", runtime.TemplateSpec{
		DevPort: 5173, DevCmd: "echo dev", // no ScaffoldCmd — exercises the skip-scaffold branch
	})
	s.SetRuntimeManager(mgr)
	return s, uid, aid
}

// rtReq builds an authenticated request against the project-runtime route
// with both path params stamped, the way the mux would.
func rtReq(t *testing.T, method, aid, pid, uid, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, "/api/agents/"+aid+"/projects/"+pid+"/runtime", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", aid)
	req.SetPathValue("pid", pid)
	return stampAuthAndUserID(req, uid)
}

func seedProject(t *testing.T, s *Server, uid, aid, pid string) {
	t.Helper()
	if err := s.dataStore.SaveProject(context.Background(), &store.ProjectRecord{
		UserID: uid, AgentID: aid, ID: pid, Name: "runtime e2e project",
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

func decodeRuntimeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	return m
}

// TestRuntime_HTTPLifecycle walks the full project-runtime HTTP lifecycle —
// create(up) → status → preview → logs → sleep → status → wake — and
// asserts the wire JSON for each step. This is the "SaaS shell drives a
// project entirely through these endpoints" path added by ff7ed19 and made
// backend-agnostic by 418ce82.
func TestRuntime_HTTPLifecycle(t *testing.T) {
	s, uid, aid := newRuntimeTestServer(t)
	const pid = "proj_e2e_runtime"
	seedProject(t, s, uid, aid, pid)

	// 1. GET runtime before any boot → 404 "no runtime for this project".
	rec := httptest.NewRecorder()
	s.handleGetRuntime(rec, rtReq(t, http.MethodGet, aid, pid, uid, ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get-before-up status = %d, body=%s", rec.Code, rec.Body.String())
	}

	// 2. POST /runtime/up → 200 running, preview URL minted by ExposePort.
	rec = httptest.NewRecorder()
	s.handleRuntimeUp(rec, rtReq(t, http.MethodPost, aid, pid, uid, `{"templateRef":"shipany-tanstack"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("up status = %d, body=%s", rec.Code, rec.Body.String())
	}
	up := decodeRuntimeJSON(t, rec)
	if up["status"] != "running" || up["projectId"] != pid || up["templateRef"] != "shipany-tanstack" {
		t.Fatalf("up record = %+v", up)
	}
	if up["devPort"] != float64(3000) || up["hostPort"] != float64(0) {
		t.Errorf("up ports = devPort=%v hostPort=%v; want 3000 / 0", up["devPort"], up["hostPort"])
	}
	if up["previewUrl"] != "https://3000-fake.e2b.app" {
		t.Errorf("up previewUrl = %v, want https://3000-fake.e2b.app", up["previewUrl"])
	}

	// 3. GET /runtime → same running record, persisted (round-trip through
	//    the store).
	rec = httptest.NewRecorder()
	s.handleGetRuntime(rec, rtReq(t, http.MethodGet, aid, pid, uid, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, body=%s", rec.Code, rec.Body.String())
	}
	got := decodeRuntimeJSON(t, rec)
	if got["status"] != "running" || got["previewUrl"] != "https://3000-fake.e2b.app" {
		t.Fatalf("get record = %+v", got)
	}

	// 4. GET /preview → {previewUrl, status}.
	rec = httptest.NewRecorder()
	s.handleRuntimePreview(rec, rtReq(t, http.MethodGet, aid, pid, uid, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("preview status = %d, body=%s", rec.Code, rec.Body.String())
	}
	prev := decodeRuntimeJSON(t, rec)
	if prev["status"] != "running" || prev["previewUrl"] != "https://3000-fake.e2b.app" {
		t.Fatalf("preview = %+v", prev)
	}

	// 5. GET /runtime/logs → 200 with a "logs" key.
	rec = httptest.NewRecorder()
	s.handleRuntimeLogs(rec, rtReq(t, http.MethodGet, aid, pid, uid, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("logs status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := decodeRuntimeJSON(t, rec)["logs"]; !ok {
		t.Fatalf("logs response missing 'logs' key: %s", rec.Body.String())
	}

	// 6. POST /runtime/sleep → {"ok":true,"status":"sleeping"}.
	rec = httptest.NewRecorder()
	s.handleRuntimeSleep(rec, rtReq(t, http.MethodPost, aid, pid, uid, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("sleep status = %d, body=%s", rec.Code, rec.Body.String())
	}
	sleep := decodeRuntimeJSON(t, rec)
	if sleep["ok"] != true || sleep["status"] != "sleeping" {
		t.Fatalf("sleep = %+v", sleep)
	}

	// 7. GET /runtime → sleeping, preview URL cleared.
	rec = httptest.NewRecorder()
	s.handleGetRuntime(rec, rtReq(t, http.MethodGet, aid, pid, uid, ""))
	got = decodeRuntimeJSON(t, rec)
	if got["status"] != "sleeping" || got["previewUrl"] != "" {
		t.Fatalf("after-sleep record = %+v", got)
	}

	// 8. POST /runtime/wake → running again, reusing the stored template ref
	//    (empty body on the handler).
	rec = httptest.NewRecorder()
	s.handleRuntimeWake(rec, rtReq(t, http.MethodPost, aid, pid, uid, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("wake status = %d, body=%s", rec.Code, rec.Body.String())
	}
	woke := decodeRuntimeJSON(t, rec)
	if woke["status"] != "running" || woke["previewUrl"] != "https://3000-fake.e2b.app" {
		t.Fatalf("wake record = %+v", woke)
	}

	// NOTE: DELETE /runtime (handleRuntimeStop) is deliberately NOT covered
	// here. Stop unconditionally invokes `docker volume rm` (removeVolume in
	// runtime.go) even on the pooled path, which the no-real-docker
	// constraint forbids. Sleep→Wake covers the container lifecycle without
	// touching docker; the "tear down" HTTP shape is exercised at the
	// manager level in internal/runtime instead.
}

// TestRuntime_NotEnabled_Returns503 verifies the runtime endpoints degrade
// to 503 when a deployment hasn't wired a runtime manager — the optional-
// wiring contract documented on Server.SetRuntimeManager.
func TestRuntime_NotEnabled_Returns503(t *testing.T) {
	s, uid, aid := setupFileUploadTest(t) // runtimeMgr stays nil

	rec := httptest.NewRecorder()
	s.handleGetRuntime(rec, rtReq(t, http.MethodGet, aid, "proj_x", uid, ""))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	s.handleScopePreview(rec, rtReq(t, http.MethodGet, aid, "proj_x", uid, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("scope-preview status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if m := decodeRuntimeJSON(t, rec); m["status"] != "none" {
		t.Fatalf("scope-preview body = %+v, want status none", m)
	}

	rec = httptest.NewRecorder()
	s.handleChangedFiles(rec, rtReq(t, http.MethodGet, aid, "proj_x", uid, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("changed-files status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if m := decodeRuntimeJSON(t, rec); m["available"] != false {
		t.Fatalf("changed-files body = %+v, want available=false", m)
	}
}

// TestRuntime_Up_RequiresExistingProject — /up refuses to mint a runtime
// for a typo'd / non-existent project id (prevents container-for-a-typo).
func TestRuntime_Up_RequiresExistingProject(t *testing.T) {
	s, uid, aid := newRuntimeTestServer(t)
	// No project seeded.

	rec := httptest.NewRecorder()
	s.handleRuntimeUp(rec, rtReq(t, http.MethodPost, aid, "proj_ghost", uid, `{"templateRef":"shipany-tanstack"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "project not found") {
		t.Fatalf("body = %s, want 'project not found'", rec.Body.String())
	}
}

// TestRuntime_Up_UnknownTemplate — with 2 registered templates the manager
// does NOT leniently canonicalize an unknown ref, so /up fails 500 and no
// pool/docker machinery is ever reached.
func TestRuntime_Up_UnknownTemplate(t *testing.T) {
	s, uid, aid := newRuntimeTestServer(t)
	const pid = "proj_unknown_tmpl"
	seedProject(t, s, uid, aid, pid)

	rec := httptest.NewRecorder()
	s.handleRuntimeUp(rec, rtReq(t, http.MethodPost, aid, pid, uid, `{"templateRef":"does-not-exist"}`))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unknown template") {
		t.Fatalf("body = %s, want 'unknown template' error", rec.Body.String())
	}
}

// TestRuntime_Up_EmptyTemplate_FirstBoot — an empty/absent body on a fresh
// runtime is invalid: first Up requires a template ref.
func TestRuntime_Up_EmptyTemplate_FirstBoot(t *testing.T) {
	s, uid, aid := newRuntimeTestServer(t)
	const pid = "proj_empty_tmpl"
	seedProject(t, s, uid, aid, pid)

	rec := httptest.NewRecorder()
	s.handleRuntimeUp(rec, rtReq(t, http.MethodPost, aid, pid, uid, ""))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "templateRef") {
		t.Fatalf("body = %s, want templateRef-required error", rec.Body.String())
	}
}

// TestRuntime_AuthGates verifies the ownership / writability guards on the
// runtime endpoints: unauthenticated writes 401, read-only (actAs) writes
// 403, and a non-owner caller can't read another user's runtime row.
func TestRuntime_AuthGates(t *testing.T) {
	s, uid, aid := newRuntimeTestServer(t)
	const pid = "proj_auth_gates"

	// Unauthenticated write → 401 (requireWritable).
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.SetPathValue("id", aid)
	req.SetPathValue("pid", pid)
	rec := httptest.NewRecorder()
	s.handleRuntimeUp(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated up status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}

	// Read-only (super_admin actAs) write → 403 (requireWritable).
	req = httptest.NewRequest(http.MethodPost, "/", nil)
	req.SetPathValue("id", aid)
	req.SetPathValue("pid", pid)
	req = stampAuth(req, uid, true)
	rec = httptest.NewRecorder()
	s.handleRuntimeUp(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read-only up status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}

	// Non-owner read → 403 (requireAgentReadable: agent is private).
	rec = httptest.NewRecorder()
	s.handleGetRuntime(rec, rtReq(t, http.MethodGet, aid, pid, "user_intruder", ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("intruder get status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// TestRuntime_ScopePreview_Live — the chat-workspace preview lookup
// (sessionId or projectId query) returns the live preview for the current
// scope, the "open preview" entry added in ff7ed19.
func TestRuntime_ScopePreview_Live(t *testing.T) {
	s, uid, aid := newRuntimeTestServer(t)
	const pid = "proj_scope_live"
	seedProject(t, s, uid, aid, pid)

	// Boot the app first.
	rec := httptest.NewRecorder()
	s.handleRuntimeUp(rec, rtReq(t, http.MethodPost, aid, pid, uid, `{"templateRef":"vite-react"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("up status = %d, body=%s", rec.Code, rec.Body.String())
	}

	// projectId query → the same runtime's preview.
	req := httptest.NewRequest(http.MethodGet, "/api/agents/"+aid+"/preview?projectId="+pid, nil)
	req.SetPathValue("id", aid)
	req = stampAuthAndUserID(req, uid)
	rec = httptest.NewRecorder()
	s.handleScopePreview(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("scope preview status = %d, body=%s", rec.Code, rec.Body.String())
	}
	m := decodeRuntimeJSON(t, rec)
	if m["status"] != "running" || m["previewUrl"] != "https://5173-fake.e2b.app" {
		t.Fatalf("scope preview = %+v", m)
	}

	// No runtime for a fresh scope → 200 {"status":"none"} (render
	// conditionally, not an error).
	req = httptest.NewRequest(http.MethodGet, "/api/agents/"+aid+"/preview?projectId=proj_never_started", nil)
	req.SetPathValue("id", aid)
	req = stampAuthAndUserID(req, uid)
	rec = httptest.NewRecorder()
	s.handleScopePreview(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("scope preview (none) status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if m := decodeRuntimeJSON(t, rec); m["status"] != "none" {
		t.Fatalf("scope preview (none) = %+v, want status none", m)
	}
}

// TestRuntime_ChangedFiles_Live — the changed-files view lists only what
// the agent produced vs the template baseline. On the fake pooled executor
// there is no git baseline marker, so the handler reports available=true
// with an empty file list (the "fall back to full file list" is the
// counterpart, covered by the not-enabled case above).
func TestRuntime_ChangedFiles_Live(t *testing.T) {
	s, uid, aid := newRuntimeTestServer(t)
	const pid = "proj_changed"
	seedProject(t, s, uid, aid, pid)

	rec := httptest.NewRecorder()
	s.handleRuntimeUp(rec, rtReq(t, http.MethodPost, aid, pid, uid, `{"templateRef":"shipany-tanstack"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("up status = %d, body=%s", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/agents/"+aid+"/changed-files?projectId="+pid, nil)
	req.SetPathValue("id", aid)
	req = stampAuthAndUserID(req, uid)
	rec = httptest.NewRecorder()
	s.handleChangedFiles(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("changed-files status = %d, body=%s", rec.Code, rec.Body.String())
	}
	m := decodeRuntimeJSON(t, rec)
	if m["available"] != true {
		t.Fatalf("changed-files = %+v, want available=true", m)
	}
	files, ok := m["files"].([]any)
	if !ok {
		t.Fatalf("changed-files 'files' = %v (%T), want array", m["files"], m["files"])
	}
	if len(files) != 0 {
		t.Fatalf("changed-files = %+v, want empty file list (no git baseline on fake)", m)
	}
}
