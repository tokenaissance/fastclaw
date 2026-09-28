package sandbox

// The ordering rule (change register row 87) on real infrastructure — one leg per
// instrument, and both directions.
//
// The unit witnesses (lifecycle_sync_order_test.go) model the guest clock. This
// file does not model anything: it uses a real E2B sandbox, whose guest clock the
// live measurement puts ≈1.2 s ahead of the pod's with a ≈2 s warm round trip
// (TestE2BLiveGuestClockOffset), and a real LocalFS store, whose mtime is this
// pod's clock.
//
//	FASTAGENT_E2B_LIVE=1 E2B_API_KEY=e2b_... \
//	  go test ./internal/sandbox/ -run TestE2BLiveSyncOrder -v -count=1
//
// leg A — the sandbox is the successor, by the CLOCK instrument alone: the script
//   REWRITES the file (so the bytes do not nest, and containment cannot speak) and
//   the store is collected after it. This is the `MEMORY.md`/`_p11.log` shape from
//   §13.4, where the refusal repeated 269/142 times.
// leg B — the store is the successor, with the CONTAINMENT instrument carrying it:
//   the store's copy is the longer one and the sandbox holds a strict prefix of
//   it, which is what a live append looks like from the reconcile's side.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/workspace"
)

func TestE2BLiveSyncOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// ── leg A: the sandbox rewrote the file; the store must collect it ──────
	t.Run("sandbox rewrite is collected", func(t *testing.T) {
		const handed = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n"
		const rewritten = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBB" // no prefix relation: not an append
		agent := liveAgentName()
		logs, store, lp, ex := liveE2B(t, agent, func(s *workspace.LocalFS) {
			if err := putFile(ctx, s, agent, "report.txt", handed); err != nil {
				t.Fatalf("seed store: %v", err)
			}
		})
		if out, err := ex.Exec(ctx, "cat /workspace/report.txt", 60*time.Second); err != nil {
			t.Fatalf("hydrate check: %v (%s)", err, out)
		} else if !strings.Contains(out, "AAAA") {
			t.Fatalf("the hydrate did not deliver the store's copy: %q", out)
		}
		// The rewrite: a script, not a tool — and its guest clock is what the
		// reconcile has to translate to see that this write is the later one.
		if out, err := ex.Exec(ctx, "printf '"+rewritten+"' > /workspace/report.txt", 60*time.Second); err != nil {
			t.Fatalf("sandbox rewrite: %v (%s)", err, out)
		}
		lp.syncSnapshot(ctx, sandboxScope{agentID: agent, sessionID: liveScopeSession}, ex, "order-leg-a")

		if got := readStore(t, store, agent, "report.txt"); !strings.Contains(got, "BBBB") {
			t.Errorf("the store did not receive the sandbox's newer copy: %q\nsync trace:\n%s",
				got, excerpt(logs.String(), "sandbox sync:"))
		}
	})

	// ── leg B: the store appended; the sandbox must be given the longer copy ──
	t.Run("store append is delivered", func(t *testing.T) {
		const handed = "line one\n"
		const appended = "line one\nline two\n"
		agent := liveAgentName()
		logs, store, lp, ex := liveE2B(t, agent, func(s *workspace.LocalFS) {
			if err := putFile(ctx, s, agent, "notes.txt", handed); err != nil {
				t.Fatalf("seed store: %v", err)
			}
		})
		if err := putFile(ctx, store, agent, "notes.txt", appended); err != nil {
			t.Fatalf("host append: %v", err)
		}
		lp.syncSnapshot(ctx, sandboxScope{agentID: agent, sessionID: liveScopeSession}, ex, "order-leg-b")

		out, err := ex.Exec(ctx, "cat /workspace/notes.txt", 60*time.Second)
		if err != nil {
			t.Fatalf("read back: %v (%s)", err, out)
		}
		if !strings.Contains(out, "line two") {
			t.Errorf("the sandbox was not given the store's longer copy: %q\nsync trace:\n%s",
				out, excerpt(logs.String(), "sandbox sync:"))
		}
	})
}
