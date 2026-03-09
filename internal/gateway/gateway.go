package gateway

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/channels"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
)

// Gateway is the main orchestrator that starts all services.
type Gateway struct {
	config *config.Config
	bus    *bus.MessageBus
	agent  *agent.Agent
	chanMgr *channels.Manager
}

// New creates a new Gateway.
func New(cfg *config.Config) (*Gateway, error) {
	mb := bus.New()

	// Create LLM provider
	providerCfg := cfg.Providers["openai"]
	llm := provider.NewOpenAI(providerCfg.APIKey, providerCfg.APIBase)

	// Create agent
	ag := agent.NewAgent(llm, cfg.Agents.Defaults, mb)

	// Create channel manager
	chanMgr := channels.NewManager(mb)

	// Register Telegram channel if enabled
	if cfg.Channels.Telegram.Enabled {
		tg, err := channels.NewTelegram(cfg.Channels.Telegram.BotToken, mb)
		if err != nil {
			return nil, err
		}
		chanMgr.Register(tg)
	}

	return &Gateway{
		config:  cfg,
		bus:     mb,
		agent:   ag,
		chanMgr: chanMgr,
	}, nil
}

// Run starts the gateway and blocks until shutdown signal.
func (g *Gateway) Run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		slog.Info("received signal, shutting down", "signal", sig)
		cancel()
	}()

	var wg sync.WaitGroup

	// Start inbound message processor
	wg.Add(1)
	go func() {
		defer wg.Done()
		g.processInbound(ctx)
	}()

	// Start channel manager (blocks until ctx cancelled)
	wg.Add(1)
	go func() {
		defer wg.Done()
		g.chanMgr.Start(ctx)
	}()

	slog.Info("gateway started")

	wg.Wait()
	slog.Info("gateway stopped")
	return nil
}

func (g *Gateway) processInbound(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-g.bus.Inbound:
			slog.Info("processing message", "channel", msg.Channel, "chat_id", msg.ChatID)

			// Process in a goroutine to handle concurrent messages
			go func(m bus.InboundMessage) {
				reply := g.agent.HandleMessage(ctx, m)
				g.bus.Outbound <- bus.OutboundMessage{
					Channel: m.Channel,
					ChatID:  m.ChatID,
					Text:    reply,
				}
			}(msg)
		}
	}
}
