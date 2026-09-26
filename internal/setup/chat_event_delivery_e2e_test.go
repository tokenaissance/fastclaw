package setup

// Cross-replica delivery for /api/chat/subscribe.
//
// The SSE subscriber and the turn that produces the events are two independent
// HTTP requests, so they can land on different replicas. The hub is in-process,
// which means the subscriber then sees nothing at all: not slowly, permanently
// (its long-lived connection never reconnects while the 30s keepalive holds it
// open, so the one-shot replay at connect time never runs again).
//
// These tests build the shape of two replicas: two Servers, **one** store, two
// separate hubs. Everything the subscriber gets must therefore come from the
// store.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/auth"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// newReplicaPair returns two Servers that share one store and hold separate
// event hubs — the shape of two gateway replicas with a real database between
// them.
func newReplicaPair(t *testing.T, prov provider.Provider, maxToolIterations int) (podA, podB *Server, db *store.DBStore) {
	t.Helper()
	var err error
	db, err = store.NewDBStore("sqlite", "file:"+filepath.Join(t.TempDir(), "chat.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	podA, _ = newChatHarness(t, prov, maxToolIterations)
	podB, _ = newChatHarness(t, prov, maxToolIterations)
	podA.dataStore, podB.dataStore = db, db
	if podA.chatEventHub() == podB.chatEventHub() {
		t.Fatal("the two replicas must not share an event hub, or the test proves nothing")
	}
	return podA, podB, db
}

func chatSubscribeRequest(t *testing.T, agentID, sessionID string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet,
		"/api/chat/subscribe?agentId="+agentID+"&sessionId="+sessionID, nil)
	return r.WithContext(auth.WithIdentity(r.Context(), auth.Identity{
		UserID: "u_1", Role: "user", AuthMethod: "session",
	}))
}

// subscribeOn opens an SSE subscription and returns the recorder, a channel that
// closes when the handler returns, and a stop func that cancels the request and
// waits for it to unwind.
func subscribeOn(t *testing.T, s *Server, agentID, sessionID string) (*sseRecorder, chan struct{}, func()) {
	t.Helper()
	rec := newSSERecorder()
	ctx, cancel := context.WithCancel(context.Background())
	req := chatSubscribeRequest(t, agentID, sessionID)
	req = req.WithContext(auth.WithIdentity(ctx, auth.Identity{
		UserID: "u_1", Role: "user", AuthMethod: "session",
	}))
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleChatSubscribe(rec, req)
	}()
	// The handler flushes ": ok" as soon as the headers are set, before it
	// replays; waiting for it means the subscription exists.
	deadline := time.Now().Add(2 * time.Second)
	for !rec.seen(": ok") {
		if time.Now().After(deadline) {
			t.Fatal("subscribe handler never flushed its opening comment")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("subscribe handler did not return after its request was cancelled")
		}
	}
	return rec, done, stop
}

// waitForBody polls the recorded SSE body for a substring.
func waitForBody(t *testing.T, rec *sseRecorder, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !rec.seen(want) {
		if time.Now().After(deadline) {
			t.Fatalf("subscriber never received %q within %s; body=%q", want, within, rec.body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The one this change exists for: the turn runs on replica A, the subscriber
// watches on replica B, and the tool result still arrives.
func TestChatSubscribeSeesEventsPersistedByAnotherPod(t *testing.T) {
	podA, podB, _ := newReplicaPair(t, &fanOutE2EProvider{fanout: 1}, 3)

	sub, _, stopSub := subscribeOn(t, podB, "agt_e2e", "chat-xpod")
	defer stopSub()

	body := runSingleChatStream(t, podA, "chat-xpod", "go-xpod")
	if !strings.Contains(body, "final answer") {
		t.Fatalf("the turn on replica A never delivered its answer; stream=%q", body)
	}

	// The sub-agent's result crosses the replica boundary through the event log.
	waitForBody(t, sub, `"type":"tool_result"`, 3*time.Second)
	if !sub.seen("brief for part-1") {
		t.Fatalf("the tool_result arrived without its content; body=%q", sub.body.String())
	}
	// The turn's terminal event too, so the client stops waiting.
	waitForBody(t, sub, `"type":"done"`, 3*time.Second)

	// The token-level delta is live-only by design: it must NOT come back through
	// the store (the active tab renders it from its own POST stream; a tail copy
	// would double-render it).
	if sub.seen("content_delta") {
		t.Fatalf("a live-only event came through the tail; body=%q", sub.body.String())
	}
}

// A tab that did not start the turn has only the hub for the live half:
// content_delta is never persisted, so the tail cannot carry it either.
//
// The skip this pins was written for a different assumption — that the only
// client of a subscription is the tab that owns the POST, so forwarding deltas
// "here" would double-render them. Every other tab was the price: a second
// browser watching the same session saw nothing until `done`, then the whole
// answer at once. The guard belongs on the client, which can tell the two cases
// apart (it knows whether *its* POST is in flight); the server cannot.
func TestChatSubscribeForwardsLiveOnlyEventsFromTheHub(t *testing.T) {
	podA, _, _ := newReplicaPair(t, &fanOutE2EProvider{fanout: 1}, 3)

	sub, _, stopSub := subscribeOn(t, podA, "agt_e2e", "chat-delta")
	defer stopSub()

	// seq = -1 is what the emitter stamps on a live-only event: the log never
	// keeps it, so the hub is its only transport.
	podA.chatEventHub().Publish("u_1", "agt_e2e", "chat-delta", agent.EventEnvelope{
		Seq:   -1,
		Event: agent.ChatEvent{Type: "content_delta", Data: map[string]any{"delta": "tok"}},
	})

	waitForBody(t, sub, `"type":"content_delta"`, 2*time.Second)
	if !sub.seen(`"delta":"tok"`) {
		t.Fatalf("the delta arrived without its text; body=%q", sub.body.String())
	}
}

// Same-pod delivery must not become two deliveries: the hub already sent the
// event, the tail must skip it (one cursor, three writers).
func TestChatSubscribeDoesNotDoubleDeliverWhatTheHubAlreadySent(t *testing.T) {
	podA, _, _ := newReplicaPair(t, &fanOutE2EProvider{fanout: 1}, 3)

	sub, _, stopSub := subscribeOn(t, podA, "agt_e2e", "chat-same-pod")
	defer stopSub()

	runSingleChatStream(t, podA, "chat-same-pod", "go-same-pod")
	waitForBody(t, sub, `"type":"done"`, 3*time.Second)

	// Give the tail a few ticks to (wrongly) re-send what the hub delivered.
	time.Sleep(1200 * time.Millisecond)
	if n := strings.Count(sub.body.String(), `"type":"tool_result"`); n != 1 {
		t.Fatalf("tool_result delivered %d times, want exactly 1; body=%q", n, sub.body.String())
	}
	if n := strings.Count(sub.body.String(), `"type":"done"`); n != 1 {
		t.Fatalf("done delivered %d times, want exactly 1; body=%q", n, sub.body.String())
	}
}

// A live-only type that somehow reaches the table must still not be fanned out
// by the tail: that is the type guard, and without it the active tab
// double-renders.
func TestChatSubscribeNeverTailsLiveOnlyEvents(t *testing.T) {
	_, podB, db := newReplicaPair(t, &e2eProvider{reply: "unused"}, 2)

	sub, _, stopSub := subscribeOn(t, podB, "agt_e2e", "chat-live-only")
	defer stopSub()

	// Write a content_delta row directly: the emitter never persists these, so
	// this is the "someone changed that" case.
	if _, err := db.AppendSessionEvent(context.Background(), "u_1", "agt_e2e", "chat-live-only",
		"content_delta", json.RawMessage(`{"delta":"tick"}`)); err != nil {
		t.Fatalf("seed content_delta row: %v", err)
	}
	if _, err := db.AppendSessionEvent(context.Background(), "u_1", "agt_e2e", "chat-live-only",
		"content", json.RawMessage(`{"content":"full text"}`)); err != nil {
		t.Fatalf("seed content row: %v", err)
	}

	// The persisted `content` event proves the tail is running...
	waitForBody(t, sub, "full text", 3*time.Second)
	// ...and the live-only one must not ride along.
	if sub.seen(`"type":"content_delta"`) {
		t.Fatalf("the tail forwarded a live-only event; body=%q", sub.body.String())
	}
}

// The connect replay reads the same log as the tail, so the live-only rule has to hold there
// too — and with the rows already in the table this is not a race. This is the deterministic
// half of the test above, which caught the same defect by luck in CI (the subscriber wrote its
// rows between the opening frame and the replay scan; measured 2026-09-26).
func TestAChatSubscribeReplaySkipsLiveOnlyEvents(t *testing.T) {
	_, podB, db := newReplicaPair(t, &e2eProvider{reply: "unused"}, 2)

	// Seeded BEFORE subscribing, so both rows are certainly inside the replay range. A
	// subscriber with no cursor gets the whole log (sinceSeq = -1 ⇒ `seq > -1`), which is the
	// path a page load takes when it has nothing to resume from.
	if _, err := db.AppendSessionEvent(context.Background(), "u_1", "agt_e2e", "chat-replay-live-only",
		"content_delta", json.RawMessage(`{"delta":"tick"}`)); err != nil {
		t.Fatalf("seed content_delta row: %v", err)
	}
	if _, err := db.AppendSessionEvent(context.Background(), "u_1", "agt_e2e", "chat-replay-live-only",
		"content", json.RawMessage(`{"content":"full text"}`)); err != nil {
		t.Fatalf("seed content row: %v", err)
	}

	sub, _, stopSub := subscribeOn(t, podB, "agt_e2e", "chat-replay-live-only")
	defer stopSub()

	// The persisted event proves the replay ran...
	waitForBody(t, sub, "full text", 3*time.Second)
	// ...and the live-only one must not ride along, out of the replay or the tail.
	if sub.seen(`"type":"content_delta"`) {
		t.Fatalf("the replay forwarded a live-only event; body=%q", sub.body.String())
	}
}

// failingTailStore is a real store whose tail query always fails: the handler
// must degrade to hub-only, not drop the subscription.
type failingTailStore struct {
	store.Store
}

func (f failingTailStore) ListSessionEventsSince(context.Context, string, string, string, int64) ([]store.SessionEventRecord, error) {
	return nil, context.DeadlineExceeded
}

func TestChatSubscribeSurvivesATailQueryFailure(t *testing.T) {
	podA, _, db := newReplicaPair(t, &e2eProvider{reply: "still alive"}, 2)
	podA.dataStore = failingTailStore{Store: db}

	sub, subDone, stopSub := subscribeOn(t, podA, "agt_e2e", "chat-tail-fails")
	defer stopSub()

	// Same pod, so the hub path still works; the turn must complete and the
	// subscriber must see it even though every tail query errors.
	body := runSingleChatStream(t, podA, "chat-tail-fails", "go-tail-fails")
	if !strings.Contains(body, "still alive") {
		t.Fatalf("the turn did not finish; stream=%q", body)
	}
	waitForBody(t, sub, "still alive", 3*time.Second)

	// Two tail ticks later the subscription must still be open: a store error
	// degrades delivery to the same-pod hub, it does not end the stream.
	time.Sleep(1200 * time.Millisecond)
	select {
	case <-subDone:
		t.Fatal("the subscription was torn down by a failing tail query")
	default:
	}
}
