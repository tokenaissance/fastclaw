package channels

import (
	"context"
	"log/slog"
	"sync"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

// Manager manages all channel instances and routes outbound messages.
type Manager struct {
	mu       sync.Mutex
	channels map[string]Channel // key: "channel:accountID"
	bus      *bus.MessageBus
	// Captured by Start so RegisterAndStart can hot-launch goroutines for
	// channels added after the initial bootstrap. nil until Start runs.
	rootCtx context.Context
}

// NewManager creates a new channel manager.
func NewManager(mb *bus.MessageBus) *Manager {
	return &Manager{
		channels: make(map[string]Channel),
		bus:      mb,
	}
}

// Register adds a channel to the manager keyed by channel:accountID.
// Use this BEFORE Start; for hot-add after Start, use RegisterAndStart.
func (m *Manager) Register(ch Channel) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := channelKey(ch.Name(), ch.AccountID())
	m.channels[key] = ch
}

// RegisterAndStart adds a channel AND, if Start has already run, kicks
// off its polling goroutine immediately. Used by the dashboard's
// channel-config handlers so a freshly-saved Telegram bot starts
// receiving updates without a process restart.
//
// Safe to call before Start too — falls back to plain Register in that
// case (Start picks it up like any other entry).
func (m *Manager) RegisterAndStart(ch Channel) {
	m.mu.Lock()
	key := channelKey(ch.Name(), ch.AccountID())
	m.channels[key] = ch
	ctx := m.rootCtx
	m.mu.Unlock()
	if ctx == nil {
		return
	}
	go func() {
		slog.Info("hot-starting channel", "key", key)
		if err := ch.Start(ctx); err != nil {
			slog.Error("channel stopped with error", "key", key, "error", err)
		}
	}()
}

// Unregister removes a channel from the routing table. The channel's
// own Start goroutine doesn't get cancelled here — it'll exit when the
// root ctx ends. For now this just stops outbound routing; the bot
// adapter's polling loop is left alone (Telegram's GetUpdatesChan
// can't be cancelled mid-poll without tearing the whole manager down).
// Good enough for delete-from-UI: the next process restart starts
// clean and the binding is gone from DB so inbound messages no longer
// route to the agent.
func (m *Manager) Unregister(channelType, accountID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.channels, channelKey(channelType, accountID))
}

// Start launches all channels and the outbound message router.
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	m.rootCtx = ctx
	chans := make(map[string]Channel, len(m.channels))
	for k, v := range m.channels {
		chans[k] = v
	}
	m.mu.Unlock()

	var wg sync.WaitGroup

	// Start outbound router
	wg.Add(1)
	go func() {
		defer wg.Done()
		m.routeOutbound(ctx)
	}()

	// Start each channel
	for key, ch := range chans {
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
			m.mu.Lock()
			ch, ok := m.channels[key]
			m.mu.Unlock()
			if !ok {
				slog.Warn("unknown outbound channel", "key", key)
				continue
			}
			if err := ch.SendMessage(msg); err != nil {
				slog.Error("send message failed", "key", key, "error", err)
			}
		}
	}
}

// BotUsername returns the bot username for a given channel:accountID pair.
func (m *Manager) BotUsername(channel, accountID string) string {
	key := channelKey(channel, accountID)
	m.mu.Lock()
	defer m.mu.Unlock()
	ch, ok := m.channels[key]
	if !ok {
		return ""
	}
	return ch.BotUsername()
}

// SendTyping sends a typing indicator for the given channel and chat.
func (m *Manager) SendTyping(channel, accountID, chatID string) {
	key := channelKey(channel, accountID)
	m.mu.Lock()
	ch, ok := m.channels[key]
	m.mu.Unlock()
	if !ok {
		return
	}
	if err := ch.SendTyping(chatID); err != nil {
		slog.Debug("send typing failed", "key", key, "error", err)
	}
}

// Has returns true when a channel with the given key is registered.
// Used by handlers to short-circuit redundant hot-starts.
func (m *Manager) Has(channel, accountID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.channels[channelKey(channel, accountID)]
	return ok
}

// Get returns the registered adapter for (channel, accountID), or nil.
// Used by the Feishu webhook handler to find the adapter that should
// dispatch an incoming event — the HTTP route receives the raw POST
// and needs to call the right Feishu instance's HandleWebhook based on
// the {accountId} (Feishu App ID) in the URL path.
func (m *Manager) Get(channel, accountID string) Channel {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.channels[channelKey(channel, accountID)]
}

func channelKey(channel, accountID string) string {
	if accountID == "" {
		return channel + ":"
	}
	return channel + ":" + accountID
}
