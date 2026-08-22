package tools

// E2E coverage for upstream commit 583a1a7 (local c488a8e)
// "feat(security): hard-block SKILL.md exfiltration via tools, sandbox,
// and downloads".
//
// This file covers the TOOLS-layer block (a): the host-file route
// (read_file / edit_file) must refuse a bundled skill's SKILL.md for a
// non-admin chatter — the sibling of the identity-file (SOUL.md) gate —
// while leaving write_file open so a chatter can still author their OWN
// skills. The gate is exercised through the real tool execution path
// (Registry.Execute) so it covers the wiring in file.go, not just the
// predicate.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecurityTools_ReadFileRefusesBundledSkillManifest drives read_file
// through the real tool and asserts a non-admin chatter gets the
// SkillManifestRefusal for every protected shape: the absolute sandbox-mount
// path (`read_file("/skills/foo/SKILL.md")`) and the relative agent-home
// skill path when no per-user bucket is configured.
func TestSecurityTools_ReadFileRefusesBundledSkillManifest(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(dir, dir) // callerIsAdmin defaults to false (fail-closed)

	cases := []struct{ name, path string }{
		{"absolute sandbox mount", "/skills/foo/SKILL.md"},
		{"nested absolute mount", "/var/lib/skills/foo/SKILL.md"},
		{"relative agent-home skill", "skills/foo/SKILL.md"},
	}
	for _, c := range cases {
		res, err := r.Execute(context.Background(), "read_file",
			fmt.Sprintf(`{"path":%q}`, c.path))
		if err != nil {
			t.Fatalf("%s: read_file err = %v", c.name, err)
		}
		if res != SkillManifestRefusal {
			t.Errorf("%s: read_file = %q, want SkillManifestRefusal", c.name, res)
		}
	}
}

// TestSecurityTools_EditFileRefusesBundledSkillManifest asserts the edit
// path is gated the same way as read — editing returns surrounding content
// and would let a chatter tamper with the agent's IP.
func TestSecurityTools_EditFileRefusesBundledSkillManifest(t *testing.T) {
	r := NewRegistry(t.TempDir(), t.TempDir())
	res, err := r.Execute(context.Background(), "edit_file",
		`{"path":"/skills/foo/SKILL.md","old_string":"a","new_string":"b"}`)
	if err != nil {
		t.Fatalf("edit_file err = %v", err)
	}
	if res != SkillManifestRefusal {
		t.Errorf("edit_file = %q, want SkillManifestRefusal", res)
	}
}

// TestSecurityTools_AdminSkipsSkillManifestGate: the owner / channel admin
// legitimately maintains skills, so the gate must not fire for them. The
// read then fails only because the manifest doesn't exist on this host —
// an os error, never a gate refusal.
func TestSecurityTools_AdminSkipsSkillManifestGate(t *testing.T) {
	r := NewRegistry(t.TempDir(), t.TempDir())
	r.SetCallerIsAdmin(true)
	res, err := r.Execute(context.Background(), "read_file", `{"path":"/skills/foo/SKILL.md"}`)
	if err == nil {
		t.Fatalf("admin read of a missing manifest = %q, want an os error (file absent, gate must stay silent)", res)
	}
	if res == SkillManifestRefusal || strings.HasPrefix(res, "[refused:") {
		t.Errorf("admin must not be gate-refused, got %q", res)
	}
}

// TestSecurityTools_ReadFileOwnSkillManifestAllowed: with a per-user skills
// bucket configured, the same relative path resolves to the chatter's OWN
// skill — their content, not the agent's IP — so it stays readable.
func TestSecurityTools_ReadFileOwnSkillManifestAllowed(t *testing.T) {
	userSkills := t.TempDir()
	r := NewRegistry(t.TempDir(), t.TempDir())
	r.SetUserSkillsRoot(userSkills)

	full := filepath.Join(userSkills, "skills", "my-skill", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir own skill: %v", err)
	}
	if err := os.WriteFile(full, []byte("my own skill"), 0o644); err != nil {
		t.Fatalf("write own skill: %v", err)
	}

	res, err := r.Execute(context.Background(), "read_file", `{"path":"skills/my-skill/SKILL.md"}`)
	if err != nil {
		t.Fatalf("read own skill manifest: %v", err)
	}
	if strings.HasPrefix(res, "[refused:") {
		t.Errorf("chatter's own skill manifest must not be refused, got %q", res)
	}
	if res != "my own skill" {
		t.Errorf("read own skill = %q, want %q", res, "my own skill")
	}
}

// TestSecurityTools_WorkspaceSkillManifestNotGated: a SKILL.md the chatter
// authored in /workspace is NOT protected — only a bundled skill's manifest
// is the agent's IP. The read fails only because the host file doesn't
// exist, never with a gate refusal.
func TestSecurityTools_WorkspaceSkillManifestNotGated(t *testing.T) {
	r := NewRegistry(t.TempDir(), t.TempDir())
	res, err := r.Execute(context.Background(), "read_file", `{"path":"/workspace/SKILL.md"}`)
	if err == nil {
		t.Fatalf("read of missing /workspace/SKILL.md = %q, want an os error", res)
	}
	if res == SkillManifestRefusal || strings.HasPrefix(res, "[refused:") {
		t.Errorf("chatter-authored workspace SKILL.md must not be gate-refused, got %q", res)
	}
}

// TestSecurityTools_WriteFileStaysOpen: write_file is deliberately NOT
// gated — a chatter authors their own skills via the skill-creator flow,
// which lands in the per-user bucket. Assert the write succeeds and the
// file actually lands on disk.
func TestSecurityTools_WriteFileStaysOpen(t *testing.T) {
	userSkills := t.TempDir()
	r := NewRegistry(t.TempDir(), t.TempDir())
	r.SetUserSkillsRoot(userSkills)

	res, err := r.Execute(context.Background(), "write_file",
		`{"path":"skills/my-skill/SKILL.md","content":"chatter authored"}`)
	if err != nil {
		t.Fatalf("write own skill manifest: %v", err)
	}
	if strings.HasPrefix(res, "[refused:") {
		t.Errorf("write_file must stay open for the chatter's own skill, got %q", res)
	}
	full := filepath.Join(userSkills, "skills", "my-skill", "SKILL.md")
	if data, err := os.ReadFile(full); err != nil || string(data) != "chatter authored" {
		t.Errorf("own skill not written: data=%q err=%v", data, err)
	}
}

// TestSecurityTools_IsProtectedSkillManifestPath pins down the pure path
// classifier behind the gate (the "blocked" decision also factors in
// callerIsAdmin, which the existing skill_manifest_gate_test covers). Note
// the classifier is case-sensitive ("skill.md" is NOT the manifest), while
// the download endpoint uses EqualFold — an intentional asymmetry: the tool
// gate mirrors the file system's exact filename, the download guard is
// defense-in-depth.
func TestSecurityTools_IsProtectedSkillManifestPath(t *testing.T) {
	cases := []struct {
		name           string
		path           string
		userSkillsRoot string
		want           bool
	}{
		{"empty", "", "", false},
		{"absolute mount manifest", "/skills/foo/SKILL.md", "", true},
		{"nested absolute mount manifest", "/var/x/skills/foo/SKILL.md", "", true},
		{"absolute skill script", "/skills/foo/main.py", "", false},
		{"absolute workspace lookalike", "/workspace/SKILL.md", "", false},
		{"relative agent-home manifest", "skills/foo/SKILL.md", "", true},
		{"relative own-skill manifest", "skills/foo/SKILL.md", "/home/u/skills", false},
		{"nested relative note", "notes/SKILL.md", "", false},
		{"lowercase manifest", "/skills/foo/skill.md", "", false},
		{"bare skills dir", "skills", "", false},
	}
	for _, c := range cases {
		r := &Registry{userSkillsRoot: c.userSkillsRoot}
		if got := r.isProtectedSkillManifestPath(c.path); got != c.want {
			t.Errorf("%s: isProtectedSkillManifestPath(%q) = %v, want %v",
				c.name, c.path, got, c.want)
		}
	}
}
