package agent

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/provider"
	"github.com/fastclaw-ai/fastclaw/internal/session"
)

// Turn-lease admission: the cross-replica half of clause W.
//
// The in-process gate (session.AcquireTurn) is a FIFO queue inside one pod; the
// lease is the exclusion across pods. Both are held for the whole turn: the
// lease first (it is the one that can be lost to a peer), then the local slot.
// Holding the lease before the local slot cannot deadlock — a local turn that
// holds the slot must have held the lease first, and we hold it now.

const (
	// turnLeaseGrace is added to the turn budget: the lease must outlive the
	// longest turn, or a live writer gets evicted mid-turn (obligation L3 —
	// the 09-18 turn sat 605 s inside one batch of tool calls).
	turnLeaseGrace = 60 * time.Second
	// turnLeaseRetry is how often a queued caller re-tries the lease. The
	// queued event is emitted once per attempt, so a waiter is visible.
	turnLeaseRetry = 2 * time.Second
	// defaultTurnLeaseTTL is used when the caller's context carries no
	// deadline: the platform's turn budget (45 m) plus the grace window.
	defaultTurnLeaseTTL = 45*time.Minute + turnLeaseGrace
	// turnLeaseStopJoin bounds how long Stop waits for the renewal goroutine
	// to finish what it is doing. The wait is a join, not a courtesy: Stop is
	// the turn's last statement and callers close their event channel after
	// it, so a notice emitted past that point is a send on a closed channel.
	// The bound exists only so a reader that stopped reading cannot hang the
	// turn's return — the emit respects the turn's context, so it ends on its
	// own once that context does.
	turnLeaseStopJoin = 2 * time.Second
)

// turnLeaseGuard owns one possession: it renews on a timer, records a loss,
// and frees the lease when the turn ends.
type turnLeaseGuard struct {
	lease SessionLease
	key   SessionKey
	ttl   time.Duration
	sess  *session.Session

	emit func(ChatEvent)

	mu   sync.Mutex
	turn *Turn
	lost bool
	stop chan struct{}
	// done is closed by renewLoop when it returns, which is the instant the
	// guard can no longer emit. Stop waits on it.
	done chan struct{}
}

// turnSupersededNotice is what the user sees when this turn lost the session to
// a peer mid-flight: it says what happened, what was preserved, and what was
// not — never "interrupted", which would claim a death that did not occur.
const turnSupersededNotice = "another turn took this session over while this one was running; this turn stopped writing, and nothing it produced after that point is part of the conversation"

// lostNotice is emitted once, at the moment the renewal proves the loss (A1.4:
// a lost lease stops the turn AND says so).
const lostNoticeEvent = "notice"

// turnCancelledNotice is what the user sees when their own stop request landed.
// It is a σ about their action, not about the world: the turn stopped because
// they asked, and the history says so instead of leaving an unexplained gap.
const turnCancelledNotice = "stopped at your request"

// Current is the possession a write must present. Nil after a loss — the
// session's fence is cleared at the same moment, so a post-loss append is
// refused by the store rather than silently landing.
func (g *turnLeaseGuard) Current() *Turn {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lost {
		return nil
	}
	cur := *g.turn
	return &cur
}

// Cancelled reports the user's stop request for this possession. It is read at
// the loop's iteration boundary — the same place the loss is noticed — so the
// request reaches the holder wherever it runs (the row is shared, the boundary
// is local). A read error is "not cancelled": stopping on an unreadable row
// would let a store hiccup kill live turns.
func (g *turnLeaseGuard) Cancelled(ctx context.Context) bool {
	cur := g.Current()
	if cur == nil {
		return false
	}
	asked, err := g.lease.CancelRequested(ctx, g.key, cur)
	if err != nil {
		slog.Warn("turn lease: cancel check failed", "session", g.key.SessionKey, "err", err)
		return false
	}
	return asked
}

// Lost reports that a peer took the session over. The turn checks this at its
// iteration boundary and stops; it never keeps writing (A1.4).
func (g *turnLeaseGuard) Lost() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lost
}

// Stop cancels the renewer, waits for it to leave, and frees the lease. The
// wait is part of the contract rather than a politeness: the loss notice is
// emitted from the renewer, Stop is the turn's last statement, and callers
// close their event channel once the turn returns — so an emit that outlives
// Stop is a send on a closed channel. The release runs on a background context
// on purpose: a cancelled turn must not leave its peers waiting for the TTL.
//
// The fence is cleared ONLY when this turn still owns the session. After a
// loss the stale fence stays on the session on purpose: that is what makes
// every later write of the superseded turn be refused by the store (A1.4 —
// "it never keeps writing"), including anything the loop emits after it
// notices the loss. The next turn overwrites it at admission.
func (g *turnLeaseGuard) Stop() {
	close(g.stop)
	select {
	case <-g.done:
	case <-time.After(turnLeaseStopJoin):
		slog.Warn("turn lease: the renewer is still busy after Stop; it stops when the turn's context ends",
			"agent", g.key.AgentID, "session", g.key.SessionKey)
	}
	if g.Lost() {
		return
	}
	g.sess.ClearTurnFence()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()
	if cur := g.Current(); cur != nil {
		if err := g.lease.Release(ctx, g.key, cur); err != nil {
			slog.Warn("turn lease: release failed; a peer waits for the TTL",
				"agent", g.key.AgentID, "session", g.key.SessionKey, "err", err)
		}
	}
}

// beginTurnLease acquires the session's turn lease, waiting (and reporting
// `queued`) while a live holder exists, then starts the renewal timer and binds
// the possession to the session as its write fence.
//
// ok=false means the turn must not start: ctx ended while queued, or the store
// was unreachable. The latter fails closed on purpose — refusing to start is
// recoverable, two writers are not.
func (a *Agent) beginTurnLease(ctx context.Context, sess *session.Session, emit func(ChatEvent)) (*turnLeaseGuard, bool) {
	key := a.turnLeaseKey(sess)
	ttl := a.turnLeaseTTL(ctx)
	for {
		turn, err := a.lease().Acquire(ctx, key, ttl)
		if err == nil {
			sess.SetTurnFence(turn.Holder, turn.Epoch)
			g := &turnLeaseGuard{lease: a.lease(), key: key, ttl: ttl, sess: sess, turn: turn, emit: emit, stop: make(chan struct{}), done: make(chan struct{})}
			go g.renewLoop()
			return g, true
		}
		var busy *SessionTurnBusy
		if !errors.As(err, &busy) {
			slog.Error("turn admission: lease unavailable; refusing to start (fail closed)",
				"agent", a.name, "session", key.SessionKey, "err", err)
			return nil, false
		}
		if ctx.Err() != nil {
			return nil, false
		}
		// Tell the dashboard who is ahead of it and until when: "position 1"
		// alone cannot say whose turn you are waiting for (A4.1).
		data := map[string]any{"position": sess.TurnWaiters() + 1}
		if busy.Holder != "" {
			data["holder"] = busy.Holder
		}
		if !busy.ExpiresAt.IsZero() {
			// camelCase, to match the live-turn fact (`turnActivePayload`): the
			// same concept — "when does this possession lapse" — had two wire
			// spellings across two events, and the client tolerated both. One
			// fact, one shape (contract C1, docs 08 §10.4).
			data["expiresAt"] = busy.ExpiresAt.UTC().Format(time.RFC3339)
		}
		// Whose submission is waiting. This is the one fact whose reader has to act on a specific
		// submission: a tab that learns "something of mine is queued" from a re-emission (a reload,
		// or any tab that did not POST) can only withdraw it if it knows the submission's identity —
		// the id the SERVER minted at acceptance (internal/agent/turn_id.go), never a value the
		// caller chose.
		// The event ENVELOPE deliberately carries no identity of its own — turns are serialized per
		// session, so "the holder ended" answers every other question, and a wire field with no
		// second reader is the kind of thing this roster keeps deleting.
		if id := TurnIDFromContext(ctx); id != "" {
			data["turnId"] = id
		}
		emit(ChatEvent{Type: "queued", Data: data})
		retry := turnLeaseRetry
		if a.turnLeaseRetryOverride > 0 {
			retry = a.turnLeaseRetryOverride
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(retry):
		}
	}
}

// renewLoop keeps the possession alive and records the loss. It renews at
// TTL/3 so two consecutive failures still leave a window before the row goes
// stale — the same cadence channels.Leaser uses.
func (g *turnLeaseGuard) renewLoop() {
	defer close(g.done)
	ticker := time.NewTicker(g.ttl / 3)
	defer ticker.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-ticker.C:
			g.mu.Lock()
			cur := g.turn
			g.mu.Unlock()
			next, err := g.lease.Renew(context.Background(), g.key, cur, g.ttl)
			if err != nil {
				var busy *SessionTurnBusy
				slog.Warn("turn lease: lost; the turn must stop writing",
					"agent", g.key.AgentID, "session", g.key.SessionKey,
					"superseded_by", busyHolder(busy, err), "err", err)
				g.mu.Lock()
				g.lost = true
				g.mu.Unlock()
				// The fence STAYS (stale) so every later append is refused by
				// the store (L4a) even if the loop has not noticed yet. It is
				// not cleared here: clearing would un-fence the very writes the
				// loss is supposed to stop.
				if g.emit != nil {
					g.emit(ChatEvent{Type: lostNoticeEvent, Data: map[string]any{
						"message": turnSupersededNotice,
						"holder":  busyHolderOnLoss(err),
					}})
				}
				return
			}
			g.mu.Lock()
			g.turn = next
			g.mu.Unlock()
			g.sess.SetTurnFence(next.Holder, next.Epoch)
		}
	}
}

func busyHolder(busy *SessionTurnBusy, err error) string {
	if errors.As(err, &busy) {
		return busy.Holder
	}
	return ""
}

// busyHolderOnLoss names the peer that took over, when the error carries it.
func busyHolderOnLoss(err error) string {
	var busy *SessionTurnBusy
	return busyHolder(busy, err)
}

// openCallAnswer chooses the sentence the projection uses for tool calls
// stored history left open (docs/session-turn-integrity.md A2.1). It is the
// one place that decides, and it decides from the lease:
//
//   - the lease cannot be read            → no fact: say "not known"
//   - a live holder exists that is not us → a peer may still be running that
//     call: say "still running, do not re-issue"
//   - nobody else holds the session       → no other turn can be running, so an
//     open call is provably finished: "interrupted" is now the true sentence
//
// The third case is also what a single-instance install gets (NopSessionLease
// never reports a holder), which matches its reality: the in-process gate
// already guarantees no second turn.
func (a *Agent) openCallAnswer(ctx context.Context, sess *session.Session) string {
	live, err := a.lease().Live(ctx, a.turnLeaseKey(sess))
	if err != nil {
		return provider.NoReplyUnknownResult
	}
	if live == nil {
		return provider.StoppedToolResult
	}
	if f := sess.Fence(); f != nil && f.Holder == live.Holder {
		return provider.StoppedToolResult
	}
	return provider.NoReplyTurnAliveResult
}

// turnLeaseKey is the session's own identity — never the chat triple.
func (a *Agent) turnLeaseKey(sess *session.Session) SessionKey {
	return SessionKey{UserID: a.ownerUserID, AgentID: a.name, SessionKey: sess.Key()}
}

// turnLeaseTTL is the turn budget plus the grace window. The budget is the
// caller's deadline when it has one (the chat handler hands the turn its own
// clock); otherwise the platform default. turnLeaseTTLOverride pins it for
// tests, which otherwise would have to wait 15 minutes for a renewal tick.
func (a *Agent) turnLeaseTTL(ctx context.Context) time.Duration {
	if a.turnLeaseTTLOverride > 0 {
		return a.turnLeaseTTLOverride
	}
	return defaultTurnLeaseTTLFor(ctx)
}

func defaultTurnLeaseTTLFor(ctx context.Context) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		if d := time.Until(dl) + turnLeaseGrace; d > turnLeaseGrace {
			return d
		}
	}
	return defaultTurnLeaseTTL
}
