package setup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/api"
	"github.com/fastclaw-ai/fastclaw/internal/auth"
	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
)

// sseRecorder is an http.ResponseWriter that records the SSE body while the
// handler streams from another goroutine.
type sseRecorder struct {
	mu     sync.Mutex
	body   strings.Builder
	header http.Header
	status int
}

func newSSERecorder() *sseRecorder { return &sseRecorder{header: make(http.Header)} }

func (r *sseRecorder) Header() http.Header { return r.header }
func (r *sseRecorder) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status == 0 {
		r.status = code
	}
}
func (r *sseRecorder) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.Write(b)
}
func (r *sseRecorder) Flush() {}
func (r *sseRecorder) seen(substr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Contains(r.body.String(), substr)
}

// e2eProvider counts the turns that actually reached the model.
type e2eProvider struct {
	mu      sync.Mutex
	started chan struct{}
	reply   string
}

func (p *e2eProvider) noteStart() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started != nil {
		select {
		case p.started <- struct{}{}:
		default:
		}
	}
}

func (p *e2eProvider) Chat(_ context.Context, _ []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.Response, error) {
	p.noteStart()
	return &provider.Response{Content: p.reply}, nil
}

func (p *e2eProvider) ChatStream(_ context.Context, _ []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.StreamReader, error) {
	p.noteStart()
	ch := make(chan provider.StreamChunk, 2)
	ch <- provider.StreamChunk{Content: p.reply, Done: true}
	close(ch)
	return provider.NewStreamReader(ch), nil
}

type e2eResolver struct {
	space  *api.UserSpaceView
	agents *agent.Manager
}

func (r e2eResolver) UserSpaceFor(string) (*api.UserSpaceView, error) { return r.space, nil }
func (r e2eResolver) LocalAgentManager() *agent.Manager               { return r.agents }
func (r e2eResolver) IsCloudMode() bool                               { return false }

func newQueuedChatHarness(t *testing.T) (*Server, *agent.Agent, *e2eProvider) {
	t.Helper()
	prov := &e2eProvider{started: make(chan struct{}, 4), reply: "done"}
	s, ag := newChatHarness(t, prov, 1)
	return s, ag, prov
}

// newChatHarness builds the server + agent pair both chat e2e tests drive: the
// real /api/chat handlers in front of a real agent runtime with a fake model.
// Callers supply the provider so a test can script tool rounds; maxToolIterations
// is 1 for the plain reply flows and higher when a turn has to run a tool.
func newChatHarness(t *testing.T, prov provider.Provider, maxToolIterations int) (*Server, *agent.Agent) {
	t.Helper()
	return newChatHarnessWith(t, prov, maxToolIterations)
}

// newChatHarnessWith is newChatHarness with manager options: the cross-replica
// tests need the one option that decides whether a harness is a replica or just
// a server — the store-backed turn lease.
func newChatHarnessWith(t *testing.T, prov provider.Provider, maxToolIterations int, opts ...agent.ManagerOption) (*Server, *agent.Agent) {
	t.Helper()
	home := t.TempDir()
	rc := config.ResolvedAgent{
		ID: "agt_e2e", UserID: "u_1", Home: home,
		Workspace: filepath.Join(home, "workspace"), Model: "fake-model",
		MaxTokens: 128, Temperature: 0.7, MaxToolIterations: maxToolIterations,
	}
	opts = append([]agent.ManagerOption{agent.WithUserID("u_1")}, opts...)
	mgr, err := agent.NewManager([]config.ResolvedAgent{rc}, prov, bus.New(), opts...)
	if err != nil {
		t.Fatalf("agent manager: %v", err)
	}
	ag := mgr.AgentByID("agt_e2e")
	if ag == nil {
		t.Fatal("agent not registered")
	}
	s := &Server{userResolver: e2eResolver{space: &api.UserSpaceView{UserID: "u_1", Agents: mgr}, agents: mgr}}
	return s, ag
}

func chatStreamRequest(t *testing.T, req chatRequest) *http.Request {
	t.Helper()
	body, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPost, "/api/chat/stream", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	return r.WithContext(auth.WithIdentity(r.Context(), auth.Identity{UserID: "u_1", Role: "user", AuthMethod: "session"}))
}

// A message sent while the session is busy with another turn (a cron tick, a
// second tab) is queued, says so on its stream, and can be withdrawn before it
// starts — nothing reaches the model or the session.
func TestQueuedChatTurnIsAnnouncedAndWithdrawableE2E(t *testing.T) {
	s, ag, prov := newQueuedChatHarness(t)
	sess := ag.Sessions().Get("web", "", "chat-queued", "")
	if !sess.AcquireTurn(context.Background()) {
		t.Fatal("could not take the turn slot for the test")
	}

	rec := newSSERecorder()
	req := chatStreamRequest(t, chatRequest{AgentID: "agt_e2e", SessionID: "chat-queued", Message: "queued please", TurnID: "turn-abc"})
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleChatStream(rec, req)
	}()

	// Wait until the handler registered its (still queued) turn.
	key := chatTurnKey("u_1", "agt_e2e", "chat-queued", "turn-abc")
	deadline := time.Now().Add(5 * time.Second)
	for lookupForTest(s, key) == nil {
		if time.Now().After(deadline) {
			t.Fatal("chat POST never registered a pending turn")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for !rec.seen("queued") {
		if time.Now().After(deadline) {
			t.Fatalf("stream never announced the queued turn; body=%q", rec.body.String())
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Withdraw it (the dashboard's Cancel / Edit).
	cancelRec := httptest.NewRecorder()
	cancelReq := httptest.NewRequest(http.MethodPost, "/api/chat/cancel",
		strings.NewReader(`{"agentId":"agt_e2e","sessionId":"chat-queued","turnId":"turn-abc"}`))
	cancelReq = cancelReq.WithContext(auth.WithIdentity(cancelReq.Context(), auth.Identity{UserID: "u_1", Role: "user", AuthMethod: "session"}))
	s.handleChatCancel(cancelRec, cancelReq)
	if cancelRec.Code != http.StatusOK {
		t.Fatalf("cancel status = %d body=%s; want 200", cancelRec.Code, cancelRec.Body.String())
	}

	sess.ReleaseTurn()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("withdrawn chat POST never returned")
	}

	select {
	case <-prov.started:
		t.Fatal("withdrawn turn still reached the model")
	default:
	}
	if msgs := sess.GetMessages(); len(msgs) != 0 {
		t.Fatalf("withdrawn turn wrote %d messages into the session", len(msgs))
	}
}

// A turn that is still queued has to outlive the connection that submitted it
// (docs/mcp-task-submission.md §14.6 path 1): a reload or a tab switch drops the POST,
// and the reloaded tab's Cancel — with the id the `queued` σ carried — must still
// withdraw it. Measured on dev 2026-09-26: a direct cancel with the right id answered
// {"canceled":false}, because the pending entry died with its handler. The entry now
// belongs to the turn goroutine, so its lifetime is the wait.
func TestAQueuedTurnOutlivesItsConnectionE2E(t *testing.T) {
	s, ag, prov := newQueuedChatHarness(t)
	sess := ag.Sessions().Get("web", "", "chat-reload", "")
	if !sess.AcquireTurn(context.Background()) {
		t.Fatal("could not take the turn slot for the test")
	}

	rec := newSSERecorder()
	ctx, dropConnection := context.WithCancel(context.Background())
	req := chatStreamRequest(t, chatRequest{AgentID: "agt_e2e", SessionID: "chat-reload", Message: "queued then reloaded", TurnID: "turn-reload"}).
		WithContext(auth.WithIdentity(ctx, auth.Identity{UserID: "u_1", Role: "user", AuthMethod: "session"}))
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		s.handleChatStream(rec, req)
	}()

	// The turn is queued and says so. That σ is the only thing a reloaded tab has to
	// render its Cancel from, so the test waits for it rather than for a timer.
	announced := time.Now().Add(5 * time.Second)
	for !rec.seen("queued") {
		if time.Now().After(announced) {
			t.Fatalf("stream never announced the queued turn; body=%q", rec.body.String())
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The tab reloads: the request's context dies and the handler returns immediately.
	// The turn keeps waiting — agentCtx is detached from the request on purpose.
	dropConnection()
	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after its client went away")
	}

	// The reloaded tab's Cancel, with the id the σ carried.
	cancelRec := httptest.NewRecorder()
	cancelReq := httptest.NewRequest(http.MethodPost, "/api/chat/cancel",
		strings.NewReader(`{"agentId":"agt_e2e","sessionId":"chat-reload","turnId":"turn-reload"}`))
	cancelReq = cancelReq.WithContext(auth.WithIdentity(cancelReq.Context(), auth.Identity{UserID: "u_1", Role: "user", AuthMethod: "session"}))
	s.handleChatCancel(cancelRec, cancelReq)
	if cancelRec.Code != http.StatusOK {
		t.Fatalf("cancel status = %d body=%s; want 200", cancelRec.Code, cancelRec.Body.String())
	}
	var cancelBody map[string]any
	if err := json.Unmarshal(cancelRec.Body.Bytes(), &cancelBody); err != nil {
		t.Fatalf("decode cancel body: %v", err)
	}
	// wasRunning=false is the honest half of the answer: nothing had started yet, so
	// nothing was interrupted — the running turn belongs to someone else and is untouched.
	if cancelBody["canceled"] != true || cancelBody["wasRunning"] != false {
		t.Fatalf("cancel body = %v; want the queued turn withdrawn (canceled=true, wasRunning=false)", cancelBody)
	}

	// The withdrawal is real, not just an answer: the entry is released when the turn
	// goroutine returns, the model is never reached, and the session stays empty.
	key := chatTurnKey("u_1", "agt_e2e", "chat-reload", "turn-reload")
	released := time.Now().Add(5 * time.Second)
	for lookupForTest(s, key) != nil {
		if time.Now().After(released) {
			t.Fatal("the withdrawn turn's pending entry was never released")
		}
		time.Sleep(5 * time.Millisecond)
	}
	sess.ReleaseTurn()
	select {
	case <-prov.started:
		t.Fatal("withdrawn turn still reached the model")
	default:
	}
	if msgs := sess.GetMessages(); len(msgs) != 0 {
		t.Fatalf("withdrawn turn wrote %d messages into the session", len(msgs))
	}
}

// Once the turn has started, withdrawal is refused (409) and the turn runs to
// completion — the client falls back to plain Stop semantics.
func TestStartedChatTurnCannotBeWithdrawnE2E(t *testing.T) {
	s, ag, prov := newQueuedChatHarness(t)
	sess := ag.Sessions().Get("web", "", "chat-started", "")

	rec := newSSERecorder()
	req := chatStreamRequest(t, chatRequest{AgentID: "agt_e2e", SessionID: "chat-started", Message: "go", TurnID: "turn-xyz"})
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleChatStream(rec, req)
	}()

	select {
	case <-prov.started:
	case <-time.After(10 * time.Second):
		t.Fatal("turn never reached the model")
	}

	cancelRec := httptest.NewRecorder()
	cancelReq := httptest.NewRequest(http.MethodPost, "/api/chat/cancel",
		strings.NewReader(`{"agentId":"agt_e2e","sessionId":"chat-started","turnId":"turn-xyz"}`))
	cancelReq = cancelReq.WithContext(auth.WithIdentity(cancelReq.Context(), auth.Identity{UserID: "u_1", Role: "user", AuthMethod: "session"}))
	s.handleChatCancel(cancelRec, cancelReq)
	// The old contract answered 409 "already_started" and told the caller to
	// "use the normal stop" — which only detached the client's stream while the
	// server kept working. Now a started turn is cancelled through the session's
	// lease row (design X1–X7): with no lease wired (this harness), that is an
	// honest no-op — 200, canceled=false — never an error the UI must translate.
	if cancelRec.Code != http.StatusOK {
		t.Fatalf("cancel status = %d body=%s; want 200", cancelRec.Code, cancelRec.Body.String())
	}
	var cancelBody map[string]any
	if err := json.Unmarshal(cancelRec.Body.Bytes(), &cancelBody); err != nil {
		t.Fatalf("decode cancel body: %v", err)
	}
	if cancelBody["canceled"] != false || cancelBody["wasRunning"] != false {
		t.Fatalf("cancel body = %v; want an honest no-op when nothing holds the session", cancelBody)
	}

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("started turn never finished")
	}
	if msgs := sess.GetMessages(); len(msgs) == 0 {
		t.Fatal("started turn wrote nothing into the session")
	}
}
