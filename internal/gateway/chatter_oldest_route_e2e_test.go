package gateway

// Gateway-level companion to bf9cfb3 (#29, GetUserByExternal returns
// oldest chatter): the Cloud inbound IM path (resolveChatter) must land
// on the OLDEST duplicate chatter so USER.md/MEMORY.md is preserved.
// Same DB/accounts wiring as the other gateway e2e tests; duplicate
// (owner_user_id, external_id) rows are seeded directly, as an owner
// migration would leave them.

import (
	"context"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

func TestResolveChatter_OldestDuplicate_CloudPathE2E(t *testing.T) {
	db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("NewDBStore: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if err := db.CreateUser(ctx, &store.UserRecord{
		ID: "u_owner", Username: "owner", Email: "owner@example.com",
		PasswordHash: "x", Role: users.RoleUser, Status: users.StatusActive,
		AgentQuota: -1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	accts, err := users.NewAccounts(db)
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	g := &Gateway{store: db, accounts: accts}

	// Duplicate chatters under the same owner + external_id (owner
	// migration artifact): oldest carries the memory.
	old := &store.UserRecord{
		ID: "u_chat_old", Username: "chat_old", Email: "chat_old@example.com",
		PasswordHash: "x", Role: users.RoleAppUser, Status: users.StatusActive,
		ExternalID: "telegram:111", OwnerUserID: "u_owner",
		CreatedAt: time.Now().UTC().Add(-2 * time.Hour), UpdatedAt: time.Now().UTC(),
	}
	newer := &store.UserRecord{
		ID: "u_chat_new", Username: "chat_new", Email: "chat_new@example.com",
		PasswordHash: "x", Role: users.RoleAppUser, Status: users.StatusActive,
		ExternalID: "telegram:111", OwnerUserID: "u_owner",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := db.CreateUser(ctx, old); err != nil {
		t.Fatalf("create old chatter: %v", err)
	}
	if err := db.CreateUser(ctx, newer); err != nil {
		t.Fatalf("create new chatter: %v", err)
	}

	msg := bus.InboundMessage{Channel: "telegram", AccountID: "bot-x", UserID: "111", SenderName: "Dup"}
	if got := g.resolveChatter(ctx, "u_owner", msg); got != old.ID {
		t.Fatalf("resolveChatter = %q, want oldest duplicate %q (memory preserved)", got, old.ID)
	}
}
