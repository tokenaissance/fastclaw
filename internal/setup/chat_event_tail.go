package setup

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// liveOnlyEventTypes are the events the hub carries but the log does not keep.
//
// content_delta streams roughly one row per generated token; persisting it would
// dwarf session_events for no replay value (the trailing `content` event carries
// the full text).
//
// The predicate guards **every reader of the persisted log** — the connect replay
// and the tail — which is one rule with two call sites, so it lives in one
// function (`chatEventWriter.emitPersisted`) instead of an `if` copied into each.
// It used to guard the tail only; the replay had no guard, and the difference was
// not theoretical (measured 2026-09-26: a subscriber that wrote its rows between
// the opening frame and the replay scan got the `content_delta` back on the wire —
// the intermittent `TestChatSubscribeNeverTailsLiveOnlyEvents` in CI).
//
// The hub branch still forwards these like anything else: a tab that did not start
// the turn has no other transport for them (the log does not keep them, so the
// tail cannot carry them either), and "I already have this from my own POST" is a
// judgement only the client can make — see docs/chat-event-delivery-placement.md §3.
//
// The guard is a no-op by construction today (the table cannot contain a type the
// emitter refuses to persist). It stays because it is what keeps that true the day
// one of these types does get persisted: a token chunk replayed from the store
// would be a second copy of a live stream, and no seq cursor could dedupe it (a
// live-only event carries seq = -1 by definition).
var liveOnlyEventTypes = map[string]bool{
	"content_delta": true,
}

func isLiveOnlyEventType(t string) bool { return liveOnlyEventTypes[t] }

// chatEventTailInterval is how often an open SSE subscription asks the store for
// events it has not delivered yet.
//
// The query is an indexed range scan (idx_session_events_lookup on
// user_id, agent_id, session_key, seq) that returns nothing while a session is
// idle, so a fixed interval beats any state machine on both simplicity and
// measurement. A state machine could not work here anyway: whether a turn is in
// flight lives in the memory of the pod running it — precisely the pod this
// subscription is not on.
const chatEventTailInterval = 500 * time.Millisecond

// tailSessionEvents reads the events this subscription has not sent yet.
//
// A failure is a Warn, never a torn-down stream: the subscriber degrades to
// same-pod (hub) delivery rather than losing the session. That degradation is
// the honest one — with the store unreachable there is nothing else to read.
func (s *Server) tailSessionEvents(ctx context.Context, uid, agentID, sessionID string, sinceSeq int64) []store.SessionEventRecord {
	if s.dataStore == nil {
		return nil
	}
	rows, err := s.dataStore.ListSessionEventsSince(ctx, uid, agentID, sessionID, sinceSeq)
	if err != nil {
		logSessionEventsReadFailure(ctx, "session_events tail failed", agentID, sessionID, sinceSeq, err)
		return nil
	}
	return rows
}

// replaySessionEvents reads the events a subscription missed before it connected.
//
// It is the same read as the tail against the same log, so it takes the same
// failure rule (`logSessionEventsReadFailure`); it is a separate function only so
// that each call site can be witnessed on its own.
func (s *Server) replaySessionEvents(ctx context.Context, uid, agentID, sessionID string, sinceSeq int64) []store.SessionEventRecord {
	if s.dataStore == nil {
		return nil
	}
	rows, err := s.dataStore.ListSessionEventsSince(ctx, uid, agentID, sessionID, sinceSeq)
	if err != nil {
		logSessionEventsReadFailure(ctx, "session_events replay failed", agentID, sessionID, sinceSeq, err)
		return nil
	}
	return rows
}

// logSessionEventsReadFailure reports a failed session-events read — replay and
// tail both come through here, because the rule is about the read, not the caller.
//
// The line is a health signal about the **store**, so it must not fire when the
// read was abandoned by its own caller. An SSE subscription's ctx is the request
// ctx: every closed tab, every navigating page, and every finished `codex exec`
// cancels it, and a ticker that fires inside that same instant gets
// `context.Canceled` back from a store that is perfectly healthy. Measured on
// prod 2026-09-28: all five `session_events tail failed` lines were
// `context canceled`, with no store incident behind any of them.
//
// Suppression is deliberately narrow — the caller's ctx must be done **and** the
// error must be that cancellation's own trace (`errors.Is(err, context.Canceled)`).
// Both halves are load-bearing:
//
//   - ctx done, error something else (a deadline the store hit on its own, a
//     dropped connection): that error is a fact about the store and is Warned.
//   - ctx alive, error `context.Canceled`: the caller did not cancel, so the
//     store did — that is also a fact about the store and is Warned.
//
// Narrowing it this way is what keeps the liveness of the line cheap to check:
// a suppressed line is one we can say nothing about, and a line we emit is one
// we can. (Counterfactual: drop either half and pass `context.DeadlineExceeded`
// under a canceled ctx — the store's own fault goes silent.)
func logSessionEventsReadFailure(ctx context.Context, what, agentID, sessionID string, sinceSeq int64, err error) {
	attrs := []any{"agent", agentID, "session", sessionID, "since", sinceSeq, "error", err}
	if ctx.Err() != nil && errors.Is(err, context.Canceled) {
		slog.Debug(what+": the reader went away first", attrs...)
		return
	}
	slog.Warn(what, attrs...)
}
