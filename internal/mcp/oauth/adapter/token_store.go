package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/port"
)

// FileTokenStore persists encrypted credentials under Root (default
// FASTAGENT_HOME), with atomic writes and 0600 permissions.
//
// Deprecated for deployment: real installs (single SQLite instance or
// multi-instance Postgres) use DBTokenStore. This remains only as the
// no-DB test fixture / bootstrap fallback.
type FileTokenStore struct {
	Root  string // default: config.HomeDir()
	Crypt port.Cryptor
}

// Save encrypts and atomically writes the credential file.
func (s *FileTokenStore) Save(ctx context.Context, key string, t *domain.OAuthTokens) error {
	plain, err := json.Marshal(t)
	if err != nil {
		return err
	}
	enc, err := s.Crypt.Encrypt(ctx, plain)
	if err != nil {
		return err
	}
	path := filepath.Join(s.Root, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, enc, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load reads and decrypts the credential file.
func (s *FileTokenStore) Load(ctx context.Context, key string) (*domain.OAuthTokens, error) {
	enc, err := os.ReadFile(filepath.Join(s.Root, filepath.FromSlash(key)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, port.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	plain, err := s.Crypt.Decrypt(ctx, enc)
	if err != nil {
		return nil, err
	}
	var t domain.OAuthTokens
	if err := json.Unmarshal(plain, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// Delete removes the credential file.
func (s *FileTokenStore) Delete(ctx context.Context, key string) error {
	err := os.Remove(filepath.Join(s.Root, filepath.FromSlash(key)))
	if errors.Is(err, os.ErrNotExist) {
		return port.ErrNotFound
	}
	return err
}
