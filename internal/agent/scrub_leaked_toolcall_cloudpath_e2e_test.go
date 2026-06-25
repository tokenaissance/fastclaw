package agent

// e2e for commit 33877b3 "fix agent tool handling and deletion" — the
// leaked-tool-call scrub half of the tool-handling fix.
//
// Cloud zero-impact rationale: when an open-source model (DeepSeek/Qwen
// variants) emits tool calls as raw special-token XML in the assistant
// `content` instead of the native tool_calls schema, the terminal
// synthesis path (after maxToolIterations is exhausted) used to return
// that raw `<|tool_calls|>` / `<invoke ...>` garbage verbatim — the
// Cloud web client then rendered the leaked markup in the chat bubble.
// #15 fixes it with scrubLeakedToolCallContent on the forced-final
// delivery. Cloud (Next.js app) reaches this through the /api/fastagent
// proxy's /chat path and only ever reads `content` events, so this is
// purely a quality-of-output improvement — no endpoint / auth / shape
// change.
//
// This test drives the REAL HandleMessage ReAct loop (session.Manager +
// registry + hooks + bus + a real ContextBuilder/Memory) with a fake
// provider whose ChatStream always yields the leaked-DSML shape. The
// main loop recovers the invoke into a tool call (which fails — nothing
// is registered), burns maxToolIterations, then the forced-final
// delivery must scrub the markup so the returned reply and the emitted
// `content` event contain only the model's human preamble.

import (
	"context"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/agent/tools"
	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
	"github.com/fastclaw-ai/fastclaw/internal/session"
)

// dsmlLeakProvider always returns the leaked-DSML shape: a human
// preamble followed by special-token tool-call markup. This mirrors the
// exact DeepSeek/Qwen detokenization failure mode #15's scrub guards
// against.
type dsmlLeakProvider struct{}

const dsmlLeakedContent = `I'll search the web for you.
<|tool_calls|>
<invoke name="web_search">
  <parameter name="query" string="true">fastagent release notes</parameter>
</invoke>`

func (p *dsmlLeakProvider) Chat(_ context.Context, _ []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.Response, error) {
	return &provider.Response{Content: dsmlLeakedContent}, nil
}

func (p *dsmlLeakProvider) ChatStream(_ context.Context, _ []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.StreamReader, error) {
	ch := make(chan provider.StreamChunk, 1)
	ch <- provider.StreamChunk{Content: dsmlLeakedContent, Done: true}
	close(ch)
	return provider.NewStreamReader(ch), nil
}

func TestAgent_ScrubLeakedToolCall_CloudPathE2E(t *testing.T) {
	mem := NewMemory(t.TempDir())
	a := &Agent{
		name:              "scrub-agent",
		ownerUserID:       "u_owner",
		provider:          &dsmlLeakProvider{},
		registry:          tools.NewRegistry("", ""),
		sessions:          session.NewManager(t.TempDir()),
		memory:            mem,
		ctxBuilder:        NewContextBuilder(t.TempDir(), mem, ""),
		hooks:             NewHookRegistry(),
		messageBus:        bus.New(),
		engine:            newSDKEngine(""),
		model:             "fake-model",
		maxTokens:         256,
		temperature:       0.7,
		maxToolIterations: 2,
	}

	evts := make(chan ChatEvent, 16)
	ctx := ContextWithChatEvents(context.Background(), evts)

	reply := a.HandleMessage(ctx, bus.InboundMessage{
		Channel: "web", UserID: "u_owner", Text: "search the web",
	})

	close(evts)
	var contentEvts []string
	for e := range evts {
		if e.Type == "content" {
			if c, _ := e.Data["content"].(string); c != "" {
				contentEvts = append(contentEvts, c)
			}
		}
	}

	// The forced-final delivery must strip the leaked markup. The reply
	// accumulates the model's human preamble once per tool round plus
	// once for the final synthesis, joined by the split marker — the
	// exact segment count is an implementation detail. What matters is
	// that NO leaked tool-call markup survives anywhere in it.
	if !strings.Contains(reply, "I'll search the web for you.") {
		t.Fatalf("reply = %q, want it to contain the human preamble", reply)
	}
	// Note: the fork's legit split marker is `<|split|>` (channels
	// SplitMessageMarker) — that IS expected in the reply. The leak
	// shapes are the DSML/tool-call markup, checked below.
	for _, leak := range []string{"<invoke", "<|tool", "<|tool_calls", "DSML", "parameter", "</invoke>"} {
		if strings.Contains(reply, leak) {
			t.Errorf("reply still contains leaked markup %q: %q", leak, reply)
		}
	}
	// The emitted content event (what the Cloud web client renders) must
	// also be clean.
	for _, c := range contentEvts {
		if strings.Contains(c, "<invoke") || strings.Contains(c, "<|tool") || strings.Contains(c, "DSML") {
			t.Errorf("content event still contains leaked markup: %q", c)
		}
	}
	if len(contentEvts) == 0 {
		t.Error("no content event emitted on the forced-final delivery")
	}
}
