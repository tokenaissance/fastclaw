package scope

import (
	"context"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// E2E coverage for 4e24fb9 / b2030e6 "fix(models): use dash-separated
// Anthropic model IDs". Anthropic version IDs are dash-separated
// (claude-opus-4-6, not claude-opus-4.6); the fallback merge must pass
// them through byte-for-byte so a configured model actually works when
// used. This pins the scope Setting/SaveSetting round-trip against the
// dash format across every precedence layer (system → user → agent →
// per-(user,agent)), at the same layer the web presets write to.

// Covers 4e24fb9: dash-separated Anthropic model IDs survive the save →
// merge → read round trip without being stripped, renamed, or losing a
// layer's precedence. Regression guard for the wrong dot form
// (claude-opus-4.7) that the dashboard presets originally shipped.
func TestSettingPrecedence_DashModelIDsPreserved(t *testing.T) {
	db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()

	userID := "user-dash"
	agentID := "agent-dash"

	// System layer, dash-separated.
	if err := SaveSetting(ctx, db, "", "", "agents.defaults",
		map[string]interface{}{"model": "anthropic/claude-sonnet-4-6"}); err != nil {
		t.Fatalf("save system: %v", err)
	}
	// User layer overrides system.
	if err := SaveSetting(ctx, db, userID, "", "agents.defaults",
		map[string]interface{}{"model": "anthropic/claude-haiku-4-5"}); err != nil {
		t.Fatalf("save user: %v", err)
	}
	// Agent layer overrides user.
	if err := SaveSetting(ctx, db, "", agentID, "agents.defaults",
		map[string]interface{}{"model": "anthropic/claude-opus-4-6"}); err != nil {
		t.Fatalf("save agent: %v", err)
	}

	// Merged view: agent wins, and the dash-separated ID is intact.
	var got config.AgentDefaults
	if err := SettingInto(ctx, db, "agents.defaults", userID, agentID, &got); err != nil {
		t.Fatalf("setting into: %v", err)
	}
	if got.Model != "anthropic/claude-opus-4-6" {
		t.Fatalf("agent dash ID: want anthropic/claude-opus-4-6, got %q", got.Model)
	}

	// Delete the agent row → falls back to the user's dash ID.
	if err := SaveSetting(ctx, db, "", agentID, "agents.defaults", nil); err != nil {
		t.Fatalf("delete agent: %v", err)
	}
	got = config.AgentDefaults{}
	if err := SettingInto(ctx, db, "agents.defaults", userID, agentID, &got); err != nil {
		t.Fatalf("setting into after delete: %v", err)
	}
	if got.Model != "anthropic/claude-haiku-4-5" {
		t.Fatalf("user dash ID: want anthropic/claude-haiku-4-5, got %q", got.Model)
	}

	// Delete the user row → falls back to the system dash ID.
	if err := SaveSetting(ctx, db, userID, "", "agents.defaults", nil); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	got = config.AgentDefaults{}
	if err := SettingInto(ctx, db, "agents.defaults", userID, agentID, &got); err != nil {
		t.Fatalf("setting into after user delete: %v", err)
	}
	if got.Model != "anthropic/claude-sonnet-4-6" {
		t.Fatalf("system dash ID: want anthropic/claude-sonnet-4-6, got %q", got.Model)
	}
}

// Covers 4e24fb9 (storage fidelity): the raw agent-scope row must read
// back exactly the dash-separated ID written — the loadUserSpace overlay
// reads this directly, not via Setting(), so a merge test alone could
// miss a write-time mangling.
func TestSettingRow_DashModelIDReadsBackByteForByte(t *testing.T) {
	db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()

	const modelID = "anthropic/claude-opus-4-6"
	if err := SaveSetting(ctx, db, "", "agent-dash", "agents.defaults",
		map[string]interface{}{"model": modelID}); err != nil {
		t.Fatalf("save agent: %v", err)
	}
	rec, err := db.GetConfigByName(ctx, store.KindSetting, "", "agent-dash", "agents.defaults")
	if err != nil {
		t.Fatalf("get agent-scope row: %v", err)
	}
	if rec == nil {
		t.Fatal("agent-scope row missing after save")
	}
	if v, _ := rec.Data["model"].(string); v != modelID {
		t.Fatalf("raw row model: want %q, got %q", modelID, v)
	}
}
