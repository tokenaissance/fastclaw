package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/port"
)

// FileRegistrationStore persists dynamic client registrations under
// Root/oauth/regs.json so a daemon restart reuses client_id instead of
// re-registering (client_id is public information — no encryption
// needed).
//
// Deprecated for deployment: real installs use DBRegistrationStore. This
// remains only as the no-DB test fixture / bootstrap fallback.
type FileRegistrationStore struct {
	Root string
}

func regKey(serverName, callbackURL string) string {
	return serverName + "|" + callbackURL
}

func (s *FileRegistrationStore) path() string {
	return filepath.Join(s.Root, "oauth", "regs.json")
}

func (s *FileRegistrationStore) load() (map[string]*domain.ClientRegistration, error) {
	regs := make(map[string]*domain.ClientRegistration)
	data, err := os.ReadFile(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return regs, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &regs); err != nil {
		return nil, err
	}
	return regs, nil
}

func (s *FileRegistrationStore) store(regs map[string]*domain.ClientRegistration) error {
	data, err := json.Marshal(regs)
	if err != nil {
		return err
	}
	path := s.path()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Get returns the registration for the exact callback.
func (s *FileRegistrationStore) Get(_ context.Context, serverName, callbackURL string) (*domain.ClientRegistration, error) {
	regs, err := s.load()
	if err != nil {
		return nil, err
	}
	if r, ok := regs[regKey(serverName, callbackURL)]; ok {
		return r, nil
	}
	return nil, port.ErrNotFound
}

// GetAny returns any registration for the server.
func (s *FileRegistrationStore) GetAny(_ context.Context, serverName string) (*domain.ClientRegistration, error) {
	regs, err := s.load()
	if err != nil {
		return nil, err
	}
	for k, r := range regs {
		if strings.HasPrefix(k, serverName+"|") {
			return r, nil
		}
	}
	return nil, port.ErrNotFound
}

// Save records a registration.
func (s *FileRegistrationStore) Save(_ context.Context, serverName, callbackURL string, r *domain.ClientRegistration) error {
	regs, err := s.load()
	if err != nil {
		return err
	}
	regs[regKey(serverName, callbackURL)] = r
	return s.store(regs)
}
