package agent

import (
	"context"
	"time"
)

/**
 * [INPUT]: one context, one budget.
 * [OUTPUT]: recordCtx — a context for writing a FACT that must outlive the work that produced it.
 * [POS]: The naming half of the 2026-09-28 forensics. A turn runs under several clocks that all
 *        look like "a timeout" (the gateway task budget, the turn ceiling, the lease TTL), and the
 *        choice between them is a choice about WHO is allowed to make a write fail. Naming the two
 *        kinds at the call site is what keeps that choice deliberate:
 *
 *          work      (model calls, tool calls)  → the turn's ctx, because the budget IS the point
 *          record    (events, post-turn hooks)  → recordCtx, because "the work was cancelled" is
 *                                                 not a reason to refuse writing down that it
 *                                                 was cancelled
 *          delivery  (SSE fan-out, subscribe)   → the client's ctx
 *
 *        The production evidence: two goal turns, cut at their 300 s budget, lost `error` and
 *        `done` to "context deadline exceeded" because those appends used the turn ctx — the
 *        session then looked as if nothing had happened. See docs/session-turn-integrity.md,
 *        "The four lifetimes of a context".
 * [PROTOCOL]: Values are kept (`WithoutCancel` drops only cancellation and the deadline), so the
 *        store still sees the user, agent and session it must route by. A caller that wants no
 *        bound at all uses `context.Background()` plus values (see session.Manager.ctx).
 */

const (
	// eventPersistBudget bounds one detached event append: long enough for a single indexed
	// insert, short enough that a store outage cannot pin the emitting goroutine.
	eventPersistBudget = 3 * time.Second
	// postTurnAfterFailureBudget bounds the PostTurn hooks of a turn that ended in failure. Same
	// reasoning; five seconds covers the handful of indexed writes they do.
	postTurnAfterFailureBudget = 5 * time.Second
)

// recordCtx returns a context for writing down a fact. It keeps every value the caller's ctx
// carries and drops both the cancellation and the deadline, then adds its own short bound: a
// record write is allowed to fail because the store is unavailable, never because the turn that
// produced it is already dead.
func recordCtx(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), budget)
}
