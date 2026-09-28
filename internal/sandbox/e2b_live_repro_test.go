package sandbox

// End-to-end acceptance test for the 2026-09-17 deliverable-revert repair,
// against a REAL E2B sandbox. NOT part of the normal suite — it creates a
// sandbox, so it is gated behind FASTAGENT_E2B_LIVE=1:
//
//	FASTAGENT_E2B_LIVE=1 E2B_API_KEY=e2b_... \
//	  go test ./internal/sandbox/ -run TestE2BLiveRepro -v -count=1
//
// It walks the exact order the incident did (07 §2.4) and asserts the outcome
// the repair promises — the store keeps the host's version:
//
//	1. sandbox born from the store's v1        (Hydrate — 07 §2.4 t₀)
//	2. host tool writes v2 to the store only   (HostWrite — t₁)
//	3. an exec runs                             (the trigger)
//	4. the scope is evicted                     (Sync — t₂)
//
// Before the repair this test failed with "REPRODUCED the incident" — the
// sandbox's stale v1 overwrote v2, once per exec. It passes now because the
// reconcile precondition refuses that migration and says so:
//
//	sandbox sync: BLOCKED — store object was written after this sandbox copied it
//	  path=report.txt storeBytes=54 snapshotBytes=40
//	  storeMod=… baselineMod=…          (different ⇒ host write happened)
//
// The run is self-contained: a private agent name from a fresh temp store, and
// the pool is closed on the way out. Nothing here touches a production scope.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/workspace"
)

const liveScopeSession = "repro" // fixed within a run; the agent name is unique per run

// liveE2B builds a real E2B-backed pool over a fresh temp store and returns the
// running sandbox plus everything the caller needs to assert against. Skipped
// unless FASTAGENT_E2B_LIVE=1; the API key comes from the environment.
func liveE2B(t *testing.T, agent string, seed func(store *workspace.LocalFS)) (logs *bytes.Buffer, store *workspace.LocalFS, lp *LifecyclePool, ex Executor) {
	t.Helper()
	if os.Getenv("FASTAGENT_E2B_LIVE") != "1" {
		t.Skip("live E2B: set FASTAGENT_E2B_LIVE=1 with E2B_API_KEY to run")
	}
	apiKey := os.Getenv("E2B_API_KEY")
	if apiKey == "" {
		t.Fatal("E2B_API_KEY is required")
	}
	template := os.Getenv("FASTAGENT_E2B_TEMPLATE")
	if template == "" {
		template = "base"
	}

	// slog to a buffer: the sync trace is evidence, not noise, in these tests.
	logs = &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	store = workspace.NewLocalFS(t.TempDir())
	// Seed BEFORE the pool exists: this is what makes the hydrate copy these
	// objects into the fresh sandbox (07 §2.4 t₀).
	seed(store)

	inner := NewE2BExecutorPool(apiKey, template, t.TempDir(), 10*time.Minute)
	inner.SetWorkspace(store)
	lp = NewLifecyclePool(inner, time.Hour, time.Hour)
	lp.SetWorkspace(store)
	t.Cleanup(func() { lp.CloseAll() })

	ex, err := lp.Get(context.Background(), agent, "", liveScopeSession)
	if err != nil {
		t.Fatalf("sandbox up: %v", err)
	}
	return logs, store, lp, ex
}

func TestE2BLiveRepro(t *testing.T) {
	ctx := context.Background()

	const v1 = "OLD SNAPSHOT VERSION (1111111111111111)\n"
	const v2 = "NEW HOST WRITE VERSION (2222222222222222222222222222)\n"
	agent := liveAgentName()
	logs, store, lp, ex := liveE2B(t, agent, func(s *workspace.LocalFS) {
		if err := putFile(ctx, s, agent, "report.txt", v1); err != nil {
			t.Fatalf("seed store: %v", err)
		}
	})

	// 1. birth: the hydrate delivered v1
	if out, err := ex.Exec(ctx, "cat /workspace/report.txt", 60*time.Second); err != nil {
		t.Fatalf("exec after hydrate: %v (%s)", err, out)
	} else if !strings.Contains(out, "OLD SNAPSHOT") {
		t.Fatalf("hydrate did not deliver v1; sandbox said %q", out)
	}
	t.Log("step 1 ok: sandbox born holding v1")

	// 2. the host tool writes the new version — store only, sandbox untouched
	if err := putFile(ctx, store, agent, "report.txt", v2); err != nil {
		t.Fatalf("host write: %v", err)
	}
	if got := readStore(t, store, agent, "report.txt"); got != v2 {
		t.Fatalf("store does not hold v2 after the host write: %q", got)
	}
	t.Log("step 2 ok: store holds v2, sandbox still holds v1")

	// 3. an exec — the sandbox-side activity that triggers the post-exec sync
	if out, err := ex.Exec(ctx, "ls /workspace", 60*time.Second); err != nil {
		t.Fatalf("exec: %v (%s)", err, out)
	}
	// 4. eviction sync (same code path as the incident's 12:15 line)
	lp.syncSnapshot(ctx, sandboxScope{agentID: agent, sessionID: liveScopeSession}, ex, "repro-evict")

	// ── verdict: the store survives
	//
	// This scenario deliberately SKIPS the write-through mirror, which is what a
	// pre-write-through build (or a caller that bypassed the tool layer) does.
	// The promise is still that the store keeps the host's version: the sandbox
	// holds exactly what it was handed (the v1 digest), so the reconcile
	// classifies it as "nothing moved in the sandbox" and skips it — no write,
	// no revert. The mirror's own end-to-end behaviour is
	// TestE2BLiveWriteThroughReachesSandbox.
	got := readStore(t, store, agent, "report.txt")
	t.Logf("store after sync: %q", got)
	t.Logf("sync trace:\n%s", excerpt(logs.String(), "sandbox sync:"))

	if got != v2 {
		t.Errorf("the store lost the host's version — the incident is back\n"+
			"  store now: %q\n  want:      %q\n"+
			"  see docs/文件系统形式化证明/07-formal-rootcause-and-fix.md §3.3", got, v2)
	}

	// The other half of the same fact, and the repair row 87 added: the store's
	// copy is the SUCCESSOR, so the reconcile does not stop at preserving it — it
	// delivers it INTO the sandbox, where the agent will read it. Until 2026-09-28
	// this line read "the sandbox still holds what it was handed", and that stale
	// sandbox copy is what produced `KeyError: 'current_pnl'` in production: the
	// agent kept working against a container the store had moved past (docs 11
	// §13.4, the S3 paper family).
	//
	// The delivery is not the write-through mirror: no tool call was made here (the
	// store was written directly), so the only writer that could have reached the
	// sandbox is the reconcile.
	if out, err := ex.Exec(ctx, "cat /workspace/report.txt", 60*time.Second); err != nil {
		t.Fatalf("exec after reconcile: %v (%s)", err, out)
	} else if !strings.Contains(out, "NEW HOST WRITE") {
		t.Errorf("the sandbox was NOT given the store's newer copy, so it is still working against a stale "+
			"container (sandbox said %q, store holds %q)", out, v2)
	}
	if trace := logs.String(); !strings.Contains(trace, "the store's copy is newer; delivered into the sandbox") {
		t.Errorf("the store's newer copy reached the sandbox without the reconcile saying so — "+
			"sync trace:\n%s", excerpt(trace, "sandbox sync:"))
	}

	// And the sandbox artefact branch must still work on the same instance:
	// exec output has to reach the store, or the sync would be pointless.
	if out, err := ex.Exec(ctx, "echo artifact > /workspace/from_exec.txt", 60*time.Second); err != nil {
		t.Fatalf("exec writing an artefact: %v (%s)", err, out)
	}
	lp.syncSnapshot(ctx, sandboxScope{agentID: agent, sessionID: liveScopeSession}, ex, "repro-artifact")
	if got := readStore(t, store, agent, "from_exec.txt"); !strings.Contains(got, "artifact") {
		t.Errorf("an exec artefact did not reach the store: %q", got)
	}
}

// The other half of the as-built picture, as a repeatable check instead of the
// one-off probe that established it: hydrate preserves the store's write time
// on the sandbox file, and the two copies are genuinely separate filesystems
// (a path the store does not have is 404 inside the sandbox). 01 §2 notes this
// is why mtime cannot arbitrate between the copies; 07 §1.1 recorded it by
// hand the first time.
func TestE2BLiveHydrateKeepsStoreStamp(t *testing.T) {
	ctx := context.Background()
	const body = "STAMPED CONTENT\n"
	agent := liveAgentName()
	_, store, _, ex := liveE2B(t, agent, func(s *workspace.LocalFS) {
		if err := putFile(ctx, s, agent, "stamped.txt", body); err != nil {
			t.Fatalf("seed store: %v", err)
		}
	})

	// The store's own stamp for the object it just wrote.
	info, err := store.Stat(ctx, agent, "", liveScopeSession, "stamped.txt")
	if err != nil {
		t.Fatalf("store stat: %v", err)
	}

	out, err := ex.Exec(ctx, "stat -c %Y /workspace/stamped.txt", 60*time.Second)
	if err != nil {
		t.Fatalf("stat in sandbox: %v (%s)", err, out)
	}
	// Tar stores whole seconds and the store's mtime carries sub-second
	// precision, so the sandbox copy can land one second earlier. What matters
	// is that it is the store's stamp rather than the hydrate's own clock:
	// a truncation is at most 1s, while "stamped at hydrate time" would be
	// minutes later on a warm sandbox.
	sandboxSecs := atoiOrFail(t, strings.TrimSpace(out))
	storeSecs := info.ModTime.Unix()
	if d := sandboxSecs - storeSecs; d > 1 || d < -1 {
		t.Errorf("sandbox file mtime %d is %+ds from the store's LastModified %d — the hydrate is no longer "+
			"stamping the store's write time onto the file, so reasoning about mtimes across the two copies "+
			"is unsound (01 §2, 07 §1.1)", sandboxSecs, d, storeSecs)
	}

	// A path only the store has must NOT exist inside the sandbox: that is the
	// "two copies" premise the whole analysis rests on.
	if out, err := ex.Exec(ctx, "test -e /workspace/only_in_store.txt && echo yes || echo no", 60*time.Second); err != nil {
		t.Fatalf("probe: %v (%s)", err, out)
	} else if strings.Contains(out, "yes") {
		t.Errorf("a store-only path appeared inside the sandbox — the copies are not separate")
	}
}

// liveAgentName returns this run's private agent name. The store is a temp dir
// and the scope is private, so no production row is read or written.
func liveAgentName() string { return fmt.Sprintf("live_%d", time.Now().UnixNano()) }

func atoiOrFail(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("expected an integer from the sandbox, got %q: %v", s, err)
	}
	return n
}

// The layout claim behind 01 §3.5: for a loose scope the store key carries a
// `sessions/<id>/` prefix (S3.key / LocalFS.scopeDir), and the hydrate unpacks
// the tar at /, so the materialised path matches the key — NOT the bare
// /workspace/<path> that mirrorCodingWriteToSandbox hardcodes. Pinning this on
// a real sandbox is what makes the mirror's path rule falsifiable rather than a
// code-reading claim.
func TestE2BLiveLooseScopeLayout(t *testing.T) {
	ctx := context.Background()
	agent := liveAgentName()
	_, _, _, ex := liveE2B(t, agent, func(s *workspace.LocalFS) {
		if err := putFile(ctx, s, agent, "layout.txt", "x\n"); err != nil {
			t.Fatalf("seed store: %v", err)
		}
	})

	// LocalFS.scopeDir = <root>/<agent>/sessions/<sid>/, and the tar entry is
	// "workspace/" + the path relative to that prefix ⇒ /workspace/layout.txt.
	if out, err := ex.Exec(ctx, "test -f /workspace/layout.txt && echo yes || echo no", 60*time.Second); err != nil {
		t.Fatalf("probe: %v (%s)", err, out)
	} else if !strings.Contains(out, "yes") {
		t.Errorf("a loose scope's file is not at /workspace/<path> (got %q) — the hydrate layout changed", out)
	}
	// And it is NOT duplicated under the scope prefix, which is what a mirror
	// hardcoding /workspace/<path> would produce for agent-scoped stores.
	if out, err := ex.Exec(ctx, "test -e /workspace/sessions && echo yes || echo no", 60*time.Second); err != nil {
		t.Fatalf("probe: %v (%s)", err, out)
	} else if strings.Contains(out, "yes") {
		t.Errorf("the sandbox has a /workspace/sessions tree — the hydrate now materialises the scope prefix, " +
			"so 01 §3.5's path table needs updating")
	}
}

// A sandbox edit of a path the store ALREADY has is refused and reported.
//
// This is the trade-off of removing the pod-local baseline (07 §3.3): without a
// record of what the sandbox was handed, "the sandbox edited this" and "the host
// wrote this and the mirror failed" are indistinguishable — the two copies just
// differ. The reconcile therefore refuses to choose and says so, and the agent
// settles it explicitly. What must NOT happen is silence.
func TestE2BLiveSandboxEditOfStorePathIsRefusedAndReported(t *testing.T) {
	ctx := context.Background()
	const born = "ORIGINAL\n"
	agent := liveAgentName()
	logs, store, lp, ex := liveE2B(t, agent, func(s *workspace.LocalFS) {
		if err := putFile(ctx, s, agent, "notes.md", born); err != nil {
			t.Fatalf("seed store: %v", err)
		}
	})

	// The sandbox edits it in place — no host tool involved.
	if out, err := ex.Exec(ctx, "printf 'APPENDED BY THE SANDBOX\\n' >> /workspace/notes.md && cat /workspace/notes.md", 60*time.Second); err != nil {
		t.Fatalf("exec: %v (%s)", err, out)
	}
	lp.syncSnapshot(ctx, sandboxScope{agentID: agent, sessionID: liveScopeSession}, ex, "repro-sandbox-edit")

	got := readStore(t, store, agent, "notes.md")
	if got != born {
		t.Errorf("an unattributed sandbox edit was taken without being asked for: store holds %q", got)
	}
	// And the agent is told: either the exec result carries the not-synced
	// report, or the sync logged the refusal for the same reason.
	trace := logs.String()
	if !strings.Contains(trace, "BLOCKED") {
		t.Errorf("the refusal left no trace the agent could act on:\n%s", excerpt(trace, "sandbox sync:"))
	}
}

// Write-through end-to-end: after a host write, the sandbox must be able to
// READ the new content immediately — the whole point of mirroring it in. This
// is the assertion that the incident's "sandbox keeps reading the old file"
// symptom is gone, and it is a real-filesystem claim, so it lives here.
func TestE2BLiveWriteThroughReachesSandbox(t *testing.T) {
	ctx := context.Background()
	const born = "ORIGINAL CONTENT\n"
	const host = "THE HOST REWROTE THIS FILE\n"
	agent := liveAgentName()
	_, store, lp, ex := liveE2B(t, agent, func(s *workspace.LocalFS) {
		if err := putFile(ctx, s, agent, "notes.md", born); err != nil {
			t.Fatalf("seed store: %v", err)
		}
	})
	sc := sandboxScope{agentID: agent, sessionID: liveScopeSession}

	// The sandbox starts with the born version.
	if out, err := ex.Exec(ctx, "cat /workspace/notes.md", 60*time.Second); err != nil || !strings.Contains(out, "ORIGINAL") {
		t.Fatalf("hydrate did not deliver the seeded file: %v (%s)", err, out)
	}

	// Host tool write → store + write-through.
	if err := putFile(ctx, store, agent, "notes.md", host); err != nil {
		t.Fatalf("host write: %v", err)
	}
	if _, err := lp.WriteThrough(ctx, sc, StoreScope{ProjectID: "", SessionID: liveScopeSession}, "notes.md", "/workspace/notes.md", host, ""); err != nil {
		t.Fatalf("write-through: %v", err)
	}

	out, err := ex.Exec(ctx, "cat /workspace/notes.md", 60*time.Second)
	if err != nil {
		t.Fatalf("exec after write-through: %v (%s)", err, out)
	}
	if !strings.Contains(out, "THE HOST REWROTE THIS FILE") {
		t.Errorf("the sandbox still reads the old content after a write-through: %q", out)
	}

	// And the reconcile must not touch the store, because the sandbox now holds
	// exactly what the store has.
	lp.syncSnapshot(ctx, sc, ex, "repro-after-write-through")
	if got := readStore(t, store, agent, "notes.md"); got != host {
		t.Errorf("reconcile changed the store after a write-through: %q", got)
	}
}

func putFile(ctx context.Context, st workspace.Store, agent, path, body string) error {
	return st.Put(ctx, agent, "", liveScopeSession, path, strings.NewReader(body), int64(len(body)), "text/plain")
}

func readStore(t *testing.T, st workspace.Store, agent, path string) string {
	t.Helper()
	rc, err := st.Get(context.Background(), agent, "", liveScopeSession, path)
	if err != nil {
		t.Fatalf("store get %s: %v", path, err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return string(b)
}

func excerpt(s, needle string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, needle) {
			out = append(out, "  "+line)
		}
	}
	if len(out) == 0 {
		return "  (none)"
	}
	return strings.Join(out, "\n")
}

var _ = fmt.Sprintf
