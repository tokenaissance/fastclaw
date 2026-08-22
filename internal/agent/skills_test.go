package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/config"
)

func TestBuildSkillsSummaryUsesProgressiveDisclosureByDefault(t *testing.T) {
	t.Setenv("FASTAGENT_HOME", t.TempDir())
	home := t.TempDir()
	skillDir := filepath.Join(home, "skills", "chart-maker")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `---
name: chart-maker
description: Build charts from tabular data.
---

SECRET_INLINE_BODY_SHOULD_NOT_APPEAR
Run scripts/render.py with JSON input.`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	loader := NewSkillsLoaderWithGlobal(home, t.TempDir(), "", config.SkillsConfig{}, config.SkillsCfg{})
	summary := loader.BuildSkillsSummary(loader.LoadSkills())

	if !strings.Contains(summary, "chart-maker") {
		t.Fatalf("summary missing skill name:\n%s", summary)
	}
	if !strings.Contains(summary, "Build charts from tabular data") {
		t.Fatalf("summary missing skill description:\n%s", summary)
	}
	if strings.Contains(summary, "SECRET_INLINE_BODY_SHOULD_NOT_APPEAR") {
		t.Fatalf("summary leaked SKILL.md body:\n%s", summary)
	}
	if !strings.Contains(summary, "load_skill") {
		t.Fatalf("summary should tell the model to call load_skill:\n%s", summary)
	}
}

func TestLoadSkillsDoesNotKeepBodyContentByDefault(t *testing.T) {
	t.Setenv("FASTAGENT_HOME", t.TempDir())
	home := t.TempDir()
	skillDir := filepath.Join(home, "skills", "chart-maker")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `---
name: chart-maker
description: Build charts from tabular data.
---

BODY_SHOULD_STAY_ON_DISK_UNTIL_LOAD_SKILL`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	loader := NewSkillsLoaderWithGlobal(home, t.TempDir(), "", config.SkillsConfig{}, config.SkillsCfg{})
	skills := loader.LoadSkills()

	if len(skills) != 1 {
		t.Fatalf("skills len = %d, want 1", len(skills))
	}
	if skills[0].Content != "" {
		t.Fatalf("LoadSkills should not keep default skill body in memory, got:\n%s", skills[0].Content)
	}
}

func TestBuildSkillsSummaryKeepsAlwaysLoadSkillsInline(t *testing.T) {
	t.Setenv("FASTAGENT_HOME", t.TempDir())
	home := t.TempDir()
	skillDir := filepath.Join(home, "skills", "always-inline")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `---
name: always-inline
description: Needs full instructions immediately.
---

ALWAYS_LOAD_BODY_SHOULD_APPEAR`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	loader := NewSkillsLoaderWithGlobal(
		home,
		t.TempDir(),
		"",
		config.SkillsConfig{AlwaysLoad: []string{"always-inline"}},
		config.SkillsCfg{},
	)
	summary := loader.BuildSkillsSummary(loader.LoadSkills())

	if !strings.Contains(summary, "ALWAYS_LOAD_BODY_SHOULD_APPEAR") {
		t.Fatalf("summary should inline explicitly always-loaded skill:\n%s", summary)
	}
}

func TestGatedSkillsStayInCatalogWithUnavailableReason(t *testing.T) {
	t.Setenv("FASTAGENT_HOME", t.TempDir())
	home := t.TempDir()
	skillDir := filepath.Join(home, "skills", "deepcoin-trade")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `---
name: deepcoin-trade
description: Place and manage Deepcoin orders.
metadata:
  openclaw:
    requires:
      env: ["DC_API_KEY"]
---

BODY_SHOULD_NOT_INLINE_WHEN_GATED`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	loader := NewSkillsLoaderWithGlobal(home, t.TempDir(), "", config.SkillsConfig{}, config.SkillsCfg{})
	skills := loader.LoadSkills()
	if len(skills) != 1 {
		t.Fatalf("skills len = %d, want 1", len(skills))
	}
	if !skills[0].Gated {
		t.Fatalf("skill should be gated: %+v", skills[0])
	}

	summary := loader.BuildSkillsSummary(skills)
	if !strings.Contains(summary, "deepcoin-trade") {
		t.Fatalf("summary missing gated skill:\n%s", summary)
	}
	if !strings.Contains(summary, `currently unavailable: required env var "DC_API_KEY" not set`) {
		t.Fatalf("summary missing unavailable reason:\n%s", summary)
	}
	if strings.Contains(summary, "BODY_SHOULD_NOT_INLINE_WHEN_GATED") {
		t.Fatalf("summary should not inline gated skill body:\n%s", summary)
	}
}
