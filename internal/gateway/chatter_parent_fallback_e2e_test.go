package gateway

// e2e for commit 81c90af "fix(chatter): fallback to parent user when
// looking up historical chatters".
//
// Cloud zero-impact rationale: resolveChatter is the gateway's inbound
// IM routing seam (resolveChannelOwner → resolveChatter → routeDM/Group).
// Cloud (Next.js app) talks to FastAgent through the /api/fastagent proxy
// (chat/history/sessions/channels handlers) and never invokes this
// unexported gateway function directly — grep of Cloud src/ shows zero
// references. The behavior change is entirely backend: when a channel's
// binding moves from a web user to one of its app_users (e.g. WeClaw API
// key re-binding), an inbound message on the re-bound channel walks up to
// the parent user, finds the historical chatter (preserving its
// USER.md/MEMORY.md row) and returns it instead of minting a fresh one.
// No Cloud endpoint, response shape, or auth semantics change.
//
// Known upstream limitation (faithful merge, documented not fixed):
// the commit's migration step sets acc.OwnerUserID then calls
// store.UpdateUser, but UpdateUser's SQL predates the owner_user_id
// column and does NOT persist it — in BOTH upstream and fork. So the
// "migrate so subsequent lookups are direct" optimization is a silent
// no-op: on a re-bound channel every message re-walks the parent chain.
// Correctness is unaffected (the historical chatter is still found and
// its memory preserved each time); only the intended one-shot fast path
// doesn't materialize. We mirror upstream verbatim rather than widen
// UpdateUser (store-wide blast radius) inside this commit's review.
//
// What is pinned down:
//   - resolveChatter(ownerID=app_user) where a historical chatter exists
//     under the app_user's parent (web user) returns that historical
//     chatter (same row → same USER.md/MEMORY.md), NOT a fresh mint.
//   - The resolution is stable across repeated messages.
//   - The legacy external_id variant (channel:accountID:userID) also
//     walks up to the parent and resolves to the historical row.
//   - A brand-new sender with no historical row still mints a fresh
//     app_user (fallback does not corrupt the mint path).

import (
	"context"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

func TestChatter_ParentFallback_CloudPathE2E(t *testing.T) {
	db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("NewDBStore: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Parent web user that originally owned the channel + chatter.
	parent := &store.UserRecord{
		ID:           "u_parent",
		Username:     "parent",
		Email:        "parent@example.com",
		PasswordHash: "x",
		Role:         users.RoleUser,
		Status:       users.StatusActive,
		AgentQuota:   -1,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	if err := db.CreateUser(ctx, parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	accts, err := users.NewAccounts(db)
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	g := &Gateway{store: db, accounts: accts}

	// Historical chatter minted under the PARENT (before the binding moved
	// to an app_user) — same channel + same platform user id.
	hist := bus.InboundMessage{Channel: "telegram", AccountID: "bot-old", UserID: "111", SenderName: "OldChatter"}
	histID := g.resolveChatter(ctx, parent.ID, hist)
	if histID == "" || histID == parent.ID {
		t.Fatalf("historical chatter should be an app_user under parent, got %q", histID)
	}
	histAcc, err := db.GetUser(ctx, histID)
	if err != nil {
		t.Fatalf("get historical chatter: %v", err)
	}
	if histAcc.OwnerUserID != parent.ID {
		t.Fatalf("historical chatter owner = %q, want %q", histAcc.OwnerUserID, parent.ID)
	}

	// The channel is now bound by an app_user whose owner is the parent
	// (WeClaw API key re-binding moves binding web-user → app_user).
	appBinder := &store.UserRecord{
		ID:           "u_app",
		Username:     "appbot",
		Email:        "appbot@example.com",
		PasswordHash: "x",
		Role:         users.RoleAppUser,
		Status:       users.StatusActive,
		ExternalID:   "telegram:appbot",
		OwnerUserID:  parent.ID,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	if err := db.CreateUser(ctx, appBinder); err != nil {
		t.Fatalf("create app binder: %v", err)
	}

	// Inbound message on the re-bound channel: must resolve to the
	// HISTORICAL chatter (same row → same USER.md/MEMORY.md), not a mint.
	remsg := bus.InboundMessage{Channel: "telegram", AccountID: "bot-new", UserID: "111", SenderName: "OldChatter"}
	got := g.resolveChatter(ctx, appBinder.ID, remsg)
	if got != histID {
		t.Fatalf("resolveChatter(app) = %q, want historical %q (parent fallback)", got, histID)
	}
	// Stable: the walk is repeated (UpdateUser migration is a no-op for
	// owner_user_id upstream) but always lands on the same historical row.
	if again := g.resolveChatter(ctx, appBinder.ID, remsg); again != histID {
		t.Errorf("resolveChatter repeat = %q, want %q", again, histID)
	}

	// Legacy external_id (channel:accountID:userID) under the parent must
	// also be found via the suffix branch.
	legacy := &store.UserRecord{
		ID:           "u_legacy",
		Username:     "legacy",
		Email:        "legacy@example.com",
		PasswordHash: "x",
		Role:         users.RoleAppUser,
		Status:       users.StatusActive,
		ExternalID:   "telegram:bot-old:222",
		OwnerUserID:  parent.ID,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	if err := db.CreateUser(ctx, legacy); err != nil {
		t.Fatalf("create legacy chatter: %v", err)
	}
	legacyMsg := bus.InboundMessage{Channel: "telegram", AccountID: "bot-new", UserID: "222", SenderName: "Legacy"}
	if got := g.resolveChatter(ctx, appBinder.ID, legacyMsg); got != legacy.ID {
		t.Fatalf("resolveChatter(legacy suffix) = %q, want %q", got, legacy.ID)
	}

	// Control: a brand-new sender with no historical row still mints a
	// fresh app_user under the app binder — fallback doesn't break minting.
	fresh := bus.InboundMessage{Channel: "telegram", AccountID: "bot-new", UserID: "333", SenderName: "Fresh"}
	freshID := g.resolveChatter(ctx, appBinder.ID, fresh)
	if freshID == "" || freshID == histID || freshID == legacy.ID {
		t.Fatalf("fresh sender should mint a new chatter, got %q (hist=%q legacy=%q)", freshID, histID, legacy.ID)
	}
	freshAcc, err := db.GetUser(ctx, freshID)
	if err != nil {
		t.Fatalf("get fresh chatter: %v", err)
	}
	if freshAcc.OwnerUserID != appBinder.ID {
		t.Errorf("fresh chatter owner = %q, want %q", freshAcc.OwnerUserID, appBinder.ID)
	}
}
