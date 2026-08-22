package users

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// newAccountsEnv boots a real sqlite store (migrated) with a users registry.
func newAccountsEnv(t *testing.T) (*Accounts, *store.DBStore) {
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
	accts, err := NewAccounts(st)
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	return accts, st
}

func TestAccounts_Create_IdempotentByExternal(t *testing.T) {
	accts, _ := newAccountsEnv(t)
	ctx := context.Background()
	in := CreateInput{
		Username: "u1", Email: "u1@x", Password: "pw",
		Role: RoleUser, APIKeyID: "k_alice", ExternalID: "ext-1",
	}
	a, err := accts.Create(ctx, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	b, err := accts.Create(ctx, in)
	if err != nil {
		t.Fatalf("re-create: %v", err)
	}
	if a.ID != b.ID {
		t.Errorf("idempotent create returned different IDs: %s vs %s", a.ID, b.ID)
	}
	if a.ExternalID != "ext-1" || a.APIKeyID != "k_alice" {
		t.Errorf("external/apikey not persisted: %+v", a)
	}
}

// Idempotency must not leak across different identities — the same pair
// always resolves to the same row, but a *different* pair is a new user.
func TestAccounts_Create_ExternalDistinguishesUsers(t *testing.T) {
	accts, _ := newAccountsEnv(t)
	ctx := context.Background()
	a, err := accts.Create(ctx, CreateInput{
		Username: "u2", Email: "u2@x", Password: "pw", Role: RoleUser,
	})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := accts.Create(ctx, CreateInput{
		Username: "u2b", Email: "u2b@x", Password: "pw", Role: RoleUser,
		APIKeyID: "k_alice", ExternalID: "ext-1",
	})
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	// First call had empty pair → distinct from the keyed one.
	if a.ID == b.ID {
		t.Errorf("users with different provisioning identity collided: %s", a.ID)
	}
}

func TestAccounts_Create_RequiresFields(t *testing.T) {
	accts, _ := newAccountsEnv(t)
	ctx := context.Background()
	for _, in := range []CreateInput{
		{Email: "e@x", Password: "pw"},
		{Username: "u", Password: "pw"},
		{Username: "u", Email: "e@x"},
	} {
		if _, err := accts.Create(ctx, in); err == nil {
			t.Errorf("Create(%+v) = nil error, want validation error", in)
		}
	}
}

func TestAccounts_Create_InvalidRole(t *testing.T) {
	accts, _ := newAccountsEnv(t)
	_, err := accts.Create(context.Background(), CreateInput{
		Username: "u", Email: "e@x", Password: "pw", Role: "king",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid role") {
		t.Fatalf("invalid role err = %v, want 'invalid role'", err)
	}
}

func TestAccounts_EnsureAppUser_IdempotentAndRole(t *testing.T) {
	accts, st := newAccountsEnv(t)
	ctx := context.Background()
	owner, err := accts.Create(ctx, CreateInput{Username: "alice", Email: "alice@x", Password: "pw"})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	app, err := accts.EnsureAppUser(ctx, owner.ID, "ext-alice-1", "Alice Ext", "k_alice")
	if err != nil {
		t.Fatalf("ensure app_user: %v", err)
	}
	if app.Role != RoleAppUser {
		t.Errorf("role = %s, want %s", app.Role, RoleAppUser)
	}
	again, err := accts.EnsureAppUser(ctx, owner.ID, "ext-alice-1", "Alice Ext", "k_alice")
	if err != nil {
		t.Fatalf("ensure app_user again: %v", err)
	}
	if again.ID != app.ID {
		t.Errorf("EnsureAppUser not idempotent: %s vs %s", again.ID, app.ID)
	}
	// Underlying store row must expose owner for the billing ownership gate.
	rec, err := st.GetUser(ctx, app.ID)
	if err != nil {
		t.Fatalf("get app_user row: %v", err)
	}
	if rec.OwnerUserID != owner.ID {
		t.Errorf("store row ownerUserID = %s, want %s", rec.OwnerUserID, owner.ID)
	}
	if rec.PasswordHash != "" {
		t.Errorf("store app_user passwordHash not empty")
	}
}

// App users minted under different owners are distinct even with the same
// externalID — that is the isolation boundary CanManageUser relies on.
func TestAccounts_EnsureAppUser_OwnerIsolation(t *testing.T) {
	accts, _ := newAccountsEnv(t)
	ctx := context.Background()
	alice, _ := accts.Create(ctx, CreateInput{Username: "alice", Email: "alice@x", Password: "pw"})
	bob, _ := accts.Create(ctx, CreateInput{Username: "bob", Email: "bob@x", Password: "pw"})
	aApp, _ := accts.EnsureAppUser(ctx, alice.ID, "shared-ext", "", "")
	bApp, _ := accts.EnsureAppUser(ctx, bob.ID, "shared-ext", "", "")
	if aApp.ID == bApp.ID {
		t.Errorf("same externalID under different owners must be separate rows")
	}
}

func TestAccounts_EnsureChatter_Role(t *testing.T) {
	accts, st := newAccountsEnv(t)
	ctx := context.Background()
	owner, _ := accts.Create(ctx, CreateInput{Username: "alice", Email: "alice@x", Password: "pw"})
	ch, err := accts.EnsureChatter(ctx, owner.ID, "tg-123", "Telegram User")
	if err != nil {
		t.Fatalf("ensure chatter: %v", err)
	}
	if ch.Role != RoleChannelUser {
		t.Errorf("role = %s, want %s", ch.Role, RoleChannelUser)
	}
	rec, _ := st.GetUser(ctx, ch.ID)
	if !strings.HasSuffix(rec.Email, "@channel_user") {
		t.Errorf("chatter email = %s, want @channel_user suffix", rec.Email)
	}
	again, _ := accts.EnsureChatter(ctx, owner.ID, "tg-123", "Telegram User")
	if again.ID != ch.ID {
		t.Errorf("EnsureChatter not idempotent")
	}
}

func TestAccounts_Authenticate(t *testing.T) {
	accts, _ := newAccountsEnv(t)
	ctx := context.Background()
	_, err := accts.Create(ctx, CreateInput{Username: "alice", Email: "alice@x", Password: "s3cret"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, login := range []string{"alice", "alice@x", "ALICE@x"} {
		got, err := accts.Authenticate(ctx, login, "s3cret")
		if err != nil {
			t.Errorf("Authenticate(%q) err = %v", login, err)
			continue
		}
		if got.Role != RoleUser {
			t.Errorf("Authenticate(%q) role = %s, want %s", login, got.Role, RoleUser)
		}
	}
	if _, err := accts.Authenticate(ctx, "alice", "wrong"); err != ErrInvalidCredentials {
		t.Errorf("wrong password err = %v, want ErrInvalidCredentials", err)
	}
	if _, err := accts.Authenticate(ctx, "ghost", "pw"); err != ErrInvalidCredentials {
		t.Errorf("missing user err = %v, want ErrInvalidCredentials", err)
	}
}

func TestAccounts_Authenticate_RejectsProvisionedUsers(t *testing.T) {
	accts, _ := newAccountsEnv(t)
	ctx := context.Background()
	owner, _ := accts.Create(ctx, CreateInput{Username: "alice", Email: "alice@x", Password: "pw"})
	app, _ := accts.EnsureAppUser(ctx, owner.ID, "ext-1", "", "")
	ch, _ := accts.EnsureChatter(ctx, owner.ID, "tg-1", "Chatter")
	for _, id := range []string{app.ID, ch.ID} {
		if _, err := accts.Authenticate(ctx, id, "pw"); err != ErrInvalidCredentials {
			t.Errorf("Authenticate(provisioned %s) err = %v, want ErrInvalidCredentials", id, err)
		}
	}
}

func TestAccounts_Authenticate_RejectsDisabled(t *testing.T) {
	accts, _ := newAccountsEnv(t)
	ctx := context.Background()
	u, _ := accts.Create(ctx, CreateInput{Username: "alice", Email: "alice@x", Password: "pw"})
	if _, err := accts.Update(ctx, u.ID, "", "", StatusDisabled, nil); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := accts.Authenticate(ctx, "alice", "pw"); err != ErrInvalidCredentials {
		t.Errorf("disabled login err = %v, want ErrInvalidCredentials", err)
	}
}

func TestAccounts_VerifyPassword(t *testing.T) {
	accts, _ := newAccountsEnv(t)
	ctx := context.Background()
	u, _ := accts.Create(ctx, CreateInput{Username: "alice", Email: "alice@x", Password: "s3cret"})
	if err := accts.VerifyPassword(ctx, u.ID, "s3cret"); err != nil {
		t.Errorf("VerifyPassword correct err = %v", err)
	}
	if err := accts.VerifyPassword(ctx, u.ID, "wrong"); err != ErrInvalidCredentials {
		t.Errorf("VerifyPassword wrong err = %v, want ErrInvalidCredentials", err)
	}
	app, _ := accts.EnsureAppUser(ctx, u.ID, "ext-1", "", "")
	if err := accts.VerifyPassword(ctx, app.ID, ""); err != ErrInvalidCredentials {
		t.Errorf("app_user VerifyPassword err = %v, want ErrInvalidCredentials", err)
	}
}

func TestAccounts_SetPassword(t *testing.T) {
	accts, _ := newAccountsEnv(t)
	ctx := context.Background()
	u, _ := accts.Create(ctx, CreateInput{Username: "alice", Email: "alice@x", Password: "old"})
	if err := accts.SetPassword(ctx, u.ID, "newpw"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	if _, err := accts.Authenticate(ctx, "alice", "newpw"); err != nil {
		t.Errorf("login with new password err = %v", err)
	}
	if _, err := accts.Authenticate(ctx, "alice", "old"); err != ErrInvalidCredentials {
		t.Errorf("login with old password err = %v, want ErrInvalidCredentials", err)
	}
}

func TestAccounts_Update_RoleValidation(t *testing.T) {
	accts, _ := newAccountsEnv(t)
	ctx := context.Background()
	u, _ := accts.Create(ctx, CreateInput{Username: "alice", Email: "alice@x", Password: "pw"})
	if _, err := accts.Update(ctx, u.ID, "", "banana", "", nil); err == nil {
		t.Errorf("invalid role accepted, want error")
	}
}

// Deleting the last active super_admin must fail so the install can't lock
// itself out of admin. With a second admin present, the delete goes through.
func TestAccounts_Delete_RefusesLastSuperAdmin(t *testing.T) {
	accts, _ := newAccountsEnv(t)
	ctx := context.Background()
	admin, _ := accts.Create(ctx, CreateInput{Username: "root", Email: "root@x", Password: "pw", Role: RoleSuperAdmin})
	if err := accts.Delete(ctx, admin.ID); err == nil {
		t.Fatalf("deleting last super_admin succeeded, want error")
	}
	other, _ := accts.Create(ctx, CreateInput{Username: "root2", Email: "root2@x", Password: "pw", Role: RoleSuperAdmin})
	if err := accts.Delete(ctx, admin.ID); err != nil {
		t.Fatalf("delete with second admin present: %v", err)
	}
	if _, err := accts.Get(ctx, other.ID); err != nil {
		t.Errorf("surviving admin lost: %v", err)
	}
	if err := accts.Delete(ctx, other.ID); err == nil {
		t.Errorf("deleting now-last super_admin succeeded, want error")
	}
}
