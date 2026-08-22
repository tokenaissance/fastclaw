package sandbox

// E2E coverage for upstream commit 583a1a7 (local c488a8e)
// "feat(security): hard-block SKILL.md exfiltration via tools, sandbox,
// and downloads".
//
// This file covers the SANDBOX-layer block (c): the executor must not
// ship a bundled skill's top-level SKILL.md into the container, because
// the model already has the manifest via load_skill and the sandbox only
// needs the executable scripts/resources to run `python /skills/<name>/main.py`.
// Without the exclusion, `cat /skills/<name>/SKILL.md` (or extracting it
// from the hydrate tar) would leak the agent's IP to the model.
//
// All three backends are exercised PURELY (no real containers / network):
//   - docker: the `appendSkillMounts` per-entry bind-mount builder
//   - e2b: the `tarBundle.addLocalDir` hydrate archive (gzip tar)
//   - boxlite: the `plainTarBundle.addLocalDir` hydrate archive (raw tar)
//
// The tools-layer (a) and setup/download-layer (b) blocks live in their
// own package test files (internal/agent/tools/security_e2e_test.go and
// internal/setup/security_e2e_test.go) because those predicates are
// unexported and must be asserted in-package.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// secWriteSkillTree seeds a bundled-skill-shaped directory: a top-level
// SKILL.md (the agent's IP), the executable main.py, an assets dir, and a
// nested sub dir that ALSO carries a SKILL.md — the nested one must
// survive (only the top-level manifest is the agent's IP), so the tests
// distinguish the two.
func secWriteSkillTree(t *testing.T, root string) {
	t.Helper()
	files := map[string]string{
		"SKILL.md":        "secret manifest",
		"main.py":         "print('hi')",
		"assets/logo.png": "png-bytes",
		"sub/SKILL.md":    "nested manifest",
		"sub/helper.py":   "helper",
	}
	for rel, content := range files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

// secTarEntryNames reads a (optionally gzipped) tar and returns the set
// of entry names, so tests can assert manifest absence/presence without
// caring about file contents.
func secTarEntryNames(t *testing.T, data []byte, gzipped bool) map[string]bool {
	t.Helper()
	r := bytes.NewReader(data)
	var tr *tar.Reader
	if gzipped {
		gr, err := gzip.NewReader(r)
		if err != nil {
			t.Fatalf("gunzip: %v", err)
		}
		defer gr.Close()
		tr = tar.NewReader(gr)
	} else {
		tr = tar.NewReader(r)
	}
	names := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		names[hdr.Name] = true
	}
	return names
}

// TestSecuritySandbox_AppendSkillMountsExcludesManifest: the docker
// backend mounts each top-level skill entry individually so the SKILL.md
// exclusion is a true physical absence — no nested over-mount tricks can
// resurrect it. Directories (assets, sub) mount as whole dirs.
func TestSecuritySandbox_AppendSkillMountsExcludesManifest(t *testing.T) {
	dir := t.TempDir()
	secWriteSkillTree(t, dir)

	args := appendSkillMounts(nil, dir, "/skills/foo")
	joined := strings.Join(args, " ")

	if strings.Contains(joined, "/skills/foo/SKILL.md") {
		t.Errorf("mount args leak top-level SKILL.md: %v", args)
	}
	for _, want := range []string{"/skills/foo/main.py", "/skills/foo/assets", "/skills/foo/sub"} {
		if !strings.Contains(joined, want) {
			t.Errorf("mount args missing %s: %v", want, args)
		}
	}
}

// TestSecuritySandbox_AppendSkillMountsFallbackWholeDir: when the skill
// dir can't be enumerated, appendSkillMounts falls back to a whole-dir
// read-only mount so the skill still works. That rare path doesn't hide
// the manifest, but a broken skill is worse — assert the fallback shape
// (`-v host:container:ro`) so the tradeoff is pinned down.
func TestSecuritySandbox_AppendSkillMountsFallbackWholeDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	args := appendSkillMounts(nil, missing, "/skills/foo")

	if len(args) != 2 {
		t.Fatalf("fallback args = %v; want 2 (-v host:container:ro)", args)
	}
	if args[0] != "-v" {
		t.Errorf("fallback args[0] = %q, want -v", args[0])
	}
	want := missing + ":/skills/foo:ro"
	if args[1] != want {
		t.Errorf("fallback args[1] = %q, want %q", args[1], want)
	}
}

// TestSecuritySandbox_E2BTarBundleSkipsTopLevelManifest: the e2b hydrate
// path packs each skill via tarBundle.addLocalDir into the gzip tar
// uploaded to /tmp/fc-hydrate.tar.gz. The top-level SKILL.md must be
// absent from the archive; the nested sub/SKILL.md and the scripts must
// survive.
func TestSecuritySandbox_E2BTarBundleSkipsTopLevelManifest(t *testing.T) {
	dir := t.TempDir()
	secWriteSkillTree(t, dir)

	b := newTarBundle()
	if _, err := b.addLocalDir(dir, "skills/foo"); err != nil {
		t.Fatalf("addLocalDir: %v", err)
	}
	if err := b.close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	names := secTarEntryNames(t, b.gz.Bytes(), true)
	if names["skills/foo/SKILL.md"] {
		t.Error("e2b tar contains top-level SKILL.md")
	}
	for _, want := range []string{"skills/foo/main.py", "skills/foo/sub/SKILL.md", "skills/foo/assets/logo.png"} {
		if !names[want] {
			t.Errorf("e2b tar missing %s; got %v", want, names)
		}
	}
}

// TestSecuritySandbox_BoxliteTarBundleSkipsTopLevelManifest: same gate on
// the boxlite hydrate path — plainTarBundle.addLocalDir feeds the raw tar
// PUT to /tmp/fc-hydrate.tar. The exclusion must be identical.
func TestSecuritySandbox_BoxliteTarBundleSkipsTopLevelManifest(t *testing.T) {
	dir := t.TempDir()
	secWriteSkillTree(t, dir)

	b := newPlainTarBundle()
	if _, err := b.addLocalDir(dir, "skills/foo"); err != nil {
		t.Fatalf("addLocalDir: %v", err)
	}
	if err := b.close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	names := secTarEntryNames(t, b.buf.Bytes(), false)
	if names["skills/foo/SKILL.md"] {
		t.Error("boxlite tar contains top-level SKILL.md")
	}
	for _, want := range []string{"skills/foo/main.py", "skills/foo/sub/SKILL.md", "skills/foo/assets/logo.png"} {
		if !names[want] {
			t.Errorf("boxlite tar missing %s; got %v", want, names)
		}
	}
}
