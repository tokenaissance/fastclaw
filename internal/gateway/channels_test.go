package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/scope"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/users"
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
		ID:        "ch_1",
		UserID:    "u_1",
		AgentID:   "agt_1",
		Type:      "wechat",
		AccountID: "ilink_1",
		Enabled:   true,
		BotToken:  "tok",
		Data:      map[string]interface{}{"botToken": "tok"},
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

// TestResolveChannelOwner_ChannelsFirstAndFallback drives the routing
// seam every inbound IM message crosses after the channels-table
// migration: the new channels table is consulted first, legacy configs
// rows are the fallback for pre-migration installs, and an agent-only
// channel row resolves to the agent owner. Mirrors how the Cloud proxy
// forwards platform messages to the backend.
func TestResolveChannelOwner_ChannelsFirstAndFallback(t *testing.T) {
	db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	owner := &store.UserRecord{
		ID:           "u_owner",
		Username:     "owner",
		Email:        "owner@example.com",
		PasswordHash: "x",
		Role:         users.RoleUser,
		Status:       users.StatusActive,
		AgentQuota:   -1,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	if err := db.CreateUser(ctx, owner); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if err := db.SaveAgent(ctx, &store.AgentRecord{ID: "agt_1", UserID: owner.ID, Name: "routing"}); err != nil {
		t.Fatalf("save agent: %v", err)
	}
	g := &Gateway{store: db}

	// 1. Channels table first: a row in the new table resolves directly.
	if err := db.SaveChannel(ctx, &store.ChannelRecord{
		UserID: owner.ID, AgentID: "agt_1", Type: "telegram", AccountID: "bot-new", Enabled: true,
	}); err != nil {
		t.Fatalf("save channel row: %v", err)
	}
	if got := g.resolveChannelOwner(ctx, bus.InboundMessage{Channel: "telegram", AccountID: "bot-new"}); got.ownerID != owner.ID {
		t.Errorf("channels-table lookup = %q; want %q", got.ownerID, owner.ID)
	}

	// 2. Channels table shadows a conflicting legacy configs row (the
	//    dual-write window): the new table must win, never the configs.
	if err := scope.SaveChannel(ctx, db, "u_other", "agt_other", "telegram", "bot-new", true,
		config.ChannelConfig{Enabled: true, Accounts: map[string]config.AccountConfig{"bot-new": {BotToken: "tok"}}}); err != nil {
		t.Fatalf("seed shadow configs row: %v", err)
	}
	if got := g.resolveChannelOwner(ctx, bus.InboundMessage{Channel: "telegram", AccountID: "bot-new"}); got.ownerID != owner.ID {
		t.Errorf("channels-table precedence = %q; want %q (configs row u_other must NOT win)", got.ownerID, owner.ID)
	}

	// 3. Legacy configs fallback: pre-migration installs with only a
	//    configs row still route.
	if err := scope.SaveChannel(ctx, db, owner.ID, "agt_1", "discord", "bot-legacy", true,
		config.ChannelConfig{Enabled: true, Accounts: map[string]config.AccountConfig{"bot-legacy": {BotToken: "tok"}}}); err != nil {
		t.Fatalf("seed configs fallback row: %v", err)
	}
	if got := g.resolveChannelOwner(ctx, bus.InboundMessage{Channel: "discord", AccountID: "bot-legacy"}); got.ownerID != owner.ID {
		t.Errorf("configs fallback lookup = %q; want %q", got.ownerID, owner.ID)
	}

	// 4. Agent-only channel row (system-level, no user_id) → agent owner.
	if err := db.SaveChannel(ctx, &store.ChannelRecord{
		AgentID: "agt_1", Type: "slack", AccountID: "T-sys", Enabled: true,
	}); err != nil {
		t.Fatalf("save system channel row: %v", err)
	}
	if got := g.resolveChannelOwner(ctx, bus.InboundMessage{Channel: "slack", AccountID: "T-sys"}); got.ownerID != owner.ID {
		t.Errorf("agent-only channel row = %q; want agent owner %q", got.ownerID, owner.ID)
	}

	// 5. Unknown channel → dropped (""), never silently routed.
	if got := g.resolveChannelOwner(ctx, bus.InboundMessage{Channel: "slack", AccountID: "nope"}); got.ownerID != "" {
		t.Errorf("unknown channel owner = %q; want empty", got.ownerID)
	}
}
