package agent

// The distiller's prompt is built by truncating three strings — the last 20
// messages' bodies at 300, and the current MEMORY.md / USER.md at 500 — and both
// cuts used to be byte slicing. Neither number is a rune boundary, and the file
// this agent writes is Chinese: in UTF-8 a CJK rune is 3 bytes, so 500 = 3×166+2
// puts the cut one byte inside the 167th character, and 300 is only a boundary
// while every byte before it is ASCII.
//
// The failure is silent in the way that matters: the model receives a prompt
// with a split sequence and answers anyway, so nothing downstream can tell that
// the harness corrupted its own question. 0a5a6b5 ("rune-based truncation for
// ALL UTF-8 text fields") fixed the third truncation in the same file — the
// parse-failure log preview — and left the two that feed the model; the session
// half was pinned by internal/session/truncate_test.go and this half by nothing.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/fastclaw-ai/fastclaw/internal/provider"
)

// utf8PromptProbe is the delivery point: the exact bytes the distiller asked a
// provider to read.
type utf8PromptProbe struct {
	mu     sync.Mutex
	prompt string
}

func (p *utf8PromptProbe) Chat(_ context.Context, msgs []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.Response, error) {
	p.mu.Lock()
	for _, m := range msgs {
		p.prompt += m.Content
	}
	p.mu.Unlock()
	// Nothing to remember: this test is about the question, not the answer, and
	// an empty extraction keeps MEMORY.md / USER.md untouched.
	return &provider.Response{Content: `{"memory_facts":[],"user_notes":[]}`}, nil
}

func (p *utf8PromptProbe) ChatStream(_ context.Context, msgs []provider.Message, tools []provider.Tool, model string, maxTokens int, temperature float64) (*provider.StreamReader, error) {
	if _, err := p.Chat(context.Background(), msgs, tools, model, maxTokens, temperature); err != nil {
		return nil, err
	}
	ch := make(chan provider.StreamChunk, 1)
	ch <- provider.StreamChunk{Content: `{"memory_facts":[],"user_notes":[]}`, Done: true}
	close(ch)
	return provider.NewStreamReader(ch), nil
}

func (p *utf8PromptProbe) seen() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.prompt
}

func TestTheDistillerPromptSurvivesItsOwnTruncation(t *testing.T) {
	home := t.TempDir()
	mem := NewMemory(home)

	// A MEMORY.md the way this feature writes one: Chinese prose, well past the
	// 500-byte cut.
	const memoryHead = "记住用户要求所有回复都用中文，并且先给结论。"
	memory := "## 用户偏好\n" + strings.Repeat(memoryHead, 40)
	if err := mem.SaveMemoryWithScan(memory); err != nil {
		t.Fatalf("seed MEMORY.md: %v", err)
	}
	if len(memory) <= 500 {
		t.Fatalf("fixture is not long enough to be truncated: %d bytes", len(memory))
	}

	// The message body is cut at 300 bytes. One leading ASCII byte is enough to
	// put every later rune out of phase with that boundary.
	const saidHead = "调研沙箱权限问题，先看证据再下结论。"
	said := "I " + strings.Repeat(saidHead, 40)
	if len(said) <= 300 {
		t.Fatalf("fixture is not long enough to be truncated: %d bytes", len(said))
	}

	probe := &utf8PromptProbe{}
	AutoPersistMemory(context.Background(), mem, probe, "distiller",
		[]provider.Message{{Role: "user", Content: said}})

	prompt := probe.seen()
	if prompt == "" {
		t.Fatal("the distiller never reached a provider — nothing was measured")
	}
	// Not vacuous: both Chinese inputs have to be in the prompt for the
	// truncation paths to have run at all.
	if !strings.Contains(prompt, memoryHead) {
		t.Errorf("the truncated MEMORY.md is missing from the prompt")
	}
	if !strings.Contains(prompt, saidHead) {
		t.Errorf("the truncated message body is missing from the prompt")
	}

	if !utf8.ValidString(prompt) {
		at := firstInvalidByte(prompt)
		t.Errorf("the prompt handed to the distiller is not valid UTF-8 (first bad byte at %d): %q",
			at, prompt[max(0, at-12):min(len(prompt), at+12)])
	}
	if strings.ContainsRune(prompt, utf8.RuneError) {
		t.Errorf("the prompt handed to the distiller carries U+FFFD — a truncation cut a rune in half")
	}
}

// firstInvalidByte reports the offset of the first byte that does not start a
// well-formed sequence, or -1.
func firstInvalidByte(s string) int {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 {
			return i
		}
		i += size
	}
	return -1
}
