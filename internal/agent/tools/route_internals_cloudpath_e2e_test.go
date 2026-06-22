package tools

// e2e for commit fc63397 "fix(tools): block FastClaw internals from host
// file routing" (fork commit: cherry-pick + rebrand to FastAgent).
//
// Cloud-path mirror: a Cloud (Next.js app) user talks to FastAgent
// through the /api/fastagent proxy and posts /chat messages; every file
// tool call (read_file / write_file / list_dir) is dispatched through
// Registry.routeFor, which decides sandbox vs host filesystem. The
// vulnerability this commit fixes: in local-with-sandbox mode an internal
// FastAgent path (~/.fastagent/..., /root/.fastagent/..., $FASTAGENT_HOME/...)
// used to fall through to RouteHostFS, so a chat-facing tool could read
// or overwrite the agent's runtime internals (DB, credentials, config)
// on the operator's machine. The fix routes those to RouteSandbox —
// operator maintenance must go through host_exec instead.
//
// These tests drive the REAL read_file handler (Registry.Execute) with a
// sandbox executor attached, proving internal paths hit the executor
// (never os.* on the host) while a legit explicit host-scope path still
// routes to host FS.

import (
	"context"
	"strings"
	"testing"
	"time"
)

// routeRecExecutor is a recording sandbox executor: ReadFile returns a
// sentinel payload and counts calls, so the test can prove the sandbox
// executor (not the host filesystem) handled the internal path.
type routeRecExecutor struct {
	readCalls int
}

func (r *routeRecExecutor) Exec(ctx context.Context, command string, timeout time.Duration) (string, error) {
	return "", nil
}
func (r *routeRecExecutor) ReadFile(ctx context.Context, path string) (string, error) {
	r.readCalls++
	return "sandbox-executor-bytes", nil
}
func (r *routeRecExecutor) WriteFile(ctx context.Context, path, content string) (string, error) {
	return "", nil
}
func (r *routeRecExecutor) ListDir(ctx context.Context, path string) (string, error) {
	return "", nil
}
func (r *routeRecExecutor) Backend() string { return "test" }
func (r *routeRecExecutor) Close() error    { return nil }

func TestRoute_Internals_CloudPathE2E(t *testing.T) {
	ctx := context.Background()
	ex := &routeRecExecutor{}
	r := NewRegistry(t.TempDir(), t.TempDir())
	r.SetExecutor(ex)

	// Internal roots: /root/.fastagent, ~/.fastagent, and the env-var
	// root must all route to the sandbox executor — never host disk.
	t.Run("internal-root-routes-to-sandbox", func(t *testing.T) {
		for _, p := range []string{
			"/root/.fastagent/workspaces/secret.txt",
			"/root/.fastagent/db/fastagent.db",
		} {
			before := ex.readCalls
			res, err := r.Execute(ctx, "read_file", `{"path":"`+p+`"}`)
			if err != nil {
				t.Fatalf("read_file(%q) err = %v (must route to sandbox, not fail on host)", p, err)
			}
			if !strings.HasPrefix(res, MetaSandboxPrefix) || !strings.Contains(res, "sandbox-executor-bytes") {
				t.Errorf("read_file(%q) = %q, want sandbox executor payload %q", p, res, MetaSandboxPrefix+"sandbox-executor-bytes")
			}
			if ex.readCalls != before+1 {
				t.Errorf("read_file(%q): executor.readCalls = %d, want %d (host route would leave it unchanged)", p, ex.readCalls, before+1)
			}
		}
	})

	// ~/.fastagent resolves to the operator's home on this machine; the
	// executor must still be the one serving it (RouteSandbox), not host.
	t.Run("tilde-home-internal-routes-to-sandbox", func(t *testing.T) {
		p := "~/.fastagent/config.json"
		before := ex.readCalls
		res, err := r.Execute(ctx, "read_file", `{"path":"`+p+`"}`)
		if err != nil {
			t.Fatalf("read_file(%q) err = %v", p, err)
		}
		if !strings.HasPrefix(res, MetaSandboxPrefix) {
			t.Errorf("read_file(%q) = %q, want sandbox prefix", p, res)
		}
		if ex.readCalls != before+1 {
			t.Errorf("read_file(%q): executor.readCalls = %d, want %d", p, ex.readCalls, before+1)
		}
	})

	// $FASTAGENT_HOME overrides the runtime home; an internal path under
	// it must also route to the sandbox.
	t.Run("env-home-internal-routes-to-sandbox", func(t *testing.T) {
		t.Setenv("FASTAGENT_HOME", "/srv/fastagent")
		p := "/srv/fastagent/workspaces/notes.md"
		before := ex.readCalls
		res, err := r.Execute(ctx, "read_file", `{"path":"`+p+`"}`)
		if err != nil {
			t.Fatalf("read_file(%q) err = %v", p, err)
		}
		if !strings.HasPrefix(res, MetaSandboxPrefix) {
			t.Errorf("read_file(%q) = %q, want sandbox prefix", p, res)
		}
		if ex.readCalls != before+1 {
			t.Errorf("read_file(%q): executor.readCalls = %d, want %d", p, ex.readCalls, before+1)
		}
	})

	// A legitimate explicit host-scope path (the operator's Documents)
	// keeps RouteHostFS: the executor is untouched and the handler goes
	// to os.* on the host (which errors here only because the file
	// doesn't exist — the point is it is NOT served by the executor).
	t.Run("explicit-host-scope-stays-on-host", func(t *testing.T) {
		before := ex.readCalls
		_, err := r.Execute(ctx, "read_file", `{"path":"/Users/operator/Documents/report.txt"}`)
		if ex.readCalls != before {
			t.Errorf("host-scope path must not reach the sandbox executor: readCalls %d → %d", before, ex.readCalls)
		}
		if err == nil {
			t.Fatalf("host-scope read of a missing file should error on host, got nil (executor would have returned sentinel)")
		}
		if !strings.Contains(err.Error(), "host read") {
			t.Errorf("host-scope read error = %v, want host read failure (proving RouteHostFS dispatch)", err)
		}
	})
}
