package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestChannelRecordRoundTrip exercises the channels-table CRUD surface:
// Save (auto-id), Lookup, Get, List filters, Delete.
func TestChannelRecordRoundTrip(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	ch := &ChannelRecord{
		UserID:    "u_owner",
		AgentID:   "agt_1",
		Type:      "telegram",
		AccountID: "@fastagent_bot",
		Enabled:   true,
		BotToken:  "tok_1",
		Data:      map[string]interface{}{"extra": "x"},
	}
	if err := db.SaveChannel(ctx, ch); err != nil {
		t.Fatalf("SaveChannel: %v", err)
	}
	if !strings.HasPrefix(ch.ID, "ch_") {
		t.Errorf("auto id = %q; want ch_ prefix", ch.ID)
	}

	// Lookup by (type, accountID) — the routing seam.
	got, err := db.LookupChannel(ctx, "telegram", "@fastagent_bot")
	if err != nil {
		t.Fatalf("LookupChannel: %v", err)
	}
	if got == nil || got.ID != ch.ID || got.UserID != "u_owner" || got.AgentID != "agt_1" || got.BotToken != "tok_1" {
		t.Errorf("LookupChannel = %+v; want id=%s user=u_owner agent=agt_1", got, ch.ID)
	}
	if !got.Enabled {
		t.Error("enabled should round-trip true")
	}
	if v, ok := got.Data["extra"]; !ok || v != "x" {
		t.Errorf("data blob not round-tripped: %+v", got.Data)
	}

	// Get by id.
	byID, err := db.GetChannel(ctx, ch.ID)
	if err != nil || byID == nil || byID.Type != "telegram" {
		t.Errorf("GetChannel = %+v, err=%v", byID, err)
	}

	// List filters by (user, agent).
	listed, err := db.ListChannels(ctx, "u_owner", "agt_1")
	if err != nil || len(listed) != 1 {
		t.Errorf("ListChannels(user,agent) = %d rows, err=%v; want 1", len(listed), err)
	}
	other, err := db.ListChannels(ctx, "u_other", "agt_1")
	if err != nil {
		t.Fatalf("ListChannels other user: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("ListChannels(other user) = %d rows; want 0", len(other))
	}

	// ListAll.
	all, err := db.ListAllChannels(ctx)
	if err != nil || len(all) != 1 {
		t.Errorf("ListAllChannels = %d rows, err=%v; want 1", len(all), err)
	}

	// Delete.
	if err := db.DeleteChannel(ctx, ch.ID); err != nil {
		t.Fatalf("DeleteChannel: %v", err)
	}
	if rem, err := db.LookupChannel(ctx, "telegram", "@fastagent_bot"); err == nil || rem != nil {
		t.Errorf("row still present after delete: rem=%+v err=%v", rem, err)
	}
}

// TestChannelUpsertUnique enforces UNIQUE(type, account_id): saving the
// same (type, account) twice must update the existing row in place
// (stable id), never insert a duplicate.
func TestChannelUpsertUnique(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	first := &ChannelRecord{Type: "discord", AccountID: "u_123", UserID: "u_a", AgentID: "agt_1", Enabled: true, BotToken: "tok_v1"}
	if err := db.SaveChannel(ctx, first); err != nil {
		t.Fatalf("first save: %v", err)
	}
	origID := first.ID

	// Same (type, account) — different owner + token. Must upsert, not dup.
	second := &ChannelRecord{Type: "discord", AccountID: "u_123", UserID: "u_b", AgentID: "agt_2", Enabled: false, BotToken: "tok_v2"}
	if err := db.SaveChannel(ctx, second); err != nil {
		t.Fatalf("second save: %v", err)
	}

	all, err := db.ListAllChannels(ctx)
	if err != nil {
		t.Fatalf("ListAllChannels: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("upsert created %d rows; want 1", len(all))
	}
	if all[0].ID != origID {
		t.Errorf("upsert changed id: %q -> %q", origID, all[0].ID)
	}
	if all[0].UserID != "u_b" || all[0].AgentID != "agt_2" || all[0].BotToken != "tok_v2" {
		t.Errorf("upsert did not apply latest fields: %+v", all[0])
	}
	if all[0].Enabled {
		t.Error("upsert did not apply enabled=false")
	}
}

// TestMigrateChannelsFromConfigs verifies the zero-downtime migration:
// kind='channel' configs rows are copied into the channels table on
// startup (single-bot legacy + multi-account shapes), the copy is
// skipped when channels already has data, and old configs rows are
// preserved for rollback.
func TestMigrateChannelsFromConfigs(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	seed := func(id, name, credKey, data string) {
		t.Helper()
		if _, err := db.db.ExecContext(ctx,
			`INSERT INTO configs (id, kind, scope, user_id, agent_id, name, enabled, credential_key, data)
				VALUES (?, 'channel', 'agent', 'u_owner', 'agt_1', ?, 1, ?, ?)`,
			id, name, credKey, data); err != nil {
			t.Fatalf("seed config %s: %v", id, err)
		}
	}
	// Single-bot legacy shape: no accounts map, botToken at top level.
	seed("cfg_legacy", "telegram", "cred_legacy", `{"botToken":"tok_legacy"}`)
	// Multi-account shape: one channel row per account entry.
	seed("cfg_multi", "slack", "cred_multi", `{"accounts":{"T01":{"botToken":"tok_A"},"T02":{"botToken":"tok_B","userId":"su_B"}}}`)
	// Disabled row.
	if _, err := db.db.ExecContext(ctx,
		`INSERT INTO configs (id, kind, scope, user_id, agent_id, name, enabled, credential_key, data)
			VALUES ('cfg_disabled', 'channel', 'agent', 'u_owner', 'agt_1', 'line', 0, 'cred_line', '{}')`,
	); err != nil {
		t.Fatalf("seed disabled: %v", err)
	}

	// Run the migration (openTestDB already ran once on an empty table;
	// this second run sees the just-seeded configs and empty channels).
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	rows, err := db.ListAllChannels(ctx)
	if err != nil {
		t.Fatalf("ListAllChannels: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("channels after migration = %d rows; want 4 (telegram + 2 slack accounts + line disabled)", len(rows))
	}

	byType := map[string]ChannelRecord{}
	for _, r := range rows {
		byType[r.Type+"|"+r.AccountID] = r
	}

	// Single-bot legacy → AccountID = credential_key.
	if ch, ok := byType["telegram|cred_legacy"]; !ok {
		t.Errorf("missing migrated telegram row; got %v", rows)
	} else if ch.BotToken != "tok_legacy" || ch.UserID != "u_owner" || ch.AgentID != "agt_1" {
		t.Errorf("legacy telegram row = %+v", ch)
	}

	// Multi-account → one row per account with per-account token.
	if ch, ok := byType["slack|T01"]; !ok || ch.BotToken != "tok_A" {
		t.Errorf("slack T01 row missing or token wrong: %+v", ch)
	}
	if ch, ok := byType["slack|T02"]; !ok || ch.BotToken != "tok_B" || ch.PlatformUserID != "su_B" {
		t.Errorf("slack T02 row missing or fields wrong: %+v", ch)
	}

	// Enabled flag carried over.
	if ch, ok := byType["line|cred_line"]; ok {
		if ch.Enabled {
			t.Errorf("disabled config migrated as enabled: %+v", ch)
		}
	} else {
		t.Errorf("disabled row migrated too: %v", rows)
	}

	// Old configs rows preserved for rollback.
	var cfgCount int
	if err := db.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM configs WHERE kind='channel'`).Scan(&cfgCount); err != nil {
		t.Fatalf("count configs: %v", err)
	}
	if cfgCount != 3 {
		t.Errorf("configs rows not preserved: %d remain; want 3", cfgCount)
	}

	// Idempotency: a third Migrate must skip (channels already has data).
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if again, err := db.ListAllChannels(ctx); err != nil || len(again) != 4 {
		t.Errorf("channels after second migrate = %d rows, err=%v; want still 4", len(again), err)
	}
}

// TestChannelTimestamps ensures SaveChannel stamps updated_at and keeps a
// stable created_at across upserts.
func TestChannelTimestamps(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	ch := &ChannelRecord{Type: "feishu", AccountID: "cli_1", BotToken: "tok"}
	if err := db.SaveChannel(ctx, ch); err != nil {
		t.Fatalf("save: %v", err)
	}
	if ch.CreatedAt.IsZero() || ch.UpdatedAt.IsZero() {
		t.Fatalf("timestamps not stamped: %+v", ch)
	}
	created := ch.CreatedAt
	if ch.UpdatedAt.Before(created) {
		t.Errorf("updated_at before created_at")
	}
	time.Sleep(2 * time.Millisecond)
	ch.BotToken = "tok2"
	if err := db.SaveChannel(ctx, ch); err != nil {
		t.Fatalf("resave: %v", err)
	}
	if !ch.CreatedAt.Equal(created) {
		t.Errorf("created_at changed across upsert: %v -> %v", created, ch.CreatedAt)
	}
	if !ch.UpdatedAt.After(created) {
		t.Errorf("updated_at not advanced after upsert: %v", ch.UpdatedAt)
	}
}
