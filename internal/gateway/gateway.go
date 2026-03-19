package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/channels"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/cron"
	"github.com/fastclaw-ai/fastclaw/internal/plugin"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
	"github.com/fastclaw-ai/fastclaw/internal/taskqueue"
	"github.com/fastclaw-ai/fastclaw/internal/webhook"
)

// Gateway is the main orchestrator that starts all services.
type Gateway struct {
	config       *config.Config
	bus          *bus.MessageBus
	agents       *agent.Manager
	chanMgr      *channels.Manager
	bindings     []config.Binding
	botUsernames map[string]string          // agentID -> bot username
	teams        map[string]config.TeamEntry // team name -> team config
	mu           sync.RWMutex
	dedup        sync.Map                    // dedup key -> dedupEntry
	heartbeats   []*agent.Heartbeat
	scheduler    *cron.Scheduler
	webhookSrv   *webhook.Server
	pluginMgr    *plugin.Manager
	taskQueue    *taskqueue.Queue
}

// New creates a new Gateway with multi-agent support.
func New(cfg *config.Config) (*Gateway, error) {
	mb := bus.New()

	// Create LLM provider — try known keys in order, fall back to first available
	var providerCfg config.ProviderConfig
	for _, key := range []string{"default", "openai", "openrouter"} {
		if p, ok := cfg.Providers[key]; ok {
			providerCfg = p
			break
		}
	}
	if providerCfg.APIKey == "" {
		// Use the first provider defined
		for _, p := range cfg.Providers {
			providerCfg = p
			break
		}
	}
	llm := provider.NewOpenAI(providerCfg.APIKey, providerCfg.APIBase)

	// Resolve agent configs
	resolved := config.ResolveAgents(cfg)

	// Create agent manager
	agentMgr, err := agent.NewManager(resolved, llm, mb)
	if err != nil {
		return nil, err
	}

	slog.Info("agents loaded", "count", len(resolved), "names", agentMgr.Names())

	// Create channel manager and register channel instances
	chanMgr := channels.NewManager(mb)

	if err := registerChannels(cfg, mb, chanMgr); err != nil {
		return nil, err
	}

	// Build agentID -> botUsername mapping from bindings + channel manager
	botUsernames := buildBotUsernames(cfg.Bindings, chanMgr)
	if len(botUsernames) > 0 {
		slog.Info("bot username mappings", "map", botUsernames)
	}

	teams := cfg.Teams
	if teams == nil {
		teams = make(map[string]config.TeamEntry)
	}

	// Set up group context for agents in teams
	for _, team := range teams {
		for _, agentID := range team.Agents {
			ag := agentMgr.AgentByID(agentID)
			if ag == nil {
				continue
			}
			var teammates []string
			for _, otherID := range team.Agents {
				if otherID != agentID {
					if uname, ok := botUsernames[otherID]; ok {
						teammates = append(teammates, "@"+uname)
					} else {
						teammates = append(teammates, otherID)
					}
				}
			}
			if botUname, ok := botUsernames[agentID]; ok {
				ag.SetGroupContext(&agent.GroupContext{
					BotUsername: botUname,
					Teammates:  teammates,
				})
			}
		}
	}

	// Set up heartbeats for each agent
	heartbeatInterval := time.Duration(cfg.Heartbeat.IntervalMinutes) * time.Minute
	if heartbeatInterval <= 0 {
		heartbeatInterval = agent.DefaultHeartbeatInterval
	}
	var heartbeats []*agent.Heartbeat
	for _, ag := range agentMgr.All() {
		hb := agent.NewHeartbeat(ag, mb, heartbeatInterval)
		heartbeats = append(heartbeats, hb)
	}

	// Set up cron scheduler
	var cronJobs []cron.Job
	for _, cj := range cfg.CronJobs {
		cronJobs = append(cronJobs, cron.Job{
			Name:     cj.Name,
			Type:     cron.JobType(cj.Type),
			Schedule: cj.Schedule,
			AgentID:  cj.AgentID,
			Channel:  cj.Channel,
			ChatID:   cj.ChatID,
			Message:  cj.Message,
		})
	}
	scheduler := cron.NewScheduler(cronJobs, mb)

	// Register web search tool for all agents if configured
	if cfg.WebSearch.APIKey != "" {
		for _, ag := range agentMgr.All() {
			ag.RegisterWebSearchTool(cfg.WebSearch.APIKey)
		}
		slog.Info("web search registered", "provider", cfg.WebSearch.Provider)
	}

	// Register sub-agent spawner for all agents
	spawner := &gatewaySubAgentSpawner{agents: agentMgr}
	for _, ag := range agentMgr.All() {
		ag.SetSubAgentSpawner(spawner)
	}

	// Set up webhook server if enabled
	var webhookSrv *webhook.Server
	if cfg.Hooks.Enabled {
		webhookSrv = webhook.NewServer(cfg.Hooks.Token, cfg.Hooks.Path, &webhookAgentHandler{agents: agentMgr})
	}

	// Set up plugin manager
	var pluginMgr *plugin.Manager
	if cfg.Plugins.Enabled {
		pluginMgr = plugin.NewManager(mb)

		homeDir, _ := config.HomeDir()
		pluginPaths := []string{filepath.Join(homeDir, "plugins")}
		for _, p := range cfg.Plugins.Paths {
			pluginPaths = append(pluginPaths, p)
		}

		if err := pluginMgr.Discover(pluginPaths); err != nil {
			slog.Warn("plugin discovery error", "error", err)
		}

		// Apply per-plugin config
		if len(cfg.Plugins.Entries) > 0 {
			entries := make(map[string]plugin.PluginEntryCfg, len(cfg.Plugins.Entries))
			for k, v := range cfg.Plugins.Entries {
				entries[k] = plugin.PluginEntryCfg{
					Enabled: v.Enabled,
					Config:  v.Config,
				}
			}
			pluginMgr.ApplyConfig(entries)
		}
	}

	// Create task queue with config values
	maxConcurrent := cfg.TaskQueue.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 10
	}
	taskTimeoutSec := cfg.TaskQueue.TaskTimeoutSec
	if taskTimeoutSec <= 0 {
		taskTimeoutSec = 300
	}
	taskTimeout := time.Duration(taskTimeoutSec) * time.Second

	g := &Gateway{
		config:       cfg,
		bus:          mb,
		agents:       agentMgr,
		chanMgr:      chanMgr,
		bindings:     cfg.Bindings,
		botUsernames: botUsernames,
		teams:        teams,
		heartbeats:   heartbeats,
		scheduler:    scheduler,
		webhookSrv:   webhookSrv,
		pluginMgr:    pluginMgr,
	}

	tq := taskqueue.NewQueue(maxConcurrent, taskTimeout, func(ctx context.Context, task *taskqueue.Task) (string, error) {
		ag := agentMgr.AgentByID(task.AgentID)
		if ag == nil {
			return "", fmt.Errorf("agent %q not found", task.AgentID)
		}
		reply := ag.HandleMessage(ctx, task.Message)
		mb.Outbound <- bus.OutboundMessage{
			Channel:   task.Message.Channel,
			AccountID: task.AccountID,
			ChatID:    task.Message.ChatID,
			Text:      reply,
		}
		return reply, nil
	})
	g.taskQueue = tq

	return g, nil
}

// AgentManager returns the gateway's agent manager.
func (g *Gateway) AgentManager() *agent.Manager {
	return g.agents
}

// TaskQueue returns the gateway's task queue.
func (g *Gateway) TaskQueue() *taskqueue.Queue {
	return g.taskQueue
}

// Run starts the gateway and blocks until shutdown signal.
func (g *Gateway) Run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		slog.Info("received signal, shutting down", "signal", sig)
		cancel()
	}()

	var wg sync.WaitGroup

	// Start config file watcher for hot-reload
	wg.Add(1)
	go g.startConfigWatcher(ctx, &wg)

	// Start dedup cleanup goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		g.cleanupDedup(ctx)
	}()

	// Start inbound message processor
	wg.Add(1)
	go func() {
		defer wg.Done()
		g.processInbound(ctx)
	}()

	// Start channel manager
	wg.Add(1)
	go func() {
		defer wg.Done()
		g.chanMgr.Start(ctx)
	}()

	// Start heartbeats for each agent
	for _, hb := range g.heartbeats {
		wg.Add(1)
		go func(h *agent.Heartbeat) {
			defer wg.Done()
			h.Start(ctx)
		}(hb)
	}

	// Start cron scheduler
	wg.Add(1)
	go func() {
		defer wg.Done()
		g.scheduler.Start(ctx)
	}()

	// Start webhook server if configured
	if g.webhookSrv != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			port := g.config.Hooks.Port
			if port == 0 {
				port = 18954
			}
			addr := fmt.Sprintf(":%d", port)
			if err := g.webhookSrv.ListenAndServe(ctx, addr); err != nil {
				slog.Error("webhook server error", "error", err)
			}
		}()
	}

	// Start plugins if enabled
	if g.pluginMgr != nil {
		if err := g.pluginMgr.StartAll(ctx); err != nil {
			slog.Error("plugin start error", "error", err)
		}

		// Register channel adapters for channel plugins
		for _, inst := range g.pluginMgr.ChannelPlugins() {
			adapter := plugin.NewChannelAdapter(g.pluginMgr, inst.Manifest.ID)
			g.chanMgr.Register(adapter)
			slog.Info("registered plugin channel", "plugin", inst.Manifest.ID)
		}

		// Register tool plugins with all agents
		for _, inst := range g.pluginMgr.ToolPlugins() {
			for _, ag := range g.agents.All() {
				if err := plugin.RegisterPluginTools(ctx, g.pluginMgr, inst.Manifest.ID, ag.ToolRegistry()); err != nil {
					slog.Error("register plugin tools failed", "plugin", inst.Manifest.ID, "agent", ag.Name(), "error", err)
				}
			}
		}
	}

	slog.Info("gateway started")

	wg.Wait()

	// Stop task queue
	if g.taskQueue != nil {
		g.taskQueue.Stop()
	}

	// Stop plugins on shutdown
	if g.pluginMgr != nil {
		g.pluginMgr.StopAll()
	}

	slog.Info("gateway stopped")
	return nil
}
