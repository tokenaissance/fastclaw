package agent

// e2e for commit 48ee0b3 "fix runtime settings and empty chat
// responses" — the empty-response half.
//
// Cloud zero-impact rationale: when the upstream LLM returns an empty
// turn (content + tool-calls both absent), the agent loop used to emit
// only `done` and return an empty string — the Cloud web client then saw
// a stream that "ended without any response". The fix makes HandleMessage
// emit an `error` event ("model returned an empty response") followed by
// `done` and return the message text. Cloud (Next.js app) reaches this
// through the /api/fastagent proxy's /chat path; the SSE handler forwards
// the error event to the open stream. No endpoint / auth / shape change.
//
// This test drives the REAL HandleMessage ReAct loop (session.Manager +
// registry + hooks + bus + a real ContextBuilder/Memory) with a fake
// provider whose ChatStream yields exactly one empty Done chunk, then
// asserts the loop's guard fires: error+done events on the context
// channel and the "model returned an empty response" return string.
// Mirrors the Cloud call path (web /chat → HandleMessage → events).

import (
	"context"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/agent/tools"
	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
	"github.com/fastclaw-ai/fastclaw/internal/session"
)

// emptyProvider returns a single empty Done chunk — no content, no tool
// calls. This is exactly the LLM glitch the fix guards against.
type emptyProvider struct{}

func (p *emptyProvider) Chat(_ context.Context, _ []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.Response, error) {
	return &provider.Response{Content: ""}, nil
}

func (p *emptyProvider) ChatStream(_ context.Context, _ []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.StreamReader, error) {
	ch := make(chan provider.StreamChunk, 1)
	ch <- provider.StreamChunk{Done: true}
	close(ch)
	return provider.NewStreamReader(ch), nil
}

func TestAgent_EmptyResponse_CloudPathE2E(t *testing.T) {
	mem := NewMemory(t.TempDir())
	a := &Agent{
		name:              "empty-agent",
		ownerUserID:       "u_owner",
		provider:          &emptyProvider{},
		registry:          tools.NewRegistry("", ""),
		sessions:          session.NewManager(t.TempDir()),
		memory:            mem,
		ctxBuilder:        NewContextBuilder(t.TempDir(), mem, ""),
		hooks:             NewHookRegistry(),
		messageBus:        bus.New(),
		model:             "fake-model",
		maxTokens:         256,
		temperature:       0.7,
		maxToolIterations: 2,
	}

	// Capture the events the loop emits through the context channel (the
	// same channel the legacy SSE handler reads from).
	evts := make(chan ChatEvent, 16)
	ctx := ContextWithChatEvents(context.Background(), evts)

	reply := a.HandleMessage(ctx, bus.InboundMessage{
		Channel: "web", UserID: "u_owner", Text: "hello",
	})

	close(evts)
	var got []ChatEvent
	for e := range evts {
		got = append(got, e)
	}

	// The guard returns the empty-message text AND emits error + done.
	if reply != "model returned an empty response" {
		t.Fatalf("reply = %q, want 'model returned an empty response'", reply)
	}

	var sawErr, sawDone bool
	for _, e := range got {
		if e.Type == "error" {
			sawErr = true
			if m, _ := e.Data["message"].(string); m != "model returned an empty response" {
				t.Errorf("error event message = %q, want 'model returned an empty response'", m)
			}
		}
		if e.Type == "done" {
			sawDone = true
		}
		if e.Type == "content" {
			t.Error("empty turn emitted a content event — nothing to render")
		}
	}
	if !sawErr {
		t.Errorf("no error event emitted; got %d events: %+v", len(got), got)
	}
	if !sawDone {
		t.Errorf("no done event emitted; got %d events: %+v", len(got), got)
	}
}
