package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/port"
)

// FilePendingStore persists one-shot pending authorizations under
// Root/oauth/pending.json, encrypted with the same Cryptor used for
// tokens (the code verifier must never sit on disk in plaintext). A
// daemon restart in the middle of an authorization no longer loses the
// pending state; Take still consumes the entry exactly once.
//
// Deprecated for deployment: real installs use DBPendingStore. This
// remains only as the no-DB test fixture / bootstrap fallback.
type FilePendingStore struct {
	Root  string
	Crypt port.Cryptor

	mu sync.Mutex
}

func (s *FilePendingStore) path() string {
	return filepath.Join(s.Root, "oauth", "pending.json")
}

func (s *FilePendingStore) load() (map[string]*domain.PendingAuth, error) {
	pending := make(map[string]*domain.PendingAuth)
	enc, err := os.ReadFile(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return pending, nil
	}
	if err != nil {
		return nil, err
	}
	plain, err := s.Crypt.Decrypt(context.Background(), enc)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(plain, &pending); err != nil {
		return nil, err
	}
	// Lazy TTL cleanup: drop anything past its expiry on read.
	now := time.Now().UTC()
	for state, p := range pending {
		if p.Expired(now) {
			delete(pending, state)
		}
	}
	return pending, nil
}

func (s *FilePendingStore) store(pending map[string]*domain.PendingAuth) error {
	plain, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	enc, err := s.Crypt.Encrypt(context.Background(), plain)
	if err != nil {
		return err
	}
	path := s.path()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, enc, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Save records a pending authorization keyed by state.
func (s *FilePendingStore) Save(_ context.Context, p *domain.PendingAuth) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, err := s.load()
	if err != nil {
		return err
	}
	pending[p.State] = p
	return s.store(pending)
}

// Take returns and deletes the pending authorization (single-use).
func (s *FilePendingStore) Take(_ context.Context, state string) (*domain.PendingAuth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, err := s.load()
	if err != nil {
		return nil, err
	}
	p, ok := pending[state]
	if !ok {
		return nil, port.ErrNotFound
	}
	delete(pending, state)
	if err := s.store(pending); err != nil {
		// Persist the consumption; a failure here must NOT leave the
		// state reusable, so we fail the request even though the entry
		// was already removed from our copy.
		return nil, err
	}
	if p.Expired(time.Now().UTC()) {
		return nil, port.ErrNotFound
	}
	return p, nil
}

// CountActive returns non-expired pending count for a user (load already
// prunes expired rows).
func (s *FilePendingStore) CountActive(_ context.Context, userID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, err := s.load()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range pending {
		if p.UserID == userID {
			n++
		}
	}
	return n, nil
}
