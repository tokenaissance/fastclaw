package agent

// e2e for commit fc1ab4f "fix: prefer compatible OpenAI chat parameters".
//
// Cloud zero-impact rationale: the change tunes how the gateway talks to
// the OpenAI-compatible upstream — for model families known to reject
// legacy params (gpt-5 / o1 / o3 / o4) it sends max_completion_tokens and
// omits temperature on the FIRST attempt, avoiding the 400-retry
// round-trip that #6 (3627bff) added. Cloud (Next.js app) posts /chat
// messages through the /api/fastagent proxy and reads back the streamed
// reply; the negotiation is invisible to the client (same final response,
// fewer wasted round-trips). No endpoint, response shape, or auth change.
//
// This test drives the REAL HandleMessage ReAct loop (session.Manager +
// registry + hooks + bus + a real ContextBuilder/Memory) with the REAL
// OpenAIProvider wired to a fake OpenAI-compatible SSE endpoint — so the
// actual doChatRequest → buildRequest → initialOpenAIRequestMode path
// runs, only the upstream HTTP endpoint is faked. Mirrors the Cloud call
// path (web /chat → HandleMessage → streamChatToResponse →
// provider.ChatStream).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/agent/tools"
	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
	"github.com/fastclaw-ai/fastclaw/internal/session"
)

func TestOpenAICompatParams_CloudPathE2E(t *testing.T) {
	t.Run("newer-model-prefers-modern-params", func(t *testing.T) {
		var mu sync.Mutex
		var calls int
		var firstBody map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode request: %v", err)
			}
			if calls == 1 {
				firstBody = body
			}
			// A real newer-model completion — plain content, no tool
			// calls, so the ReAct loop terminates after one turn.
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		}))
		defer srv.Close()

		reply := driveOpenAICompatAgent(t, provider.NewOpenAI("sk-e2e", srv.URL), "openai/gpt-5.5")
		if reply != "ok" {
			t.Fatalf("reply = %q, want ok", reply)
		}
		mu.Lock()
		defer mu.Unlock()
		if calls != 1 {
			t.Fatalf("LLM called %d times, want 1 (modern params accepted first try)", calls)
		}
		if _, ok := firstBody["max_completion_tokens"]; !ok {
			t.Errorf("first request missing max_completion_tokens: %#v", firstBody)
		}
		if _, ok := firstBody["max_tokens"]; ok {
			t.Errorf("first request sent legacy max_tokens: %#v", firstBody)
		}
		if _, ok := firstBody["temperature"]; ok {
			t.Errorf("first request sent unsupported temperature: %#v", firstBody)
		}
	})

	t.Run("unknown-model-keeps-legacy-params", func(t *testing.T) {
		var mu sync.Mutex
		var calls int
		var firstBody map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode request: %v", err)
			}
			if calls == 1 {
				firstBody = body
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		}))
		defer srv.Close()

		reply := driveOpenAICompatAgent(t, provider.NewOpenAI("sk-e2e", srv.URL), "compatible-model")
		if reply != "ok" {
			t.Fatalf("reply = %q, want ok", reply)
		}
		mu.Lock()
		defer mu.Unlock()
		if calls != 1 {
			t.Fatalf("LLM called %d times, want 1", calls)
		}
		if _, ok := firstBody["max_tokens"]; !ok {
			t.Errorf("first request missing legacy max_tokens: %#v", firstBody)
		}
		if _, ok := firstBody["temperature"]; !ok {
			t.Errorf("first request missing temperature: %#v", firstBody)
		}
		if _, ok := firstBody["max_completion_tokens"]; ok {
			t.Errorf("first request sent max_completion_tokens for unknown model: %#v", firstBody)
		}
	})
}

// driveOpenAICompatAgent builds a REAL Agent whose provider is the real
// OpenAIProvider (fake upstream SSE endpoint) and drives one chat message
// through the actual HandleMessage ReAct loop.
func driveOpenAICompatAgent(t *testing.T, p provider.Provider, model string) string {
	t.Helper()
	mem := NewMemory(t.TempDir())
	a := &Agent{
		name:              "openai-mode-agent",
		ownerUserID:       "u_owner",
		provider:          p,
		registry:          tools.NewRegistry("", ""),
		sessions:          session.NewManager(t.TempDir()),
		memory:            mem,
		ctxBuilder:        NewContextBuilder(t.TempDir(), mem, ""),
		hooks:             NewHookRegistry(),
		messageBus:        bus.New(),
		model:             model,
		maxTokens:         123,
		temperature:       0.7,
		maxToolIterations: 2,
	}
	evts := make(chan ChatEvent, 16)
	ctx := ContextWithChatEvents(context.Background(), evts)
	return a.HandleMessage(ctx, bus.InboundMessage{
		Channel: "web", UserID: "u_owner", Text: "hello",
	})
}
