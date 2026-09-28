package tools

// The un-scoped home (register §13.2 conclusion 2, row 86): systemRoot/MEMORY.md is ONE file per
// agent with no (agent,user) dimension, while the store row it shadows is per-chatter.
//
// The READ side has been owner-gated since the isolation fix, so a visitor could never read it. The
// MIRROR had no such gate — a visitor's MEMORY.md write landed in the owner's file on disk, and the
// owner's next disk-fallback read (an owner whose store row does not exist yet) answered with the
// visitor's memory. Owner-only is now true on both sides; retiring or migrating the home stays a
// separate, still-open decision (recorded in the register).
//
// Falsification: drop the `ownsAgentHome` check from mirrorToAgentHome ⇒ the first case reddens with
// the visitor's bytes sitting in the agent home.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readHome(t *testing.T, dir, name string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data), true
}

func TestTheAgentHomeMirrorIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	st := newCompetingStore(map[string]string{})
	r := newSystemFileRegistry(st, dir) // chatter = u_chatter, agent owner = u_owner
	ctx := context.Background()

	// A visitor writes their own MEMORY.md: the row must land under their id, and the agent home must
	// not learn about it.
	if _, err := r.Execute(ctx, "write_file", writeArgs(t, "MEMORY.md", "visitor memory")); err != nil {
		t.Fatalf("visitor write: %v", err)
	}
	if got := st.rows["u_chatter/MEMORY.md"]; got != "visitor memory" {
		t.Fatalf("the visitor's row did not receive the write: %q", got)
	}
	if body, ok := readHome(t, dir, "MEMORY.md"); ok {
		t.Fatalf("a visitor's write landed in the agent home, where the owner's disk-fallback read would find it: %q", body)
	}

	// …and edit_file's mirror takes the same gate (the two call sites are easy to fix one at a time).
	if _, err := r.Execute(ctx, "edit_file", editArgs(t, "MEMORY.md", "visitor memory", "visitor memory v2")); err != nil {
		t.Fatalf("visitor edit: %v", err)
	}
	if body, ok := readHome(t, dir, "MEMORY.md"); ok {
		t.Fatalf("a visitor's edit landed in the agent home: %q", body)
	}
	if got := st.rows["u_chatter/MEMORY.md"]; got != "visitor memory v2" {
		t.Fatalf("the visitor's edit did not reach their row: %q", got)
	}

	// Positive control: the owner's own write still mirrors, so this pod's in-process readers keep
	// seeing what the owner just saved.
	r.chatterUserID = "u_owner"
	if _, err := r.Execute(ctx, "write_file", writeArgs(t, "MEMORY.md", "owner memory")); err != nil {
		t.Fatalf("owner write: %v", err)
	}
	if body, ok := readHome(t, dir, "MEMORY.md"); !ok || body != "owner memory" {
		t.Fatalf("the owner's write did not mirror (ok=%v body=%q)", ok, body)
	}
}

// The identity files keep their old behaviour: they are owner-scoped by construction
// (`systemFileUserID` maps them to the owner), so their mirror is unaffected by the gate — and a
// visitor's attempt never reaches the mirror at all (the identity gate refuses it first).
func TestIdentityFileMirrorsStillWorkForTheOwner(t *testing.T) {
	dir := t.TempDir()
	st := newCompetingStore(map[string]string{})
	r := newSystemFileRegistry(st, dir)
	r.callerIsAdmin = true // the owner path: admin/owner may write identity files
	ctx := context.Background()

	if _, err := r.Execute(ctx, "write_file", writeArgs(t, "SOUL.md", "我是谁")); err != nil {
		t.Fatalf("owner identity write: %v", err)
	}
	if body, ok := readHome(t, dir, "SOUL.md"); !ok || !strings.Contains(body, "我是谁") {
		t.Fatalf("an identity file stopped mirroring to the home (ok=%v body=%q)", ok, body)
	}
}
