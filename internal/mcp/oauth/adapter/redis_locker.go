package adapter

import (
	"context"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisRefreshLocker is a cross-instance mutex for refresh rotation
// (SET NX + TTL, Lua-guarded release). Same pattern as rediscoord's
// channel leases, but keyed by the oauth store key.
type RedisRefreshLocker struct {
	Client *redis.Client
	Prefix string
}

func (l *RedisRefreshLocker) key(key string) string {
	prefix := strings.Trim(l.Prefix, ":")
	if prefix == "" {
		prefix = "fastagent"
	}
	return prefix + ":oauth:lock:" + key
}

// Acquire tries to take the lock for ttl. A false, nil result means
// someone else holds it.
func (l *RedisRefreshLocker) Acquire(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	return l.Client.SetNX(ctx, l.key(key), "1", ttl).Result()
}

// Release drops the lock (guarded by the token value).
func (l *RedisRefreshLocker) Release(ctx context.Context, key string) error {
	return l.Client.Del(ctx, l.key(key)).Err()
}
