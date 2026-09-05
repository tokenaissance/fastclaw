package tools

// e2e for commit c8cdc45 "fix(security): block identity file access in
// apply_patch read/write".
//
// Cloud zero-impact rationale: apply_patch is a Go agent-runtime tool
// (registered in internal/agent/tools). Cloud (Next.js app) talks to
// FastAgent through the /api/fastagent proxy and posts /chat messages;
// the tool registry and its identity gate live entirely behind the
// agent loop. No endpoint, response shape, or auth change.
//
// The vulnerability: the apply_patch tool could read AND write
// SOUL.md / IDENTITY.md / BOOTSTRAP.md / AGENTS.md / TOOLS.md /
// HEARTBEAT.md through its patch backend — bypassing the
// identityFileBlocked gate that write_file/edit_file already enforced.
// A non-admin chatter could exfiltrate the agent's persona config or
// rewrite its identity. The fix: readForPatch and writeForPatch refuse
// identity files up front with the canonical IdentityFileRefusal tool
// message.
//
// Fork adaptation: the fork adds sandbox-mode variants
// (readForPatchSandbox / writeForPatchSandbox) that serve/write single-
// segment identity files through systemFileStore before falling back to
// the sandbox executor. Those variants had NO identity gate (file.go's
// sandboxed read_file/edit_file were already gated, and host-fs
// apply_patch now is too) — so sandbox-mode apply_patch was the only
// unblocked path. This review extends the same refusal to both sandbox
// variants so the fix is complete in the fork.

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/workspace"
)

// recordingWorkspaceStore counts Get/Put so the test can prove the
// identity gate short-circuits BEFORE storage routing.
type recordingWorkspaceStore struct {
	files      map[string][]byte
	getCalls   int
	putCalls   int
	putContent map[string]string
}

func (r *recordingWorkspaceStore) Put(ctx context.Context, agentID, projectID, sessionID, path string, rd io.Reader, size int64, contentType string) error {
	r.putCalls++
	b, _ := io.ReadAll(rd)
	if r.putContent == nil {
		r.putContent = map[string]string{}
	}
	r.putContent[path] = string(b)
	return nil
}
func (r *recordingWorkspaceStore) Get(ctx context.Context, agentID, projectID, sessionID, path string) (io.ReadCloser, error) {
	r.getCalls++
	b, ok := r.files[path]
	if !ok {
		return nil, workspace.ErrNotFound
	}
	return io.NopCloser(strings.NewReader(string(b))), nil
}
func (r *recordingWorkspaceStore) Stat(ctx context.Context, agentID, projectID, sessionID, path string) (*workspace.ObjectInfo, error) {
	return nil, workspace.ErrNotFound
}
func (r *recordingWorkspaceStore) List(ctx context.Context, agentID, projectID, sessionID string) ([]workspace.ObjectInfo, error) {
	return nil, nil
}
func (r *recordingWorkspaceStore) Delete(ctx context.Context, agentID, projectID, sessionID, path string) error {
	return nil
}
func (r *recordingWorkspaceStore) Move(ctx context.Context, agentID, fromProjectID, fromSessionID, toProjectID, toSessionID string) error {
	return nil
}
func (r *recordingWorkspaceStore) SignedURL(ctx context.Context, agentID, projectID, sessionID, path string, ttl time.Duration) (string, error) {
	return "", workspace.ErrSignedURLUnsupported
}

// recordingSystemFileStore counts GetWorkspaceFile/SaveWorkspaceFile so
// the test can prove sandbox-mode apply_patch refuses BEFORE the fork's
// systemFileStore identity serving is consulted.
type recordingSystemFileStore struct {
	getCalls int
	putCalls int
	soul     []byte
}

func (r *recordingSystemFileStore) GetWorkspaceFile(ctx context.Context, agentID, userID, filename string) ([]byte, error) {
	r.getCalls++
	if r.soul != nil {
		return r.soul, nil
	}
	return nil, errors.New("not found")
}
func (r *recordingSystemFileStore) GetWorkspaceFileExact(ctx context.Context, agentID, userID, filename string) ([]byte, error) {
	r.getCalls++
	return nil, errors.New("not found")
}
func (r *recordingSystemFileStore) SaveWorkspaceFile(ctx context.Context, agentID, userID, filename string, data []byte) error {
	r.putCalls++
	return nil
}

// recordingExecutor counts ReadFile/WriteFile so the test can prove the
// sandbox executor is never reached on a refused identity file.
type recordingExecutor struct {
	readCalls  int
	writeCalls int
}

func (r *recordingExecutor) Exec(ctx context.Context, command string, timeout time.Duration) (string, error) {
	return "", nil
}
func (r *recordingExecutor) ReadFile(ctx context.Context, path string) (string, error) {
	r.readCalls++
	return "executor-bytes", nil
}
func (r *recordingExecutor) WriteFile(ctx context.Context, path, content string) (string, error) {
	r.writeCalls++
	return "", nil
}
func (r *recordingExecutor) ListDir(ctx context.Context, path string) (string, error) {
	return "", nil
}
func (r *recordingExecutor) Backend() string { return "test" }
func (r *recordingExecutor) Close() error    { return nil }

func refusalMatches(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected IdentityFileRefusal, got nil error", what)
	}
	if !strings.Contains(err.Error(), IdentityFileRefusal) {
		t.Fatalf("%s: error does not carry IdentityFileRefusal: %v", what, err)
	}
}

func TestApplyPatch_IdentityGate_CloudPathE2E(t *testing.T) {
	ctx := context.Background()
	ws := &recordingWorkspaceStore{
		files: map[string][]byte{"report.md": []byte("report-bytes")},
	}
	sfs := &recordingSystemFileStore{soul: []byte("我是拽姐，一个帮你处理事务的智能体。")}
	ex := &recordingExecutor{}

	// ── Host-fs mode (upstream fix): chatter + identity file → refusal,
	// store never consulted (gate is the FIRST statement).
	host := &Registry{workspaceStore: ws, agentID: "agt_1", callerIsAdmin: false}
	_, err := host.readForPatch(ctx, "SOUL.md")
	refusalMatches(t, err, "readForPatch(SOUL.md)")
	if err := host.writeForPatch(ctx, "SOUL.md", "hacked"); err == nil || !strings.Contains(err.Error(), IdentityFileRefusal) {
		t.Fatalf("writeForPatch(SOUL.md): expected refusal, got %v", err)
	}
	if ws.getCalls != 0 || ws.putCalls != 0 {
		t.Errorf("identity refusal must short-circuit storage: get=%d put=%d, want 0/0", ws.getCalls, ws.putCalls)
	}

	// Host pass-through: a normal workspace file still flows to the store.
	if got, err := host.readForPatch(ctx, "report.md"); err != nil || got != "report-bytes" {
		t.Errorf("readForPatch(report.md) = %q, %v; want report-bytes, nil", got, err)
	}
	if err := host.writeForPatch(ctx, "report.md", "v2"); err != nil {
		t.Fatalf("writeForPatch(report.md): %v", err)
	}
	if ws.getCalls != 1 || ws.putCalls != 1 {
		t.Errorf("non-identity file must reach store: get=%d put=%d, want 1/1", ws.getCalls, ws.putCalls)
	}
	if ws.putContent["report.md"] != "v2" {
		t.Errorf("writeForPatch(report.md) stored %q, want v2", ws.putContent["report.md"])
	}

	// ── Sandbox mode (fork adaptation): chatter + identity file → refusal
	// BEFORE the fork's systemFileStore serving or the executor.
	sb := &Registry{workspaceStore: ws, systemFileStore: sfs, agentID: "agt_1", callerIsAdmin: false}
	_, err = sb.readForPatchSandbox(ctx, ex, "SOUL.md")
	refusalMatches(t, err, "readForPatchSandbox(SOUL.md)")
	if err := sb.writeForPatchSandbox(ctx, ex, "SOUL.md", "hacked"); err == nil || !strings.Contains(err.Error(), IdentityFileRefusal) {
		t.Fatalf("writeForPatchSandbox(SOUL.md): expected refusal, got %v", err)
	}
	if sfs.getCalls != 0 || sfs.putCalls != 0 {
		t.Errorf("sandbox identity refusal must short-circuit systemFileStore: get=%d put=%d, want 0/0", sfs.getCalls, sfs.putCalls)
	}
	if ex.readCalls != 0 || ex.writeCalls != 0 {
		t.Errorf("sandbox identity refusal must not reach executor: read=%d write=%d, want 0/0", ex.readCalls, ex.writeCalls)
	}

	// Sandbox pass-through: a normal workspace file still flows to the
	// workspace store (workspace path branch).
	if got, err := sb.readForPatchSandbox(ctx, ex, "report.md"); err != nil || got != "report-bytes" {
		t.Errorf("readForPatchSandbox(report.md) = %q, %v; want report-bytes, nil", got, err)
	}
	if err := sb.writeForPatchSandbox(ctx, ex, "report.md", "v3"); err != nil {
		t.Fatalf("writeForPatchSandbox(report.md): %v", err)
	}

	// ── Admin bypass: owner may legitimately read identity files — the
	// gate respects callerIsAdmin and does NOT refuse.
	admin := &Registry{workspaceStore: ws, systemFileStore: sfs, agentID: "agt_1", callerIsAdmin: true}
	if admin.identityFileBlocked("SOUL.md") {
		t.Error("admin must not be blocked on identity files")
	}
	_, err = admin.readForPatch(ctx, "SOUL.md")
	if err != nil && strings.Contains(err.Error(), IdentityFileRefusal) {
		t.Fatalf("admin readForPatch(SOUL.md) must bypass the gate, got refusal: %v", err)
	}
}
