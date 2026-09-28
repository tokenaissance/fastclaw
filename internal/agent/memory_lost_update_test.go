package agent

// The lost update the identity files were open to, and the belt that closes it
// (docs/fs-formal-proof/11-change-register.md §13.2: five writers, one key, no
// shared precondition).
//
// The run against prod on 2026-09-28 found the shape: a turn's own `write_file`
// lands in MEMORY.md, and the PostTurn distiller — which read MEMORY.md *before*
// that tool call — writes its own append back on top, putting the pre-write
// content in place and silently undone-ing the model's edit.
//
// This runs the real store and the real adapter: a witness built on a stub that
// always accepts the precondition would only be a witness about the stub.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/provider"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// midTurnWriterProvider does what the model's file tool does: it writes one of the
// identity files through the same adapter while the turn is still running, then
// answers the distillation prompt. `filename` empty means "write nothing" — the
// positive control.
type midTurnWriterProvider struct {
	adapter  *MemoryStoreAdapter
	agentID  string
	userID   string
	filename string
	write    string
	reply    string
}

func (p *midTurnWriterProvider) Chat(ctx context.Context, _ []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.Response, error) {
	if p.filename != "" {
		if err := p.adapter.SaveWorkspaceFile(ctx, p.agentID, p.userID, p.filename, []byte(p.write)); err != nil {
			return nil, err
		}
	}
	return &provider.Response{Content: p.reply}, nil
}

func (p *midTurnWriterProvider) ChatStream(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.StreamReader, error) {
	return nil, errors.New("not used")
}

func newMemoryStoreHarness(t *testing.T) (*store.DBStore, *MemoryStoreAdapter, *Memory) {
	t.Helper()
	db, err := store.NewDBStore("sqlite", "file:"+t.TempDir()+"/memory.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	adapter := NewMemoryStoreAdapter(db)
	mem := NewMemoryWithStoreForUser(t.TempDir(), adapter, "u_lost", "agt_lost")
	return db, adapter, mem
}

// The witness: the distiller must refuse rather than put its pre-write copy back.
func TestTheDistillerRefusesToUndoAWriteItDidNotSee(t *testing.T) {
	ctx := context.Background()
	_, adapter, mem := newMemoryStoreHarness(t)
	if err := adapter.SaveMemory(ctx, "agt_lost", "u_lost", "seed\n"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const written = "seed\n- the model wrote this during the turn\n"
	prov := &midTurnWriterProvider{
		adapter: adapter, agentID: "agt_lost", userID: "u_lost",
		filename: memoryFilename, write: written,
		reply: `{"memory_facts": ["the user's timezone is Asia/Shanghai"], "user_notes": []}`,
	}

	// The distiller reads MEMORY.md, then (in the same turn, through the provider)
	// the file tool writes it, then the distiller writes its append back.
	AutoPersistMemory(ctx, mem, prov, "fake-model", []provider.Message{
		{Role: "user", Content: "remember my timezone"},
		{Role: "assistant", Content: "noted"},
	})

	got, err := adapter.GetMemory(ctx, "agt_lost", "u_lost")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got != written {
		t.Fatalf("the model's write did not survive the distiller:\n got %q\nwant %q", got, written)
	}
}

// The positive control: with nobody else writing, the same call must still land.
// Without this, a guard that refused everything would pass the witness above.
func TestTheDistillerStillLandsWhenNobodyElseWrote(t *testing.T) {
	ctx := context.Background()
	_, adapter, mem := newMemoryStoreHarness(t)
	if err := adapter.SaveMemory(ctx, "agt_lost", "u_lost", "seed\n"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	prov := &midTurnWriterProvider{
		adapter: adapter, agentID: "agt_lost", userID: "u_lost",
		reply: `{"memory_facts": ["the user's timezone is Asia/Shanghai"], "user_notes": []}`,
	}
	AutoPersistMemory(ctx, mem, prov, "fake-model", []provider.Message{
		{Role: "user", Content: "remember my timezone"},
		{Role: "assistant", Content: "noted"},
	})

	got, err := adapter.GetMemory(ctx, "agt_lost", "u_lost")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.HasPrefix(got, "seed\n") || !strings.Contains(got, "Asia/Shanghai") {
		t.Fatalf("the distillation did not land:\n%q", got)
	}
}

// The same rule on USER.md: it is a second key the distiller writes in the same
// call, and giving a precondition to only one of them would leave the other open.
func TestTheDistillerRefusesToUndoAUserFileWriteItDidNotSee(t *testing.T) {
	ctx := context.Background()
	_, adapter, mem := newMemoryStoreHarness(t)
	if err := adapter.SaveWorkspaceFile(ctx, "agt_lost", "u_lost", "USER.md", []byte("seed profile\n")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const written = "seed profile\n- the model wrote this during the turn\n"
	writer := &midTurnWriterProvider{
		adapter: adapter, agentID: "agt_lost", userID: "u_lost",
		filename: "USER.md", write: written,
		reply: `{"memory_facts": [], "user_notes": ["the user goes by Rei"]}`,
	}

	AutoPersistMemory(ctx, mem, writer, "fake-model", []provider.Message{
		{Role: "user", Content: "call me by my nickname"},
	})

	got, err := adapter.GetWorkspaceFileExact(ctx, "agt_lost", "u_lost", "USER.md")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != written {
		t.Fatalf("the model's USER.md write did not survive the distiller:\n got %q\nwant %q", got, written)
	}
}
