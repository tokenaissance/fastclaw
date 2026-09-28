package tools

// ③a — the identity-file branch of the read-modify-write tools gets the precondition the workspace
// branch has had since putGuarded (§13.2 / row 83 of docs/fs-formal-proof/11-change-register.md).
//
// Why this branch and not the model's hands: edit_file and apply_patch read the file inside their own
// call, so the fact they must present — the bytes they read — is already theirs. write_file is a
// blind replacement by design and keeps overwriting; the third test pins that boundary so the guard
// cannot leak into it.
//
// Falsification: make edit_file's identity branch call SaveWorkspaceFile instead of
// SaveWorkspaceFileIfUnchanged and test 1 reddens — the competitor's version is replaced and the tool
// says "Edited …", which is the false σ this change removes.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// competingStore is a SystemFileStore that lets a second writer land in the window between the
// tool's read and its write — the only window a precondition exists for.
type competingStore struct {
	rows map[string]string
	// onRead runs once, after the row has been handed to the reader: the place a test puts "somebody
	// else wrote first".
	onRead func()

	guarded int
	blind   int
}

func newCompetingStore(rows map[string]string) *competingStore {
	return &competingStore{rows: rows}
}

func (c *competingStore) key(userID, filename string) string { return userID + "/" + filename }

func (c *competingStore) read(userID, filename string) ([]byte, error) {
	cur := c.rows[c.key(userID, filename)]
	if c.onRead != nil {
		hook := c.onRead
		c.onRead = nil
		hook()
	}
	if cur == "" {
		return nil, store.ErrNotFound
	}
	return []byte(cur), nil
}

func (c *competingStore) GetWorkspaceFile(_ context.Context, _, userID, filename string) ([]byte, error) {
	return c.read(userID, filename)
}

func (c *competingStore) GetWorkspaceFileExact(_ context.Context, _, userID, filename string) ([]byte, error) {
	return c.read(userID, filename)
}

func (c *competingStore) SaveWorkspaceFile(_ context.Context, _, userID, filename string, data []byte) error {
	c.blind++
	c.rows[c.key(userID, filename)] = string(data)
	return nil
}

// SaveWorkspaceFileIfUnchanged is the store's rule, not a stricter reading of it: a row that exists
// and holds something else is a conflict; an absent row makes this a create.
func (c *competingStore) SaveWorkspaceFileIfUnchanged(_ context.Context, _, userID, filename string, data []byte, expected string) error {
	c.guarded++
	if cur, ok := c.rows[c.key(userID, filename)]; ok && cur != expected {
		return store.ErrAgentFileConflict
	}
	c.rows[c.key(userID, filename)] = string(data)
	return nil
}

func writeArgs(t *testing.T, path, content string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"path": path, "content": content})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The witness: somebody else's write lands between the read and the write, and the tool must refuse
// rather than report "Edited" over a version it never saw.
func TestEditFileRefusesAnIdentityFileThatMovedUnderIt(t *testing.T) {
	const competitor = "## 事实\n- 同行的那个人写的\n"
	st := newCompetingStore(map[string]string{"u_owner/MEMORY.md": "## 事实\n- 原来那条\n"})
	st.onRead = func() { st.rows["u_owner/MEMORY.md"] = competitor }
	r := newSystemFileRegistry(st, t.TempDir())
	r.chatterUserID = "u_owner" // the agent's own account: per-user file reads use its row

	out, err := r.Execute(context.Background(), "edit_file",
		editArgs(t, "MEMORY.md", "原来那条", "我改的那条"))
	if err == nil {
		t.Fatalf("the edit reported success over somebody else's version: %q", out)
	}
	if !strings.Contains(err.Error(), "another writer changed") {
		t.Fatalf("the refusal does not tell the model what happened: %v", err)
	}
	if got := st.rows["u_owner/MEMORY.md"]; got != competitor {
		t.Fatalf("the competitor's version was replaced:\n got %q\nwant %q", got, competitor)
	}
	if st.guarded == 0 {
		t.Fatal("the write did not carry a precondition; this witness would be vacuous")
	}
	if st.blind != 0 {
		t.Fatal("the identity-file branch used the unconditional write")
	}
}

// Positive control: with nobody else writing, the same edit still lands. Without it a guard that
// refused everything would pass the witness above.
func TestEditFileStillLandsWhenNobodyElseWrote(t *testing.T) {
	st := newCompetingStore(map[string]string{"u_owner/MEMORY.md": "## 事实\n- 原来那条\n"})
	r := newSystemFileRegistry(st, t.TempDir())
	r.chatterUserID = "u_owner"

	out, err := r.Execute(context.Background(), "edit_file",
		editArgs(t, "MEMORY.md", "原来那条", "我改的那条"))
	if err != nil {
		t.Fatalf("a lone edit was refused: %v", err)
	}
	if !strings.Contains(out, "Edited") {
		t.Fatalf("the edit did not report success: %q", out)
	}
	if got := st.rows["u_owner/MEMORY.md"]; !strings.Contains(got, "我改的那条") {
		t.Fatalf("the edit did not land: %q", got)
	}
}

// apply_patch reads the pre-image and hands it to the writer, so it takes the same guard. This case
// exists because the two call sites are easy to fix one at a time.
func TestApplyPatchRefusesAnIdentityFileThatMovedUnderIt(t *testing.T) {
	const competitor = "## 事实\n- 同行的那个人写的\n"
	st := newCompetingStore(map[string]string{"u_owner/MEMORY.md": "## 事实\n- 原来那条\n"})
	st.onRead = func() { st.rows["u_owner/MEMORY.md"] = competitor }
	r := newSystemFileRegistry(st, t.TempDir())
	r.chatterUserID = "u_owner"

	patch := "*** Begin Patch\n*** Update File: MEMORY.md\n@@\n-## 事实\n-- 原来那条\n+## 事实\n+- 我改的那条\n*** End Patch\n"
	args, _ := json.Marshal(map[string]any{"input": patch})
	out, err := r.Execute(context.Background(), "apply_patch", string(args))
	if err == nil && !strings.Contains(out, "another writer changed") {
		t.Fatalf("the patch reported success over somebody else's version: %q", out)
	}
	if got := st.rows["u_owner/MEMORY.md"]; got != competitor {
		t.Fatalf("the competitor's version was replaced:\n got %q\nwant %q", got, competitor)
	}
	if st.blind != 0 {
		t.Fatal("apply_patch's identity-file branch used the unconditional write")
	}
}

// The boundary: write_file is a replacement, and a replacement does not present a precondition.
// Worse than unnecessary, a guard here would refuse exactly what the tool promises to do.
func TestWriteFileStillReplacesAnIdentityFile(t *testing.T) {
	st := newCompetingStore(map[string]string{"u_owner/MEMORY.md": "## 事实\n- 原来那条\n"})
	r := newSystemFileRegistry(st, t.TempDir())
	r.chatterUserID = "u_owner"

	out, err := r.Execute(context.Background(), "write_file",
		writeArgs(t, "MEMORY.md", "## 事实\n- 整份替换\n"))
	if err != nil {
		t.Fatalf("write_file was refused: %v (out=%q)", err, out)
	}
	if got := st.rows["u_owner/MEMORY.md"]; got != "## 事实\n- 整份替换\n" {
		t.Fatalf("write_file did not replace the row: %q", got)
	}
	if st.guarded != 0 {
		t.Fatal("write_file grew a precondition; its semantics are 'this file is now this'")
	}
}

// A store that cannot tell is not a store that agreed: the conflict must be identified by value, not
// by the shape of the message.
func TestTheConflictIsIdentifiedByValue(t *testing.T) {
	st := newCompetingStore(map[string]string{"u_owner/MEMORY.md": "a"})
	err := st.SaveWorkspaceFileIfUnchanged(context.Background(), "agt_1", "u_owner", "MEMORY.md", []byte("b"), "c")
	if !errors.Is(err, store.ErrAgentFileConflict) {
		t.Fatalf("the fake's refusal is not the store's sentinel: %v", err)
	}
}
