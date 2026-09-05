// Package port declares the OAuth client ports (interfaces). Use cases
// depend only on these interfaces; adapters implement them; the framework
// wires them. No adapter may import a use case.
package port

import (
	"context"
	"errors"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
)

// ErrNotFound is returned by stores when a key is absent.
var ErrNotFound = errors.New("oauth: not found")

// MetadataFetcher fetches OAuth Authorization Server Metadata.
type MetadataFetcher interface {
	Fetch(ctx context.Context, serverURL string) (*domain.DiscoveryMetadata, error)
}

// ClientRegistrar performs dynamic client registration (RFC 7591).
type ClientRegistrar interface {
	Register(ctx context.Context, registrationEndpoint string, redirectURIs []string) (*domain.ClientRegistration, error)
}

// AuthorizationCodeExchanger speaks the token endpoint: exchange code,
// refresh, and revoke.
type AuthorizationCodeExchanger interface {
	// Exchange trades the authorization code for tokens (PKCE). resource is
	// the RFC 8707 resource indicator; ASs that issued the code for a
	// resource (e.g. Quandora) require it here too.
	Exchange(ctx context.Context, tokenEndpoint, code, codeVerifier, redirectURI, resource string, reg *domain.ClientRegistration) (*domain.OAuthTokens, error)
	// Refresh rotates an access token. resource is passed through so the AS
	// scopes the new pair to the same protected resource (RFC 8707).
	Refresh(ctx context.Context, tokenEndpoint string, tokens *domain.OAuthTokens, resource string, reg *domain.ClientRegistration) (*domain.OAuthTokens, error)
	// Revoke revokes a token at the provider. resource is optional and only
	// sent when the caller knows the protected resource (RFC 8707).
	Revoke(ctx context.Context, revocationEndpoint, token, clientID, resource string) error
}

// TokenStore persists encrypted credentials per key (user/agent/server).
type TokenStore interface {
	Save(ctx context.Context, key string, t *domain.OAuthTokens) error
	Load(ctx context.Context, key string) (*domain.OAuthTokens, error)
	Delete(ctx context.Context, key string) error
}

// PendingAuthStore holds one-shot pending authorizations; Take consumes
// the entry (single-use state).
type PendingAuthStore interface {
	Save(ctx context.Context, p *domain.PendingAuth) error
	Take(ctx context.Context, state string) (*domain.PendingAuth, error)
	// CountActive returns the number of non-expired pending authorizations
	// for a user (used for the per-tenant capacity cap). Implementations
	// may lazily purge expired rows while counting.
	CountActive(ctx context.Context, userID string) (int, error)
}

// ClientRegistrationStore persists dynamic registrations keyed by
// (serverName, callbackURL) — the same server can have distinct clients
// for the HTTPS web callback and a CLI loopback callback.
type ClientRegistrationStore interface {
	Get(ctx context.Context, serverName, callbackURL string) (*domain.ClientRegistration, error)
	// GetAny returns any registration for the server (refresh/revoke don't
	// depend on the exact callback — client_id is public information).
	GetAny(ctx context.Context, serverName string) (*domain.ClientRegistration, error)
	Save(ctx context.Context, serverName, callbackURL string, r *domain.ClientRegistration) error
}

// BrowserOpener opens the system browser for the interactive flow.
type BrowserOpener interface {
	Open(ctx context.Context, url string) error
}

// CallbackReceiver listens for a loopback OAuth callback.
type CallbackReceiver interface {
	Listen(ctx context.Context) (<-chan domain.CallbackParams, error)
}

// Cryptor encrypts/decrypts stored credentials.
type Cryptor interface {
	Encrypt(ctx context.Context, plaintext []byte) ([]byte, error)
	Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error)
}

// DistributedLocker is an optional cross-instance mutex for refresh
// rotation. When unavailable (nil / Redis down) the process-level lock
// plus the optimistic retry in RefreshToken still keep rotation safe —
// the distributed lock only avoids wasted rotations.
type DistributedLocker interface {
	Acquire(ctx context.Context, key string, ttl time.Duration) (bool, error)
	Release(ctx context.Context, key string) error
}
