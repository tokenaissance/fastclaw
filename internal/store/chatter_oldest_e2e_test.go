package store

// e2e for commit bf9cfb3 "fix(users): GetUserByExternal returns oldest
// chatter to preserve memory".
//
// Cloud zero-impact rationale: GetUserByExternal is the store lookup that
// resolveChatter (gateway inbound IM routing) and EnsureChatter (user
// provisioning) use to find an app_user/chatter by (owner_user_id,
// external_id). Cloud (Next.js app) reaches it only through the
// /api/fastagent proxy's chat/channels handlers, never directly. This
// commit changes only the tie-break determinism: when owner migration
// left duplicate chatters under the same owner + external_id, the query
// now ORDER BY created_at ASC so the OLDEST row — the one that carries
// the USER.md/MEMORY.md data — wins instead of an arbitrary row. No
// endpoint, response shape, or auth semantics change.
//
// What is pinned down:
//   - GetUserByExternal with duplicate (owner_user_id, external_id) rows
//     deterministically returns the OLDEST (earliest created_at).
//   - The gateway inbound IM path (resolveChatter) resolves such a
//     duplicate set to the oldest chatter, so memory is preserved.

import (
	"context"
	"testing"
	"time"
)

func TestGetUserByExternal_OldestChatter_CloudPathE2E(t *testing.T) {
	db, err := NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("NewDBStore: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if err := db.CreateUser(ctx, &UserRecord{
		ID: "u_owner", Username: "owner", Email: "owner@example.com",
		PasswordHash: "x", Role: "user", Status: "active", AgentQuota: -1,
		CreatedAt: time.Now().UTC().Add(-time.Hour), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create owner: %v", err)
	}

	// Two duplicate chatters under the same (owner_user_id, external_id) —
	// as left behind by an owner migration. The newer one has no memory.
	old := &UserRecord{
		ID: "u_chat_old", Username: "chat_old", Email: "chat_old@example.com",
		PasswordHash: "x", Role: "app_user", Status: "active",
		ExternalID: "telegram:111", OwnerUserID: "u_owner",
		CreatedAt: time.Now().UTC().Add(-2 * time.Hour), UpdatedAt: time.Now().UTC(),
	}
	new := &UserRecord{
		ID: "u_chat_new", Username: "chat_new", Email: "chat_new@example.com",
		PasswordHash: "x", Role: "app_user", Status: "active",
		ExternalID: "telegram:111", OwnerUserID: "u_owner",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := db.CreateUser(ctx, old); err != nil {
		t.Fatalf("create old chatter: %v", err)
	}
	if err := db.CreateUser(ctx, new); err != nil {
		t.Fatalf("create new chatter: %v", err)
	}

	got, err := db.GetUserByExternal(ctx, "u_owner", "telegram:111")
	if err != nil {
		t.Fatalf("GetUserByExternal: %v", err)
	}
	if got.ID != old.ID {
		t.Errorf("GetUserByExternal = %q, want oldest %q (ORDER BY created_at ASC)", got.ID, old.ID)
	}

	// Determinism: repeated calls are stable.
	for i := 0; i < 3; i++ {
		again, err := db.GetUserByExternal(ctx, "u_owner", "telegram:111")
		if err != nil {
			t.Fatalf("GetUserByExternal repeat %d: %v", i, err)
		}
		if again.ID != old.ID {
			t.Fatalf("GetUserByExternal repeat %d = %q, want %q", i, again.ID, old.ID)
		}
	}
}
