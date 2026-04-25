package gateway

import (
	"context"
	"encoding/json"
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
	"github.com/fastclaw-ai/fastclaw/internal/session"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/taskqueue"
	"github.com/fastclaw-ai/fastclaw/internal/toolproviders"
	"github.com/fastclaw-ai/fastclaw/internal/toolproviders/imagegen"
	"github.com/fastclaw-ai/fastclaw/internal/toolproviders/tts"
	"github.com/fastclaw-ai/fastclaw/internal/toolproviders/websearch"
	"github.com/fastclaw-ai/fastclaw/internal/usage"
	"github.com/fastclaw-ai/fastclaw/internal/webhook"
	"github.com/fastclaw-ai/fastclaw/internal/workspace"
)

// toolProviderRegistry is the process-global registry of built-in tool
// providers. Populated once at startup; reads from it are concurrency-safe.
// Keeping it as a package var lets both the Gateway and per-user spaces share
// it without threading the reference through half a dozen constructors.
var toolProviderRegistry = func() *toolproviders.Registry {
	r := toolproviders.NewRegistry()
	websearch.RegisterAll(r)
	imagegen.RegisterAll(r)
	tts.RegisterAll(r)
	return r
}()

// ToolProviderRegistry exposes the registry for callers (e.g. admin API
// handlers) that want to list available providers.
func ToolProviderRegistry() *toolproviders.Registry { return toolProviderRegistry }

// defaultStr returns v when non-empty, otherwise fallback. Single helper
// so slog labels stay tidy.
func defaultStr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// mergeCloudConfig copies product-level fields from the DB-persisted config
// into the live env-bootstrapped config. Infra fields (Gateway, Storage,
// ObjectStore, Sandbox) are left alone — they must come from env / Secret
// so ops keeps control of the deployment surface even after a UI save.
func mergeCloudConfig(dst, src *config.Config) {
	if src == nil {
		return
	}
	if len(src.Providers) > 0 {
		dst.Providers = src.Providers
	}
	if len(src.Channels) > 0 {
		dst.Channels = src.Channels
	}
	if len(src.Bindings) > 0 {
		dst.Bindings = src.Bindings
	}
	if len(src.Teams) > 0 {
		dst.Teams = src.Teams
	}
	if len(src.CronJobs) > 0 {
		dst.CronJobs = src.CronJobs
	}
	if src.Agents.Defaults.Model != "" {
		dst.Agents.Defaults = src.Agents.Defaults
	}
	if len(src.ToolProviders) > 0 {
		dst.ToolProviders = src.ToolProviders
	}
	if len(src.Tools) > 0 {
		dst.Tools = src.Tools
	}
	if src.Heartbeat.IntervalMinutes > 0 {
		dst.Heartbeat = src.Heartbeat
	}
}

// registerAgentToolChains wires every provider-backed tool category onto the
// given agents. Each agent gets its own Chain that honors its per-agent
// toolProviders/tools overrides from agent.json — one agent can point
// web_search at searxng while another uses exa.
func registerAgentToolChains(cfg *config.Config, agents []*agent.Agent) {
	for _, ag := range agents {
		resolved := cfg.MergedAgentConfigForUser(config.AgentEntry{ID: ag.Name()}, config.DefaultUserID)
		if chain := buildToolChainFromResolved(resolved, "web_search"); chain != nil {
			ag.RegisterWebSearchChain(chain)
			slog.Info("web_search chain registered", "agent", ag.Name(), "providers", chain.Order)
		}
		if chain := buildToolChainFromResolved(resolved, "image_gen"); chain != nil {
			ag.RegisterImageGenChain(chain)
			slog.Info("image_gen chain registered", "agent", ag.Name(), "providers", chain.Order)
		}
		if chain := buildToolChainFromResolved(resolved, "tts"); chain != nil {
			ag.RegisterTTSChain(chain)
			slog.Info("tts chain registered", "agent", ag.Name(), "providers", chain.Order)
		}
	}
}

// buildToolChainFromResolved builds a Chain using the agent's merged view
// (global defaults + agent.json overrides). Returns nil when the category
// isn't configured or has no registered providers, which hides the tool.
func buildToolChainFromResolved(resolved config.ResolvedAgent, category string) *toolproviders.Chain {
	cat, ok := resolved.Tools[category]
	if !ok {
		return nil
	}
	order := cat.Chain()
	if len(order) == 0 {
		return nil
	}
	providers := resolved.ToolProviders // captured by closure below
	chain := &toolproviders.Chain{
		Category:     category,
		Order:        order,
		AutoFallback: cat.FallbackEnabled(),
		Registry:     toolProviderRegistry,
		GetConfig: func(name string) toolproviders.ProviderConfig {
			pc := providers[name]
			return toolproviders.ProviderConfig{
				APIKey:   pc.APIKey,
				Endpoint: pc.Endpoint,
				Options:  pc.Options,
			}
		},
	}
	if !chain.Available() {
		return nil
	}
	return chain
}

// Gateway is the main orchestrator that starts all services.
//
// It owns a registry of per-user spaces. The "local" user is loaded at
// startup and drives channels, cron, webhooks and plugins (these remain
// host-global features). In cloud mode, additional users are loaded lazily
// on first HTTP request and get isolated agents + sessions + memory.
type Gateway struct {
	config       *config.Config // local user's config
	bus          *bus.MessageBus
	users        *userSpaceRegistry
	localSpace   *UserSpace // shortcut to users.get(config.DefaultUserID)
	agents       *agent.Manager // alias for localSpace.Agents (channels/cron/plugins)
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
	store        store.Store
	workspace    workspace.Store
	usage        usage.Meter
}

// Workspace returns the durable artifact store (local FS or S3). Handlers
// and tools use this to read/write agent-generated files.
func (g *Gateway) Workspace() workspace.Store { return g.workspace }

// Usage returns the per-tenant resource meter. Admin endpoints use this to
// answer "how much did X consume", which is the input side of billing /
// quota. Always non-nil — defaults to an in-memory meter when nothing
// durable is configured.
func (g *Gateway) Usage() usage.Meter { return g.usage }

// New creates a new Gateway with multi-agent support.
func New(cfg *config.Config) (*Gateway, error) {
	mb := bus.New()

	// Initialize storage backend. Local user's config on the filesystem is
	// always the bootstrap; after reading it, we open the configured store
	// (FileStore or DBStore) for all subsequent per-user data.
	homeDir, _ := config.HomeDir()
	st, err := store.New(&store.StorageConfig{
		Type:        store.StorageType(cfg.Storage.Type),
		DSN:         cfg.Storage.DSN,
		AutoMigrate: cfg.Storage.AutoMigrate,
	}, homeDir)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	// Cloud pods boot with an empty fastclaw.json (env bootstrap). Hydrate
	// product fields (providers, agents.defaults, channels, bindings, …)
	// from the shared dataStore so any replica serves the same config that
	// the setup wizard persisted. Infra fields stay on the pod-local/env
	// side — we only pull product knobs out of the DB.
	if cfg.Gateway.Mode == "cloud" && st != nil {
		if gc, err := st.GetConfig(context.Background()); err == nil && gc != nil && len(gc.Data) > 0 {
			if blob, err := json.Marshal(gc.Data); err == nil {
				var stored config.Config
				if err := json.Unmarshal(blob, &stored); err == nil {
					mergeCloudConfig(cfg, &stored)
				} else {
					slog.Warn("decode stored config", "error", err)
				}
			}
		} else if err != nil {
			slog.Warn("store GetConfig failed", "error", err)
		}
	}

	// Workspace blob store (local FS or S3). Distinct from the Store above:
	// that one holds small structured state (sessions, identity md files);
	// this one holds arbitrary artifacts (generated PDFs/images/audio).
	osCfg := cfg.ObjectStore
	wsInner, err := workspace.Factory{
		Type:         osCfg.Type,
		LocalDir:     osCfg.Local.Root,
		AccountID:    osCfg.AccountID,
		AliyunIntern: osCfg.AliyunIntern,
		S3: workspace.S3Config{
			Endpoint:  osCfg.S3.Endpoint,
			Region:    osCfg.S3.Region,
			Bucket:    osCfg.S3.Bucket,
			Prefix:    osCfg.S3.Prefix,
			AccessKey: osCfg.S3.AccessKey,
			SecretKey: osCfg.S3.SecretKey,
			UseSSL:    osCfg.S3.UseSSL,
		},
	}.New(filepath.Join(homeDir, "workspaces"))
	if err != nil {
		return nil, fmt.Errorf("open object store: %w", err)
	}
	slog.Info("object store", "type", defaultStr(osCfg.Type, "local"))

	// Usage meter sits on top of the workspace store so every agent Put is
	// automatically counted as workspace_bytes. In-memory for now;
	// swapping in a DB-backed implementation is a one-line change.
	meter := usage.NewMemMeter()
	ws := workspace.NewMetered(wsInner, func(ctx context.Context, agentID string, bytes int64) {
		// agentID is the metering "agent" dimension. The API-key dimension
		// comes from the request context; workspace writes from inside
		// the agent loop don't carry it, so we record it as "" (admin).
		// For tenant attribution we'd need to thread the caller through
		// the tool stack — left as a follow-up.
		meter.Add(ctx, "", agentID, usage.WorkspaceBytes, bytes)
	})

	// Resolve provider + agents for the local user. This mirrors what
	// loadUserSpace does, but we keep the inline version here because the
	// caller already handed us a *config.Config (avoiding a redundant disk
	// read) and we want to surface any provider log lines.
	llm := newProviderFromConfig(cfg)
	slog.Info("local provider resolved", "defaultModel", cfg.Agents.Defaults.Model)

	// In Postgres-backed deployments a freshly-scheduled pod has an empty
	// filesystem — DiscoverAgents returns nothing even though agents exist
	// in the DB. Supplement filesystem discovery with the Store's agent
	// list so any pod can serve any agent without relying on pod-local FS.
	var storeAgentIDs []string
	if st != nil {
		if records, err := st.ListAgents(context.Background()); err == nil {
			for _, ar := range records {
				storeAgentIDs = append(storeAgentIDs, ar.ID)
			}
		} else {
			slog.Warn("store ListAgents failed", "error", err)
		}
	}
	resolved := config.ResolveAgentsWithExtra(cfg, "", storeAgentIDs)
	managerOpts := []agent.ManagerOption{agent.WithUserID(config.DefaultUserID)}
	if st != nil {
		managerOpts = append(managerOpts,
			agent.WithSessionStore(session.NewStoreAdapter(st)),
			agent.WithMemoryStore(agent.NewMemoryStoreAdapter(st)),
		)
	}
	if ws != nil {
		managerOpts = append(managerOpts, agent.WithWorkspaceStore(ws))
	}
	agentMgr, err := agent.NewManager(resolved, llm, mb, managerOpts...)
	if err != nil {
		return nil, err
	}

	// Attach sandbox (E2B / Docker) to every local-user agent so exec +
	// file tool calls run inside an isolated env, not on the pod's host
	// shell. loadUserSpace does the same for cloud users; this inline
	// bootstrap used to skip it entirely — which meant admin chats
	// ignored the sandbox config no matter what env we set.
	sandboxPool := attachSandboxToAgents(config.DefaultUserID, resolved, agentMgr, ws)

	localSpace := &UserSpace{
		UserID:      config.DefaultUserID,
		Config:      cfg,
		Provider:    llm,
		Agents:      agentMgr,
		SandboxPool: sandboxPool,
	}
	userReg := newUserSpaceRegistry(mb, st)
	userReg.workspace = ws
	userReg.put(localSpace)

	slog.Info("agents loaded", "user", config.DefaultUserID, "count", len(resolved), "names", agentMgr.Names())

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

	// Register provider-backed tools (web_search, image_gen, tts, …) on every
	// agent. Categories with no configured providers are simply hidden from
	// the LLM's tool list.
	registerAgentToolChains(cfg, agentMgr.All())

	// Register sub-agent spawner for all agents
	spawner := &gatewaySubAgentSpawner{agents: agentMgr}
	for _, ag := range agentMgr.All() {
		ag.SetSubAgentSpawner(spawner)
	}

	// Set up webhook server if enabled
	var webhookSrv *webhook.Server
	if cfg.Hooks.Enabled {
		webhookSrv = webhook.NewServer(cfg.Hooks.Token, cfg.Hooks.Path, &webhookAgentHandler{agents: agentMgr}, nil)
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
		store:        st,
		workspace:    ws,
		usage:        meter,
		users:        userReg,
		localSpace:   localSpace,
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

		// Send typing indicator and keep sending every 5s until done
		chanMgr.SendTyping(task.Message.Channel, task.AccountID, task.Message.ChatID)
		typingDone := make(chan struct{})
		go func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-typingDone:
					return
				case <-ctx.Done():
					return
				case <-ticker.C:
					chanMgr.SendTyping(task.Message.Channel, task.AccountID, task.Message.ChatID)
				}
			}
		}()

		reply := ag.HandleMessage(ctx, task.Message)
		close(typingDone)

		mb.Outbound <- bus.OutboundMessage{
			Channel:      task.Message.Channel,
			AccountID:    task.AccountID,
			ChatID:       task.Message.ChatID,
			Text:         reply,
			ReplyToMsgID: task.Message.MessageID,
		}
		return reply, nil
	})
	g.taskQueue = tq

	return g, nil
}

// AgentManager returns the local user's agent manager.
// For per-user routing in cloud mode, use UserSpaceFor.
func (g *Gateway) AgentManager() *agent.Manager {
	return g.agents
}

// LocalUserSpace returns the preloaded local ("host admin") user space that
// owns channels, cron jobs, plugins, and the webhook ingress.
func (g *Gateway) LocalUserSpace() *UserSpace {
	return g.localSpace
}

// UserSpaceFor returns the user space for the given user ID, loading it
// lazily from disk if needed. In local mode callers normally pass
// config.DefaultUserID and get the preloaded space; in cloud mode the HTTP
// auth middleware resolves the real user ID and this method fetches the
// matching space.
func (g *Gateway) UserSpaceFor(userID string) (*UserSpace, error) {
	if userID == "" {
		userID = config.DefaultUserID
	}
	return g.users.getOrLoad(userID)
}

// LocalAgentManager satisfies the api.UserResolver interface by exposing
// the local user's agent manager.
func (g *Gateway) LocalAgentManager() *agent.Manager {
	return g.agents
}

// IsCloudMode reports whether the gateway is configured to accept multiple
// users via HTTP auth. Channels, cron and plugins remain bound to the
// local user regardless.
func (g *Gateway) IsCloudMode() bool {
	return g.config != nil && g.config.Gateway.Mode == "cloud"
}

// Store returns the gateway's storage backend.
func (g *Gateway) Store() store.Store {
	return g.store
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

	// Start idle user space evictor (cloud mode: free memory for inactive users)
	wg.Add(1)
	go func() {
		defer wg.Done()
		g.users.startEvictor(ctx)
	}()

	// Start config file watcher for hot-reload. Cloud pods have an
	// ephemeral /data/.fastclaw and each request that touches the API
	// may write a bootstrap stub — letting the watcher act on that would
	// overwrite the DB-hydrated in-memory config (providers wiped, fake
	// "default" agent added). Config truth in cloud lives in the DB.
	if g.config.Gateway.Mode != "cloud" {
		wg.Add(1)
		go g.startConfigWatcher(ctx, &wg)
	} else {
		slog.Info("config watcher disabled in cloud mode")
	}

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

		// Register plugin-provided tool providers (e.g. a custom web_search
		// backend) into the same provider registry the built-ins use, so
		// fallback chains treat them uniformly.
		plugin.RegisterPluginProviders(ctx, g.pluginMgr, toolProviderRegistry)

		// Register hook plugins with all agents
		for _, inst := range g.pluginMgr.HookPlugins() {
			for _, ag := range g.agents.All() {
				if err := plugin.RegisterPluginHooks(ctx, g.pluginMgr, inst.Manifest.ID, ag.HookRegistry(), ag.Name()); err != nil {
					slog.Error("register plugin hooks failed", "plugin", inst.Manifest.ID, "agent", ag.Name(), "error", err)
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

	// Tear down every live sandbox across every loaded user space. On
	// rolling restart this means E2B instances stop billing immediately
	// instead of waiting for their server-side max-TTL.
	for _, sp := range g.users.all() {
		if sp.SandboxPool != nil {
			sp.SandboxPool.CloseAll()
		}
	}

	slog.Info("gateway stopped")
	return nil
}
