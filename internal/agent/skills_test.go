package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/workspace"
)

// skillEnvTestStore satisfies workspace.Store for loader tests that only
// need to attach an agentID (SkillEnvVars never touches the store).
type skillEnvTestStore struct {
	workspace.Store
}

func TestSplitSkillFrontmatterTopLevelEnv(t *testing.T) {
	data := []byte(`---
name: baoyu-skills
description: WeChat publishing suite.
env:
  - name: WECHAT_APP_ID
    description: WeChat Official Account AppID
    required: true
  - name: WECHAT_APP_SECRET
    description: WeChat Official Account AppSecret
    required: true
    secret: true
---
# Body
`)
	fm, body := SplitSkillFrontmatter(data)
	if fm == nil {
		t.Fatal("SplitSkillFrontmatter returned nil frontmatter")
	}
	if len(fm.Env) != 2 {
		t.Fatalf("env len = %d, want 2: %+v", len(fm.Env), fm.Env)
	}
	if fm.Env[0].Name != "WECHAT_APP_ID" || !fm.Env[0].Required || fm.Env[0].Secret {
		t.Fatalf("WECHAT_APP_ID spec mismatch: %+v", fm.Env[0])
	}
	if fm.Env[1].Name != "WECHAT_APP_SECRET" || !fm.Env[1].Required || !fm.Env[1].Secret {
		t.Fatalf("WECHAT_APP_SECRET spec mismatch: %+v", fm.Env[1])
	}
	if !strings.Contains(body, "# Body") {
		t.Fatalf("body should keep prose after frontmatter, got: %q", body)
	}
}

func TestParseSkillMetadataFastagentEnv(t *testing.T) {
	data := []byte(`---
name: baoyu-skills
metadata:
  fastagent:
    env:
      - name: WECHAT_APP_ID
        required: true
      - name: WECHAT_APP_SECRET
        required: true
        secret: true
---
`)
	fm, _ := SplitSkillFrontmatter(data)
	if fm == nil {
		t.Fatal("SplitSkillFrontmatter returned nil frontmatter")
	}
	meta := ParseSkillMetadata(&fm.Metadata)
	if meta == nil || meta.Meta() == nil {
		t.Fatal("ParseSkillMetadata returned nil metadata")
	}
	env := meta.Meta().Env
	if len(env) != 2 {
		t.Fatalf("metadata env len = %d, want 2: %+v", len(env), env)
	}
	if env[1].Name != "WECHAT_APP_SECRET" || !env[1].Secret {
		t.Fatalf("nested secret spec mismatch: %+v", env[1])
	}
}

func TestParseSkillMetadataOpenClawEnvFallback(t *testing.T) {
	// baoyu-post-to-wechat keeps upstream `metadata.openclaw` for
	// compatibility; Meta() must fall back to it when `fastagent` is
	// absent so materialized per-skill installs surface env vars too.
	data := []byte(`---
name: baoyu-post-to-wechat
metadata:
  openclaw:
    homepage: https://github.com/JimLiu/baoyu-skills#baoyu-post-to-wechat
    requires:
      anyBins:
        - bun
        - npx
    env:
      - name: WECHAT_APP_ID
        required: true
      - name: WECHAT_APP_SECRET
        required: true
        secret: true
---
`)
	fm, _ := SplitSkillFrontmatter(data)
	if fm == nil {
		t.Fatal("SplitSkillFrontmatter returned nil frontmatter")
	}
	meta := ParseSkillMetadata(&fm.Metadata)
	if meta == nil || meta.Meta() == nil {
		t.Fatal("ParseSkillMetadata returned nil metadata")
	}
	env := meta.Meta().Env
	if len(env) != 2 {
		t.Fatalf("openclaw env len = %d, want 2: %+v", len(env), env)
	}
	if env[0].Name != "WECHAT_APP_ID" || !env[0].Required {
		t.Fatalf("openclaw env[0] mismatch: %+v", env[0])
	}
	if env[1].Name != "WECHAT_APP_SECRET" || !env[1].Secret {
		t.Fatalf("openclaw env[1] mismatch: %+v", env[1])
	}
}

func TestSplitSkillFrontmatterTopLevelEnvWinsOverMetadata(t *testing.T) {
	data := []byte(`---
name: x
env:
  - name: TOP_LEVEL_VAR
metadata:
  fastagent:
    env:
      - name: NESTED_VAR
---
`)
	fm, _ := SplitSkillFrontmatter(data)
	if fm == nil {
		t.Fatal("nil frontmatter")
	}
	if len(fm.Env) != 1 || fm.Env[0].Name != "TOP_LEVEL_VAR" {
		t.Fatalf("top-level env should win over metadata.env, got: %+v", fm.Env)
	}
}

func TestSkillEnvVarsPerAgentOverrideAndGlobalFallback(t *testing.T) {
	cfg := config.SkillsCfg{
		Entries: map[string]config.SkillEntryCfg{
			"baoyu-skills": {
				Env: map[string]string{
					"WECHAT_APP_ID": "global-app-id",
				},
			},
		},
		AgentEntries: map[string]map[string]config.SkillEntryCfg{
			"agent-1": {
				"baoyu-skills": {
					Env: map[string]string{
						"WECHAT_APP_ID":     "agent-app-id",
						"WECHAT_APP_SECRET": "agent-secret",
					},
				},
			},
			"agent-2": {
				"baoyu-skills": {}, // empty per-agent row must fall back to global
			},
		},
	}

	newLoader := func(agentID string) *SkillsLoader {
		return NewSkillsLoaderWithGlobal(t.TempDir(), t.TempDir(), "", config.SkillsConfig{}, cfg).
			WithObjectStore(skillEnvTestStore{}, agentID)
	}

	// Per-agent override wins and merges the agent-only secret.
	env1 := newLoader("agent-1").SkillEnvVars("baoyu-skills")
	if env1["WECHAT_APP_ID"] != "agent-app-id" {
		t.Fatalf("agent override should win, got: %+v", env1)
	}
	if env1["WECHAT_APP_SECRET"] != "agent-secret" {
		t.Fatalf("agent secret missing, got: %+v", env1)
	}

	// Empty per-agent row falls back to the global entry.
	env2 := newLoader("agent-2").SkillEnvVars("baoyu-skills")
	if env2["WECHAT_APP_ID"] != "global-app-id" {
		t.Fatalf("global fallback failed, got: %+v", env2)
	}
	if _, ok := env2["WECHAT_APP_SECRET"]; ok {
		t.Fatalf("global entry should not contain the agent-only secret, got: %+v", env2)
	}

	// Unknown skill returns nil.
	if got := newLoader("agent-1").SkillEnvVars("does-not-exist"); got != nil {
		t.Fatalf("unknown skill should return nil, got: %+v", got)
	}
}

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
