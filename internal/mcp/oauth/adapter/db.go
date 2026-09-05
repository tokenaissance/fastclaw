package adapter

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/port"
)

// DBTokenStore persists encrypted credentials in the shared
// mcp_oauth_tokens table (Postgres in production → visible to every
// gateway instance).
type DBTokenStore struct {
	DB      *sql.DB
	Dialect string // "postgres" or "sqlite"
	Crypt   port.Cryptor
}

// Save upserts the encrypted credential row.
func (s *DBTokenStore) Save(ctx context.Context, key string, t *domain.OAuthTokens) error {
	plain, err := json.Marshal(t)
	if err != nil {
		return err
	}
	enc, err := s.Crypt.Encrypt(ctx, plain)
	if err != nil {
		return err
	}
	q := "INSERT INTO mcp_oauth_tokens (token_key, ciphertext, updated_at) VALUES (" +
		ph(s.Dialect, 1) + ", " + ph(s.Dialect, 2) + ", " + ph(s.Dialect, 3) + ") " +
		"ON CONFLICT (token_key) DO UPDATE SET ciphertext = EXCLUDED.ciphertext, updated_at = EXCLUDED.updated_at"
	_, err = s.DB.ExecContext(ctx, q, key, enc, time.Now().UTC())
	return err
}

// Load reads and decrypts a credential row.
func (s *DBTokenStore) Load(ctx context.Context, key string) (*domain.OAuthTokens, error) {
	var enc []byte
	err := s.DB.QueryRowContext(ctx, "SELECT ciphertext FROM mcp_oauth_tokens WHERE token_key = "+ph(s.Dialect, 1), key).Scan(&enc)
	if errors.Is(err, sql.ErrNoRows) {
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

// Delete removes a credential row.
func (s *DBTokenStore) Delete(ctx context.Context, key string) error {
	_, err := s.DB.ExecContext(ctx, "DELETE FROM mcp_oauth_tokens WHERE token_key = "+ph(s.Dialect, 1), key)
	return err
}

// DBPendingStore persists one-shot pending authorizations in the shared
// mcp_oauth_pending table. Take is a single DELETE...RETURNING so two
// gateway instances can never consume the same state.
type DBPendingStore struct {
	DB      *sql.DB
	Dialect string
	Crypt   port.Cryptor
}

// Save upserts an encrypted pending authorization.
func (s *DBPendingStore) Save(ctx context.Context, p *domain.PendingAuth) error {
	plain, err := json.Marshal(p)
	if err != nil {
		return err
	}
	enc, err := s.Crypt.Encrypt(ctx, plain)
	if err != nil {
		return err
	}
	q := "INSERT INTO mcp_oauth_pending (state, ciphertext, user_id, agent_id, server_name, created_at, expires_at) VALUES (" +
		ph(s.Dialect, 1) + ", " + ph(s.Dialect, 2) + ", " + ph(s.Dialect, 3) + ", " + ph(s.Dialect, 4) + ", " +
		ph(s.Dialect, 5) + ", " + ph(s.Dialect, 6) + ", " + ph(s.Dialect, 7) + ") " +
		"ON CONFLICT (state) DO UPDATE SET ciphertext = EXCLUDED.ciphertext, user_id = EXCLUDED.user_id, agent_id = EXCLUDED.agent_id, server_name = EXCLUDED.server_name, created_at = EXCLUDED.created_at, expires_at = EXCLUDED.expires_at"
	_, err = s.DB.ExecContext(ctx, q, p.State, enc, p.UserID, p.AgentID, p.ServerName, p.CreatedAt, p.ExpiresAt)
	return err
}

// Take atomically consumes a pending authorization.
func (s *DBPendingStore) Take(ctx context.Context, state string) (*domain.PendingAuth, error) {
	q := "DELETE FROM mcp_oauth_pending WHERE state = " + ph(s.Dialect, 1) +
		" RETURNING ciphertext, expires_at"
	var enc []byte
	var expiresAt time.Time
	err := s.DB.QueryRowContext(ctx, q, state).Scan(&enc, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, port.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !expiresAt.IsZero() && time.Now().UTC().After(expiresAt) {
		return nil, port.ErrNotFound
	}
	plain, err := s.Crypt.Decrypt(ctx, enc)
	if err != nil {
		return nil, err
	}
	var p domain.PendingAuth
	if err := json.Unmarshal(plain, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// CountActive returns non-expired pending count for a user, lazily
// deleting expired rows for that user.
func (s *DBPendingStore) CountActive(ctx context.Context, userID string) (int, error) {
	now := time.Now().UTC()
	if _, err := s.DB.ExecContext(ctx,
		"DELETE FROM mcp_oauth_pending WHERE user_id = "+ph(s.Dialect, 1)+" AND expires_at < "+ph(s.Dialect, 2),
		userID, now); err != nil {
		return 0, err
	}
	var n int
	err := s.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM mcp_oauth_pending WHERE user_id = "+ph(s.Dialect, 1), userID).Scan(&n)
	return n, err
}

// DBRegistrationStore persists dynamic client registrations in the
// shared mcp_oauth_clients table (client_id is public info — stored
// plaintext by design).
type DBRegistrationStore struct {
	DB      *sql.DB
	Dialect string
}

// Get returns the registration for the exact callback URL.
func (s *DBRegistrationStore) Get(ctx context.Context, serverName, callbackURL string) (*domain.ClientRegistration, error) {
	var clientID string
	var registeredAt time.Time
	err := s.DB.QueryRowContext(ctx,
		"SELECT client_id, registered_at FROM mcp_oauth_clients WHERE server_name = "+ph(s.Dialect, 1)+" AND callback_url = "+ph(s.Dialect, 2),
		serverName, callbackURL).Scan(&clientID, &registeredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, port.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &domain.ClientRegistration{
		ClientID:                clientID,
		RedirectURIs:            []string{callbackURL},
		TokenEndpointAuthMethod: "none",
		RegisteredAt:            registeredAt,
	}, nil
}

// GetAny returns any registration for the server.
func (s *DBRegistrationStore) GetAny(ctx context.Context, serverName string) (*domain.ClientRegistration, error) {
	var clientID, callbackURL string
	var registeredAt time.Time
	err := s.DB.QueryRowContext(ctx,
		"SELECT client_id, callback_url, registered_at FROM mcp_oauth_clients WHERE server_name = "+ph(s.Dialect, 1)+" ORDER BY registered_at LIMIT 1",
		serverName).Scan(&clientID, &callbackURL, &registeredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, port.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &domain.ClientRegistration{
		ClientID:                clientID,
		RedirectURIs:            []string{callbackURL},
		TokenEndpointAuthMethod: "none",
		RegisteredAt:            registeredAt,
	}, nil
}

// Save records a registration (upsert).
func (s *DBRegistrationStore) Save(ctx context.Context, serverName, callbackURL string, r *domain.ClientRegistration) error {
	q := "INSERT INTO mcp_oauth_clients (server_name, callback_url, client_id, registered_at) VALUES (" +
		ph(s.Dialect, 1) + ", " + ph(s.Dialect, 2) + ", " + ph(s.Dialect, 3) + ", " + ph(s.Dialect, 4) + ") " +
		"ON CONFLICT (server_name, callback_url) DO UPDATE SET client_id = EXCLUDED.client_id, registered_at = EXCLUDED.registered_at"
	_, err := s.DB.ExecContext(ctx, q, serverName, callbackURL, r.ClientID, time.Now().UTC())
	return err
}

// ph returns the dialect-appropriate SQL placeholder for the nth
// argument (sqlite "?", postgres "$n").
func ph(dialect string, n int) string {
	if dialect == "postgres" {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}
