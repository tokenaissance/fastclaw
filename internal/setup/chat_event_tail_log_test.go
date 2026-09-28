package setup

// A failed session-events read is a health signal about the **store**, so the
// Warn must not fire when the reader abandoned the read itself.
//
// Measured on prod 2026-09-28: five `session_events tail failed` lines, every one
// of them `context canceled` — one per closed tab / finished `codex exec`, with no
// store incident behind any of them. The subscription's ctx *is* the request ctx,
// and the tail is a 500ms ticker, so a tick landing inside the same instant the
// caller goes away gets that error back from a perfectly healthy store.
//
// The rule (`logSessionEventsReadFailure`) is deliberately narrow: suppress only
// when the caller's ctx is done **and** the error is that cancellation's own
// trace. The two counterfactuals below are the reason — each one is a fact about
// the store that this test would go silent on if the guard were widened.

import (
	"bytes"
	"context"

	"log/slog"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// cannedTailStore is a store whose session-events read always fails the same way.
// It counts calls, so a witness that expects silence can prove the read actually
// happened instead of the branch never being reached.
type cannedTailStore struct {
	store.Store
	err   error
	calls int
}

func (c *cannedTailStore) ListSessionEventsSince(context.Context, string, string, string, int64) ([]store.SessionEventRecord, error) {
	c.calls++
	return nil, c.err
}

// captureLogs installs a default logger writing to the returned buffer at Debug
// level, so both the Warn and the Debug branch are visible.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// The store read must still be attempted under a canceled ctx — the guard is on
// the *report*, not on making the call, so suppressing the line cannot turn into
// suppressing the degradation.
func TestASessionEventsReadIsStillAttemptedWithADeadContext(t *testing.T) {
	pod, _ := newChatHarness(t, &e2eProvider{reply: "unused"}, 2)
	captureLogs(t)
	fake := &cannedTailStore{err: context.Canceled}
	pod.dataStore = fake

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if rows := pod.tailSessionEvents(ctx, "u_1", "agt_1", "s-1", -1); rows != nil {
		t.Fatalf("a failed read must yield no rows, got %d", len(rows))
	}
	if fake.calls == 0 {
		t.Fatal("the read was skipped; the witness below would be vacuous")
	}
}

// The witness: the caller's own cancellation is reported at Debug, never Warn —
// on both call sites of the read.
func TestACallersOwnCancellationIsNotReportedAsAStoreFailure(t *testing.T) {
	cases := []struct {
		name string
		read func(*Server, context.Context) []store.SessionEventRecord
		what string
	}{
		{
			name: "the tail",
			read: func(s *Server, ctx context.Context) []store.SessionEventRecord {
				return s.tailSessionEvents(ctx, "u_1", "agt_1", "s-1", -1)
			},
			what: "session_events tail failed",
		},
		{
			name: "the replay",
			read: func(s *Server, ctx context.Context) []store.SessionEventRecord {
				return s.replaySessionEvents(ctx, "u_1", "agt_1", "s-1", -1)
			},
			what: "session_events replay failed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod, _ := newChatHarness(t, &e2eProvider{reply: "unused"}, 2)
			buf := captureLogs(t)
			fake := &cannedTailStore{err: context.Canceled}
			pod.dataStore = fake

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			tc.read(pod, ctx)

			if fake.calls == 0 {
				t.Fatal("the read was skipped; this witness would be vacuous")
			}
			// Captured after the harness booted, so anything at Warn here is the
			// read's own line — the fixture's own warnings are not in the buffer.
			if strings.Contains(buf.String(), "level=WARN") {
				t.Fatalf("a caller's own cancellation was reported as a store failure:\n%s", buf.String())
			}
			// Silence is only meaningful if the Debug branch ran, so the fact is
			// still on the record for anyone who turns the level up.
			if !strings.Contains(buf.String(), tc.what+": the reader went away first") {
				t.Fatalf("the abandoned read left no trace at Debug:\n%s", buf.String())
			}
		})
	}
}

// Counterfactual 1: same canceled ctx, but the error is the store's own deadline.
// That is a fact about the store — drop the `errors.Is` half of the guard and this
// goes silent.
func TestAStoresOwnDeadlineUnderACanceledContextStillWarns(t *testing.T) {
	pod, _ := newChatHarness(t, &e2eProvider{reply: "unused"}, 2)
	buf := captureLogs(t)
	pod.dataStore = &cannedTailStore{err: context.DeadlineExceeded}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pod.tailSessionEvents(ctx, "u_1", "agt_1", "s-1", -1)

	// The exact Warn shape, not the message prefix: the suppressed branch's Debug
	// line starts with the same words, so a looser assertion here is green either
	// way (measured — the first version of this test was).
	if !strings.Contains(buf.String(), `level=WARN msg="session_events tail failed"`) {
		t.Fatalf("the store's own deadline was swallowed:\n%s", buf.String())
	}
}

// Counterfactual 2: a live ctx, but `context.Canceled` came back anyway. Nobody
// asked for this, so the store did — drop the `ctx.Err()` half of the guard and
// this goes silent.
func TestACancellationNobodyAskedForStillWarns(t *testing.T) {
	pod, _ := newChatHarness(t, &e2eProvider{reply: "unused"}, 2)
	buf := captureLogs(t)
	pod.dataStore = &cannedTailStore{err: context.Canceled}

	pod.tailSessionEvents(context.Background(), "u_1", "agt_1", "s-1", -1)

	if !strings.Contains(buf.String(), `level=WARN msg="session_events tail failed"`) {
		t.Fatalf("a cancellation with no canceller was swallowed:\n%s", buf.String())
	}
}

// A healthy store, for contrast: the rule must not be reachable at all, so this
// also pins that the two reads are still wired to `dataStore`.
func TestAHealthySessionEventsReadSaysNothing(t *testing.T) {
	_, _, db := newReplicaPair(t, &e2eProvider{reply: "unused"}, 2)

	pod, _ := newChatHarness(t, &e2eProvider{reply: "unused"}, 2)
	buf := captureLogs(t)
	pod.dataStore = db
	if _, err := db.AppendSessionEvent(context.Background(), "u_1", "agt_1", "s-1",
		"content", []byte(`{"content":"hi"}`)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if rows := pod.replaySessionEvents(context.Background(), "u_1", "agt_1", "s-1", -1); len(rows) != 1 {
		t.Fatalf("the replay did not reach the store: %d rows", len(rows))
	}
	if buf.Len() != 0 {
		t.Fatalf("a healthy read logged something:\n%s", buf.String())
	}
}

// Guard against a future edit that makes the two helpers disagree: they read the
// same log, so they must be the same read.
func TestBothSessionEventsReadsUseTheSameStoreMethod(t *testing.T) {
	pod, _ := newChatHarness(t, &e2eProvider{reply: "unused"}, 2)
	fake := &cannedTailStore{err: nil}
	pod.dataStore = fake
	pod.tailSessionEvents(context.Background(), "u_1", "agt_1", "s-1", -1)
	pod.replaySessionEvents(context.Background(), "u_1", "agt_1", "s-1", -1)
	if fake.calls != 2 {
		t.Fatalf("expected both reads to go through ListSessionEventsSince, got %d calls", fake.calls)
	}
}
