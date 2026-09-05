package usecase

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/port"
)

const refreshLockTTL = 30 * time.Second

// RefreshLocks provides per-key single-flight refresh locks. It MUST be a
// process-level singleton shared by every agent loop — concurrent loops
// using the same refresh token would otherwise race the rotation and one
// of them would fail with an already-used token.
type RefreshLocks struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewRefreshLocks creates an empty lock table.
func NewRefreshLocks() *RefreshLocks {
	return &RefreshLocks{locks: make(map[string]*sync.Mutex)}
}

// Lock returns the unlock func for key, creating the mutex on first use.
func (l *RefreshLocks) Lock(key string) func() {
	l.mu.Lock()
	m, ok := l.locks[key]
	if !ok {
		m = &sync.Mutex{}
		l.locks[key] = m
	}
	l.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// RefreshToken rotates an expired access token using the refresh token.
type RefreshToken struct {
	Meta     port.MetadataFetcher
	Regs     port.ClientRegistrationStore
	Tokens   port.TokenStore
	Exchange port.AuthorizationCodeExchanger
	Locks    *RefreshLocks
	// Locker is the optional cross-instance mutex. Best-effort: when nil
	// or unavailable, the process lock + optimistic retry still keep
	// rotation safe.
	Locker port.DistributedLocker
}

// RefreshInput identifies whose token to refresh and where to discover
// the token endpoint.
type RefreshInput struct {
	UserID     string
	AgentID    string
	ServerName string
	ServerURL  string
	// ActorUserID is the session principal making the call. Only
	// TokenProvider.AccessToken consumes it for the owner-only gate
	// (scheme A); refresh/status/revoke operate on the identity named by
	// UserID regardless of actor. Empty = legacy single-user mode.
	ActorUserID string
}

// Execute refreshes and persists the rotated token pair.
func (uc *RefreshToken) Execute(ctx context.Context, in RefreshInput) (*domain.OAuthTokens, error) {
	key := domain.StoreKey(in.UserID, in.AgentID, in.ServerName)
	// Cross-instance lock: wait a bounded time for the holder to finish,
	// then fall through — the optimistic retry below is the safety net
	// when no distributed lock is available.
	if uc.Locker != nil {
		locked := false
		for attempt := 0; attempt < 10; attempt++ {
			ok, err := uc.Locker.Acquire(ctx, key, refreshLockTTL)
			if err != nil {
				// Lock backend failure: fall back to process lock + retry.
				slog.Warn("oauth refresh distributed lock unavailable, falling back", "error", err)
				break
			}
			if ok {
				locked = true
				break
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
		if locked {
			defer func() {
				_ = uc.Locker.Release(context.Background(), key)
			}()
		} else {
			// Lock backend failure: fall back to process lock + retry.
			slog.Warn("oauth refresh distributed lock not acquired, continuing with retry safety net")
		}
	}
	unlock := uc.Locks.Lock(key)
	defer unlock()

	tokens, err := uc.Tokens.Load(ctx, key)
	if err != nil {
		return nil, err
	}
	// Double-check under the lock: two callers may both have observed the
	// same stale token; only the first needs to burn a rotation.
	if !domain.TokenNeedsRefresh(tokens, time.Now().UTC(), domain.RefreshBuffer) {
		return tokens, nil
	}
	if tokens.RefreshToken == "" {
		return nil, fmt.Errorf("oauth: no refresh token stored")
	}
	md, err := uc.Meta.Fetch(ctx, in.ServerURL)
	if err != nil {
		return nil, fmt.Errorf("oauth: discovery: %w", err)
	}
	reg, err := uc.Regs.GetAny(ctx, in.ServerName)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		fresh, err := uc.Exchange.Refresh(ctx, md.TokenEndpoint, tokens, in.ServerURL, reg)
		if err == nil {
			// Provider didn't rotate the refresh token: keep the old one.
			if fresh.RefreshToken == "" {
				fresh.RefreshToken = tokens.RefreshToken
			}
			fresh.Issuer = md.Issuer
			if err := uc.Tokens.Save(ctx, key, fresh); err != nil {
				return nil, fmt.Errorf("oauth: refresh succeeded at provider but persisting failed (%v); re-authorization required", err)
			}
			return fresh, nil
		}
		lastErr = err
		// Rotation race with another instance: the loser's refresh token
		// was just invalidated by the winner. Reload — if the store now
		// holds a fresh token, the other instance rotated it for us.
		if current, loadErr := uc.Tokens.Load(ctx, key); loadErr == nil &&
			!domain.TokenNeedsRefresh(current, time.Now().UTC(), domain.RefreshBuffer) {
			return current, nil
		}
		if attempt < 2 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	return nil, fmt.Errorf("oauth: refresh: %w", lastErr)
}
