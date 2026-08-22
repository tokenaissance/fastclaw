package scope

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

func openScopeDB(t *testing.T) *store.DBStore {
	t.Helper()
	db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// TestGetValueScopePrecedence pins the configs_kv resolution order:
// system → user → agent → per-(user, agent), innermost wins.
func TestGetValueScopePrecedence(t *testing.T) {
	db := openScopeDB(t)
	defer db.Close()
	ctx := context.Background()

	set := func(sc, sid, name, val string) {
		t.Helper()
		if err := db.SetConfigValue(ctx, store.KindSetting, sc, sid, name, val); err != nil {
			t.Fatalf("SetConfigValue(%s,%s,%s): %v", sc, sid, name, err)
		}
	}
	set(System, "", "theme", "system-theme")
	set(User, "user-a", "theme", "user-theme")
	set(Agent, "agent-x", "theme", "agent-theme")
	set(UserAgent, "user-a/agent-x", "theme", "per-theme")

	v, found, err := GetValue(ctx, db, store.KindSetting, "theme", "user-a", "agent-x")
	if err != nil || !found {
		t.Fatalf("GetValue full chain: v=%q found=%v err=%v", v, found, err)
	}
	if v != "per-theme" {
		t.Fatalf("per-(user,agent) should win, got %q", v)
	}

	// Without the per-(user,agent) row → agent wins.
	if err := db.DeleteConfigValue(ctx, store.KindSetting, UserAgent, "user-a/agent-x", "theme"); err != nil {
		t.Fatalf("delete per: %v", err)
	}
	v, _, _ = GetValue(ctx, db, store.KindSetting, "theme", "user-a", "agent-x")
	if v != "agent-theme" {
		t.Fatalf("agent should win without per layer, got %q", v)
	}

	// User only (agent empty) → user wins.
	v, _, _ = GetValue(ctx, db, store.KindSetting, "theme", "user-a", "")
	if v != "user-theme" {
		t.Fatalf("user should win when agent empty, got %q", v)
	}

	// Neither user nor agent → system.
	v, _, _ = GetValue(ctx, db, store.KindSetting, "theme", "", "")
	if v != "system-theme" {
		t.Fatalf("system should win when no user/agent, got %q", v)
	}
}

// TestProviderPerUserAgentIsolation is the fork-adaptation regression
// guard: a provider key bound at per-(user, agent) scope must stay
// isolated to that agent and never leak to the user's other agents.
func TestProviderPerUserAgentIsolation(t *testing.T) {
	db := openScopeDB(t)
	defer db.Close()
	ctx := context.Background()

	// Bind a provider key at per-(user-a, agent-x) scope.
	if err := SaveProvider(ctx, db, "user-a", "agent-x", "openai", config.ProviderConfig{
		APIKey:  "sk-per",
		APIBase: "https://per.agent",
	}); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}

	// The bound agent sees it…
	provs, err := Providers(ctx, db, "user-a", "agent-x")
	if err != nil {
		t.Fatalf("Providers(bound): %v", err)
	}
	if got := provs["openai"].APIKey; got != "sk-per" {
		t.Fatalf("bound agent openai.APIKey = %q, want sk-per", got)
	}

	// …a sibling agent of the same user must NOT.
	provs, err = Providers(ctx, db, "user-a", "agent-y")
	if err != nil {
		t.Fatalf("Providers(sibling): %v", err)
	}
	if _, ok := provs["openai"]; ok {
		t.Fatalf("sibling agent leaked per-(user,agent) provider: %+v", provs)
	}

	// The value was written at the user-agent layer, not the user layer.
	v, err := db.GetConfigValue(ctx, store.KindProvider, UserAgent, "user-a/agent-x", "openai.api_key")
	if err != nil || v != "sk-per" {
		t.Fatalf("configs_kv user-agent openai.api_key = %q err=%v", v, err)
	}
	if _, err := db.GetConfigValue(ctx, store.KindProvider, User, "user-a", "openai.api_key"); err == nil {
		t.Fatalf("per-(user,agent) key must not be stored at user layer")
	}
}

// TestProvidersDualWriteReadsFromKV verifies the read path prefers
// configs_kv once populated, and falls back to the legacy configs table
// when configs_kv is empty.
func TestProvidersDualWriteReadsFromKV(t *testing.T) {
	db := openScopeDB(t)
	defer db.Close()
	ctx := context.Background()

	// SaveProvider dual-writes to configs_kv.
	if err := SaveProvider(ctx, db, "user-a", "", "openai", config.ProviderConfig{
		APIKey:  "sk-1",
		APIBase: "https://api.openai.com",
	}); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}

	// configs_kv now holds the flattened row.
	v, err := db.GetConfigValue(ctx, store.KindProvider, User, "user-a", "openai.api_key")
	if err != nil || v != "sk-1" {
		t.Fatalf("configs_kv openai.api_key = %q err=%v", v, err)
	}

	// Read prefers configs_kv: overwrite it and confirm Providers reflects
	// the KV value, not the legacy configs row.
	if err := db.SetConfigValue(ctx, store.KindProvider, User, "user-a", "openai.api_key", "sk-kv-override"); err != nil {
		t.Fatalf("SetConfigValue override: %v", err)
	}
	provs, err := Providers(ctx, db, "user-a", "")
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	if got := provs["openai"].APIKey; got != "sk-kv-override" {
		t.Fatalf("Providers preferred configs_kv: got %q, want sk-kv-override", got)
	}

	// Fallback: wipe configs_kv, Providers must read the legacy table.
	if err := db.DeleteConfigPrefix(ctx, store.KindProvider, User, "user-a", "openai."); err != nil {
		t.Fatalf("DeleteConfigPrefix: %v", err)
	}
	provs, err = Providers(ctx, db, "user-a", "")
	if err != nil {
		t.Fatalf("Providers fallback: %v", err)
	}
	if got := provs["openai"].APIKey; got != "sk-1" {
		t.Fatalf("Providers fallback read legacy configs: got %q, want sk-1", got)
	}
}

// TestSettingDualWriteReadsFromKV covers the same prefer-KV-then-fallback
// behavior for settings namespaces.
func TestSettingDualWriteReadsFromKV(t *testing.T) {
	db := openScopeDB(t)
	defer db.Close()
	ctx := context.Background()

	if err := SaveSetting(ctx, db, "user-a", "", "agents.defaults", map[string]interface{}{
		"model": "deepseek/deepseek-v4-pro",
		"temp":  0.7,
	}); err != nil {
		t.Fatalf("SaveSetting: %v", err)
	}

	// agents.defaults maps to the "agent." KV prefix (upstream contract).
	v, err := db.GetConfigValue(ctx, store.KindSetting, User, "user-a", "agent.model")
	if err != nil || v != "deepseek/deepseek-v4-pro" {
		t.Fatalf("configs_kv agent.model = %q err=%v", v, err)
	}

	got, err := Setting(ctx, db, "agents.defaults", "user-a", "")
	if err != nil {
		t.Fatalf("Setting: %v", err)
	}
	if m, _ := got["model"].(string); m != "deepseek/deepseek-v4-pro" {
		t.Fatalf("Setting model = %v, want deepseek/deepseek-v4-pro", got["model"])
	}

	// Fallback to legacy configs after wiping configs_kv.
	if err := db.DeleteConfigPrefix(ctx, store.KindSetting, User, "user-a", "agent."); err != nil {
		t.Fatalf("DeleteConfigPrefix: %v", err)
	}
	got, err = Setting(ctx, db, "agents.defaults", "user-a", "")
	if err != nil {
		t.Fatalf("Setting fallback: %v", err)
	}
	if m, _ := got["model"].(string); m != "deepseek/deepseek-v4-pro" {
		t.Fatalf("Setting fallback model = %v", got["model"])
	}
}

// TestParseKVValue pins value type inference: bools, numbers, JSON
// arrays/objects, and plain strings.
func TestParseKVValue(t *testing.T) {
	cases := []struct {
		in   string
		want interface{}
	}{
		{"true", true},
		{"false", false},
		{"42", float64(42)},
		{"[1,2]", []interface{}{float64(1), float64(2)}},
		{`{"a":1}`, map[string]interface{}{"a": float64(1)}},
		{"hello", "hello"},
		{"", ""},
	}
	for _, c := range cases {
		if got := parseKVValue(c.in); !jsonEqual(got, c.want) {
			t.Fatalf("parseKVValue(%q) = %#v (%T), want %#v", c.in, got, got, c.want)
		}
	}
}

// TestSnakeCamelRoundTrip pins the snake_case ↔ camelCase converters the
// flatten/reconstruct path relies on.
func TestSnakeCamelRoundTrip(t *testing.T) {
	for _, c := range []struct{ camel, snake string }{
		{"apiKey", "api_key"},
		{"apiBase", "api_base"},
		{"authType", "auth_type"},
		{"model", "model"},
	} {
		if got := camelToSnake(c.camel); got != c.snake {
			t.Fatalf("camelToSnake(%q) = %q, want %q", c.camel, got, c.snake)
		}
		if got := snakeToCamel(c.snake); got != c.camel {
			t.Fatalf("snakeToCamel(%q) = %q, want %q", c.snake, got, c.camel)
		}
	}
}

func jsonEqual(a, b interface{}) bool {
	as, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bs, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return string(as) == string(bs)
}
