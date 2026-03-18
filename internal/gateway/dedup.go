package gateway

import (
	"context"
	"fmt"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

const dedupTTL = 60 * time.Second

// dedupEntry tracks when a message was first seen.
type dedupEntry struct {
	seenAt time.Time
}

// isDuplicate returns true if this group message has already been seen.
// Uses channel:chatID:messageID as the dedup key.
func (g *Gateway) isDuplicate(msg bus.InboundMessage) bool {
	// In Telegram supergroups, each bot gets a different message_id for the same message.
	// So we deduplicate using chatID + userID + text hash instead.
	if msg.PeerKind != "group" {
		return false
	}
	key := fmt.Sprintf("%s:%s:%s:%x", msg.Channel, msg.ChatID, msg.UserID, hashString(msg.Text))
	_, loaded := g.dedup.LoadOrStore(key, dedupEntry{seenAt: time.Now()})
	return loaded
}

// cleanupDedup periodically removes expired entries from the dedup cache.
func (g *Gateway) cleanupDedup(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			g.dedup.Range(func(key, value any) bool {
				entry := value.(dedupEntry)
				if now.Sub(entry.seenAt) > dedupTTL {
					g.dedup.Delete(key)
				}
				return true
			})
		}
	}
}

func hashString(s string) uint32 {
	var h uint32
	for _, c := range s {
		h = h*31 + uint32(c)
	}
	return h
}
