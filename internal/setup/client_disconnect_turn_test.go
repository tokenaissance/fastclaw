package setup

// The web turn's client is not its owner: POST /api/chat/stream detaches the
// agent's ctx from the request on purpose (handlers.go), so a browser that goes
// away must not end a turn the user already paid for. What had broken that
// property was where cancel() was deferred — on the handler, which returns as
// soon as the client's connection drops, so the deferred cancel ran there and
// took the turn down with it.
//
// Production, pod fastagent-gateway-568cc96dcb-6pzbl, 2026-09-16T15:26:29Z: a
// web turn 26 minutes into a babysitting loop logged `turn ctx ended with a tool
// in flight … cause="context canceled"` — the only thing that had happened was
// the client's connection going away — and one 60s grace window later the
// in-flight `sleep 200` exec died with "e2b exec body read: context canceled
// (got 68 bytes) … pid 1928", while the sandbox was healthy and the job kept
// running. This test fails on that shape ("client disconnect cancelled the
// turn") and passes once cancel() belongs to the agent goroutine.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/auth"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
)

// parkProvider parks inside the model call until the test releases it, and
// records whether the ctx it was handed died while it waited. The model call
// carries the turn's own ctx with no tool grace in front of it, which makes it
// the cheapest honest probe of "did something cancel this turn".
type parkProvider struct {
	started chan struct{}
	release chan struct{}

	mu       sync.Mutex
	canceled bool
	once     sync.Once
}

func (p *parkProvider) park(ctx context.Context) error {
	p.once.Do(func() { close(p.started) })
	select {
	case <-p.release:
		return nil
	case <-ctx.Done():
		p.mu.Lock()
		p.canceled = true
		p.mu.Unlock()
		return ctx.Err()
	}
}

func (p *parkProvider) sawCancel() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.canceled
}

func (p *parkProvider) Chat(ctx context.Context, _ []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.Response, error) {
	if err := p.park(ctx); err != nil {
		return nil, err
	}
	return &provider.Response{Content: "done"}, nil
}

func (p *parkProvider) ChatStream(ctx context.Context, _ []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.StreamReader, error) {
	if err := p.park(ctx); err != nil {
		return nil, err
	}
	ch := make(chan provider.StreamChunk, 1)
	ch <- provider.StreamChunk{Content: "done", Done: true}
	close(ch)
	return provider.NewStreamReader(ch), nil
}

func TestClientDisconnectDoesNotKillTheTurnE2E(t *testing.T) {
	prov := &parkProvider{started: make(chan struct{}), release: make(chan struct{})}
	s, ag := newChatHarness(t, prov, 1)
	sess := ag.Sessions().Get("web", "", "chat-drop", "")

	body, _ := json.Marshal(chatRequest{AgentID: "agt_e2e", SessionID: "chat-drop", Message: "babysit the job"})
	reqCtx, clientDisconnect := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api/chat/stream", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.WithIdentity(reqCtx, auth.Identity{UserID: "u_1", Role: "user", AuthMethod: "session"}))

	rec := newSSERecorder()
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		s.handleChatStream(rec, req)
	}()

	select {
	case <-prov.started:
	case <-time.After(10 * time.Second):
		t.Fatal("turn never reached the model")
	}

	// The browser goes away mid-turn: reload, closed tab, sleeping laptop.
	clientDisconnect()
	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("chat handler did not return after the client dropped")
	}

	// Nothing may have cancelled the turn. The model call in flight runs on the
	// turn's own ctx, so a cancel observed here is the production failure.
	time.Sleep(250 * time.Millisecond)
	if prov.sawCancel() {
		t.Fatal("client disconnect cancelled the turn; the turn must outlive its client")
	}

	// And the turn must still be able to finish: release the model and watch the
	// reply land in the session with no client listening for it.
	close(prov.release)
	deadline := time.Now().Add(10 * time.Second)
	for len(sess.GetMessages()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("turn never landed its reply after the client dropped")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if prov.sawCancel() {
		t.Fatal("turn was cancelled while finishing without its client")
	}

	// Let the detached turn finish before this test returns. The assistant message
	// lands mid-flight — the loop keeps going afterwards (skill refresh, turn files,
	// auto-persist) and writes into the harness home, which is a `t.TempDir`. Returning
	// while that goroutine still has files to create makes the framework's own cleanup
	// race it, and the failure reads as this test's fault rather than as a flake:
	//
	//	--- FAIL: TestClientDisconnectDoesNotKillTheTurnE2E
	//	    testing.go:1369: TempDir RemoveAll cleanup: unlinkat /tmp/…/001: directory not empty
	//
	// (Measured in CI 2026-09-25; the same test passed 3/3 and the whole package passed
	// under -race on a quieter machine, which is the signature of a race, not a break.)
	// Waiting for the turn slot to be free is the honest signal: the lease is released
	// last (see the ordering note in beginTurnLease).
	deadline = time.Now().Add(10 * time.Second)
	for sess.TurnActive() {
		if time.Now().After(deadline) {
			t.Fatal("turn slot was never released after the reply landed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
