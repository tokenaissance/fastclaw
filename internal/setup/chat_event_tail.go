package setup

import (
	"context"
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
		slog.Warn("session_events tail failed",
			"agent", agentID, "session", sessionID, "since", sinceSeq, "error", err)
		return nil
	}
	return rows
}
