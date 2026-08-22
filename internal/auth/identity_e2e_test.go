package auth

// E2E coverage for upstream commit 365a7e1 (local 755061b)
// "feat: stable app-user identity across api-key rotation + workspace/chat UX".
//
// The identity contract under test: an app_user (an end-user provisioned on
// behalf of a calling app via an api_key) is keyed on the api_key's OWNER
// account, NOT the api_key id. That means the calling app can rotate /
// replace its api_key — a new key row under the same owner account — without
// orphaning the end-user's sessions / files / agents. All three entry points
// (X-Fastagent-End-User header, the OpenAI `user` field, and POST /v1/users)
// route through Resolver.SwitchToAppUser, which is what we exercise here at
// the pure DB layer (no HTTP).

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

// newIdentityEnv boots a real migrated sqlite store wired the same way the
// gateway boots it: Accounts + APIKeys + auth Resolver over one store.
func newIdentityEnv(t *testing.T) (*Resolver, *store.DBStore, *users.Accounts, *users.APIKeys) {
	t.Helper()
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "test.db") + "?_pragma=foreign_keys(1)"
	st, err := store.NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	accts, err := users.NewAccounts(st)
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	keys, err := users.NewAPIKeys(st)
	if err != nil {
		t.Fatalf("apikeys: %v", err)
	}
	resolver, err := NewResolver(st)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	return resolver, st, accts, keys
}

// createOwnerWithKey mints a real human account plus a type=user api_key for
// it, returning the owner id, the api-key id, and the plaintext token.
func createOwnerWithKey(t *testing.T, accts *users.Accounts, keys *users.APIKeys, name string) (ownerID, keyID, token string) {
	t.Helper()
	acc, err := accts.Create(context.Background(), users.CreateInput{
		Username: name, Email: name + "@x", Password: "pw",
	})
	if err != nil {
		t.Fatalf("create owner %s: %v", name, err)
	}
	key, tok, err := keys.Create(context.Background(), acc.ID, name+"-key", users.APIKeyTypeUser, nil)
	if err != nil {
		t.Fatalf("create key for %s: %v", name, err)
	}
	return acc.ID, key.ID, tok
}

// appUserRows returns every app_user row minted under (owner, externalID).
func appUserRows(t *testing.T, st *store.DBStore, owner, externalID string) []store.UserRecord {
	t.Helper()
	all, err := st.ListUsers(context.Background())
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	var out []store.UserRecord
	for _, u := range all {
		if u.Role == users.RoleAppUser && u.OwnerUserID == owner && u.ExternalID == externalID {
			out = append(out, u)
		}
	}
	return out
}

// TestIdentity_AppUserStableAcrossApiKeyRotation is the core e2e for the
// commit: mint an app_user under a first api_key, then "rotate" by issuing a
// second key under the SAME owner account and resolving the same external
// identifier again. Both lookups must resolve to the SAME app_user id
// (idempotent), proving the app's key rotation never orphans its end-users.
func TestIdentity_AppUserStableAcrossApiKeyRotation(t *testing.T) {
	resolver, st, accts, keys := newIdentityEnv(t)
	ctx := context.Background()

	// One owner account, two api_keys under it — key B is the "rotated"
	// replacement. The rotation contract: resolving the same external id
	// under the new key must return the same app_user.
	acc, err := accts.Create(ctx, users.CreateInput{
		Username: "alice", Email: "alice@x", Password: "pw",
	})
	if err != nil {
		t.Fatalf("create owner alice: %v", err)
	}
	ownerID := acc.ID
	keyA, tokenA, err := keys.Create(ctx, ownerID, "alice-key", users.APIKeyTypeUser, nil)
	if err != nil {
		t.Fatalf("create key A: %v", err)
	}
	keyB, tokenB, err := keys.Create(ctx, ownerID, "alice-key-rotated", users.APIKeyTypeUser, nil)
	if err != nil {
		t.Fatalf("create key B: %v", err)
	}
	keyAID, keyBID := keyA.ID, keyB.ID

	const externalID = "ext-e2e-1"

	// 1. First key → lazy mint via the resolver path Cloud uses.
	identA, err := resolver.ResolveBearer(ctx, tokenA)
	if err != nil {
		t.Fatalf("resolve bearer A: %v", err)
	}
	switchedA, err := resolver.SwitchToAppUser(ctx, identA, externalID)
	if err != nil {
		t.Fatalf("switch to app user (key A): %v", err)
	}
	appUser1 := switchedA.UserID
	if appUser1 == ownerID {
		t.Fatalf("switched UserID = %q, want a minted app_user distinct from owner %q", appUser1, ownerID)
	}
	if switchedA.Role != users.RoleAppUser {
		t.Errorf("switched role = %q, want %q", switchedA.Role, users.RoleAppUser)
	}
	// The apikey ACL must survive the switch — APIKeyID/Agents are preserved.
	if switchedA.APIKeyID != keyAID {
		t.Errorf("switched APIKeyID = %q, want %q (ACL must be preserved)", switchedA.APIKeyID, keyAID)
	}

	// 2. The minted row is owner-keyed AND apikey-audited.
	row, err := st.GetUserByExternal(ctx, ownerID, externalID)
	if err != nil {
		t.Fatalf("GetUserByExternal after mint: %v", err)
	}
	if row.ID != appUser1 || row.Role != users.RoleAppUser || row.OwnerUserID != ownerID || row.ExternalID != externalID {
		t.Errorf("minted row = %+v; want id=%s role=%s owner=%s external=%s",
			row, appUser1, users.RoleAppUser, ownerID, externalID)
	}
	if row.APIKeyID != keyAID {
		t.Errorf("row.APIKeyID = %q, want %q (audit: minting key)", row.APIKeyID, keyAID)
	}

	// 3. GetUserByAPIKeyExternal resolves the freshly minted row, id stable
	//    across repeated calls.
	byKey, err := st.GetUserByAPIKeyExternal(ctx, keyAID, externalID)
	if err != nil {
		t.Fatalf("GetUserByAPIKeyExternal after mint: %v", err)
	}
	if byKey.ID != appUser1 {
		t.Errorf("GetUserByAPIKeyExternal id = %q, want %q", byKey.ID, appUser1)
	}
	if again, err := st.GetUserByAPIKeyExternal(ctx, keyAID, externalID); err != nil || again.ID != appUser1 {
		t.Errorf("repeat GetUserByAPIKeyExternal = (%v, %v), want id %q stable", again, err, appUser1)
	}

	// 4. API-key rotation: the app replaces its key (key B, same owner).
	//    Resolving the SAME external id must return the SAME app_user — not
	//    a second minted row under the new key.
	identB, err := resolver.ResolveBearer(ctx, tokenB)
	if err != nil {
		t.Fatalf("resolve bearer B: %v", err)
	}
	if identB.UserID != ownerID {
		t.Fatalf("key B owner = %q, want %q", identB.UserID, ownerID)
	}
	switchedB, err := resolver.SwitchToAppUser(ctx, identB, externalID)
	if err != nil {
		t.Fatalf("switch to app user (key B): %v", err)
	}
	if switchedB.UserID != appUser1 {
		t.Errorf("after rotation switched id = %q, want same app_user %q", switchedB.UserID, appUser1)
	}
	// The ACL rides the new key: the caller's APIKeyID is now key B while
	// the app_user identity (and its owner-keyed row) stays put.
	if switchedB.APIKeyID != keyBID {
		t.Errorf("after rotation switched APIKeyID = %q, want %q", switchedB.APIKeyID, keyBID)
	}

	// 5. Exactly one app_user row exists for (owner, external) — the
	//    rotation did not mint a duplicate.
	if rows := appUserRows(t, st, ownerID, externalID); len(rows) != 1 {
		t.Errorf("app_user rows for (owner, ext) = %d, want 1: %+v", len(rows), rows)
	}
}

// TestIdentity_DifferentOwnersGetDifferentAppUsers proves the isolation
// boundary: the same external_id under two different owner accounts mints
// two distinct app_users. Without this, one app's end-users would collide
// with another tenant's.
func TestIdentity_DifferentOwnersGetDifferentAppUsers(t *testing.T) {
	resolver, st, accts, keys := newIdentityEnv(t)
	ctx := context.Background()

	aliceID, _, tokA := createOwnerWithKey(t, accts, keys, "alice2")
	bobID, _, tokB := createOwnerWithKey(t, accts, keys, "bob")

	identA, _ := resolver.ResolveBearer(ctx, tokA)
	identB, _ := resolver.ResolveBearer(ctx, tokB)

	const externalID = "ext-shared"
	aliceApp, err := resolver.SwitchToAppUser(ctx, identA, externalID)
	if err != nil {
		t.Fatalf("switch alice: %v", err)
	}
	bobApp, err := resolver.SwitchToAppUser(ctx, identB, externalID)
	if err != nil {
		t.Fatalf("switch bob: %v", err)
	}
	if aliceApp.UserID == bobApp.UserID {
		t.Errorf("alice/bob app_user collided: both %q", aliceApp.UserID)
	}
	if rows := appUserRows(t, st, aliceID, externalID); len(rows) != 1 {
		t.Errorf("alice app_user rows = %d, want 1", len(rows))
	}
	if rows := appUserRows(t, st, bobID, externalID); len(rows) != 1 {
		t.Errorf("bob app_user rows = %d, want 1", len(rows))
	}
}

// TestIdentity_EmptyExternalIDPassesThrough: a caller that names no
// end-user stays as the api_key owner — no mint, no error.
func TestIdentity_EmptyExternalIDPassesThrough(t *testing.T) {
	resolver, st, accts, keys := newIdentityEnv(t)
	ctx := context.Background()

	ownerID, _, tok := createOwnerWithKey(t, accts, keys, "carol")
	ident, err := resolver.ResolveBearer(ctx, tok)
	if err != nil {
		t.Fatalf("resolve bearer: %v", err)
	}
	switched, err := resolver.SwitchToAppUser(ctx, ident, "")
	if err != nil {
		t.Fatalf("switch with empty external id: %v", err)
	}
	if switched.UserID != ownerID {
		t.Errorf("switched UserID = %q, want owner %q untouched", switched.UserID, ownerID)
	}
	all, _ := st.ListUsers(ctx)
	for _, u := range all {
		if u.Role == users.RoleAppUser {
			t.Errorf("empty external id minted an app_user: %+v", u)
		}
	}
}

// TestIdentity_SwitchToAppUserRequiresApikeyAuth: the switch is only valid
// for api_key callers; session callers must stay as-is and get an error.
func TestIdentity_SwitchToAppUserRequiresApikeyAuth(t *testing.T) {
	resolver, _, _, _ := newIdentityEnv(t)
	ctx := context.Background()

	sessionIdent := Identity{
		UserID:     "u_session",
		Role:       users.RoleUser,
		AuthMethod: "session",
	}
	const wantErr = "auth.SwitchToAppUser: api_key auth required"
	if _, err := resolver.SwitchToAppUser(ctx, sessionIdent, "ext-1"); err == nil {
		t.Error("session caller must be rejected by SwitchToAppUser")
	} else if err.Error() != wantErr {
		t.Errorf("unexpected error: %v; want %q", err, wantErr)
	}
}

// TestIdentity_AlreadyAppUserIsNoOp: a request that has already been
// switched must not re-key off the app_user's own id (which would mint a
// nested user). The resolver returns the ident unchanged.
func TestIdentity_AlreadyAppUserIsNoOp(t *testing.T) {
	resolver, st, accts, keys := newIdentityEnv(t)
	ctx := context.Background()

	ownerID, _, tok := createOwnerWithKey(t, accts, keys, "dave")
	ident, err := resolver.ResolveBearer(ctx, tok)
	if err != nil {
		t.Fatalf("resolve bearer: %v", err)
	}
	first, err := resolver.SwitchToAppUser(ctx, ident, "ext-1")
	if err != nil {
		t.Fatalf("first switch: %v", err)
	}

	// A second request already carrying the app_user role must no-op.
	appIdent := Identity{
		UserID:     first.UserID,
		Role:       users.RoleAppUser,
		AuthMethod: "apikey",
		APIKeyID:   ident.APIKeyID,
	}
	again, err := resolver.SwitchToAppUser(ctx, appIdent, "ext-1")
	if err != nil {
		t.Fatalf("re-switch: %v", err)
	}
	if again.UserID != first.UserID {
		t.Errorf("re-switch UserID = %q, want %q (no nested mint)", again.UserID, first.UserID)
	}
	// Still exactly one app_user for the owner, no nested row keyed on the
	// app_user's own id.
	if rows := appUserRows(t, st, ownerID, "ext-1"); len(rows) != 1 {
		t.Errorf("app_user rows after re-switch = %d, want 1", len(rows))
	}
}
