package gateway

// Gateway-level e2e for the feishu + IM-chatter commits that landed
// upstream:
//
//   - c90fc25 (json 2.0 cards) and 91434c0 (mentions for group routing)
//     — the mention-carrying group message produced by the feishu
//     adapter must survive the gateway's identity chain
//     (resolveChannelOwner → resolveChatter) with PeerKind + Mentions
//     intact, because routeGroup consumes msg.Mentions to pick the
//     addressed agent.
//   - 3107659 (isolate IM chatter memory by sender) — resolveChatter
//     must mint a distinct app_user per (channel, sender); the same raw
//     platform user id on two channels must NOT share a USER.md /
//     MEMORY.md row.
//
// Everything runs against an in-memory sqlite store; no real network,
// no agent runtime started. The feishu adapter itself is exercised in
// internal/channels/feishu_e2e_test.go; here we drive the exact
// InboundMessage shape it emits through the routing seam.

import (
	"context"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

// TestResolveChatterSameUserIDSeparateAcrossChannels covers upstream
// 3107659 at the cross-channel boundary: one platform user id arriving
// on different channels must resolve to different app_users (so memory
// stays isolated per IM surface), while re-resolving the same sender on
// the same channel — even via a different bot account — stays stable.
func TestResolveChatterSameUserIDSeparateAcrossChannels(t *testing.T) {
	db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()

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
	accts, err := users.NewAccounts(db)
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	g := &Gateway{store: db, accounts: accts}

	// Same raw platform user id "111" on three different channels.
	telegram := bus.InboundMessage{Channel: "telegram", AccountID: "bot-a", UserID: "111", SenderName: "Same"}
	discord := bus.InboundMessage{Channel: "discord", AccountID: "bot-d", UserID: "111", SenderName: "Same"}
	feishu := bus.InboundMessage{Channel: "feishu", AccountID: "cli_a", UserID: "111", SenderName: "Same"}

	tgID := g.resolveChatter(ctx, owner.ID, telegram)
	dcID := g.resolveChatter(ctx, owner.ID, discord)
	fsID := g.resolveChatter(ctx, owner.ID, feishu)
	if tgID == "" || tgID == owner.ID {
		t.Fatalf("telegram sender should resolve to app_user, got %q", tgID)
	}
	if dcID == "" || dcID == owner.ID {
		t.Fatalf("discord sender should resolve to app_user, got %q", dcID)
	}
	if fsID == "" || fsID == owner.ID {
		t.Fatalf("feishu sender should resolve to app_user, got %q", fsID)
	}
	if tgID == dcID || tgID == fsID || dcID == fsID {
		t.Fatalf("same user id on different channels merged into one app_user: %s/%s/%s", tgID, dcID, fsID)
	}

	// Same channel + same sender → stable, and stable even across a bot
	// reconnection (accountID change): identity is channel+userID, not
	// account-scoped, so per-sender memory survives bot swaps.
	if again := g.resolveChatter(ctx, owner.ID, telegram); again != tgID {
		t.Fatalf("same channel/sender unstable: got %q want %q", again, tgID)
	}
	reconnect := bus.InboundMessage{Channel: "telegram", AccountID: "bot-b", UserID: "111", SenderName: "Same"}
	if got := g.resolveChatter(ctx, owner.ID, reconnect); got != tgID {
		t.Fatalf("bot reconnection changed app_user: got %q want %q", got, tgID)
	}

	// External ids are channel-scoped so USER.md/MEMORY.md rows can't
	// collide across platforms.
	tgAcc, err := db.GetUser(ctx, tgID)
	if err != nil {
		t.Fatalf("get telegram app_user: %v", err)
	}
	if tgAcc.ExternalID != "telegram:111" {
		t.Errorf("telegram external_id = %q; want telegram:111", tgAcc.ExternalID)
	}
	if tgAcc.OwnerUserID != owner.ID {
		t.Errorf("telegram owner_user_id = %q; want %q", tgAcc.OwnerUserID, owner.ID)
	}
	fsAcc, err := db.GetUser(ctx, fsID)
	if err != nil {
		t.Fatalf("get feishu app_user: %v", err)
	}
	if fsAcc.ExternalID != "feishu:111" {
		t.Errorf("feishu external_id = %q; want feishu:111", fsAcc.ExternalID)
	}
}

// TestFeishuGroupMessageIdentityChainE2E is the gateway-level e2e for
// upstream 91434c0 (mentions preserved for group routing) driven the way
// a real feishu webhook would: the adapter's InboundMessage shape (the
// adapter itself is unit-tested in internal/channels) crosses
// resolveChannelOwner → resolveChatter and must keep PeerKind=group and
// Mentions=@bot intact so routeGroup can select the addressed agent.
func TestFeishuGroupMessageIdentityChainE2E(t *testing.T) {
	db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()

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
	if err := db.SaveAgent(ctx, &store.AgentRecord{ID: "agt_1", UserID: owner.ID, Name: "assistant"}); err != nil {
		t.Fatalf("save agent: %v", err)
	}
	// Feishu app row in the channels table: this is what a real
	// connect flow persists for the bot, and what resolveChannelOwner
	// keys on.
	if err := db.SaveChannel(ctx, &store.ChannelRecord{
		UserID: owner.ID, AgentID: "agt_1", Type: "feishu", AccountID: "cli_e2e", Enabled: true,
	}); err != nil {
		t.Fatalf("save feishu channel: %v", err)
	}
	accts, err := users.NewAccounts(db)
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	g := &Gateway{store: db, accounts: accts}

	// The exact InboundMessage the feishu adapter emits for a group
	// @-mention (see dispatchInbound in internal/channels/feishu.go).
	msg := bus.InboundMessage{
		Channel:    "feishu",
		AccountID:  "cli_e2e",
		ChatID:     "oc_group",
		UserID:     "ou_sender",
		MessageID:  "om_g1",
		Text:       "@机器人 你好",
		PeerKind:   "group",
		SenderName: "张三",
		Mentions:   []string{"机器人"},
	}

	// Step 1: resolve the owning user from the channels table.
	ownerInfo := g.resolveChannelOwner(ctx, msg)
	if ownerInfo.ownerID != owner.ID {
		t.Fatalf("resolveChannelOwner = %q; want owner %q", ownerInfo.ownerID, owner.ID)
	}
	ownerID := ownerInfo.ownerID

	// Step 2: normalize the platform sender id into a fastagent app_user.
	chatterID := g.resolveChatter(ctx, ownerID, msg)
	if chatterID == "" || chatterID == owner.ID {
		t.Fatalf("resolveChatter = %q; want app_user distinct from owner", chatterID)
	}
	// Stable on re-resolution (lazy-mint is idempotent).
	if again := g.resolveChatter(ctx, ownerID, msg); again != chatterID {
		t.Fatalf("resolveChatter unstable: got %q want %q", again, chatterID)
	}
	chatter, err := db.GetUser(ctx, chatterID)
	if err != nil {
		t.Fatalf("get chatter: %v", err)
	}
	if chatter.ExternalID != "feishu:ou_sender" {
		t.Errorf("chatter external_id = %q; want feishu:ou_sender", chatter.ExternalID)
	}

	// Step 3: the routing inputs routeGroup needs must have survived the
	// identity chain untouched — mentions drive agentByMention, peer_kind
	// selects the group branch.
	if msg.PeerKind != "group" {
		t.Errorf("peer_kind lost through identity chain: %q", msg.PeerKind)
	}
	if len(msg.Mentions) != 1 || msg.Mentions[0] != "机器人" {
		t.Errorf("mentions lost through identity chain: %#v", msg.Mentions)
	}

	// Unknown bot account on the same channel type → no owner, never
	// silently routed to the default identity.
	unknown := bus.InboundMessage{Channel: "feishu", AccountID: "cli_other", UserID: "ou_x"}
	if got := g.resolveChannelOwner(ctx, unknown); got.ownerID != "" {
		t.Errorf("resolveChannelOwner(unknown account) = %q; want empty", got.ownerID)
	}
}
