package agent

import (
	"context"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// TestMemory_EmptyUserIDFailClosedE2E covers upstream commit 1d8a8b1
// (local ef9e266): NewMemoryWithStoreForUser no longer panics on an empty
// userID — it logs and keeps the Memory alive. Against a real DB store the
// empty owner must fail closed: reads return empty and writes error out
// BEFORE touching the store, so an unscoped Memory can neither read nor
// overwrite a real user's MEMORY.md / USER.md.
func TestMemory_EmptyUserIDFailClosedE2E(t *testing.T) {
	db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()

	const (
		agentID = "mem_e2e_agent"
		realID  = "mem_e2e_real"
	)
	if err := db.CreateUser(ctx, &store.UserRecord{
		ID: realID, Username: realID, Email: realID + "@example.com",
		Role: "user", Status: "active", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	adapter := NewMemoryStoreAdapter(db)

	// Seed a real user's MEMORY.md + USER.md so we can prove the
	// empty-owner Memory never reaches them.
	if err := adapter.SaveMemory(ctx, agentID, realID, "real-user-secret"); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	if err := adapter.SaveWorkspaceFile(ctx, agentID, realID, "USER.md", []byte("real-user-profile")); err != nil {
		t.Fatalf("seed user file: %v", err)
	}

	// Empty userID: the constructor must NOT panic.
	mem := NewMemoryWithStoreForUser(t.TempDir(), adapter, "", agentID)
	if mem == nil {
		t.Fatal("expected memory for empty userID")
	}

	// Fail-closed reads: must NOT fall through to the real user's rows.
	if got := mem.LoadMemory(); got != "" {
		t.Fatalf("LoadMemory() = %q, want empty (leaked real user's MEMORY.md)", got)
	}
	if got := mem.LoadUserFile(); got != "" {
		t.Fatalf("LoadUserFile() = %q, want empty (leaked real user's USER.md)", got)
	}

	// Fail-closed writes: error out before the store, so the real user's
	// rows stay untouched.
	if err := mem.SaveMemory("overwrite attempt"); err == nil {
		t.Fatal("SaveMemory() error = nil, want error on empty userID")
	}
	if err := mem.SaveUserFile("overwrite attempt"); err == nil {
		t.Fatal("SaveUserFile() error = nil, want error on empty userID")
	}

	// Prove the guards fired: the real user's rows are intact.
	if got, err := adapter.GetMemory(ctx, agentID, realID); err != nil || got != "real-user-secret" {
		t.Fatalf("real user MEMORY.md after empty-owner writes = %q err=%v, want intact", got, err)
	}
	if got, err := adapter.GetWorkspaceFileExact(ctx, agentID, realID, "USER.md"); err != nil || string(got) != "real-user-profile" {
		t.Fatalf("real user USER.md after empty-owner writes = %q err=%v, want intact", got, err)
	}

	// Sanity: a Memory rebound to the real user still reads/writes its own
	// rows through the same adapter.
	bound := mem.WithUserID(realID)
	if got := bound.LoadMemory(); got != "real-user-secret" {
		t.Fatalf("bound LoadMemory() = %q, want real-user-secret", got)
	}
	if err := bound.SaveMemory("updated"); err != nil {
		t.Fatalf("bound SaveMemory() error = %v", err)
	}
	if got, _ := adapter.GetMemory(ctx, agentID, realID); got != "updated" {
		t.Fatalf("bound SaveMemory didn't persist, got %q", got)
	}
}
