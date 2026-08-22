package store

import (
	"context"
	"testing"
)

// e2e for commit fb7d32c "refactor(channels): single-write to channels
// table, remove configs dual-write" — adopted portion only.
//
// Upstream fb7d32c bundles two things: (1) a genuine migration-order fix
// (add the shared_identity column BEFORE migrateChannelsFromConfigs copies
// configs rows, so SaveChannel during the copy no longer references a
// column that doesn't exist yet) and (2) a single-write refactor that
// REMOVES the configs dual-write/fallback. The fork's #55 design
// deliberately keeps the dual-write + configs fallback (configs channel
// rows are live data for LookupChannelByCredential routing — 20 call
// sites across gateway/setup/store — and #46 already decided the fork
// migration never deletes the configs source). So (2) is NOT adopted.
//
// This file pins (1), the migration-order fix. Without the reorder, an
// install whose channels table predates the shared_identity column (empty
// table + configs kind='channel' rows) would have its copy silently fail
// on the first post-upgrade startup (SaveChannel logs the missing-column
// error and continues), only self-healing on a second restart once
// migrateChannelsAddSharedIdentity had added the column. With the reorder
// the copy succeeds on the first pass.
//
// What is pinned down:
//   - A pre-column channels table + a configs kind='channel' row migrate
//     into channels in a SINGLE Migrate() pass (column added before copy).
//   - Cloud zero-impact: the migration runs at startup for every install;
//     no endpoint, response shape, or auth semantics change. The
//     /channels endpoint surface is unchanged (established in the #55
//     review).

func TestMigrate_ChannelsSharedIdentityBeforeConfigsCopy_CloudPathE2E(t *testing.T) {
	db, err := NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("NewDBStore: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	// Simulate an install created BEFORE shared_identity was added to the
	// canonical channels schema: channels exists but lacks the column, and
	// configs still carries a legacy kind='channel' row (the configs
	// dual-write source the fork keeps).
	raw := db.DB()
	stmts := []string{
		`CREATE TABLE channels (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL DEFAULT '',
			agent_id TEXT NOT NULL DEFAULT '',
			type TEXT NOT NULL,
			account_id TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			bot_token TEXT NOT NULL DEFAULT '',
			base_url TEXT NOT NULL DEFAULT '',
			platform_user_id TEXT NOT NULL DEFAULT '',
			data TEXT NOT NULL DEFAULT '{}',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE (type, account_id)
		)`,
		`CREATE TABLE configs (
			id TEXT PRIMARY KEY,
			kind TEXT NOT NULL,
			scope TEXT NOT NULL DEFAULT '',
			user_id TEXT NOT NULL DEFAULT '',
			agent_id TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL,
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			credential_key TEXT NOT NULL DEFAULT '',
			data TEXT NOT NULL DEFAULT '{}',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE (kind, user_id, agent_id, name)
		)`,
		`INSERT INTO configs (id, kind, user_id, agent_id, name, enabled, credential_key, data)
		 VALUES ('cfg_legacy', 'channel', 'owner_legacy', 'agent_legacy', 'telegram', TRUE, 'bot_legacy', '{"botToken":"t_legacy"}')`,
	}
	for _, stmt := range stmts {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed old-schema tables: %v\nSQL: %s", err, stmt)
		}
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// The configs channel row must have migrated into channels in this
	// single pass. Before the reorder, SaveChannel's INSERT (which lists
	// the shared_identity column) would have hit "no such column" and been
	// logged + skipped, leaving channels empty until a second restart.
	rows, err := db.ListAllChannels(ctx)
	if err != nil {
		t.Fatalf("ListAllChannels: %v", err)
	}
	found := false
	for _, ch := range rows {
		if ch.Type == "telegram" && ch.AccountID == "bot_legacy" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("configs channel row not migrated into channels: got %+v", rows)
	}

	// The column must now exist for the copy to have used it.
	has, err := db.tableHasColumn(ctx, "channels", "shared_identity")
	if err != nil {
		t.Fatalf("tableHasColumn: %v", err)
	}
	if !has {
		t.Fatal("channels.shared_identity column missing after Migrate")
	}
}
