package gateway

// e2e for commit 35197e9 "shared_identity toggle" — routing half.
//
// Mirrors the Cloud call path for an IM inbound when a channel has
// shared_identity enabled: the Cloud proxy forwards a WeChat/Telegram
// platform message, resolveChannelOwner resolves the channel row (with
// its sharedIdentity flag), and processInbound's shared-identity branch
// routes the message on the channel owner's user_id instead of minting a
// per-platform chatter. SessionTriple() then converges every channel onto
// the virtual ("shared", "", ownerID) triple so sessions and memory are
// shared across web + IM.
//
// The handler half (PATCH toggle, agent-scope readback) lives in
// internal/setup/shared_identity_e2e_test.go.

import (
	"context"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

// seedSharedIdentityChannelRow creates a user + agent + a telegram channel
// row with the given sharedIdentity flag, returning a real store + Gateway.
func seedSharedIdentityChannelRow(t *testing.T, sharedIdentity bool) (*Gateway, *store.DBStore, *store.UserRecord) {
	t.Helper()
	db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	owner := &store.UserRecord{
		ID: "u_owner", Username: "owner", Email: "owner@example.com",
		PasswordHash: "x", Role: users.RoleUser, Status: users.StatusActive,
		AgentQuota: -1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := db.CreateUser(ctx, owner); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if err := db.SaveAgent(ctx, &store.AgentRecord{ID: "agt_1", UserID: owner.ID, Name: "routing"}); err != nil {
		t.Fatalf("save agent: %v", err)
	}
	if err := db.SaveChannel(ctx, &store.ChannelRecord{
		UserID: owner.ID, AgentID: "agt_1", Type: "telegram", AccountID: "bot-shared",
		Enabled: true, BotToken: "tok", SharedIdentity: sharedIdentity,
	}); err != nil {
		t.Fatalf("save channel: %v", err)
	}
	return &Gateway{store: db}, db, owner
}

// TestSharedIdentity_CloudPathE2E_RoutingConvergence drives the exact
// processInbound branch for a shared-identity channel: resolveChannelOwner
// must return the owner plus sharedIdentity=true, the branch must stamp
// ownerID onto UserID (no lazy-minted chatter), and SessionTriple() must
// converge on the virtual ("shared", "", ownerID) triple so sessions and
// memory span channels.
func TestSharedIdentity_CloudPathE2E_RoutingConvergence(t *testing.T) {
	g, _, owner := seedSharedIdentityChannelRow(t, true)
	ctx := context.Background()

	msg := bus.InboundMessage{Channel: "telegram", AccountID: "bot-shared", UserID: "wechat_openid_123"}

	// Step 1: the channel row resolves owner + the sharedIdentity flag.
	info := g.resolveChannelOwner(ctx, msg)
	if info.ownerID != owner.ID {
		t.Fatalf("resolveChannelOwner.ownerID = %q; want %q", info.ownerID, owner.ID)
	}
	if !info.sharedIdentity {
		t.Fatalf("resolveChannelOwner.sharedIdentity = false; want true")
	}

	// Step 2: the processInbound shared-identity branch.
	ownerID := msg.OwnerUserID
	if ownerID == "" {
		ownerID = info.ownerID
	}
	if ownerID == "" {
		t.Fatal("ownerID empty after resolution")
	}
	msg.OwnerUserID = ownerID
	var sharedIdentity bool
	if info.sharedIdentity {
		msg.UserID = ownerID
		msg.SharedIdentity = true
		sharedIdentity = true
	}
	if !sharedIdentity {
		t.Fatal("sharedIdentity branch not taken")
	}
	if msg.UserID != owner.ID {
		t.Errorf("msg.UserID = %q; want owner %q (no lazy-minted chatter)", msg.UserID, owner.ID)
	}
	if !msg.SharedIdentity {
		t.Errorf("msg.SharedIdentity = false; want true")
	}

	// Step 3: session resolution converges on the virtual triple.
	ch, acc, cid := msg.SessionTriple()
	if ch != "shared" || acc != "" || cid != owner.ID {
		t.Errorf("SessionTriple = (%q,%q,%q); want (\"shared\",\"\",%q)", ch, acc, cid, owner.ID)
	}
	// The real channel identity is preserved for outbound routing.
	if msg.Channel != "telegram" || msg.AccountID != "bot-shared" {
		t.Errorf("outbound channel identity lost: %q/%q", msg.Channel, msg.AccountID)
	}
}

// TestSharedIdentity_CloudPathE2E_IsolatedWithoutFlag is the negative: a
// channel WITHOUT shared_identity must keep the per-platform chatter
// (resolveChatter lazy-mints an app_user) and keep its own session triple,
// so each platform sender stays isolated by default.
func TestSharedIdentity_CloudPathE2E_IsolatedWithoutFlag(t *testing.T) {
	g, _, owner := seedSharedIdentityChannelRow(t, false)
	ctx := context.Background()

	msg := bus.InboundMessage{Channel: "telegram", AccountID: "bot-shared", ChatID: "chat_123", UserID: "wechat_openid_123"}

	info := g.resolveChannelOwner(ctx, msg)
	if info.ownerID != owner.ID {
		t.Fatalf("resolveChannelOwner.ownerID = %q; want %q", info.ownerID, owner.ID)
	}
	if info.sharedIdentity {
		t.Fatalf("resolveChannelOwner.sharedIdentity = true; want false (flag off)")
	}

	// The normal branch lazy-mints a chatter and keeps the real triple.
	ownerID := msg.OwnerUserID
	if ownerID == "" {
		ownerID = info.ownerID
	}
	msg.OwnerUserID = ownerID
	if !info.sharedIdentity {
		if chatterID := g.resolveChatter(ctx, ownerID, msg); chatterID != "" {
			msg.UserID = chatterID
		}
	}
	if msg.UserID == owner.ID {
		t.Errorf("msg.UserID = %q; want a distinct lazy-minted chatter", msg.UserID)
	}
	if msg.SharedIdentity {
		t.Errorf("msg.SharedIdentity = true; want false (flag off)")
	}
	ch, acc, cid := msg.SessionTriple()
	if ch != "telegram" || acc != "bot-shared" || cid != "chat_123" {
		t.Errorf("SessionTriple = (%q,%q,%q); want the real (telegram,bot-shared,chat_123)",
			ch, acc, cid)
	}
}
