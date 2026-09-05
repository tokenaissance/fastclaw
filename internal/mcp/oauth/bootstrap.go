// Package oauth is the fastagent MCP OAuth client: authorization-code +
// PKCE S256 + refresh rotation for remote MCP servers such as Quandora.
//
// It follows the Clean Architecture split in ./domain, ./port, ./usecase
// and ./adapter. The Bootstrap below is the framework-level assembly — a
// process singleton so the refresh lock (and stores) are shared across
// every concurrent agent loop.
package oauth

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/adapter"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/port"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/usecase"
)

const (
	discoveryCacheTTL        = time.Hour
	defaultMaxPendingPerUser = 20
)

// DBProvider is the minimal shared-persistence surface the bootstrap
// needs; *store.DBStore satisfies it. The SQL stores are dialect-aware
// and shared by every deployment shape: Postgres for multi-instance
// production, SQLite for single-instance/local — the deployed daemon and
// CLI always pass a DB and never fall back to file stores.
type DBProvider interface {
	DB() *sql.DB
	Dialect() string
}

// Options controls bootstrap assembly.
type Options struct {
	// Home is the FASTAGENT_HOME root; only used by the file-backed
	// test fallback when DB is nil. Deployed paths always pass DB.
	Home string
	// DB selects the shared SQL stores (sqlite single instance /
	// postgres multi instance). nil → file-backed stores (tests /
	// no-DB degradation only — not a deployment path).
	DB DBProvider
	// Locker is the optional cross-instance refresh lock (Redis).
	Locker port.DistributedLocker
	// MaxPendingPerUser caps concurrent pending authorizations per user
	// ("待授权会话堆积" threat). 0 → default 20.
	MaxPendingPerUser int
}

// Bootstrap wires every OAuth use case to concrete adapters.
type Bootstrap struct {
	Meta      port.MetadataFetcher
	Registrar port.ClientRegistrar
	Regs      port.ClientRegistrationStore
	Pending   port.PendingAuthStore
	Tokens    port.TokenStore
	Exchange  port.AuthorizationCodeExchanger
	Locks     *usecase.RefreshLocks

	Start    *usecase.StartAuthorization
	Complete *usecase.CompleteAuthorization
	Refresh  *usecase.RefreshToken
	Provider *usecase.TokenProvider
	Revoke   *usecase.RevokeToken
	Status   *usecase.Status
}

var (
	globalMu sync.RWMutex
	global   *Bootstrap
)

// Init assembles the process singleton. secret must be non-empty
// (fail-closed: we never persist refresh tokens unencrypted).
func Init(secret string, opts Options) (*Bootstrap, error) {
	if secret == "" {
		return nil, errors.New("oauth: FASTAGENT_OAUTH_SECRET is required to enable MCP OAuth")
	}
	cryptor, err := adapter.NewAESGCMCryptor(secret)
	if err != nil {
		return nil, err
	}

	var tokens port.TokenStore
	var pending port.PendingAuthStore
	var regs port.ClientRegistrationStore
	if opts.DB != nil {
		db, dialect := opts.DB.DB(), opts.DB.Dialect()
		tokens = &adapter.DBTokenStore{DB: db, Dialect: dialect, Crypt: cryptor}
		pending = &adapter.DBPendingStore{DB: db, Dialect: dialect, Crypt: cryptor}
		regs = &adapter.DBRegistrationStore{DB: db, Dialect: dialect}
	} else {
		// File-backed stores are the test / no-DB degradation path only.
		// Deployed daemons and the CLI always pass a DB (sqlite single
		// instance / postgres multi instance); OAuth state must stay in
		// the shared database, never on instance-local files.
		slog.Warn("oauth: no DB provider passed; using file-backed stores (test-only fallback, not a deployment path)")
		if opts.Home == "" {
			return nil, errors.New("oauth: Home is required for file-backed stores")
		}
		tokens = &adapter.FileTokenStore{Root: opts.Home, Crypt: cryptor}
		pending = &adapter.FilePendingStore{Root: opts.Home, Crypt: cryptor}
		regs = &adapter.FileRegistrationStore{Root: opts.Home}
	}

	cli := &http.Client{Timeout: 30 * time.Second}
	b := &Bootstrap{
		Meta:      adapter.NewCachingMetadataFetcher(cli, discoveryCacheTTL),
		Registrar: &adapter.HTTPClientRegistrar{Client: cli},
		Regs:      regs,
		Pending:   pending,
		Tokens:    tokens,
		Exchange:  &adapter.HTTPCodeExchanger{Client: cli},
		Locks:     usecase.NewRefreshLocks(),
	}
	maxPending := opts.MaxPendingPerUser
	if maxPending == 0 {
		maxPending = defaultMaxPendingPerUser
	}
	b.Start = &usecase.StartAuthorization{
		Meta: b.Meta, Registrar: b.Registrar, Regs: b.Regs, Pending: b.Pending,
		MaxPendingPerUser: maxPending,
	}
	b.Complete = &usecase.CompleteAuthorization{Meta: b.Meta, Pending: b.Pending, Regs: b.Regs, Tokens: b.Tokens, Exchange: b.Exchange}
	b.Refresh = &usecase.RefreshToken{Meta: b.Meta, Regs: b.Regs, Tokens: b.Tokens, Exchange: b.Exchange, Locks: b.Locks, Locker: opts.Locker}
	b.Provider = &usecase.TokenProvider{Tokens: b.Tokens, Refresh: b.Refresh}
	b.Revoke = &usecase.RevokeToken{Meta: b.Meta, Regs: b.Regs, Tokens: b.Tokens, Exchange: b.Exchange}
	b.Status = &usecase.Status{Tokens: b.Tokens}

	globalMu.Lock()
	global = b
	globalMu.Unlock()
	return b, nil
}

// Global returns the process singleton, or nil when OAuth is not enabled.
func Global() *Bootstrap {
	globalMu.RLock()
	defer globalMu.RUnlock()
	return global
}
