// Package users manages the cloud-mode user registry.
//
// In local (single-user) mode the registry is unused and the implicit user
// is config.DefaultUserID ("local"). In cloud mode the registry is loaded
// from ~/.fastclaw/users.json and maps bearer tokens to user IDs so the
// HTTP API can route requests to the right per-user agent manager.
package users

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/config"
)

// User is one entry in the registry.
type User struct {
	ID        string    `json:"id"`
	Name      string    `json:"name,omitempty"`
	Tokens    []string  `json:"tokens"`
	CreatedAt time.Time `json:"createdAt"`
}

// Registry is an in-memory, file-backed user store.
type Registry struct {
	path  string
	mu    sync.RWMutex
	users map[string]*User // id -> User
	byTok map[string]string
}

// DefaultPath returns the path to the registry file (~/.fastclaw/users.json).
func DefaultPath() (string, error) {
	home, err := config.HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "users.json"), nil
}

// Load reads the registry from disk. Missing file is treated as empty.
func Load() (*Registry, error) {
	path, err := DefaultPath()
	if err != nil {
		return nil, err
	}
	return LoadFrom(path)
}

// LoadFrom reads the registry from an explicit path.
func LoadFrom(path string) (*Registry, error) {
	r := &Registry{
		path:  path,
		users: make(map[string]*User),
		byTok: make(map[string]string),
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read users.json: %w", err)
	}
	var list []*User
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parse users.json: %w", err)
	}
	for _, u := range list {
		r.users[u.ID] = u
		for _, t := range u.Tokens {
			r.byTok[t] = u.ID
		}
	}
	return r, nil
}

// Save persists the registry to disk.
func (r *Registry) Save() error {
	r.mu.RLock()
	list := make([]*User, 0, len(r.users))
	for _, u := range r.users {
		list = append(list, u)
	}
	r.mu.RUnlock()

	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(r.path, data, 0o600)
}

// Add creates a new user with a fresh token and returns it.
// Returns an error if the ID already exists.
func (r *Registry) Add(id, name string) (*User, string, error) {
	if id == "" {
		return nil, "", errors.New("user id is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.users[id]; exists {
		return nil, "", fmt.Errorf("user %q already exists", id)
	}
	token, err := newToken()
	if err != nil {
		return nil, "", err
	}
	u := &User{
		ID:        id,
		Name:      name,
		Tokens:    []string{token},
		CreatedAt: time.Now().UTC(),
	}
	r.users[id] = u
	r.byTok[token] = id
	return u, token, nil
}

// IssueToken mints a new token for an existing user.
func (r *Registry) IssueToken(id string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.users[id]
	if !ok {
		return "", fmt.Errorf("user %q not found", id)
	}
	token, err := newToken()
	if err != nil {
		return "", err
	}
	u.Tokens = append(u.Tokens, token)
	r.byTok[token] = id
	return token, nil
}

// Remove deletes a user and all their tokens from the registry.
// Does NOT delete the user's on-disk workspace (~/.fastclaw/users/{id}/).
func (r *Registry) Remove(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.users[id]
	if !ok {
		return fmt.Errorf("user %q not found", id)
	}
	for _, t := range u.Tokens {
		delete(r.byTok, t)
	}
	delete(r.users, id)
	return nil
}

// List returns a snapshot of all users, sorted by ID.
func (r *Registry) List() []*User {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*User, 0, len(r.users))
	for _, u := range r.users {
		// Copy so callers can't mutate internal state.
		cp := *u
		cp.Tokens = append([]string(nil), u.Tokens...)
		out = append(out, &cp)
	}
	return out
}

// Get returns a user by ID.
func (r *Registry) Get(id string) (*User, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	u, ok := r.users[id]
	if !ok {
		return nil, false
	}
	cp := *u
	return &cp, true
}

// LookupByToken returns the user ID associated with a bearer token.
func (r *Registry) LookupByToken(token string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.byTok[token]
	return id, ok
}

// Count returns the number of registered users.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.users)
}

func newToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "fc_" + hex.EncodeToString(buf[:]), nil
}
