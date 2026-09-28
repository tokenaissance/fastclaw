package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// missingRowStore answers every read with store.ErrNotFound — the state of a
// chatter whose MEMORY.md / USER.md row has never been written, and of a file
// that only ever existed on disk.
type missingRowStore struct {
	saved map[string]string
}

func newMissingRowStore() *missingRowStore {
	return &missingRowStore{saved: map[string]string{}}
}

func (m *missingRowStore) GetWorkspaceFile(context.Context, string, string, string) ([]byte, error) {
	return nil, store.ErrNotFound
}

func (m *missingRowStore) GetWorkspaceFileExact(context.Context, string, string, string) ([]byte, error) {
	return nil, store.ErrNotFound
}

func (m *missingRowStore) SaveWorkspaceFile(_ context.Context, _ /* agentID */, userID, filename string, data []byte) error {
	m.saved[userID+"/"+filename] = string(data)
	return nil
}

// SaveWorkspaceFileIfUnchanged enforces the precondition the way the store does, so a witness built
// on this fake is a witness about the rule rather than about a stub that always says yes.
//
// The shape is the store's SQL, not a stricter reading of it: a row that EXISTS and holds something
// else is a conflict, while an absent row makes this a create — the caller's base may have come from
// the disk copy (`readSystemFileWithFallback`), so "expected" is a statement about the row only when
// there is one. Pinned for the real store in agent_file_version_test.go.
func (m *missingRowStore) SaveWorkspaceFileIfUnchanged(_ context.Context, _ /* agentID */, userID, filename string, data []byte, expected string) error {
	if cur, ok := m.saved[userID+"/"+filename]; ok && cur != expected {
		return store.ErrAgentFileConflict
	}
	m.saved[userID+"/"+filename] = string(data)
	return nil
}

func newSystemFileRegistry(st SystemFileStore, systemRoot string) *Registry {
	r := NewRegistry(systemRoot, "") // file tools + a live tools map
	r.systemFileStore = st
	r.agentID = "agt_1"
	r.userID = "u_owner"
	r.chatterUserID = "u_chatter"
	r.agentOwnerUserID = "u_owner"
	return r
}

func editArgs(t *testing.T, path, oldS, newS string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"path": path, "old_string": oldS, "new_string": newS})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Reported symptom: the model edits MEMORY.md (content it read from disk) and
// the tool answers "system file get: store: not found" — a storage-internal
// string, because edit_file read the DB row directly while read_file falls back
// to the agent's systemRoot on disk.
func TestEditFileMissingMemoryRowDoesNotLeakStoreError(t *testing.T) {
	r := newSystemFileRegistry(newMissingRowStore(), t.TempDir())

	out, err := r.Execute(context.Background(), "edit_file",
		editArgs(t, "MEMORY.md", "- 旧事实", "- 新事实"))

	if err != nil && strings.Contains(err.Error(), "store: not found") {
		t.Fatalf("edit_file leaked the raw store error: %v", err)
	}
	// Nothing exists to edit, so an actionable failure is right — it just must
	// name the file instead of the storage layer.
	if err == nil {
		t.Fatalf("expected an actionable old_string error, got success: %q", out)
	}
	if !strings.Contains(err.Error(), "MEMORY.md") {
		t.Errorf("error should name the file so the model can re-read it, got: %v", err)
	}
}

// The agent's own account edits a MEMORY.md that only exists on its home
// directory (no row yet — e.g. an agent whose row was never written). The disk
// copy is the base, and the result lands in the store.
func TestEditFileOwnerUsesDiskBaseWhenRowIsMissing(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "MEMORY.md")
	if err := os.WriteFile(disk, []byte("## 事实\n- 剔除稳定币 (USDC-USDT)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := newMissingRowStore()
	r := newSystemFileRegistry(st, dir)
	r.chatterUserID = "u_owner" // the agent's own account

	out, err := r.Execute(context.Background(), "edit_file",
		editArgs(t, "MEMORY.md", "USDC-USDT", "USDC-USDT、XAUT-USDT"))
	if err != nil {
		t.Fatalf("edit_file on a disk-only MEMORY.md: %v", err)
	}
	if !strings.Contains(out, "Edited") {
		t.Errorf("unexpected tool output: %q", out)
	}
	if got := st.saved["u_owner/MEMORY.md"]; got != "## 事实\n- 剔除稳定币 (USDC-USDT、XAUT-USDT)\n" {
		t.Errorf("saved content = %q, want the edited disk content", got)
	}
}

// Per-chatter files are private: systemRoot/MEMORY.md is ONE un-scoped mirror
// per agent (whoever wrote last owns its bytes), so a visitor whose own row
// doesn't exist yet must NOT inherit it — neither through read_file nor as the
// base of an edit. Mirrors ContextBuilder.loadFileForUser /
// memory_store_adapter.GetMemory, which never inherit for USER.md / MEMORY.md.
func TestPerChatterFileNeverInheritsTheOwnersDiskCopy(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("disk memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := newMissingRowStore()
	r := newSystemFileRegistry(st, dir) // chatterUserID = u_chatter

	out, err := r.Execute(context.Background(), "read_file", `{"path":"MEMORY.md"}`)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if strings.Contains(out, "disk memory") {
		t.Errorf("visitor read the owner's disk MEMORY.md: %q", out)
	}

	if _, err := r.Execute(context.Background(), "edit_file",
		editArgs(t, "MEMORY.md", "disk memory", "leaked")); err == nil {
		t.Error("edit_file used the owner's disk MEMORY.md as its base")
	}
	if len(st.saved) != 0 {
		t.Errorf("visitor wrote rows: %v", st.saved)
	}
}

// The agent's own account still reads its home-directory copy — that is the
// legacy layout (identity files live on disk) and the per-write mirror.
func TestOwnerReadsDiskCopyWhenRowIsMissing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("disk memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := newSystemFileRegistry(newMissingRowStore(), dir)
	r.chatterUserID = "u_owner"

	out, err := r.Execute(context.Background(), "read_file", `{"path":"MEMORY.md"}`)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if !strings.Contains(out, "disk memory") {
		t.Errorf("owner read_file = %q, want the disk content", out)
	}
}

// Shared identity files ARE the agent's template: the overlay hands a chatter
// the owner's row, so the disk copy stays readable too — otherwise an agent
// whose SOUL.md only ever lived on disk would lose its persona for the
// operator (and any admin-run channel). Only per-chatter files are gated on
// ownership.
func TestSharedIdentityFileFallsBackToDiskForNonOwnerCaller(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("# SOUL\n- warm\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := newSystemFileRegistry(newMissingRowStore(), dir) // chatterUserID = u_chatter
	r.SetCallerIsAdmin(true)                              // the caller allowed to touch identity files

	out, err := r.Execute(context.Background(), "read_file", `{"path":"SOUL.md"}`)
	if err != nil {
		t.Fatalf("read_file SOUL.md: %v", err)
	}
	if !strings.Contains(out, "warm") {
		t.Errorf("read_file SOUL.md = %q, want the shared template", out)
	}
}
