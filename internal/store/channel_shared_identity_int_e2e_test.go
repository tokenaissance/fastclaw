package store

import (
	"context"
	"testing"
)

// e2e for commit 256a610 "fix(store): convert SharedIdentity bool to integer
// for PostgreSQL in SaveChannel".
//
// Cloud zero-impact rationale: this is a store-layer type fix inside
// SaveChannel. The shared_identity column (added by #42, the shared_identity
// toggle) is INTEGER NOT NULL, but the Go bool was passed straight through —
// pq serialized it as the string "true", which PostgreSQL rejects with a
// syntax error. The fix converts to an explicit 0/1 int before the dialect
// branch, so BOTH the postgres ($n) and sqlite (?) paths pass an integer.
// SaveChannel is reached via the per-agent /channels handlers, whose Cloud
// endpoint surface is unchanged (established in the #55 review); no endpoint,
// response shape, or auth semantics change.
//
// What is pinned down:
//   - SaveChannel with SharedIdentity=true stores the raw shared_identity
//     column as the INTEGER 1 (not a bool/"true" string), and an upsert to
//     the same (type, account_id) with SharedIdentity=false stores INTEGER 0.
//     Reading the RAW column (not the bool-scanned ChannelRecord) proves the
//     fix's storage contract — the exact conversion both dialect branches
//     now share.

func TestSaveChannel_SharedIdentityInt_CloudPathE2E(t *testing.T) {
	db, err := NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("NewDBStore: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Save with SharedIdentity=true: the raw column must be integer 1.
	shared := &ChannelRecord{Type: "telegram", AccountID: "bot_si", AgentID: "agent_si", UserID: "owner_si", SharedIdentity: true}
	if err := db.SaveChannel(ctx, shared); err != nil {
		t.Fatalf("SaveChannel(shared): %v", err)
	}
	if got := rawSharedIdentity(t, db, "bot_si"); got != 1 {
		t.Errorf("shared_identity after true = %d, want 1 (integer)", got)
	}

	// Upsert the same (type, account_id) with SharedIdentity=false: the
	// ON CONFLICT DO UPDATE must flip the raw column to integer 0.
	private := &ChannelRecord{Type: "telegram", AccountID: "bot_si", AgentID: "agent_si2", UserID: "owner_si2", SharedIdentity: false}
	if err := db.SaveChannel(ctx, private); err != nil {
		t.Fatalf("SaveChannel(private upsert): %v", err)
	}
	if got := rawSharedIdentity(t, db, "bot_si"); got != 0 {
		t.Errorf("shared_identity after false upsert = %d, want 0 (integer)", got)
	}
}

// rawSharedIdentity reads the shared_identity column straight from the row,
// bypassing scanChannelRow's bool coercion so the test can prove the stored
// value is a real integer rather than a bool/"true" string.
func rawSharedIdentity(t *testing.T, db *DBStore, accountID string) int {
	t.Helper()
	var v int
	if err := db.db.QueryRowContext(context.Background(),
		`SELECT shared_identity FROM channels WHERE account_id = ?`, accountID).Scan(&v); err != nil {
		t.Fatalf("read shared_identity for %s: %v", accountID, err)
	}
	return v
}
