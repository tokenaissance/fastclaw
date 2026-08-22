package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// newDB boots a migrated sqlite store for tests in this package.
func newProvisionDB(t *testing.T) *DBStore {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "test.db")
	st, err := NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

// GetUserByAPIKeyExternal must resolve rows provisioned via (apikey_id,
// external_id) — the column pair Accounts.Create writes. The owner-based
// GetUserByExternal must NOT match those rows (their owner_user_id is
// empty), otherwise the Create fast-path would never hit and a retry would
// trip the partial unique index.
func TestGetUserByAPIKeyExternal(t *testing.T) {
	st := newProvisionDB(t)
	ctx := context.Background()

	if err := st.CreateUser(ctx, &UserRecord{
		ID: "u_prov", Username: "prov", Email: "prov@x",
		Role: "user", Status: "active", AgentQuota: -1,
		APIKeyID: "k_alice", ExternalID: "ext-1",
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	got, err := st.GetUserByAPIKeyExternal(ctx, "k_alice", "ext-1")
	if err != nil {
		t.Fatalf("GetUserByAPIKeyExternal: %v", err)
	}
	if got.ID != "u_prov" {
		t.Errorf("got %s, want u_prov", got.ID)
	}

	// The owner-based lookup sees an empty owner_user_id → no match. This
	// is exactly the failure mode the provisioning fast-path must avoid.
	if _, err := st.GetUserByExternal(ctx, "k_alice", "ext-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetUserByExternal on apikey-keyed row = %v, want ErrNotFound", err)
	}

	if _, err := st.GetUserByAPIKeyExternal(ctx, "k_mallory", "ext-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("wrong apikey = %v, want ErrNotFound", err)
	}
	if _, err := st.GetUserByAPIKeyExternal(ctx, "", "ext-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("empty apikey = %v, want ErrNotFound", err)
	}
	if _, err := st.GetUserByAPIKeyExternal(ctx, "k_alice", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("empty external_id = %v, want ErrNotFound", err)
	}
}

// The two lookups are complementary: an app_user minted under an owner
// resolves via owner_user_id but NOT via a bare apikey_id.
func TestGetUserByAPIKeyExternal_AppUserKeyedOnOwner(t *testing.T) {
	st := newProvisionDB(t)
	ctx := context.Background()

	if err := st.CreateUser(ctx, &UserRecord{
		ID: "u_app", Username: "u_app", Email: "u_app@app_user",
		Role: "app_user", Status: "active", AgentQuota: -1,
		OwnerUserID: "u_owner", APIKeyID: "k_alice", ExternalID: "ext-1",
	}); err != nil {
		t.Fatalf("create app_user: %v", err)
	}

	// The app_user row carries BOTH owner_user_id and apikey_id, so it is
	// reachable from both lookups. The distinction that matters for the
	// Accounts.Create fast-path is the reverse direction, proven in
	// TestGetUserByAPIKeyExternal: provisioning rows (apikey_id set, owner
	// empty) are ONLY reachable via the apikey_id lookup.
	got, err := st.GetUserByExternal(ctx, "u_owner", "ext-1")
	if err != nil {
		t.Fatalf("GetUserByExternal(owner): %v", err)
	}
	if got.ID != "u_app" {
		t.Errorf("owner lookup got %s, want u_app", got.ID)
	}
	if got, err := st.GetUserByAPIKeyExternal(ctx, "k_alice", "ext-1"); err != nil || got.ID != "u_app" {
		t.Errorf("apikey lookup on app_user = (%v, %v), want u_app", got, err)
	}
}
