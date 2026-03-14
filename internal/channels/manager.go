package channels

import (
	"context"
	"log/slog"
	"sync"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

// Manager manages all channel instances and routes outbound messages.
type Manager struct {
	channels map[string]Channel // key: "channel:accountID"
	bus      *bus.MessageBus
}

// NewManager creates a new channel manager.
func NewManager(mb *bus.MessageBus) *Manager {
	return &Manager{
		channels: make(map[string]Channel),
		bus:      mb,
	}
}

// Register adds a channel to the manager keyed by channel:accountID.
func (m *Manager) Register(ch Channel) {
	key := channelKey(ch.Name(), ch.AccountID())
	m.channels[key] = ch
}

// Start launches all channels and the outbound message router.
func (m *Manager) Start(ctx context.Context) {
	var wg sync.WaitGroup

	// Start outbound router
	wg.Add(1)
	go func() {
		defer wg.Done()
		m.routeOutbound(ctx)
	}()

	// Start each channel
	for key, ch := range m.channels {
		wg.Add(1)
		go func(k string, c Channel) {
			defer wg.Done()
			slog.Info("starting channel", "key", k)
			if err := c.Start(ctx); err != nil {
				slog.Error("channel stopped with error", "key", k, "error", err)
			}
		}(key, ch)
	}

	wg.Wait()
}

func (m *Manager) routeOutbound(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-m.bus.Outbound:
			key := channelKey(msg.Channel, msg.AccountID)
			ch, ok := m.channels[key]
			if !ok {
				slog.Warn("unknown outbound channel", "key", key)
				continue
			}
			if err := ch.Send(msg.ChatID, msg.Text); err != nil {
				slog.Error("send message failed", "key", key, "error", err)
			}
		}
	}
}

// BotUsername returns the bot username for a given channel:accountID pair.
func (m *Manager) BotUsername(channel, accountID string) string {
	key := channelKey(channel, accountID)
	ch, ok := m.channels[key]
	if !ok {
		return ""
	}
	return ch.BotUsername()
}

func channelKey(channel, accountID string) string {
	if accountID == "" {
		return channel + ":"
	}
	return channel + ":" + accountID
}
