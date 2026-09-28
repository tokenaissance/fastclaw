package sandbox

// B5: what a failed sandbox-expiry extension means, and what the pool does about it.
//
// `extendBudget` exists for one hazard (see ScopeExtender): an operation that starts near the end of
// the instance's life and outlives it. Production 2026-09-28 had four failures of it in two days,
// ALL of them `e2b extend timeout <id> HTTP 404` — i.e. the instance the scope named was already
// gone, which the log reported as "could not extend the sandbox timeout": a sentence about the
// operation, sending a reader to look for a capacity problem that does not exist.
//
// Two classes, two behaviours, and the difference is the whole point:
//   * instance gone  → do not start the operation; hand the verdict to the replacement path
//                      (release + the note on the caller's error), so the retry gets a fresh sandbox;
//   * transient      → run it, keep the instance, and say what happened without indicting it.
//
// Falsifications for this file: drop the `p.unusable(err)` classification ⇒ the gone case logs the
// old operation-shaped sentence (test 1 reddens on the log assertion); drop the early return in
// execOnce ⇒ the command runs on the corpse (test 1 reddens on `execs`), which is the pre-B5 shape.

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// errFakeInstanceGone stands in for e2b's 404 on /timeout: a verdict about the instance.
var errFakeInstanceGone = errors.New("e2b extend timeout sb-test HTTP 404: sandbox not found")

// errFakeTransient stands in for the class a retry could cure (a network blip, a 5xx): a verdict
// about the call, not about the instance.
var errFakeTransient = errors.New("e2b extend timeout sb-test: dial tcp: connection reset")

// extendFailingPool is the snapping fixture plus the two capabilities the extend path asks for. The
// project's own fakePool already answers `Unusable` for its cut-stream sentinel; this adds the
// instance-gone shape, which is the one production actually produced.
type extendFailingPool struct {
	*snappingPool
	extendErr error
	extends   int32
}

func newExtendFailingPool(files map[string][]byte) *extendFailingPool {
	return &extendFailingPool{snappingPool: newSnappingPool(files)}
}

func (p *extendFailingPool) ExtendScope(_ context.Context, _, _, _ string, _ time.Duration) error {
	atomic.AddInt32(&p.extends, 1)
	return p.extendErr
}

func (p *extendFailingPool) Unusable(err error) bool {
	return errors.Is(err, errFakeInstanceGone) || p.snappingPool.Unusable(err)
}

func newExtendFixture(t *testing.T) (*extendFailingPool, *LifecyclePool) {
	t.Helper()
	pool := newExtendFailingPool(map[string][]byte{"report.html": []byte("x")})
	lp := NewLifecyclePool(pool, time.Hour, time.Hour)
	lp.SetWorkspace(newFakeWorkspace())
	return pool, lp
}

// The gone class: a long operation is not started on an instance we already know is gone, the
// instance is replaced, and the caller is told both facts — "was not started" (nothing to replay)
// and the replacement note (the next call gets a fresh sandbox).
func TestALongOperationIsNotStartedOnAnInstanceTheScopeAlreadyLost(t *testing.T) {
	pool, lp := newExtendFixture(t)
	pool.extendErr = errFakeInstanceGone
	buf := captureSandboxWarnings(t)

	ctx := context.Background()
	ex, err := lp.Get(ctx, "erin", "", "")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	out, err := ex.Exec(ctx, "python generate.py", 2*time.Minute)
	if err == nil {
		t.Fatalf("a long operation ran on an instance the scope had already lost (out=%q)", out)
	}
	// Assert on the COMMAND stream, not on an exec count: a remote-workspace backend spends an exec
	// on its own post-exec `find` probe, so "one exec" is not the same thing as "the operation ran".
	if cmds := pool.current.execCommands(); len(cmds) != 0 {
		t.Fatalf("the command was started anyway (%v); the extend verdict was not acted on", cmds)
	}
	if pool.releases == 0 {
		t.Fatal("the gone instance was not replaced, so the retry would land on it again")
	}
	if !strings.Contains(err.Error(), "was not started") {
		t.Fatalf("the caller cannot tell that nothing ran: %v", err)
	}
	if !strings.Contains(err.Error(), "sandbox replaced") {
		t.Fatalf("the caller was not told the instance was replaced: %v", err)
	}
	// And the operator's line names the INSTANCE, which is the fact (②): the old text named the
	// operation and sent a reader looking for a capacity problem.
	if !strings.Contains(buf.String(), "the sandbox this scope held is gone") {
		t.Fatalf("the log did not say what happened:\n%s", buf.String())
	}
	if atomic.LoadInt32(&pool.extends) == 0 {
		t.Fatal("the extend was never attempted; the witness would be vacuous")
	}
}

// The transient class: the operation still runs, the instance is NOT thrown away (that would lose
// unsynced work over a network blip), and the log does not pretend the instance died.
func TestATransientExtendFailureStillRunsTheOperation(t *testing.T) {
	pool, lp := newExtendFixture(t)
	pool.extendErr = errFakeTransient
	buf := captureSandboxWarnings(t)

	ctx := context.Background()
	ex, err := lp.Get(ctx, "erin", "", "")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	out, err := ex.Exec(ctx, "python generate.py", 2*time.Minute)
	if err != nil {
		t.Fatalf("a transient extend failure failed the operation: %v (out=%q)", err, out)
	}
	cmds := pool.current.execCommands()
	if len(cmds) == 0 || cmds[0] != "python generate.py" {
		t.Fatalf("the operation did not run first on the healthy instance: %v", cmds)
	}
	if runs := strings.Count(strings.Join(cmds, "\x00"), "python generate.py"); runs != 1 {
		t.Fatalf("the operation ran %d times; a transient failure must not replay it: %v", runs, cmds)
	}
	if pool.releases != 0 {
		t.Fatal("a healthy instance was replaced over a transient failure — that is how unsynced sandbox work is lost")
	}
	if !strings.Contains(buf.String(), "could not extend the sandbox timeout before a long operation") {
		t.Fatalf("the transient failure was not reported at all:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "is gone") {
		t.Fatalf("a transient failure was reported as a lost instance:\n%s", buf.String())
	}
}

// A short operation never asks: the round trip is not worth it, and the classification above must
// not turn every exec into an extend attempt.
func TestAShortOperationDoesNotAskToExtend(t *testing.T) {
	pool, lp := newExtendFixture(t)
	pool.extendErr = errFakeInstanceGone

	ctx := context.Background()
	ex, err := lp.Get(ctx, "erin", "", "")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, err := ex.Exec(ctx, "true", 30*time.Second); err != nil {
		t.Fatalf("a short operation was refused: %v", err)
	}
	if got := atomic.LoadInt32(&pool.extends); got != 0 {
		t.Fatalf("a %s operation asked to extend the expiry (%d calls)", 30*time.Second, got)
	}
}
