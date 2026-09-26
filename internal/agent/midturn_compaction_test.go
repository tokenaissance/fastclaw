package agent

// The other half of context compaction.
//
// CompactMessages runs once per turn, at the top (loop.go:2590 / :3445), on the
// history the turn starts with. Everything the turn itself appends after that
// was never re-measured — and a turn appends two messages per round, one of
// which is unbounded: a tool result. A 30-round turn therefore walks past the
// window with the harness watching, and the request the provider refuses is one
// the harness could have fixed by compacting.
//
// The witness is the delivery point, as with the switch rows: what the provider
// saw on a LATER round. The marker is the placeholder pruneOldToolResults
// writes — the only thing in a prompt that says "a result was dropped to stay
// inside the window".

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
)

const (
	turnCompactionProbeTool = "bulk_probe"
	// turnCompactionDropped is the head of compaction.truncatedPlaceholder.
	turnCompactionDropped = "Result dropped by context compaction"
	// One tool result, in bytes. EstimateTokens counts chars/4, so this is
	// ~2k tokens per round — small enough that the threshold needs 40 rounds,
	// which is the point: the history also has to be deep enough that
	// pruneOldToolResults' "keep the last PruneTurnAge messages" actually
	// drops something.
	turnCompactionResultLen = 8000
	turnCompactionRounds    = 40
)

// turnCompactionProvider scripts a long turn: N rounds that each call the probe
// tool, then a final text answer. It records every prompt it is handed, so both
// "did a later round see a compacted history" and "did the prompt stop growing"
// are answered by what left the process rather than by reading the loop.
type turnCompactionProvider struct {
	mu     sync.Mutex
	calls  [][]provider.Message
	rounds int
	script int
}

func (p *turnCompactionProvider) record(msgs []provider.Message) {
	cp := make([]provider.Message, len(msgs))
	copy(cp, msgs)
	p.mu.Lock()
	p.calls = append(p.calls, cp)
	p.mu.Unlock()
}

// isSummarizerCall recognises compaction's own summarizer: a text-only call
// whose system row is the one compressOlderMessages writes. It must not consume
// a scripted round, or a compaction would shorten the script by one.
func isSummarizerCall(msgs []provider.Message, tools []provider.Tool) bool {
	return len(tools) == 0 && len(msgs) > 0 && strings.Contains(msgs[0].Content, "conversation summarizer")
}

func (p *turnCompactionProvider) next(msgs []provider.Message, tools []provider.Tool) *provider.Response {
	p.record(msgs)
	if isSummarizerCall(msgs, tools) {
		return &provider.Response{Content: "earlier rounds summarised"}
	}
	p.mu.Lock()
	p.rounds++
	n := p.rounds
	p.mu.Unlock()
	if n > p.script {
		return &provider.Response{Content: "done"}
	}
	return &provider.Response{ToolCalls: []provider.ToolCall{{
		ID: fmt.Sprintf("call_%d", n), Type: "function",
		Function: provider.FunctionCall{
			Name:      turnCompactionProbeTool,
			Arguments: fmt.Sprintf(`{"n":"%d"}`, n),
		},
	}}}
}

func (p *turnCompactionProvider) Chat(_ context.Context, msgs []provider.Message, tools []provider.Tool, _ string, _ int, _ float64) (*provider.Response, error) {
	return p.next(msgs, tools), nil
}

func (p *turnCompactionProvider) ChatStream(_ context.Context, msgs []provider.Message, tools []provider.Tool, _ string, _ int, _ float64) (*provider.StreamReader, error) {
	resp := p.next(msgs, tools)
	ch := make(chan provider.StreamChunk, 2)
	ch <- provider.StreamChunk{Content: resp.Content, ToolCalls: resp.ToolCalls, Done: true}
	close(ch)
	return provider.NewStreamReader(ch), nil
}

// prompts returns a copy of every prompt the provider was handed, oldest first.
func (p *turnCompactionProvider) prompts() [][]provider.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]provider.Message, len(p.calls))
	copy(out, p.calls)
	return out
}

// promptText flattens one prompt to the text a provider would send.
func promptText(msgs []provider.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		sb.WriteString(m.Role)
		sb.WriteString("|")
		sb.WriteString(m.Content)
		sb.WriteString("\n")
	}
	return sb.String()
}

// newTurnCompactionAgent wires the scripted provider into a working agent whose
// probe tool returns a result big enough to matter, one round at a time.
func newTurnCompactionAgent(t *testing.T, prov *turnCompactionProvider) *Agent {
	t.Helper()
	a, _ := newGateAgent(t)
	a.provider = prov
	// Compaction writes its pre-compaction transcript under the agent's home
	// (writeHistoryLog). The gate agent has none, which would drop memory/logs
	// into the package directory.
	a.homePath = t.TempDir()
	// The script needs more rounds than a default turn allows: the fixture is
	// not about the iteration cap.
	a.maxToolIterations = turnCompactionRounds + 5
	a.maxToolContinues = 0
	a.registry.Register(turnCompactionProbeTool, "test tool", nil, func(_ context.Context, args json.RawMessage) (string, error) {
		return strings.Repeat("x", turnCompactionResultLen) + "|" + string(args), nil
	})
	return a
}

// assertTheTurnCompactedItsOwnPrompt is the shared body of the two witnesses
// below: one for the non-streaming loop, one for the streaming loop, because
// the round boundary that grows the prompt exists in both.
func assertTheTurnCompactedItsOwnPrompt(t *testing.T, prov *turnCompactionProvider) {
	t.Helper()
	prompts := prov.prompts()
	if len(prompts) == 0 {
		t.Fatal("the turn never reached a provider")
	}

	// 1. Some later round saw a history where an older tool result had been
	//    replaced. Before the round boundary re-checked the prompt, no call
	//    ever carried this marker: the only compaction ran before the turn's
	//    first round, on a history that had not grown yet.
	sawDropped := false
	for _, p := range prompts {
		if strings.Contains(promptText(p), turnCompactionDropped) {
			sawDropped = true
			break
		}
	}
	if !sawDropped {
		t.Errorf("no model call saw %q: the turn grew to %d messages across %d rounds and was never re-compacted mid-turn",
			turnCompactionDropped, len(prompts[len(prompts)-1]), len(prompts))
	}

	// 2. Compaction must not eat the work in flight: the newest tool result is
	//    what the next round was asked to build on.
	last := prompts[len(prompts)-1]
	newest := fmt.Sprintf("|{\"n\":\"%d\"}", turnCompactionRounds)
	if !strings.Contains(promptText(last), newest) {
		t.Errorf("the last prompt lost the newest tool result (%s)", newest)
	}

	// 3. And the growth has to actually stop: the prompt the turn ends on is
	//    smaller than the largest one it sent.
	biggest := 0
	for _, p := range prompts {
		if n := EstimateTokens(p); n > biggest {
			biggest = n
		}
	}
	if got := EstimateTokens(last); got >= biggest {
		t.Errorf("the prompt never shrank: last call %d tokens, largest call %d tokens", got, biggest)
	}
}

func TestALongTurnCompactsItsOwnPrompt(t *testing.T) {
	prov := &turnCompactionProvider{script: turnCompactionRounds}
	a := newTurnCompactionAgent(t, prov)

	msg := bus.InboundMessage{Channel: "web", UserID: "u_owner", ChatID: "chat-midturn", Text: "go"}
	a.HandleMessage(context.Background(), msg)

	assertTheTurnCompactedItsOwnPrompt(t, prov)
}

func TestAStreamingTurnCompactsItsOwnPromptToo(t *testing.T) {
	prov := &turnCompactionProvider{script: turnCompactionRounds}
	a := newTurnCompactionAgent(t, prov)

	msg := bus.InboundMessage{Channel: "web", UserID: "u_owner", ChatID: "chat-midturn-stream", Text: "go"}
	drainStream(a.HandleMessageStream(context.Background(), msg))

	assertTheTurnCompactedItsOwnPrompt(t, prov)
}
