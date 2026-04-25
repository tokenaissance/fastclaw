package store

import (
	"context"

	"github.com/fastclaw-ai/fastclaw/internal/config"
)

// userIDFromCtx resolves the per-request user ID for SQL row scoping.
// Falls back to config.DefaultUserID ("local") when the context carries no
// user — that path covers single-user / CLI / heartbeat callers, while
// HTTP, /v1/, and channel ingress already inject the resolved user via
// config.WithUserID in their middleware.
func userIDFromCtx(ctx context.Context) string {
	if uid := config.UserIDFromContext(ctx); uid != "" {
		return uid
	}
	return config.DefaultUserID
}
