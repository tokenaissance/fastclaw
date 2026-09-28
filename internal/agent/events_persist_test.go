package agent

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// A sink that records what it was handed, including whether the ctx it received was still alive.
type recordingSink struct {
	ctxErr   error
	calls    int
	lastType string
}

func (s *recordingSink) AppendSessionEvent(ctx context.Context, _, _, _, eventType string, _ []byte) (int64, error) {
	s.calls++
	s.lastType = eventType
	s.ctxErr = ctx.Err()
	return int64(s.calls), nil
}

// The record of a turn must outlive the ctx that bounded the turn.
//
// This is the production defect of 2026-09-28 written as a test: two goal turns were cut at their
// 300 s budget and, at that instant, `error` and `done` were refused by the store with "context
// deadline exceeded" because they were appended with the already-expired turn ctx — so the session
// looked as if nothing had happened.
//
// Falsification: append with the caller's ctx again (drop the `WithoutCancel`) and `ctxErr` comes
// back `context.Canceled` / `DeadlineExceeded` — which is the write being refused by the store.
func TestATerminalEventSurvivesTheTurnContext(t *testing.T) {
	sink := &recordingSink{}
	ctx, cancel := context.WithCancel(context.Background())
	ctx = ContextWithStream(ctx, nil, sink, nil, "u_1", "agt_1", "sess-1")
	cancel() // the turn is already over: its ctx is dead

	if _, err := emitEventChecked(ctx, ChatEvent{Type: "done", Data: map[string]any{"ending": EndingReplied}}); err != nil {
		t.Fatalf("persisting the terminal event failed: %v", err)
	}
	if sink.calls != 1 || sink.lastType != "done" {
		t.Fatalf("sink calls=%d last=%q; want the `done` event appended once", sink.calls, sink.lastType)
	}
	if sink.ctxErr != nil {
		t.Fatalf("the append ran with a dead ctx (%v); the record would have been refused", sink.ctxErr)
	}

	// And the detach keeps the values the store needs to route the write.
	deadline, ok := ctx.Deadline()
	if ok && time.Until(deadline) > 0 {
		t.Fatal("this fixture was supposed to hand over a dead ctx")
	}
}

// The same rule, one layer out: through the REAL store (sqlite, the same schema production runs),
// a terminal event emitted on a dead ctx still lands in `session_events`.
//
// This is the test that would have caught 2026-09-28: the warning in production was
// `persist chat event failed … context deadline exceeded`, and the row was simply absent
// afterwards. Here a cancelled turn emits `done` and the row has to be readable back.
//
// Falsification: append with the caller's ctx again (`context.WithTimeout(ctx, …)`) and the store
// refuses with `context.Canceled` — no row.
func TestATerminalEventReachesTheRealStoreAfterTheTurnDies(t *testing.T) {
	db, err := store.NewDBStore("sqlite", "file:"+filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ctx = ContextWithStream(ctx, nil, db, nil, "u_1", "agt_1", "sess-1")
	cancel() // the turn is already gone

	if _, err := emitEventChecked(ctx, ChatEvent{Type: "done", Data: map[string]any{"ending": EndingFailed}}); err != nil {
		t.Fatalf("the terminal event was refused by the real store: %v", err)
	}
	events, err := db.ListSessionEventsSince(context.Background(), "u_1", "agt_1", "sess-1", -1)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(events) != 1 || events[0].Type != "done" {
		t.Fatalf("session_events holds %d row(s) (%v); want the `done` the dead turn emitted", len(events), events)
	}
}
