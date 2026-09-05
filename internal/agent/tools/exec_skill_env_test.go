package tools

import (
	"testing"
)

func TestResolveSkillEnvHostPath(t *testing.T) {
	skillDirs := []string{"/home/user/agents/a1/skills"}
	provider := func(skillName string) map[string]string {
		if skillName == "baoyu-skills" {
			return map[string]string{"WECHAT_APP_ID": "wx123", "WECHAT_APP_SECRET": "sec"}
		}
		return nil
	}

	env := resolveSkillEnv(
		"bun /home/user/agents/a1/skills/baoyu-skills/scripts/wechat-api.ts --md article.md",
		provider,
		skillDirs,
	)
	if env == nil || env["WECHAT_APP_ID"] != "wx123" || env["WECHAT_APP_SECRET"] != "sec" {
		t.Fatalf("host-path env injection failed, got: %+v", env)
	}
}

func TestResolveSkillEnvSandboxPath(t *testing.T) {
	provider := func(skillName string) map[string]string {
		if skillName == "baoyu-skills" {
			return map[string]string{"WECHAT_APP_ID": "wx456"}
		}
		return nil
	}

	env := resolveSkillEnv(
		"bun /skills/baoyu-skills/scripts/wechat-api.ts --md article.md",
		provider,
		nil,
	)
	if env == nil || env["WECHAT_APP_ID"] != "wx456" {
		t.Fatalf("sandbox /skills/<name> env injection failed, got: %+v", env)
	}
}

func TestResolveSkillEnvNoMatch(t *testing.T) {
	provider := func(skillName string) map[string]string {
		return map[string]string{"WECHAT_APP_ID": "wx789"}
	}
	skillDirs := []string{"/home/user/agents/a1/skills"}

	if env := resolveSkillEnv("bun /tmp/scripts/wechat-api.ts", provider, skillDirs); env != nil {
		t.Fatalf("command outside any skill dir should get no env, got: %+v", env)
	}
}
