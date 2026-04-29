// Package gateway is the runtime orchestrator. It opens the store, hosts
// per-user UserSpaces (lazy-loaded on first auth), and starts the channel
// manager / cron scheduler / webhook server / plugin manager.
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
	"github.com/fastclaw-ai/fastclaw/internal/scope"
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

var toolProviderRegistry = func() *toolproviders.Registry {
	r := toolproviders.NewRegistry()
	websearch.RegisterAll(r)
	imagegen.RegisterAll(r)
	tts.RegisterAll(r)
	return r
}()

// ToolProviderRegistry exposes the registry for callers that want to list
// available providers (admin API).
func ToolProviderRegistry() *toolproviders.Registry { return toolProviderRegistry }

// registerAgentToolChains wires every provider-backed tool category onto
// the given agents using their merged config view (system + user + agent
// scopes overlaid by the resolver).
func registerAgentToolChains(cfg *config.Config, agents []*agent.Agent) {
	for _, ag := range agents {
		resolved := cfg.MergedAgentConfig(config.AgentEntry{ID: ag.Name()})
		if chain := buildToolChainFromResolved(resolved, "web_search"); chain != nil {
			ag.RegisterWebSearchChain(chain)
		}
		if chain := buildToolChainFromResolved(resolved, "image_gen"); chain != nil {
			ag.RegisterImageGenChain(chain)
		}
		if chain := buildToolChainFromResolved(resolved, "tts"); chain != nil {
			ag.RegisterTTSChain(chain)
		}
	}
}

func buildToolChainFromResolved(resolved config.ResolvedAgent, category string) *toolproviders.Chain {
	cat, ok := resolved.Tools[category]
	if !ok {
		return nil
	}
	order := cat.Chain()
	if len(order) == 0 {
		return nil
	}
	providers := resolved.ToolProviders
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

// Gateway is the runtime orchestrator. It does not load any agents at
// boot; UserSpaces are constructed lazily when an authenticated request
// resolves to their owner.
type Gateway struct {
	bus        *bus.MessageBus
	users      *userSpaceRegistry
	chanMgr    *channels.Manager
	scheduler  *cron.Scheduler
	webhookSrv *webhook.Server
	pluginMgr  *plugin.Manager
	taskQueue  *taskqueue.Queue
	store      store.Store
	workspace  workspace.Store
	usage      usage.Meter
	envCfg     *config.EnvConfig
	mu         sync.RWMutex
	dedup      sync.Map
}

// Workspace returns the durable artifact store.
func (g *Gateway) Workspace() workspace.Store { return g.workspace }

// Usage returns the per-tenant resource meter.
func (g *Gateway) Usage() usage.Meter { return g.usage }

// Store returns the gateway's storage backend.
func (g *Gateway) Store() store.Store { return g.store }

// TaskQueue returns the gateway's task queue.
func (g *Gateway) TaskQueue() *taskqueue.Queue { return g.taskQueue }

// EnvConfig returns the bootstrap config (FASTCLAW_* env vars).
func (g *Gateway) EnvConfig() *config.EnvConfig { return g.envCfg }

// New creates a Gateway. Storage + workspace + plugin manager + channel
// manager + cron scheduler + webhook all initialize here, but no agents
// are loaded until an authenticated request hits a user.
func New(env *config.EnvConfig) (*Gateway, error) {
	if env == nil {
		env = &config.EnvConfig{}
	}
	mb := bus.New()

	homeDir, _ := config.HomeDir()
	st, err := store.New(&store.StorageConfig{
		Type:        store.StorageType(env.Storage.Type),
		DSN:         env.Storage.DSN,
		AutoMigrate: env.Storage.AutoMigrate || env.Storage.Type == "" || env.Storage.Type == "sqlite",
	}, homeDir)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	// Wire layer-3 agent config (per-agent overrides) to read from the DB.
	config.AgentFileConfigLoader = makeStoreFirstAgentFileLoader(st)

	// Object store for agent-produced artifacts. Object store config lives
	// in system_settings for runtime-edited fields and FASTCLAW_OBJECT_STORE_*
	// env vars for ops-managed overrides.
	osCfg := readObjectStoreCfg(st)
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

	meter := usage.NewMemMeter()
	ws := workspace.NewMetered(wsInner, func(ctx context.Context, agentID string, bytes int64) {
		meter.Add(ctx, "", agentID, usage.WorkspaceBytes, bytes)
	})

	chanMgr := channels.NewManager(mb)

	// Cron scheduler reads jobs directly from the DB on each tick — no
	// in-memory job list, no fastclaw.json copy. Each fired job carries
	// its OwnerUserID so processInbound can route into the right space.
	scheduler := cron.NewSchedulerFromStore(&cronStoreAdapter{st: st}, mb)

	systemHooks := readSystemHooks(st)
	var webhookSrv *webhook.Server
	if systemHooks.Enabled {
		webhookSrv = webhook.NewServer(systemHooks.Token, systemHooks.Path, nil, nil)
	}

	var pluginMgr *plugin.Manager
	systemPlugins := readSystemPlugins(st)
	if systemPlugins.Enabled {
		pluginMgr = plugin.NewManager(mb)
		pluginPaths := []string{filepath.Join(homeDir, "plugins")}
		pluginPaths = append(pluginPaths, systemPlugins.Paths...)
		if err := pluginMgr.Discover(pluginPaths); err != nil {
			slog.Warn("plugin discovery error", "error", err)
		}
		if len(systemPlugins.Entries) > 0 {
			entries := make(map[string]plugin.PluginEntryCfg, len(systemPlugins.Entries))
			for k, v := range systemPlugins.Entries {
				entries[k] = plugin.PluginEntryCfg{Enabled: v.Enabled, Config: v.Config}
			}
			pluginMgr.ApplyConfig(entries)
		}
	}

	taskCfg := readSystemTaskQueue(st)
	maxConcurrent := taskCfg.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 10
	}
	taskTimeoutSec := taskCfg.TaskTimeoutSec
	if taskTimeoutSec <= 0 {
		taskTimeoutSec = 300
	}
	taskTimeout := time.Duration(taskTimeoutSec) * time.Second

	g := &Gateway{
		bus:        mb,
		store:      st,
		workspace:  ws,
		usage:      meter,
		users:      newUserSpaceRegistry(mb, st, ws),
		chanMgr:    chanMgr,
		scheduler:  scheduler,
		webhookSrv: webhookSrv,
		pluginMgr:  pluginMgr,
		envCfg:     env,
	}

	if webhookSrv != nil {
		webhookSrv.SetHandler(&webhookAgentHandler{gateway: g})
	}

	tq := taskqueue.NewQueue(maxConcurrent, taskTimeout, func(ctx context.Context, task *taskqueue.Task) (string, error) {
		space, err := g.users.getOrLoad(ctx, task.OwnerUserID)
		if err != nil {
			return "", fmt.Errorf("load user space: %w", err)
		}
		ag := space.Agents.AgentByID(task.AgentID)
		if ag == nil {
			return "", fmt.Errorf("agent %q not found", task.AgentID)
		}
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

	// Register all enabled channel rows from the DB.
	if err := registerChannelsFromStore(st, mb, chanMgr); err != nil {
		slog.Warn("registerChannelsFromStore", "error", err)
	}

	return g, nil
}

// UserSpaceFor returns the resolved user's UserSpace, lazy-loading on
// first call. There is no implicit/local user — userID must be a real
// users.id.
func (g *Gateway) UserSpaceFor(userID string) (*UserSpace, error) {
	return g.UserSpaceForCtx(context.Background(), userID)
}

// UserSpaceForCtx is the ctx-aware variant; HTTP handlers should prefer
// it so the underlying DB queries inherit the request deadline.
func (g *Gateway) UserSpaceForCtx(ctx context.Context, userID string) (*UserSpace, error) {
	if userID == "" {
		return nil, fmt.Errorf("UserSpaceFor: userID required")
	}
	return g.users.getOrLoad(ctx, userID)
}

// LocalAgentManager satisfies the api.UserResolver interface — but there
// is no longer a "local" pinned manager. Callers that legitimately need
// any agent manager should resolve the request's own user_id and call
// UserSpaceFor.
func (g *Gateway) LocalAgentManager() *agent.Manager { return nil }

// IsCloudMode is retained for a few callers that still branch on it but
// always returns true now: multi-user is unconditional.
func (g *Gateway) IsCloudMode() bool { return true }

// Run starts the gateway and blocks until the process gets SIGINT/SIGTERM.
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
	wg.Add(1)
	go func() { defer wg.Done(); g.users.startEvictor(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); g.cleanupDedup(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); g.processInbound(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); g.chanMgr.Start(ctx) }()
	if g.scheduler != nil {
		wg.Add(1)
		go func() { defer wg.Done(); g.scheduler.Start(ctx) }()
	}
	if g.webhookSrv != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			port := readSystemHooks(g.store).Port
			if port == 0 {
				port = 18954
			}
			addr := fmt.Sprintf(":%d", port)
			if err := g.webhookSrv.ListenAndServe(ctx, addr); err != nil {
				slog.Error("webhook server error", "error", err)
			}
		}()
	}
	if g.pluginMgr != nil {
		if err := g.pluginMgr.StartAll(ctx); err != nil {
			slog.Error("plugin start error", "error", err)
		}
		for _, inst := range g.pluginMgr.ChannelPlugins() {
			adapter := plugin.NewChannelAdapter(g.pluginMgr, inst.Manifest.ID)
			g.chanMgr.Register(adapter)
		}
		plugin.RegisterPluginProviders(ctx, g.pluginMgr, toolProviderRegistry)
	}
	slog.Info("gateway started")
	wg.Wait()
	if g.taskQueue != nil {
		g.taskQueue.Stop()
	}
	if g.pluginMgr != nil {
		g.pluginMgr.StopAll()
	}
	for _, sp := range g.users.all() {
		if sp.SandboxPool != nil {
			sp.SandboxPool.CloseAll()
		}
	}
	slog.Info("gateway stopped")
	return nil
}

// makeStoreFirstAgentFileLoader returns a loader that reads per-agent
// config from the agents.config column.
func makeStoreFirstAgentFileLoader(st store.Store) func(string, string) (config.AgentFileConfig, bool) {
	return func(agentID, _ string) (config.AgentFileConfig, bool) {
		if st == nil || agentID == "" {
			return config.AgentFileConfig{}, false
		}
		// We need user_id for GetAgent now; iterate every user is
		// expensive. Instead use ListAllAgents and pick.
		all, err := st.ListAllAgents(context.Background())
		if err != nil {
			return config.AgentFileConfig{}, false
		}
		for _, ar := range all {
			if ar.ID != agentID {
				continue
			}
			if len(ar.Config) == 0 {
				return config.AgentFileConfig{}, false
			}
			blob, _ := json.Marshal(ar.Config)
			var cfg config.AgentFileConfig
			if err := json.Unmarshal(blob, &cfg); err == nil {
				return cfg, true
			}
		}
		return config.AgentFileConfig{}, false
	}
}

func defaultStr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// readObjectStoreCfg pulls the "objectstore" setting namespace, then
// layers FASTCLAW_OBJECT_STORE_* env vars on top.
func readObjectStoreCfg(st store.Store) config.ObjectStoreCfg {
	cfg := &config.Config{}
	if st != nil {
		_ = scope.SettingInto(context.Background(), st, NSObjectStore, "", "", "", &cfg.ObjectStore)
	}
	config.LoadEnv().ApplyToConfig(cfg)
	return cfg.ObjectStore
}

func readSystemHooks(st store.Store) config.HooksCfg {
	var out config.HooksCfg
	if st != nil {
		_ = scope.SettingInto(context.Background(), st, NSHooks, "", "", "", &out)
	}
	return out
}

func readSystemPlugins(st store.Store) config.PluginsCfg {
	var out config.PluginsCfg
	if st != nil {
		_ = scope.SettingInto(context.Background(), st, NSPlugins, "", "", "", &out)
	}
	return out
}

func readSystemTaskQueue(st store.Store) config.TaskQueueCfg {
	var out config.TaskQueueCfg
	if st != nil {
		_ = scope.SettingInto(context.Background(), st, NSTaskQueue, "", "", "", &out)
	}
	return out
}

// Setting namespace constants. Each maps to one row in configs
// with kind="setting". Adding a new namespace is a one-line append; the
// scope.Setting / SettingInto helpers handle merging across scopes.
const (
	NSAgentDefaults = "agents.defaults"
	NSSandbox       = "sandbox"
	NSObjectStore   = "objectstore"
	NSHooks         = "hooks"
	NSPlugins       = "plugins"
	NSTaskQueue     = "taskqueue"
	NSToolProviders = "tools.providers"
	NSToolCategories = "tools.categories"
	NSSkillsInstall = "skills.install"
	NSSkillsEntries = "skills.entries"
	NSMemory        = "memory"
	NSPrivacy       = "privacy"
	NSSkillsLearner = "skillsLearner"
	NSHeartbeat     = "heartbeat"
	NSTeams         = "teams"
	NSBindings      = "bindings"
)

// registerChannelsFromStore loads every enabled kind="channel" row from
// configs and starts a channel adapter for each, regardless of
// scope. The owner is captured per-row and resolved at message receipt
// time via LookupChannelByCredential.
func registerChannelsFromStore(st store.Store, mb *bus.MessageBus, chanMgr *channels.Manager) error {
	if st == nil {
		return nil
	}
	rows, err := allChannelRows(st)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if !r.Enabled {
			continue
		}
		if err := registerChannelInstance(r, mb, chanMgr); err != nil {
			slog.Warn("register channel failed",
				"type", r.Name, "scope", r.Scope, "scope_id", r.ScopeID, "error", err)
		}
	}
	return nil
}

func allChannelRows(st store.Store) ([]store.ConfigRecord, error) {
	var out []store.ConfigRecord
	for _, sc := range []string{store.ScopeSystem, store.ScopeUser, store.ScopeAgent} {
		rows, err := st.ListConfigs(context.Background(), store.KindChannel, sc, "")
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}
