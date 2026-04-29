package gateway

import (
	"context"
	"log/slog"
)

// InvalidateUser drops a user's cached UserSpace so the next access reloads
// it from the DB. Called by admin handlers after agent / provider /
// channel writes so changes take effect without a process restart.
func (g *Gateway) InvalidateUser(userID string) {
	if g.users == nil || userID == "" {
		return
	}
	g.users.invalidate(userID)
	slog.Info("user space invalidated; will reload on next access", "user", userID)
}

// ReloadAgents is kept on Gateway for callers (admin API after agent CRUD)
// that want to force a refresh of every loaded space. The new model lazy-
// loads on every auth, so the practical effect is just dropping caches.
func (g *Gateway) ReloadAgents() error {
	if g.users == nil {
		return nil
	}
	for _, sp := range g.users.all() {
		g.users.invalidate(sp.UserID)
	}
	slog.Info("hot-reload: invalidated all loaded user spaces")
	return nil
}

// reloadAgentForUser is a finer-grained invalidate used by setup handlers
// after a single user mutates their own agents.
func (g *Gateway) reloadAgentForUser(_ context.Context, userID string) {
	g.InvalidateUser(userID)
}
