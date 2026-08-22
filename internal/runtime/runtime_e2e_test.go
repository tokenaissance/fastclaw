package runtime

// Pure-logic e2e tests for the project runtime layer, covering the
// backend-agnostic execution added upstream in:
//
//   - ff7ed19 feat: project runtime (coding-agent preview) + dashboard UI refinements
//   - 418ce82 feat: backend-agnostic project runtime + multi-template + preview UX
//
// Everything here runs in-process against a scripted sandbox.Executor — no
// real docker / E2B / boxlite connection, no network, no containers. The
// docker container path (Manager.upViaPool == false → sandbox.DockerSandbox
// → sb.Create()) is explicitly NOT exercised: it requires a live docker
// daemon. The pooled path (the code 418ce82 added) is fully covered by
// driving Manager.Up/Sleep/Wake through a stub executor that implements
// sandbox.PortExposer.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/sandbox"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// --- test doubles (no real sandbox backend) ---

// pooledExecFn scripts the Executor responses the pooled preview boot path
// probes for, so Up() completes end-to-end. Same contract as the setup
// package's fakeRuntimeExecutor, kept in-package for the pure-logic tests:
//
//   - "test -f .../package.json" → MISSING  (needsScaffoldExec → scaffold)
//   - curl --max-time 5          → 000     (startDevServerExec probe: not up)
//   - "... & echo started"       → started (startDevServerExec boot)
//   - curl --max-time 60         → 200     (waitForDevServerExec probe: up)
func pooledExecFn(_ context.Context, command string, _ time.Duration) (string, error) {
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

// stubExecutor implements sandbox.Executor + sandbox.PortExposer (like the
// real E2B executor), delegating Exec to an optional scripted function so
// tests can force probe failures.
type stubExecutor struct {
	execFn func(ctx context.Context, command string, timeout time.Duration) (string, error)
	// exposeErr forces ExposePort to fail, to assert the error path.
	exposeErr error
}

func (s *stubExecutor) Exec(ctx context.Context, command string, timeout time.Duration) (string, error) {
	if s.execFn != nil {
		return s.execFn(ctx, command, timeout)
	}
	return "", nil
}
func (s *stubExecutor) ReadFile(context.Context, string) (string, error)          { return "", nil }
func (s *stubExecutor) WriteFile(context.Context, string, string) (string, error) { return "", nil }
func (s *stubExecutor) ListDir(context.Context, string) (string, error)           { return "", nil }
func (s *stubExecutor) Backend() string                                           { return "e2b" }
func (s *stubExecutor) Close() error                                              { return nil }
func (s *stubExecutor) ExposePort(_ context.Context, port int) (string, error) {
	if s.exposeErr != nil {
		return "", s.exposeErr
	}
	return fmt.Sprintf("https://%d-stub.e2b.app", port), nil
}

// boxliteOnlyExecutor implements ONLY sandbox.Executor (like the real
// boxlite executor, which lacks PortExposer). The backend-agnostic runtime
// must detect the missing capability and fail Up with a clear error rather
// than minting a dead preview.
type boxliteOnlyExecutor struct {
	execFn func(ctx context.Context, command string, timeout time.Duration) (string, error)
}

func (b *boxliteOnlyExecutor) Exec(ctx context.Context, command string, timeout time.Duration) (string, error) {
	if b.execFn != nil {
		return b.execFn(ctx, command, timeout)
	}
	return "", nil
}
func (b *boxliteOnlyExecutor) ReadFile(context.Context, string) (string, error)          { return "", nil }
func (b *boxliteOnlyExecutor) WriteFile(context.Context, string, string) (string, error) { return "", nil }
func (b *boxliteOnlyExecutor) ListDir(context.Context, string) (string, error)           { return "", nil }
func (b *boxliteOnlyExecutor) Backend() string                                           { return "boxlite" }
func (b *boxliteOnlyExecutor) Close() error                                              { return nil }

type stubPool struct {
	ex sandbox.Executor
}

func (p *stubPool) Get(context.Context, string, string, string) (sandbox.Executor, error) {
	return p.ex, nil
}
func (p *stubPool) Release(string, string, string) error { return nil }
func (p *stubPool) CloseAll()                            {}
func (p *stubPool) Backend() string                      { return "e2b" }

// openRuntimeDB opens a fresh in-memory sqlite store with migrations
// applied — the same pattern the store package's own tests use.
func openRuntimeDB(t *testing.T) store.Store {
	t.Helper()
	db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate test db: %v", err)
	}
	return db
}

// --- multi-template + backend selection (418ce82) ---

// TestRuntime_MultiTemplate_Registration verifies the multi-template
// registry contract: the first registered ref stays the default no matter
// how many come later, and Templates() lists default-first then the rest
// sorted — the menu the preview tool advertises to the model.
func TestRuntime_MultiTemplate_Registration(t *testing.T) {
	m := NewManager(openRuntimeDB(t), t.TempDir(), "img", &sandbox.Policy{}, "", "", nil)
	if got := m.DefaultTemplate(); got != "" {
		t.Fatalf("DefaultTemplate before any registration = %q, want empty", got)
	}

	m.RegisterTemplate("shipany-tanstack", TemplateSpec{DevPort: 3000})
	m.RegisterTemplate("vite-react", TemplateSpec{DevPort: 5173})
	m.RegisterTemplate("next-js", TemplateSpec{DevPort: 3000})

	if got := m.DefaultTemplate(); got != "shipany-tanstack" {
		t.Fatalf("DefaultTemplate = %q, want first-registered shipany-tanstack", got)
	}
	got := m.Templates()
	want := []string{"shipany-tanstack", "next-js", "vite-react"} // default first, rest sorted
	if len(got) != len(want) {
		t.Fatalf("Templates() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Templates()[%d] = %q, want %q (full %v)", i, got[i], want[i], got)
		}
	}
}

// TestRuntime_BackendSelection is the backend-agnostic switch as a table:
// the pooled path is taken iff a non-empty, non-docker backend is paired
// with a non-nil pool; anything else falls back to the docker path (which
// this suite never runs against a real daemon).
func TestRuntime_BackendSelection(t *testing.T) {
	pool := &stubPool{ex: &stubExecutor{}}
	noPool := sandbox.ExecutorPool(nil)

	cases := []struct {
		name    string
		backend string
		pool    sandbox.ExecutorPool
		want    bool
	}{
		{"nil pool, empty backend", "", noPool, false},
		{"nil pool, e2b backend", "e2b", noPool, false}, // no pool → docker path
		{"pool, empty backend", "", pool, false},        // "" → docker default
		{"pool, docker backend", "docker", pool, false}, // docker never pooled
		{"pool, e2b backend", "e2b", pool, true},        // 418ce82 pooled path
		{"pool, boxlite backend", "boxlite", pool, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(openRuntimeDB(t), t.TempDir(), "img", &sandbox.Policy{}, "", tc.backend, tc.pool)
			if got := m.usesPool(); got != tc.want {
				t.Fatalf("usesPool() = %v, want %v (backend=%q poolNil=%v)", got, tc.want, tc.backend, tc.pool == nil)
			}
		})
	}
}

// --- pooled Up / Sleep / Wake lifecycle (418ce82) ---

// newPooledManager wires a manager on the backend-agnostic pooled path with
// a scripted executor, so Up() runs end-to-end without a real backend.
func newPooledManager(t *testing.T, ex sandbox.Executor) *Manager {
	t.Helper()
	return NewManager(openRuntimeDB(t), t.TempDir(), "img", &sandbox.Policy{}, "", "e2b", &stubPool{ex: ex})
}

func mustRegister(t *testing.T, m *Manager, ref string, spec TemplateSpec) {
	t.Helper()
	m.RegisterTemplate(ref, spec)
}

// TestRuntime_Up_Pooled drives a full pooled boot: scaffold → start dev
// server → wait for HTTP → ExposePort mints the preview URL. This is the
// exact flow 418ce82 added for cloud backends (dev server shares the
// agent's pooled sandbox; no host bind mount).
func TestRuntime_Up_Pooled(t *testing.T) {
	m := newPooledManager(t, &stubExecutor{execFn: pooledExecFn})
	mustRegister(t, m, "shipany-tanstack", TemplateSpec{DevPort: 3000, ScaffoldCmd: "pnpm install", DevCmd: "pnpm dev"})

	rec, err := m.Up(context.Background(), "u1", "a1", "p1", "", "shipany-tanstack")
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if rec.Status != StatusRunning {
		t.Fatalf("Up status = %q, want running", rec.Status)
	}
	if rec.DevPort != 3000 {
		t.Fatalf("DevPort = %d, want 3000", rec.DevPort)
	}
	if rec.HostPort != 0 {
		t.Fatalf("HostPort = %d, want 0 (pooled backends have no host port)", rec.HostPort)
	}
	if rec.PreviewURL != "https://3000-stub.e2b.app" {
		t.Fatalf("PreviewURL = %q, want https://3000-stub.e2b.app", rec.PreviewURL)
	}

	// Persisted and idempotent: a second Get returns the same running row.
	got, err := m.Get(context.Background(), "u1", "a1", "p1", "")
	if err != nil {
		t.Fatalf("Get after Up: %v", err)
	}
	if got.Status != StatusRunning || got.PreviewURL != "https://3000-stub.e2b.app" {
		t.Fatalf("Get record = %+v, want running with preview", got)
	}
}

// TestRuntime_Up_Pooled_TemplateRequired — first Up with no template ref is
// a hard error (the store row can't be provisioned without a spec).
func TestRuntime_Up_Pooled_TemplateRequired(t *testing.T) {
	m := newPooledManager(t, &stubExecutor{execFn: pooledExecFn})
	mustRegister(t, m, "shipany-tanstack", TemplateSpec{DevPort: 3000})

	if _, err := m.Up(context.Background(), "u1", "a1", "p1", "", ""); err == nil {
		t.Fatal("Up with empty templateRef succeeded, want error")
	} else if !strings.Contains(err.Error(), "templateRef") {
		t.Fatalf("error = %q, want templateRef-required", err.Error())
	}
}

// TestRuntime_Up_Pooled_UnknownTemplate — with 2+ registered templates the
// lenient single-template canonicalization is off, so a bad ref fails.
func TestRuntime_Up_Pooled_UnknownTemplate(t *testing.T) {
	m := newPooledManager(t, &stubExecutor{execFn: pooledExecFn})
	mustRegister(t, m, "shipany-tanstack", TemplateSpec{DevPort: 3000})
	mustRegister(t, m, "vite-react", TemplateSpec{DevPort: 5173})

	if _, err := m.Up(context.Background(), "u1", "a1", "p1", "", "does-not-exist"); err == nil {
		t.Fatal("Up with unknown template succeeded, want error")
	} else if !strings.Contains(err.Error(), "unknown template") {
		t.Fatalf("error = %q, want unknown-template", err.Error())
	}
}

// TestRuntime_Up_Pooled_LenientSingleTemplate — with exactly one registered
// template the manager canonicalizes any loose ref to it (the model passing
// "shipany" for "shipany-tanstack"), the lenient behavior that makes a
// one-template deployment frictionless.
func TestRuntime_Up_Pooled_LenientSingleTemplate(t *testing.T) {
	m := newPooledManager(t, &stubExecutor{execFn: pooledExecFn})
	mustRegister(t, m, "shipany-tanstack", TemplateSpec{DevPort: 3000, DevCmd: "pnpm dev"})

	rec, err := m.Up(context.Background(), "u1", "a1", "p1", "", "shipany")
	if err != nil {
		t.Fatalf("Up('shipany'): %v", err)
	}
	if rec.TemplateRef != "shipany-tanstack" {
		t.Fatalf("TemplateRef = %q, want canonicalized shipany-tanstack", rec.TemplateRef)
	}
	if rec.Status != StatusRunning {
		t.Fatalf("status = %q, want running", rec.Status)
	}
}

// TestRuntime_Up_Pooled_BackendCannotExposePort — a backend that lacks
// PortExposer (boxlite) can't host a preview; the runtime must fail Up with
// an actionable error instead of reporting a dead "running" app.
func TestRuntime_Up_Pooled_BackendCannotExposePort(t *testing.T) {
	m := NewManager(openRuntimeDB(t), t.TempDir(), "img", &sandbox.Policy{}, "", "boxlite", &stubPool{ex: &boxliteOnlyExecutor{execFn: pooledExecFn}})
	mustRegister(t, m, "shipany-tanstack", TemplateSpec{DevPort: 3000, DevCmd: "pnpm dev"})

	if _, err := m.Up(context.Background(), "u1", "a1", "p1", "", "shipany-tanstack"); err == nil {
		t.Fatal("Up on a non-exposing backend succeeded, want error")
	} else if !strings.Contains(err.Error(), "can't expose preview ports") {
		t.Fatalf("error = %q, want can't-expose-preview-ports", err.Error())
	}
}

// TestRuntime_Up_Pooled_ExposePortFailure — even with the capability, a
// backend outage minting the URL must crash the runtime, not report running.
func TestRuntime_Up_Pooled_ExposePortFailure(t *testing.T) {
	st := &stubExecutor{execFn: pooledExecFn, exposeErr: errors.New("e2b: sandbox not created")}
	m := newPooledManager(t, st)
	mustRegister(t, m, "shipany-tanstack", TemplateSpec{DevPort: 3000, DevCmd: "pnpm dev"})

	if _, err := m.Up(context.Background(), "u1", "a1", "p1", "", "shipany-tanstack"); err == nil {
		t.Fatal("Up with ExposePort failure succeeded, want error")
	} else if !strings.Contains(err.Error(), "expose port") {
		t.Fatalf("error = %q, want expose-port error", err.Error())
	}
}

// TestRuntime_SleepWake_Pooled — Sleep keeps the record (status sleeping,
// preview cleared); Wake re-boots it reusing the stored template ref. The
// pooled path has no container to tear down, so this is the full lifecycle
// the SaaS shell drives via POST /runtime/sleep + /runtime/wake.
func TestRuntime_SleepWake_Pooled(t *testing.T) {
	m := newPooledManager(t, &stubExecutor{execFn: pooledExecFn})
	mustRegister(t, m, "vite-react", TemplateSpec{DevPort: 5173, DevCmd: "pnpm dev"})

	if _, err := m.Up(context.Background(), "u1", "a1", "p1", "", "vite-react"); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if err := m.Sleep(context.Background(), "u1", "a1", "p1", ""); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	slept, err := m.Get(context.Background(), "u1", "a1", "p1", "")
	if err != nil {
		t.Fatalf("Get after Sleep: %v", err)
	}
	if slept.Status != StatusSleeping || slept.PreviewURL != "" {
		t.Fatalf("after Sleep = %+v, want sleeping with cleared preview", slept)
	}

	// Wake passes no template ref — the stored one (vite-react) is reused.
	woke, err := m.Wake(context.Background(), "u1", "a1", "p1", "")
	if err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if woke.Status != StatusRunning || woke.TemplateRef != "vite-react" {
		t.Fatalf("after Wake = %+v, want running on vite-react", woke)
	}
	if woke.PreviewURL != "https://5173-stub.e2b.app" {
		t.Fatalf("woke PreviewURL = %q, want https://5173-stub.e2b.app", woke.PreviewURL)
	}
}

// --- pure helpers shared by both paths (ff7ed19) ---

// TestRuntime_NmVolumeName — the per-scope node_modules volume name must be
// deterministic from scopeID and docker-name-safe (colon → dash).
func TestRuntime_NmVolumeName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"sess:abc", "fastclaw-nm-sess-abc"},
		{"proj_x", "fastclaw-nm-proj_x"},
		{"a b/c", "fastclaw-nm-a-b-c"},
		{"proj_1.2-3", "fastclaw-nm-proj_1.2-3"},
		{"", "fastclaw-nm-"},
	}
	for _, tc := range cases {
		if got := nmVolumeName(tc.in); got != tc.want {
			t.Fatalf("nmVolumeName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRuntime_DetectVitePort — scraping the "Local: http://localhost:NNNN/"
// line Vite prints on boot; 0 when absent.
func TestRuntime_DetectVitePort(t *testing.T) {
	cases := []struct {
		log  string
		want int
	}{
		{"Local: http://localhost:5173/\n  Network: http://10.0.0.1:5173/\n", 5173},
		{"  VITE v5.0.0  ready in 1200 ms\n  Local: http://localhost:3000/\n", 3000},
		{"  Local: http://localhost:51737/\n", 51737},
		{"no marker here\n", 0},
		{"Local: http://localhost:abc/\n", 0}, // non-digit after colon
	}
	for _, tc := range cases {
		if got := detectVitePort(tc.log); got != tc.want {
			t.Fatalf("detectVitePort(%q) = %d, want %d", tc.log, got, tc.want)
		}
	}
}

// TestRuntime_ShellSingleQuote — the dev-server boot wraps DevCmd in a
// single-quoted layer of `sh -c`; embedded quotes must be escaped so a
// template command like `pnpm dev --port 3000` survives intact.
func TestRuntime_ShellSingleQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"echo hi", "'echo hi'"},
		{"pnpm dev --host 0.0.0.0 --port 3000", "'pnpm dev --host 0.0.0.0 --port 3000'"},
		{"it's", `'it'\''s'`},
		{"", "''"},
	}
	for _, tc := range cases {
		if got := shellSingleQuote(tc.in); got != tc.want {
			t.Fatalf("shellSingleQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRuntime_Tail — error blobs are trimmed to the last n bytes.
func TestRuntime_Tail(t *testing.T) {
	if got := tail("short", 100); got != "short" {
		t.Fatalf("tail short = %q, want unchanged", got)
	}
	if got := tail("abcdef", 3); got != "def" {
		t.Fatalf("tail abcdef/3 = %q, want def", got)
	}
	if got := tail("", 5); got != "" {
		t.Fatalf("tail empty = %q, want empty", got)
	}
}

// TestRuntime_RtKey — the live-map key is the stable (user|agent|scope)
// triple.
func TestRuntime_RtKey(t *testing.T) {
	if got := rtKey("u1", "a1", "p1"); got != "u1|a1|p1" {
		t.Fatalf("rtKey = %q, want u1|a1|p1", got)
	}
}
