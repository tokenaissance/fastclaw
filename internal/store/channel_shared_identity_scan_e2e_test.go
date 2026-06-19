package store

import (
	"context"
	"testing"
)

// e2e for commit 3df4341 "fix(channels): scan shared_identity as int for
// PostgreSQL compatibility".
//
// Cloud zero-impact rationale: this is the read-side companion to #35
// (256a610). scanChannelRow / scanChannels now scan the INTEGER
// shared_identity column into an intermediate int and convert `!= 0` to the
// bool ChannelRecord.SharedIdentity — PostgreSQL INTEGER columns don't
// implicitly convert to Go bool on scan in all driver versions. This mirrors
// the int-based write path in SaveChannel. GetChannel / ListChannels are
// reached via the per-agent /channels handlers, whose Cloud endpoint surface
// is unchanged (established in the #55 review); no endpoint, response shape,
// or auth semantics change.
//
// What is pinned down:
//   - GetChannel (scanChannelRow) round-trips SharedIdentity true/false
//     through the bool field.
//   - ListChannels (scanChannels) round-trips both values through the slice
//     path — the bulk scan used by the channel list handlers.

func TestChannel_SharedIdentityScan_CloudPathE2E(t *testing.T) {
	db, err := NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("NewDBStore: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// SharedIdentity=true → both scan paths read it back as bool true.
	shared := &ChannelRecord{Type: "feishu", AccountID: "bot_scan", AgentID: "agent_scan", UserID: "owner_scan", SharedIdentity: true}
	if err := db.SaveChannel(ctx, shared); err != nil {
		t.Fatalf("SaveChannel(shared): %v", err)
	}
	byID, err := db.GetChannel(ctx, shared.ID)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	if !byID.SharedIdentity {
		t.Errorf("GetChannel.SharedIdentity = %v, want true (scanChannelRow)", byID.SharedIdentity)
	}
	byList, err := db.ListChannels(ctx, "owner_scan", "agent_scan")
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	if len(byList) != 1 || !byList[0].SharedIdentity {
		t.Errorf("ListChannels[0].SharedIdentity = %+v, want [single true] (scanChannels)", byList)
	}

	// Upsert same (type, account_id) to false → both scan paths flip.
	private := &ChannelRecord{Type: "feishu", AccountID: "bot_scan", AgentID: "agent_scan2", UserID: "owner_scan2", SharedIdentity: false}
	if err := db.SaveChannel(ctx, private); err != nil {
		t.Fatalf("SaveChannel(private): %v", err)
	}
	byID, err = db.GetChannel(ctx, shared.ID)
	if err != nil {
		t.Fatalf("GetChannel(flipped): %v", err)
	}
	if byID.SharedIdentity {
		t.Errorf("GetChannel.SharedIdentity after flip = %v, want false", byID.SharedIdentity)
	}
	byList, err = db.ListChannels(ctx, "owner_scan2", "agent_scan2")
	if err != nil {
		t.Fatalf("ListChannels(flipped): %v", err)
	}
	if len(byList) != 1 || byList[0].SharedIdentity {
		t.Errorf("ListChannels[0].SharedIdentity after flip = %+v, want [single false]", byList)
	}
}
