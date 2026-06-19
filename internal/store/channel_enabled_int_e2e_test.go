package store

import (
	"context"
	"testing"
)

// e2e for commit 47255ef "fix(channels): convert Enabled bool to int for
// PostgreSQL INTEGER column".
//
// Cloud zero-impact rationale: this is the write+read side companion to
// #35 (256a610, SharedIdentity) applied to the `enabled` column. Both
// Enabled and SharedIdentity are Go bool fields written to PostgreSQL
// INTEGER columns; pq serializes a bool as the string "true"/"false",
// which INTEGER rejects. The fix converts BOTH to int 0/1 before the
// dialect branch and scans both via int on read (scanChannelRow /
// scanChannels). GetChannel / ListChannels are reached via the per-agent
// /channels handlers, whose Cloud endpoint surface is unchanged
// (established in the #55 review); no endpoint, response shape, or auth
// semantics change.
//
// What is pinned down:
//   - SaveChannel with Enabled=true stores the raw `enabled` column as
//     INTEGER 1, and an upsert to the same (type, account_id) with
//     Enabled=false stores INTEGER 0 (write side — raw column, not the
//     bool-scanned field).
//   - GetChannel (scanChannelRow) and ListChannels (scanChannels) read
//     the int column back as the bool field true/false (read side).

func TestSaveChannel_EnabledInt_CloudPathE2E(t *testing.T) {
	db, err := NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("NewDBStore: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Save with Enabled=true: raw column must be integer 1, both scan paths
	// read back bool true.
	on := &ChannelRecord{Type: "telegram", AccountID: "bot_en", AgentID: "agent_en", UserID: "owner_en", Enabled: true}
	if err := db.SaveChannel(ctx, on); err != nil {
		t.Fatalf("SaveChannel(enabled): %v", err)
	}
	if got := rawChannelEnabled(t, db, "bot_en"); got != 1 {
		t.Errorf("enabled after true = %d, want 1 (integer)", got)
	}
	byID, err := db.GetChannel(ctx, on.ID)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	if !byID.Enabled {
		t.Errorf("GetChannel.Enabled = %v, want true (scanChannelRow)", byID.Enabled)
	}
	byList, err := db.ListChannels(ctx, "owner_en", "agent_en")
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	if len(byList) != 1 || !byList[0].Enabled {
		t.Errorf("ListChannels[0].Enabled = %+v, want [single true] (scanChannels)", byList)
	}

	// Upsert the same (type, account_id) with Enabled=false: the
	// ON CONFLICT DO UPDATE must flip raw column to integer 0 and both
	// scan paths to bool false.
	off := &ChannelRecord{Type: "telegram", AccountID: "bot_en", AgentID: "agent_en2", UserID: "owner_en2", Enabled: false}
	if err := db.SaveChannel(ctx, off); err != nil {
		t.Fatalf("SaveChannel(enabled false upsert): %v", err)
	}
	if got := rawChannelEnabled(t, db, "bot_en"); got != 0 {
		t.Errorf("enabled after false upsert = %d, want 0 (integer)", got)
	}
	byID, err = db.GetChannel(ctx, on.ID)
	if err != nil {
		t.Fatalf("GetChannel(flipped): %v", err)
	}
	if byID.Enabled {
		t.Errorf("GetChannel.Enabled after flip = %v, want false", byID.Enabled)
	}
	byList, err = db.ListChannels(ctx, "owner_en2", "agent_en2")
	if err != nil {
		t.Fatalf("ListChannels(flipped): %v", err)
	}
	if len(byList) != 1 || byList[0].Enabled {
		t.Errorf("ListChannels[0].Enabled after flip = %+v, want [single false]", byList)
	}
}

// rawChannelEnabled reads the `enabled` column straight from the row,
// bypassing scanChannelRow/scanChannels' bool coercion so the test can prove
// the stored value is a real integer rather than a bool/"true" string.
func rawChannelEnabled(t *testing.T, db *DBStore, accountID string) int {
	t.Helper()
	var v int
	if err := db.db.QueryRowContext(context.Background(),
		`SELECT enabled FROM channels WHERE account_id = ?`, accountID).Scan(&v); err != nil {
		t.Fatalf("read enabled for %s: %v", accountID, err)
	}
	return v
}
