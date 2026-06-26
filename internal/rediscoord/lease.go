package rediscoord

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Leaser implements channels.Leaser with Redis keys. Each lease is a single
// key whose value is the current gateway holder ID and whose TTL is the
// freshness window.
type Leaser struct {
	client *redis.Client
	prefix string
}

func NewLeaser(client *redis.Client, prefix string) *Leaser {
	prefix = strings.Trim(prefix, ":")
	if prefix == "" {
		prefix = "fastagent"
	}
	return &Leaser{client: client, prefix: prefix}
}

func (l *Leaser) Acquire(ctx context.Context, channel, accountID, holderID string, ttl time.Duration) (bool, error) {
	key := l.key(channel, accountID)
	ok, err := l.client.SetNX(ctx, key, holderID, ttl).Result()
	if err != nil {
		return false, err
	}
	if ok {
		return true, nil
	}
	current, err := l.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if current != holderID {
		return false, nil
	}
	return l.Renew(ctx, channel, accountID, holderID, ttl)
}

func (l *Leaser) Renew(ctx context.Context, channel, accountID, holderID string, ttl time.Duration) (bool, error) {
	res, err := renewScript.Run(ctx, l.client, []string{l.key(channel, accountID)}, holderID, int64(ttl/time.Millisecond)).Int()
	if err != nil {
		return false, err
	}
	return res == 1, nil
}

func (l *Leaser) Release(ctx context.Context, channel, accountID, holderID string) error {
	_, err := releaseScript.Run(ctx, l.client, []string{l.key(channel, accountID)}, holderID).Int()
	return err
}

func (l *Leaser) key(channel, accountID string) string {
	if accountID == "" {
		accountID = "default"
	}
	return fmt.Sprintf("%s:lease:channel:%s:%s", l.prefix, channel, accountID)
}

var renewScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0
`)

var releaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)
