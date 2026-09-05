package gateway

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// Cross-replica reload fallback for deployments WITHOUT Redis, scoped
// per USER. Account-level state changes (e.g. an MCP OAuth authorization
// for one of the user's agents) stamp that user's epoch row; every
// replica's poller notices and drops ONLY that user's cached UserSpace —
// never a system-wide reload, so one tenant's action cannot flush every
// other tenant's caches. With Redis the broadcast is instant; the poller
// still runs as a safety net for lost messages / Redis outages.
const (
	// reloadEpochPollInterval is how often replicas check the epoch
	// table. Only rows for users who ever authorized/revoked exist.
	reloadEpochPollInterval = 30 * time.Second
)

// agentReloadEpochs tracks per-user "state changed" markers in shared
// configs_kv rows (kind=mcp_oauth_reload, scope=user, scope_id=userID).
type agentReloadEpochs struct {
	db  *store.DBStore
	mu  sync.Mutex
	now func() time.Time
	// last is the epoch each user had when this instance last acted.
	last map[string]string
}

func newAgentReloadEpochs(db *store.DBStore) *agentReloadEpochs {
	e := &agentReloadEpochs{db: db, now: time.Now, last: map[string]string{}}
	// Seed at boot so old epochs left by a previous process do not
	// trigger reloads on startup.
	if rows, err := db.ListAgentReloadEpochs(context.Background()); err == nil {
		for _, r := range rows {
			e.last[r.UserID] = r.Epoch
		}
	}
	return e
}

// Bump stamps a fresh epoch for userID and marks it as seen locally (the
// caller has already invalidated its own caches for that user).
func (e *agentReloadEpochs) Bump(ctx context.Context, userID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	v := strconv.FormatInt(e.now().UnixNano(), 10)
	if err := e.db.UpsertAgentReloadEpoch(ctx, userID, v); err != nil {
		return err
	}
	e.last[userID] = v
	return nil
}

// SyncUser marks a user's current epoch as seen without triggering a
// reload. Used by the Redis broadcast path so a receiver that already
// invalidated the user doesn't reload again on the next poll.
func (e *agentReloadEpochs) SyncUser(ctx context.Context, userID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if v, err := e.db.GetAgentReloadEpoch(ctx, userID); err == nil && v != "" {
		e.last[userID] = v
	}
}

// Poll returns the users whose epoch changed since this instance last
// looked; the caller should InvalidateUser each of them.
func (e *agentReloadEpochs) Poll(ctx context.Context) ([]string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rows, err := e.db.ListAgentReloadEpochs(ctx)
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, r := range rows {
		if r.Epoch == "" {
			continue
		}
		if e.last[r.UserID] != r.Epoch {
			e.last[r.UserID] = r.Epoch
			changed = append(changed, r.UserID)
		}
	}
	return changed, nil
}
