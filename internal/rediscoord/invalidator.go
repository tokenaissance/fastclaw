package rediscoord

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/redis/go-redis/v9"
)

// Invalidator broadcasts "reload agents" across gateway replicas over
// Redis pub/sub. Used when an OAuth authorization completes on one
// instance so every other instance drops its cached UserSpace and
// reconnects the now-authorized MCP server on the next request.
// No-op path is the caller's choice: Publish when the client is nil.
type Invalidator struct {
	client  *redis.Client
	channel string
}

type reloadMessage struct {
	Kind   string `json:"kind"`
	UserID string `json:"userId"`
}

// NewInvalidator creates a broadcaster on channel (prefix:channel).
func NewInvalidator(client *redis.Client, channel string) *Invalidator {
	return &Invalidator{client: client, channel: channel}
}

// PublishReloadFor asks every replica to invalidate the given user's
// cached UserSpace.
func (i *Invalidator) PublishReloadFor(ctx context.Context, userID string) error {
	if i == nil || i.client == nil {
		return nil
	}
	raw, _ := json.Marshal(reloadMessage{Kind: "reload-agents", UserID: userID})
	return i.client.Publish(ctx, i.channel, raw).Err()
}

// Run subscribes and invokes onReload with the target userID for each
// reload message until ctx is done.
func (i *Invalidator) Run(ctx context.Context, onReload func(userID string)) error {
	if i == nil || i.client == nil {
		<-ctx.Done()
		return nil
	}
	sub := i.client.Subscribe(ctx, i.channel)
	defer sub.Close()
	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-ch:
			if !ok {
				return nil
			}
			if strings.Contains(msg.Payload, `"kind":"reload-agents"`) {
				var m reloadMessage
				_ = json.Unmarshal([]byte(msg.Payload), &m)
				onReload(m.UserID)
			}
		}
	}
}
