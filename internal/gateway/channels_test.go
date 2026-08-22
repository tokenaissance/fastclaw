package gateway

import (
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// TestDecodeChannelFromRecord verifies ChannelRecord → ChannelConfig:
// the Data JSON blob is decoded first (preserving Accounts, AppToken),
// top-level Enabled is authoritative, and a non-empty top-level
// BotToken overrides whatever the blob carried.
func TestDecodeChannelFromRecord(t *testing.T) {
	rec := store.ChannelRecord{
		Enabled:  true,
		BotToken: "top_level_tok",
		Data: map[string]interface{}{
			"botToken": "blob_tok",
			"appToken": "app_tok",
			"accounts": map[string]interface{}{
				"@b1": map[string]interface{}{"botToken": "acct_1", "baseUrl": "https://x"},
				"@b2": map[string]interface{}{"botToken": "acct_2"},
			},
		},
	}
	cc := decodeChannelFromRecord(rec)

	if cc.BotToken != "top_level_tok" {
		t.Errorf("BotToken = %q; want top-level override %q", cc.BotToken, "top_level_tok")
	}
	if !cc.Enabled {
		t.Error("Enabled not taken from record")
	}
	if cc.AppToken != "app_tok" {
		t.Errorf("AppToken = %q; want blob value app_tok", cc.AppToken)
	}
	if len(cc.Accounts) != 2 {
		t.Fatalf("Accounts = %d; want 2 from blob", len(cc.Accounts))
	}
	if a := cc.Accounts["@b1"]; a.BaseURL != "https://x" || a.BotToken != "acct_1" {
		t.Errorf("Accounts[@b1] = %+v", a)
	}

	// Empty Data blob → zero value, Enabled still from record.
	empty := decodeChannelFromRecord(store.ChannelRecord{Enabled: false})
	if empty.Enabled {
		t.Error("Empty record should decode Enabled=false")
	}
}

// TestExpandChannelRecordBindings covers binding synthesis from
// ChannelRecord rows: disabled rows are skipped, a record whose Data
// blob has no Accounts map yields one binding keyed on the top-level
// AccountID, and an Accounts map expands to one binding per account.
func TestExpandChannelRecordBindings(t *testing.T) {
	const agentID = "agt_1"

	// Single-bot record: Data has no accounts map → one binding from
	// top-level AccountID.
	single := store.ChannelRecord{
		Type:      "telegram",
		AccountID: "@bot",
		Enabled:   true,
		Data:      map[string]interface{}{"botToken": "tok"},
	}
	// Multi-account record: Data carries an Accounts map.
	multi := store.ChannelRecord{
		Type:      "slack",
		AccountID: "fallback",
		Enabled:   true,
		Data: map[string]interface{}{
			"accounts": map[string]interface{}{
				"T01": map[string]interface{}{"botToken": "a"},
				"T02": map[string]interface{}{"botToken": "b"},
			},
		},
	}
	// Disabled record must be dropped.
	disabled := store.ChannelRecord{Type: "line", AccountID: "cli", Enabled: false}

	bindings := expandChannelRecordBindings([]store.ChannelRecord{single, multi, disabled}, agentID)

	if len(bindings) != 3 {
		t.Fatalf("bindings = %d; want 3 (telegram + 2 slack), disabled dropped", len(bindings))
	}
	want := map[string]string{
		"telegram|@bot": "@bot",
		"slack|T01":     "T01",
		"slack|T02":     "T02",
	}
	for _, b := range bindings {
		if b.AgentID != agentID {
			t.Errorf("binding AgentID = %q; want %q", b.AgentID, agentID)
		}
		key := b.Match.Channel + "|" + b.Match.AccountID
		if want[key] == "" {
			t.Errorf("unexpected binding %q", key)
			continue
		}
		if b.Match.AccountID != want[key] {
			t.Errorf("binding %q accountId = %q; want %q", key, b.Match.AccountID, want[key])
		}
	}

	// Empty input → empty output.
	if out := expandChannelRecordBindings(nil, agentID); len(out) != 0 {
		t.Errorf("nil rows yielded %d bindings; want 0", len(out))
	}
}

// TestChannelRecordToConfigRecord verifies the backward-compat shim
// used by the WeChat adapter path (needs a ConfigRecord shape).
func TestChannelRecordToConfigRecord(t *testing.T) {
	ch := store.ChannelRecord{
		ID:       "ch_1",
		UserID:   "u_1",
		AgentID:  "agt_1",
		Type:     "wechat",
		AccountID: "ilink_1",
		Enabled:  true,
		BotToken: "tok",
		Data:     map[string]interface{}{"botToken": "tok"},
	}
	cfg := channelRecordToConfigRecord(ch)
	if cfg.Kind != store.KindChannel {
		t.Errorf("Kind = %q; want channel", cfg.Kind)
	}
	if cfg.Name != "wechat" || cfg.CredentialKey != "ilink_1" || cfg.UserID != "u_1" || cfg.AgentID != "agt_1" {
		t.Errorf("ConfigRecord mapping wrong: %+v", cfg)
	}
	if cfg.Enabled != true {
		t.Errorf("Enabled not carried over")
	}
	_ = config.ChannelConfig{} // import used
}
