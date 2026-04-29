// Package users owns the platform's identity layer: real user accounts
// (Account) and the programmatic tokens they issue (APIKey). Both types are
// thin facades over store.Store so a single SQL backend remains the source
// of truth across pods.
//
// The legacy "apikey == user" model is gone. An account is what owns
// agents/sessions/cron jobs; an apikey is just a scoped credential pointing
// at one account, with an explicit list of agents it may operate on.
package users

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/store"
	"golang.org/x/crypto/bcrypt"
)

// Roles. super_admin can manage every user/agent/provider on the platform;
// user can only touch their own resources. There is intentionally no
// fine-grained scheme — anything more complex lives in the apikey ACL layer.
const (
	RoleSuperAdmin = "super_admin"
	RoleUser       = "user"
)

// Statuses.
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// ErrInvalidCredentials masks "no such user" and "wrong password" so the
// login handler can't be used as an email-existence oracle.
var ErrInvalidCredentials = errors.New("invalid credentials")

// Account is the public representation of a user row. PasswordHash never
// leaves the package — we read it during Authenticate and zero it out
// before returning to callers.
type Account struct {
	ID          string    `json:"id"`
	Username    string    `json:"username"`
	Email       string    `json:"email"`
	DisplayName string    `json:"displayName,omitempty"`
	Role        string    `json:"role"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// Accounts is the registry for user accounts.
type Accounts struct {
	store store.Store
}

// NewAccounts returns an account registry backed by st. Refuses nil — the
// platform has no in-memory mode.
func NewAccounts(st store.Store) (*Accounts, error) {
	if st == nil {
		return nil, errors.New("users.NewAccounts: store is required")
	}
	return &Accounts{store: st}, nil
}

// Count returns the number of users on the platform. Onboarding gates on
// `Count(ctx) == 0` to decide whether to show the wizard.
func (a *Accounts) Count(ctx context.Context) (int, error) {
	return a.store.CountUsers(ctx)
}

// Create writes a new account. Password is hashed with bcrypt; plaintext
// is never persisted. ID is generated when empty.
func (a *Accounts) Create(ctx context.Context, username, email, password, displayName, role string) (*Account, error) {
	username = strings.TrimSpace(username)
	email = strings.ToLower(strings.TrimSpace(email))
	if username == "" || email == "" || password == "" {
		return nil, errors.New("users.Create: username, email, password are required")
	}
	if role == "" {
		role = RoleUser
	}
	if role != RoleSuperAdmin && role != RoleUser {
		return nil, errors.New("users.Create: invalid role")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	id, err := newID("u_")
	if err != nil {
		return nil, err
	}
	rec := &store.UserRecord{
		ID:           id,
		Username:     username,
		Email:        email,
		PasswordHash: string(hash),
		DisplayName:  displayName,
		Role:         role,
		Status:       StatusActive,
	}
	if err := a.store.CreateUser(ctx, rec); err != nil {
		return nil, err
	}
	return toAccount(rec), nil
}

// Authenticate validates a username-or-email + password pair. Returns the
// account on success, ErrInvalidCredentials on every failure mode (missing
// user, wrong password, disabled account) so callers can't distinguish.
func (a *Accounts) Authenticate(ctx context.Context, login, password string) (*Account, error) {
	login = strings.TrimSpace(login)
	if login == "" || password == "" {
		return nil, ErrInvalidCredentials
	}
	if strings.Contains(login, "@") {
		login = strings.ToLower(login)
	}
	rec, err := a.store.GetUserByLogin(ctx, login)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	if rec.Status != StatusActive {
		return nil, ErrInvalidCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(rec.PasswordHash), []byte(password)); err != nil {
		return nil, ErrInvalidCredentials
	}
	return toAccount(rec), nil
}

// Get returns the account for id, or store.ErrNotFound.
func (a *Accounts) Get(ctx context.Context, id string) (*Account, error) {
	rec, err := a.store.GetUser(ctx, id)
	if err != nil {
		return nil, err
	}
	return toAccount(rec), nil
}

// List returns all accounts. Super-admin endpoints only.
func (a *Accounts) List(ctx context.Context) ([]*Account, error) {
	recs, err := a.store.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Account, 0, len(recs))
	for i := range recs {
		out = append(out, toAccount(&recs[i]))
	}
	return out, nil
}

// Update applies non-credential changes (display name, role, status). Use
// SetPassword for password rotation.
func (a *Accounts) Update(ctx context.Context, id, displayName, role, status string) (*Account, error) {
	rec, err := a.store.GetUser(ctx, id)
	if err != nil {
		return nil, err
	}
	if displayName != "" {
		rec.DisplayName = displayName
	}
	if role != "" {
		if role != RoleSuperAdmin && role != RoleUser {
			return nil, errors.New("users.Update: invalid role")
		}
		rec.Role = role
	}
	if status != "" {
		if status != StatusActive && status != StatusDisabled {
			return nil, errors.New("users.Update: invalid status")
		}
		rec.Status = status
	}
	if err := a.store.UpdateUser(ctx, rec); err != nil {
		return nil, err
	}
	return toAccount(rec), nil
}

// SetPassword rotates an account's password. Caller is responsible for
// permission checks (self vs. super_admin).
func (a *Accounts) SetPassword(ctx context.Context, id, newPassword string) error {
	if newPassword == "" {
		return errors.New("users.SetPassword: empty password")
	}
	rec, err := a.store.GetUser(ctx, id)
	if err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	rec.PasswordHash = string(hash)
	return a.store.UpdateUser(ctx, rec)
}

// Delete removes an account and its owned rows (cascade implemented in the
// store). Refuses to drop the last super_admin so the install doesn't lock
// itself out.
func (a *Accounts) Delete(ctx context.Context, id string) error {
	target, err := a.store.GetUser(ctx, id)
	if err != nil {
		return err
	}
	if target.Role == RoleSuperAdmin {
		all, err := a.store.ListUsers(ctx)
		if err != nil {
			return err
		}
		admins := 0
		for _, u := range all {
			if u.Role == RoleSuperAdmin && u.Status == StatusActive {
				admins++
			}
		}
		if admins <= 1 {
			return errors.New("users.Delete: refusing to remove the last active super_admin")
		}
	}
	return a.store.DeleteUser(ctx, id)
}

func toAccount(r *store.UserRecord) *Account {
	if r == nil {
		return nil
	}
	return &Account{
		ID:          r.ID,
		Username:    r.Username,
		Email:       r.Email,
		DisplayName: r.DisplayName,
		Role:        r.Role,
		Status:      r.Status,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}
}

// newID returns a short unique id with the given prefix. ~80 bits of
// entropy — collisions vanishingly unlikely at platform scale.
func newID(prefix string) (string, error) {
	var buf [10]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(buf[:]), nil
}
