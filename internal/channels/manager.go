package channels

import (
	"context"
	"log/slog"
	"sync"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

// Manager manages all channel instances and routes outbound messages.
type Manager struct {
	channels map[string]Channel
	bus      *bus.MessageBus
}

// NewManager creates a new channel manager.
func NewManager(mb *bus.MessageBus) *Manager {
	return &Manager{
		channels: make(map[string]Channel),
		bus:      mb,
	}
}

// Register adds a channel to the manager.
func (m *Manager) Register(ch Channel) {
	m.channels[ch.Name()] = ch
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
	for name, ch := range m.channels {
		wg.Add(1)
		go func(n string, c Channel) {
			defer wg.Done()
			slog.Info("starting channel", "name", n)
			if err := c.Start(ctx); err != nil {
				slog.Error("channel stopped with error", "name", n, "error", err)
			}
		}(name, ch)
	}

	wg.Wait()
}

func (m *Manager) routeOutbound(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-m.bus.Outbound:
			ch, ok := m.channels[msg.Channel]
			if !ok {
				slog.Warn("unknown outbound channel", "channel", msg.Channel)
				continue
			}
			if err := ch.Send(msg.ChatID, msg.Text); err != nil {
				slog.Error("send message failed", "channel", msg.Channel, "error", err)
			}
		}
	}
}
