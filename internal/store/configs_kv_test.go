package store

import (
	"context"
	"testing"
)

// TestConfigsKVCRUD covers the single-value configs_kv table operations:
// upsert, point read, prefix scan, single delete, prefix delete.
func TestConfigsKVCRUD(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	if err := db.SetConfigValue(ctx, KindProvider, "user", "user-a", "openai.api_key", "sk-1"); err != nil {
		t.Fatalf("SetConfigValue: %v", err)
	}
	// Upsert overwrites.
	if err := db.SetConfigValue(ctx, KindProvider, "user", "user-a", "openai.api_key", "sk-2"); err != nil {
		t.Fatalf("SetConfigValue upsert: %v", err)
	}
	if err := db.SetConfigValue(ctx, KindProvider, "user", "user-a", "openai.api_base", "https://api.openai.com"); err != nil {
		t.Fatalf("SetConfigValue base: %v", err)
	}

	v, err := db.GetConfigValue(ctx, KindProvider, "user", "user-a", "openai.api_key")
	if err != nil {
		t.Fatalf("GetConfigValue: %v", err)
	}
	if v != "sk-2" {
		t.Fatalf("GetConfigValue = %q, want %q", v, "sk-2")
	}

	// Prefix scan.
	m, err := db.ListConfigValues(ctx, KindProvider, "user", "user-a", "openai.")
	if err != nil {
		t.Fatalf("ListConfigValues prefix: %v", err)
	}
	if len(m) != 2 || m["openai.api_key"] != "sk-2" {
		t.Fatalf("ListConfigValues prefix = %v", m)
	}

	// Empty prefix scans the whole (kind, scope, scope_id) set.
	m, err = db.ListConfigValues(ctx, KindProvider, "user", "user-a", "")
	if err != nil {
		t.Fatalf("ListConfigValues all: %v", err)
	}
	if len(m) != 2 {
		t.Fatalf("ListConfigValues all len = %d, want 2", len(m))
	}

	// Delete a single value.
	if err := db.DeleteConfigValue(ctx, KindProvider, "user", "user-a", "openai.api_base"); err != nil {
		t.Fatalf("DeleteConfigValue: %v", err)
	}
	if _, err := db.GetConfigValue(ctx, KindProvider, "user", "user-a", "openai.api_base"); err == nil {
		t.Fatalf("expected ErrNotFound after DeleteConfigValue")
	}

	// Prefix delete removes the rest.
	if err := db.DeleteConfigPrefix(ctx, KindProvider, "user", "user-a", "openai."); err != nil {
		t.Fatalf("DeleteConfigPrefix: %v", err)
	}
	m, err = db.ListConfigValues(ctx, KindProvider, "user", "user-a", "")
	if err != nil {
		t.Fatalf("ListConfigValues after prefix delete: %v", err)
	}
	if len(m) != 0 {
		t.Fatalf("ListConfigValues after prefix delete len = %d, want 0", len(m))
	}
}

// TestMigrateConfigsToKV flattens legacy configs JSON rows into
// configs_kv single-value rows. It must preserve the per-(user, agent)
// layer as a distinct scope (fork adaptation — collapsing it onto the
// user layer would leak one agent's key across the user's other agents)
// and be idempotent.
func TestMigrateConfigsToKV(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// Isolate: clear configs_kv so the migration actually runs even if an
	// earlier test left rows in the shared in-memory DB.
	if _, err := db.db.ExecContext(ctx, `DELETE FROM configs_kv`); err != nil {
		t.Fatalf("clear configs_kv: %v", err)
	}

	// Seed configs rows across all four ownership layers.
	rows := []*ConfigRecord{
		{Kind: KindProvider, UserID: "", AgentID: "", Name: "openai", Enabled: true,
			Data: map[string]interface{}{"apiKey": "sk-sys", "apiBase": "https://sys"}},
		{Kind: KindProvider, UserID: "user-a", AgentID: "", Name: "openai", Enabled: true,
			Data: map[string]interface{}{"apiKey": "sk-user"}},
		{Kind: KindProvider, UserID: "", AgentID: "agent-x", Name: "anthropic", Enabled: true,
			Data: map[string]interface{}{"apiKey": "sk-agent", "apiBase": "https://anthropic"}},
		{Kind: KindProvider, UserID: "user-a", AgentID: "agent-x", Name: "peragent", Enabled: true,
			Data: map[string]interface{}{"apiKey": "sk-per"}},
		{Kind: KindSetting, UserID: "user-a", AgentID: "", Name: "sandbox", Enabled: true,
			Data: map[string]interface{}{"enabled": true, "timeout": 60}},
	}
	for _, r := range rows {
		if err := db.SaveConfig(ctx, r); err != nil {
			t.Fatalf("SaveConfig: %v", err)
		}
	}

	if err := db.migrateConfigsToKV(ctx); err != nil {
		t.Fatalf("migrateConfigsToKV: %v", err)
	}

	cases := []struct {
		kind, scope, scopeID, name string
		want                       string
	}{
		{KindProvider, "system", "", "openai.api_key", "sk-sys"},
		{KindProvider, "user", "user-a", "openai.api_key", "sk-user"},
		{KindProvider, "agent", "agent-x", "anthropic.api_key", "sk-agent"},
		{KindProvider, "user-agent", "user-a/agent-x", "peragent.api_key", "sk-per"},
		{KindSetting, "user", "user-a", "sandbox.enabled", "true"},
		{KindSetting, "user", "user-a", "sandbox.timeout", "60"},
	}
	for _, c := range cases {
		v, err := db.GetConfigValue(ctx, c.kind, c.scope, c.scopeID, c.name)
		if err != nil {
			t.Fatalf("GetConfigValue(%s,%s,%s,%s): %v", c.kind, c.scope, c.scopeID, c.name, err)
		}
		if v != c.want {
			t.Fatalf("migrated %s/%s/%s/%s = %q, want %q", c.kind, c.scope, c.scopeID, c.name, v, c.want)
		}
	}

	// Idempotent: a second run skips because configs_kv already has data
	// and leaves existing rows untouched.
	if err := db.migrateConfigsToKV(ctx); err != nil {
		t.Fatalf("second migrateConfigsToKV: %v", err)
	}
	v, err := db.GetConfigValue(ctx, KindProvider, "system", "", "openai.api_key")
	if err != nil || v != "sk-sys" {
		t.Fatalf("after second migrate: v=%q err=%v", v, err)
	}
}

// TestCamelToSnakeAllCaps pins the a49f9d4 fix: ALL_CAPS and already_snake
// strings pass through (lowercased only) instead of getting a per-char
// underscore. REPLICATE_API_TOKEN was becoming r_e_p_l_i_c_a_t_e__a_p_i__t_o_k_e_n.
func TestCamelToSnakeAllCaps(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"apiKey", "api_key"},                          // camelCase → snake
		{"apiBase", "api_base"},                        // camelCase → snake
		{"REPLICATE_API_TOKEN", "replicate_api_token"}, // ALL_CAPS → lowercase only
		{"api_base", "api_base"},                       // already_snake → passthrough lowercased
		{"model", "model"},                             // single lowercase word
		{"HTTP", "http"},                               // single ALL_CAPS word → lowercase only (was h_t_t_p)
	}
	for _, c := range cases {
		if got := camelToSnake(c.in); got != c.want {
			t.Errorf("camelToSnake(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestMigrateConfigsToKV_AllCapsKey is the migration-path regression guard
// for a49f9d4: a provider whose Data contains an ALL_CAPS key (e.g. an env
// token like REPLICATE_API_TOKEN) must flatten to a clean dotted key, not
// the per-char-underscore mangle the old camelToSnake produced.
func TestMigrateConfigsToKV_AllCapsKey(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	if _, err := db.db.ExecContext(ctx, `DELETE FROM configs_kv`); err != nil {
		t.Fatalf("clear configs_kv: %v", err)
	}
	if err := db.SaveConfig(ctx, &ConfigRecord{
		Kind: KindProvider, UserID: "user-a", AgentID: "", Name: "replicate", Enabled: true,
		Data: map[string]interface{}{
			"apiBase":             "https://api.replicate.com",
			"REPLICATE_API_TOKEN": "r8_abc123",
		},
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if err := db.migrateConfigsToKV(ctx); err != nil {
		t.Fatalf("migrateConfigsToKV: %v", err)
	}

	v, err := db.GetConfigValue(ctx, KindProvider, "user", "user-a", "replicate.replicate_api_token")
	if err != nil || v != "r8_abc123" {
		t.Fatalf("migrated ALL_CAPS key = %q err=%v; want replicate.replicate_api_token=r8_abc123", v, err)
	}
	// The mangled per-char form must NOT exist.
	if _, err := db.GetConfigValue(ctx, KindProvider, "user", "user-a",
		"replicate.r_e_p_l_i_c_a_t_e__a_p_i__t_o_k_e_n"); err == nil {
		t.Fatalf("mangled per-char key still present after fix")
	}
}
