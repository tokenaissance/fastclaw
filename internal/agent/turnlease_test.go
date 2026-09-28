package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
)

// fakeLease is the port's test double: it can be told to report a live peer,
// to refuse the first N acquisitions, and to fail a renewal (a takeover).
type fakeLease struct {
	mu       sync.Mutex
	holder   string
	epoch    int64
	peer     *Turn // what Live reports; also the facts of a refused Acquire
	busyFor  int
	renewErr error
	liveErr  error
	cancel   bool
	releases int
}

func (f *fakeLease) setCancel(v bool) {
	f.mu.Lock()
	f.cancel = v
	f.mu.Unlock()
}

func (f *fakeLease) setEpochBump() {
	f.mu.Lock()
	f.epoch++
	f.mu.Unlock()
}

func (f *fakeLease) CancelRequested(_ context.Context, _ SessionKey, held *Turn) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if held != nil && held.Epoch != f.epoch {
		return false, nil
	}
	return f.cancel, nil
}

func (f *fakeLease) Acquire(context.Context, SessionKey, time.Duration) (*Turn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busyFor > 0 {
		f.busyFor--
		busy := &SessionTurnBusy{}
		if f.peer != nil {
			busy.Holder, busy.ExpiresAt = f.peer.Holder, f.peer.ExpiresAt
		}
		return nil, busy
	}
	f.holder, f.epoch = "pod-a/1", f.epoch+1
	return &Turn{Holder: f.holder, Epoch: f.epoch, ExpiresAt: time.Now().Add(time.Minute)}, nil
}

func (f *fakeLease) Renew(_ context.Context, _ SessionKey, held *Turn, _ time.Duration) (*Turn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.renewErr != nil {
		return nil, f.renewErr
	}
	f.epoch++
	return &Turn{Holder: held.Holder, Epoch: f.epoch, ExpiresAt: time.Now().Add(time.Minute)}, nil
}

func (f *fakeLease) Release(context.Context, SessionKey, *Turn) error {
	f.mu.Lock()
	f.releases++
	f.mu.Unlock()
	return nil
}

func (f *fakeLease) Live(context.Context, SessionKey) (*Turn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peer, f.liveErr
}

// A queued caller waits on the LEASE (the cross-replica gate) and is told who
// is ahead and until when — "position 1" alone cannot say that (A4.1).
func TestTurnAdmissionWaitsOnALiveLeaseAndReportsTheHolder(t *testing.T) {
	a, prov := newGateAgent(t)
	peer := &Turn{Holder: "pod-b/2", ExpiresAt: time.Now().Add(30 * time.Second)}
	lease := &fakeLease{peer: peer, busyFor: 1}
	a.sessionLease = lease
	a.turnLeaseTTLOverride = 200 * time.Millisecond
	a.turnLeaseRetryOverride = 20 * time.Millisecond

	events := make(chan ChatEvent, 32)
	ctx := ContextWithChatEvents(context.Background(), events)

	msg := bus.InboundMessage{Channel: "web", UserID: "u_owner", ChatID: "chat-lease-1", Text: "hi"}
	if reply := a.HandleMessage(ctx, msg); reply == "" {
		t.Fatal("the turn produced no reply after the lease was granted")
	}
	select {
	case <-prov.called:
	default:
		t.Fatal("the model was never called")
	}
	close(events)
	var queued []map[string]any
	for evt := range events {
		if evt.Type == "queued" {
			queued = append(queued, evt.Data)
		}
	}
	if len(queued) == 0 {
		t.Fatal("no queued event was emitted while the lease was held by a peer")
	}
	if got := queued[0]["holder"]; got != "pod-b/2" {
		t.Fatalf("queued holder = %v, want pod-b/2", got)
	}
	// camelCase, like the live-turn fact: the two events used to spell the same
	// concept two ways and the client tolerated both (contract C1).
	if queued[0]["expiresAt"] == nil {
		t.Fatal("queued event carries no ETA")
	}
	if queued[0]["expires_at"] != nil {
		t.Fatal("the retired snake_case spelling came back")
	}
	if lease.releases != 1 {
		t.Fatalf("lease releases = %d, want exactly 1", lease.releases)
	}
}

// An automatic turn that finds the session held by a PEER defers instead of
// queueing: queueing is what holds a task-queue worker and its budget hostage
// (P2). This is the gap the 09-19 review found — the verdict used to come from
// the in-process gate, which cannot see another replica.
func TestAutomaticTurnDefersWhenAPeerHoldsTheLease(t *testing.T) {
	a, _ := newGateAgent(t)
	a.sessionLease = &fakeLease{peer: &Turn{Holder: "pod-b/2", ExpiresAt: time.Now().Add(time.Minute)}}

	events := make(chan ChatEvent, 8)
	ctx := ContextWithChatEvents(context.Background(), events)

	msg := bus.InboundMessage{Channel: "web", UserID: "u_owner", ChatID: "chat-lease-2", Text: "tick", Source: bus.SourceCron}
	if _, err := a.RunTurn(ctx, msg); !errors.Is(err, ErrTurnNotAdmitted) {
		t.Fatalf("RunTurn error = %v, want ErrTurnNotAdmitted", err)
	}
	close(events)
	for evt := range events {
		if evt.Type == "queued" {
			t.Fatal("an automatic turn queued; it must defer")
		}
	}
	if msgs := a.sessions.Get(sessionTriple(msg, msg.ProjectID)).GetMessages(); len(msgs) != 0 {
		t.Fatalf("a deferred automatic turn wrote %d messages", len(msgs))
	}
}

// A turn superseded mid-flight says so once, stops, and leaves its (stale)
// fence in place so every later write is refused by the store rather than
// landing in the peer's history.
func TestSupersededTurnStopsAndSignals(t *testing.T) {
	a, _ := newGateAgent(t)
	lease := &fakeLease{}
	lease.renewErr = &SessionTurnBusy{Holder: "pod-b/2", ExpiresAt: time.Now().Add(time.Minute)}
	a.sessionLease = lease
	a.turnLeaseTTLOverride = 60 * time.Millisecond // renew tick = 20 ms

	events := make(chan ChatEvent, 8)
	ctx := ContextWithChatEvents(context.Background(), events)

	msg := bus.InboundMessage{Channel: "web", UserID: "u_owner", ChatID: "chat-lease-3", Text: "hi"}
	sess := a.sessions.Get(sessionTriple(msg, msg.ProjectID))
	g, ok := a.beginTurnLease(ctx, sess, func(evt ChatEvent) { emitEvent(ctx, evt) })
	if !ok {
		t.Fatal("beginTurnLease refused on a fresh session")
	}
	deadline := time.Now().Add(2 * time.Second)
	for !g.Lost() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !g.Lost() {
		t.Fatal("the guard never noticed the takeover")
	}
	// The channel has one owner at a time: the guard's renewer is a producer
	// on it (the loss notice), so the producer is stopped before the consumer
	// closes. This is the ordering the loop has — `defer lease.Stop()` runs
	// before the caller's channel is closed — and closing first is the -race
	// report this test shipped with: the close raced the notice's send.
	g.Stop()
	close(events)
	var notices []string
	var endings, reasons []string
	for evt := range events {
		if evt.Type != "notice" {
			continue
		}
		if e, _ := evt.Data["ending"].(string); e != "" {
			endings = append(endings, e)
		}
		if r, _ := evt.Data["reason"].(string); r != "" {
			reasons = append(reasons, r)
		}
		if m, _ := evt.Data["message"].(string); m != "" {
			notices = append(notices, m)
		}
	}
	if len(notices) != 1 {
		t.Fatalf("notices = %v, want exactly one", notices)
	}
	if notices[0] != turnSupersededNotice {
		t.Fatalf("notice = %q, want the superseded wording", notices[0])
	}
	// The structured half a task read consumes (docs/mcp-task-submission.md §14.3): the ending,
	// with the diagnosis kept beside it. Falsification: drop `ending` from the emitter and the
	// read has only prose to guess from — this assertion says the field is what it reads.
	// The structured half a task read consumes (docs/mcp-task-submission.md §14.3): the ending,
	// with the diagnosis kept beside it. Falsification: drop `ending` from the emitter and the
	// read has only prose to guess from — this assertion says the field is what it reads.
	if len(endings) != 1 || endings[0] != EndingStopped || reasons[0] != "superseded" {
		t.Fatalf("notice carried endings=%v reasons=%v; want one stopped/superseded", endings, reasons)
	}
	// The stale fence survives the loss: a later append is refused by the store.
	if sess.Fence() == nil {
		t.Fatal("the fence was cleared on loss; later writes would land unfenced")
	}
	if lease.releases != 0 {
		t.Fatal("a superseded guard released the peer's lease")
	}
	sess.ClearTurnFence()
}

// Stop is the turn's last statement (`defer lease.Stop()` in both loop entry
// points), so a caller that closes its event channel once the turn returns is
// entitled to assume the guard has stopped emitting. It has not: the loss
// notice is emitted from the renewal goroutine, and Stop returned while that
// send was still in flight. That is the shape of the -race report on
// TestSupersededTurnStopsAndSignals — the test's close of `events` against
// emitEventChecked's chansend1 from renewLoop — and behind it is a plain
// "send on a closed channel" waiting for the right interleaving.
//
// Falsification: red on the shipped Stop (it returns immediately, so the
// first select below takes the fatal branch); make Stop join the renewer and
// it passes.
func TestStopWaitsForTheNoticeItStarted(t *testing.T) {
	a, _ := newGateAgent(t)
	lease := &fakeLease{}
	lease.renewErr = &SessionTurnBusy{Holder: "pod-b/2", ExpiresAt: time.Now().Add(time.Minute)}
	a.sessionLease = lease
	a.turnLeaseTTLOverride = 60 * time.Millisecond // renew tick = 20 ms

	msg := bus.InboundMessage{Channel: "web", UserID: "u_owner", ChatID: "chat-lease-stop", Text: "hi"}
	sess := a.sessions.Get(sessionTriple(msg, msg.ProjectID))

	inEmit := make(chan struct{})
	letGo := make(chan struct{})
	emit := func(evt ChatEvent) {
		if evt.Type != lostNoticeEvent {
			return
		}
		close(inEmit)
		<-letGo
	}
	g, ok := a.beginTurnLease(context.Background(), sess, emit)
	if !ok {
		t.Fatal("beginTurnLease refused on a fresh session")
	}
	<-inEmit // the renewal goroutine is inside its notice

	stopped := make(chan struct{})
	go func() {
		g.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned while the guard was still emitting: a caller that closes its channel here races the send")
	case <-time.After(200 * time.Millisecond):
		// The notice is in flight, so Stop is still waiting. That is the contract.
	}
	close(letGo)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop never returned after the notice was delivered")
	}
	sess.ClearTurnFence()
}

// openCallAnswer is the whole of A2 step 2: the sentence the projection may
// use for a tool call stored history left open follows the lease, and nothing
// else. Falsification: return StoppedToolResult unconditionally (the old
// behaviour) and the peer-held case below goes red — the model would read
// "interrupted" for a sub-task that is still running, which is the 09-18
// duplication trigger.
func TestOpenCallAnswerFollowsTheLeaseFacts(t *testing.T) {
	msg := bus.InboundMessage{Channel: "web", UserID: "u_owner", ChatID: "chat-lease-4", Text: "hi"}

	cases := []struct {
		name string
		live *Turn
		err  error
		want string
	}{
		{name: "no live holder — no other turn can be running", live: nil, want: provider.StoppedToolResult},
		{name: "a peer holds the session", live: &Turn{Holder: "pod-b/2"}, want: provider.NoReplyTurnAliveResult},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := newGateAgent(t)
			a.sessionLease = &fakeLease{peer: tc.live}
			sess := a.sessions.Get(sessionTriple(msg, msg.ProjectID))
			if got := a.openCallAnswer(context.Background(), sess); got != tc.want {
				t.Fatalf("openCallAnswer = %q, want %q", got, tc.want)
			}
		})
	}

	// An unreadable lease is "not known" — never a claim either way.
	t.Run("the lease cannot be read", func(t *testing.T) {
		a, _ := newGateAgent(t)
		a.sessionLease = &fakeLease{liveErr: errors.New("store down")}
		sess := a.sessions.Get(sessionTriple(msg, msg.ProjectID))
		if got := a.openCallAnswer(context.Background(), sess); got != provider.NoReplyUnknownResult {
			t.Fatalf("openCallAnswer = %q, want the no-fact sentence", got)
		}
	})
}

// A cancel request reaches the holder through the lease row and stops the turn
// at its next boundary, with a σ (design X1–X6). It is checked next to the
// supersession check, so it works no matter which replica the turn runs on —
// the request is row-borne, the observation is local.
//
// Falsification: remove the `lease.Cancelled(ctx)` branch from the ReAct loops
// and this test's guard-side assertions still pass but the turn keeps running —
// the loop-level witness is the break, which the notice below pins.
func TestTurnLeaseGuardReportsACancelRequest(t *testing.T) {
	a, _ := newGateAgent(t)
	lease := &fakeLease{}
	a.sessionLease = lease
	msg := bus.InboundMessage{Channel: "web", UserID: "u_owner", ChatID: "chat-cancel-1", Text: "hi"}
	sess := a.sessions.Get(sessionTriple(msg, msg.ProjectID))

	g, ok := a.beginTurnLease(context.Background(), sess, func(ChatEvent) {})
	if !ok {
		t.Fatal("beginTurnLease refused on a fresh session")
	}
	defer g.Stop()

	if g.Cancelled(context.Background()) {
		t.Fatal("a fresh possession reports a cancel request")
	}
	lease.setCancel(true)
	if !g.Cancelled(context.Background()) {
		t.Fatal("the cancel request did not reach the holder")
	}
	// A request stamped for a different possession must not stop this one.
	lease.setEpochBump()
	if g.Cancelled(context.Background()) {
		t.Fatal("a stamp from another possession stopped this turn")
	}
}

// TestCancelledTurnStopsAndSignalsOnce is X1–X7's last unverified link, end to
// end and across the replica boundary the feature exists for: the *other* side
// stamps a cancel request on the live possession while this turn is inside a
// tool, and the loop's iteration boundary is the delivery point.
//
// It asserts three separate things, because any one of them alone would pass
// for the wrong reason:
//   - the turn stops (the provider is never asked for a second round),
//   - it says so exactly once, in the cancelled wording — a turn that vanishes
//     without a word is the failure the observability principle exists for,
//   - the *running* replica is the one that was stopped, not a peer (the fake
//     lease records that the second round never happened even though a tool
//     result was in the prompt and the model was ready to continue).
//
// Falsification: disable the `lease.Cancelled(ctx)` check in the main ReAct
// loop (`internal/agent/loop.go`, the one guarding the model call) and this
// fails at "notices = [], want exactly one"; running the probe against the
// *other* boundary check (the plan-mode loop further down) leaves it green,
// which is how the two sites were told apart.
func TestCancelledTurnStopsAndSignalsOnce(t *testing.T) {
	a, _ := newGateAgent(t)
	lease := &fakeLease{}
	a.sessionLease = lease
	prov := &toolRoundProvider{toolName: "slow_tool"}
	a.provider = prov
	started, release := blockingTool(t, a, "slow_tool")

	events := make(chan ChatEvent, 16)
	ctx := ContextWithChatEvents(context.Background(), events)
	msg := bus.InboundMessage{Channel: "web", UserID: "u_owner", ChatID: "chat-cancel-e2e", Text: "hi"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		a.HandleMessage(ctx, msg)
	}()

	<-started             // round 1 is inside its tool
	lease.setCancel(true) // what store.RequestSessionCancel stamps for a peer
	release()
	<-done

	close(events)
	var notices []string
	var endings, reasons []string
	for evt := range events {
		if evt.Type != lostNoticeEvent {
			continue
		}
		if e, _ := evt.Data["ending"].(string); e != "" {
			endings = append(endings, e)
		}
		if r, _ := evt.Data["reason"].(string); r != "" {
			reasons = append(reasons, r)
		}
		if m, _ := evt.Data["message"].(string); m != "" {
			notices = append(notices, m)
		}
	}
	if len(notices) != 1 {
		t.Fatalf("notices = %v, want exactly one", notices)
	}
	if notices[0] != turnCancelledNotice {
		t.Fatalf("notice = %q, want the cancelled wording", notices[0])
	}
	if len(endings) != 1 || endings[0] != EndingStopped || reasons[0] != "cancelled" {
		t.Fatalf("notice carried endings=%v reasons=%v; want one stopped/cancelled", endings, reasons)
	}
	if rounds := prov.rounds.Load(); rounds != 1 {
		t.Fatalf("provider rounds = %d, want 1: round 2 started after the cancel", rounds)
	}
}

// The streaming endpoint is a second turn implementation with its own copy of
// the boundary decision (HandleMessageStream, reached in production only from
// the OpenAI-compatible /v1 mount — internal/api/openai.go). It must mirror the
// non-streaming loop: the user's stop ends the turn there, says so exactly
// once, and — because the streaming loop's own fall-through is the forced final
// delivery — must NOT spend one more ChatStream and hand the caller a reply
// stamped iterationCapReached.
//
// Falsification: disable the `lease.Cancelled(ctx)` check in the streaming loop
// (`internal/agent/loop.go`, the copy inside HandleMessageStream) and this fails
// at "streamed content = ... want \"\"": the turn falls into
// streamFinalDeliveryAfterCap and returns the cap-sized reply. Disabling the
// *non-streaming* guard (TestCancelledTurnStopsAndSignalsOnce's site) leaves
// this green — that is how the two copies are told apart, and why each needs
// its own witness.
func TestCancelledTurnStreamsNothingAndSignalsOnce(t *testing.T) {
	a, _ := newGateAgent(t)
	lease := &fakeLease{}
	a.sessionLease = lease
	prov := &toolRoundProvider{toolName: "slow_tool"}
	a.provider = prov
	started, release := blockingTool(t, a, "slow_tool")

	events := make(chan ChatEvent, 16)
	ctx := ContextWithChatEvents(context.Background(), events)
	msg := bus.InboundMessage{Channel: "web", UserID: "u_owner", ChatID: "chat-stream-cancel", Text: "hi"}

	readerCh := make(chan *provider.StreamReader, 1)
	go func() { readerCh <- a.HandleMessageStream(ctx, msg) }()

	<-started             // round 1 is inside its tool
	lease.setCancel(true) // what store.RequestSessionCancel stamps for a peer
	release()
	reader := <-readerCh

	var content string
	for {
		chunk, ok := reader.Next()
		if !ok {
			break
		}
		content += chunk.Content
	}
	if content != "" {
		t.Fatalf("streamed content = %q, want %q: the forced final delivery ran after the user's stop", content, "")
	}

	close(events)
	var notices []string
	var endings []string
	for evt := range events {
		if evt.Type != lostNoticeEvent {
			continue
		}
		if e, _ := evt.Data["ending"].(string); e != "" {
			endings = append(endings, e)
		}
		if m, _ := evt.Data["message"].(string); m != "" {
			notices = append(notices, m)
		}
	}
	if len(notices) != 1 {
		t.Fatalf("notices = %v, want exactly one", notices)
	}
	if notices[0] != turnCancelledNotice {
		t.Fatalf("notice = %q, want the cancelled wording", notices[0])
	}
	if len(endings) != 1 || endings[0] != EndingStopped {
		t.Fatalf("endings = %v; want one %q", endings, EndingStopped)
	}
	if rounds := prov.rounds.Load(); rounds != 1 {
		t.Fatalf("provider rounds = %d, want 1: a round started after the cancel", rounds)
	}
}
