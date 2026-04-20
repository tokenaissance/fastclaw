package setup

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/config"
)

// readIdentityFile returns the bytes of one of an agent's identity files
// (SOUL.md, BOOTSTRAP.md, agent.json, …). It always goes through the
// configured Store so file-mode and Postgres-mode deployments stay consistent.
//
// When no Store is configured — only in tests or during early startup — the
// function falls back to reading the agent's home dir directly. Production
// always has one (gateway sets it unconditionally).
func (s *Server) readIdentityFile(ctx context.Context, agentID, filename string) ([]byte, error) {
	if s.dataStore != nil {
		return s.dataStore.GetWorkspaceFile(ctx, agentID, filename)
	}
	homePath, err := config.AgentHomeDir(agentID)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(homePath, filename))
}

// writeIdentityFile persists one of an agent's identity files via the Store.
// Same Store→filesystem fallback as readIdentityFile.
//
// For Postgres-backed stores this is the only durable write — the filesystem
// copy goes away on pod restart, which is the whole point of stateless pods.
// For file stores the Store writes to ~/.fastclaw/agents/<id>/agent/<name>
// which is where the agent runtime reads from, so file-mode behavior is
// unchanged.
func (s *Server) writeIdentityFile(ctx context.Context, agentID, filename string, data []byte) error {
	if s.dataStore != nil {
		return s.dataStore.SaveWorkspaceFile(ctx, agentID, filename, data)
	}
	homePath, err := config.AgentHomeDir(agentID)
	if err != nil {
		return err
	}
	target := filepath.Join(homePath, filename)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return os.WriteFile(target, data, 0o644)
}

// isStoreNotFound recognises the "file not found" signal across Store
// implementations. FileStore returns os.ErrNotExist; DBStore (database/sql)
// returns sql.ErrNoRows; occasionally we get a wrapped string form. Covering
// all three keeps the UI's "tab is empty" behavior consistent regardless of
// backend.
func isStoreNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, os.ErrNotExist) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "no rows in result set") || strings.Contains(msg, "no such file")
}
