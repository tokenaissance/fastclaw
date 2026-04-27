package agent

import (
	"embed"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

//go:embed all:bundled_skills
var bundledSkillsFS embed.FS

// BundledSkillNames returns the folder names of every skill embedded in the
// binary. Exposed so startup/reload code can protect these entries from
// the object-store hydrator's "prune local-only dirs" step (bundled skills
// aren't stored in the object store; they're always regenerated on startup
// by InstallBundledSkills).
func BundledSkillNames() []string {
	entries, err := fs.ReadDir(bundledSkillsFS, "bundled_skills")
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

// InstallBundledSkills copies any bundled skills (currently none — see
// bundled_skills/README.md) to the managed skills directory if they
// don't already exist. Honors FASTCLAW_HOME so per-product instances
// each get their own copy.
func InstallBundledSkills() {
	targetDir := managedSkillsDir()
	if targetDir == "" {
		return
	}
	os.MkdirAll(targetDir, 0o755)

	entries, err := fs.ReadDir(bundledSkillsFS, "bundled_skills")
	if err != nil {
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillName := entry.Name()
		skillTarget := filepath.Join(targetDir, skillName)

		// Skip if already exists (user may have customized it).
		if _, err := os.Stat(filepath.Join(skillTarget, "SKILL.md")); err == nil {
			continue
		}

		srcRoot := filepath.Join("bundled_skills", skillName)
		if err := copyEmbedTree(bundledSkillsFS, srcRoot, skillTarget); err != nil {
			slog.Warn("failed to install bundled skill", "name", skillName, "error", err)
			continue
		}
		slog.Info("installed bundled skill", "name", skillName, "path", skillTarget)
	}
}

// managedSkillsDir is the per-FastClaw-instance global skills location.
// Mirrors fastclawManagedDir in internal/agent/skills.go but kept local
// here so this file's only dependency is os/filepath.
func managedSkillsDir() string {
	if h := os.Getenv("FASTCLAW_HOME"); h != "" {
		return filepath.Join(h, "skills")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".fastclaw", "skills")
}

// copyEmbedTree walks src in the embed.FS and writes every regular file under
// it to the corresponding path under dst, creating intermediate directories.
func copyEmbedTree(src embed.FS, srcRoot, dst string) error {
	return fs.WalkDir(src, srcRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(src, path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}
