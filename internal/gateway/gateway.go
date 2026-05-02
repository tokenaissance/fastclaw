// Package gateway is the runtime orchestrator. It opens the store, hosts
// per-user UserSpaces (lazy-loaded on first auth), and starts the channel
// manager / cron scheduler / webhook server / plugin manager.
package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
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
	webChan    *channels.WebChannel
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

// WebChannel returns the in-process fan-out for web SSE subscribers.
// Used by the setup server to register chat-stream subscribers so cron-
// fired (and other async) outbound messages reach live dashboard tabs.
func (g *Gateway) WebChannel() *channels.WebChannel { return g.webChan }

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
	// Always-on web channel: routes cron-fired (and any other
	// async-emitted) outbound messages to the dashboard's SSE
	// subscribers so the user sees the agent's reply live instead of
	// only on the next page reload.
	webChan := channels.NewWebChannel()
	chanMgr.Register(webChan)

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
		webChan:    webChan,
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

		// IM progress relay: forward the agent loop's tool_call events
		// to the channel as short status messages so users on long
		// tool runs (image-tool taking 30s+) see something happening
		// instead of staring at a typing dot. We only react to
		// tool_call (the start signal) and skip tool_result / content
		// — final reply already comes through the Outbound below.
		// Content/text events would otherwise duplicate the final
		// answer.
		eventsCh := make(chan agent.ChatEvent, 64)
		eventsDone := make(chan struct{})
		go func() {
			defer close(eventsDone)
			seen := make(map[string]bool) // dedupe per-turn
			for evt := range eventsCh {
				if evt.Type != "tool_call" {
					continue
				}
				name, _ := evt.Data["name"].(string)
				if name == "" || seen[name] {
					continue
				}
				seen[name] = true
				progress := bus.OutboundMessage{
					Channel:   task.Message.Channel,
					AccountID: task.AccountID,
					ChatID:    task.Message.ChatID,
					Text:      "🛠 calling `" + name + "`…",
					ParseMode: "Markdown",
				}
				// Non-blocking enqueue: progress messages are
				// best-effort, dropping one is fine. A blocking
				// `mb.Outbound <- progress` deadlocked the task
				// when routeOutbound stalled on a slow Telegram
				// API call — the goroutine couldn't exit because
				// it never saw close(eventsCh), the main flow
				// waited on <-eventsDone forever, the taskQueue
				// slot stayed held, and every subsequent inbound
				// from this user piled up unanswered.
				select {
				case mb.Outbound <- progress:
				case <-ctx.Done():
					return
				default:
					slog.Debug("outbound full, dropping progress event", "tool", name)
				}
			}
		}()
		// Record turn start. After ag.HandleMessage returns we list the
		// workspace and attach every image whose ModTime >= turnStart —
		// works regardless of whether the LLM's reply contains a usable
		// markdown ref. Time-based is more robust than path-diff
		// (pre-turn snapshot timing, store backends that don't preserve
		// path stability, files overwritten in place, etc.).
		turnStart := time.Now()

		ctxWithEvents := agent.ContextWithChatEvents(ctx, eventsCh)
		reply := ag.HandleMessage(ctxWithEvents, task.Message)
		close(typingDone)
		close(eventsCh)
		<-eventsDone
		// Extract `![alt](workspace/relative/path)` markdown image refs
		// from the agent's reply, resolve their bytes via the
		// workspace.Store, and ship them as MediaItems so IM channels
		// can upload as photos. The textual placeholders are stripped
		// from the body so users don't see the raw `![](...)` syntax.
		text, items := splitMediaFromReply(ctx, g.workspace, task.AgentID, task.Message.ChatID, reply)
		// Workspace fallback: list the session's files and attach
		// every image whose mtime falls in this turn's window. Catches
		// the case where the LLM emits a broken data URL (with
		// truncated base64 / literal "..." placeholders) but
		// image-tool already saved the real file to /workspace. Dedupe
		// by filename so we don't double-send anything
		// splitMediaFromReply already resolved.
		items = appendRecentWorkspaceImages(ctx, g.workspace, task.AgentID, task.Message.ChatID, turnStart, items)
		out := bus.OutboundMessage{
			Channel:      task.Message.Channel,
			AccountID:    task.AccountID,
			AgentID:      task.AgentID,
			ChatID:       task.Message.ChatID,
			Text:         text,
			ReplyToMsgID: task.Message.MessageID,
			ParseMode:    "Markdown",
			MediaItems:   items,
		}
		// Bounded enqueue. If routeOutbound is wedged the task
		// shouldn't sit on its taskQueue slot forever — let ctx's
		// task-timeout serve as the upper bound and drop the reply
		// rather than blocking the next inbound from this user.
		select {
		case mb.Outbound <- out:
		case <-ctx.Done():
			slog.Warn("outbound enqueue cancelled", "agent", task.AgentID, "chat", task.Message.ChatID)
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

// EnsureAgent loads an agent that does not belong to userID into that
// user's UserSpace. Used by super_admin chat handlers — see
// UserSpace.EnsureAgent.
func (g *Gateway) EnsureAgent(ctx context.Context, userID, agentID string) error {
	sp, err := g.UserSpaceForCtx(ctx, userID)
	if err != nil {
		return err
	}
	return sp.EnsureAgent(ctx, g.store, g.bus, g.workspace, agentID)
}

// IsCloudMode is retained for a few callers that still branch on it but
// always returns true now: multi-user is unconditional.
func (g *Gateway) IsCloudMode() bool { return true }

// Run starts the gateway and blocks until the process gets SIGINT/SIGTERM.
// On Unix, SIGHUP triggers a hot reload of every cached UserSpace so the
// next request picks up store mutations made by the CLI or another peer.
func (g *Gateway) Run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-stopCh
		slog.Info("received signal, shutting down", "signal", sig)
		cancel()
	}()

	reloadCh := make(chan os.Signal, 1)
	notifyReloadSignal(reloadCh)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-reloadCh:
				slog.Info("received reload signal, reloading agents")
				if err := g.ReloadAgents(); err != nil {
					slog.Warn("agent reload failed", "error", err)
				}
			}
		}
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
	NSAgentDefaults  = "agents.defaults"
	NSSandbox        = "sandbox"
	NSObjectStore    = "objectstore"
	NSHooks          = "hooks"
	NSPlugins        = "plugins"
	NSTaskQueue      = "taskqueue"
	NSToolProviders  = "tools.providers"
	NSToolCategories = "tools.categories"
	NSSkillsInstall  = "skills.install"
	NSSkillsEntries  = "skills.entries"
	NSMemory         = "memory"
	NSPrivacy        = "privacy"
	NSSkillsLearner  = "skillsLearner"
	NSHeartbeat      = "heartbeat"
	NSTeams          = "teams"
	NSBindings       = "bindings"
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
		if err := registerChannelInstance(r, mb, chanMgr, false); err != nil {
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

// imgRefRegex matches markdown image references `![alt](path)`. We
// keep capture groups for both alt and path so the helper below can
// reuse them when building MediaItems and stripping the marker from
// the chat body.
var imgRefRegex = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)

// splitMediaFromReply pulls every `![alt](src)` ref out of `reply` and
// turns it into a MediaItem the IM channel can upload directly:
//
//   - data:image/...;base64,…   → decode bytes inline, strip from text
//   - /workspace/foo or foo     → fetch via workspace.Store, strip from text
//   - http:// or https://       → left in place (some IMs auto-embed URLs)
//
// Refs whose bytes can't be resolved still get **stripped** from the
// output (otherwise a 200KB base64 data URL or a broken
// `![alt](missing)` lands as raw text in the chat — caused the
// "telegram dumps base64" report). When we strip, alt text is dropped
// too because the agent's prose around it usually stands on its own.
//
// sessionID = msgChatID since the gateway routes one chat per session.
func splitMediaFromReply(ctx context.Context, ws workspace.Store, agentID, sessionID, reply string) (string, []bus.MediaItem) {
	if reply == "" {
		return reply, nil
	}
	matches := imgRefRegex.FindAllStringSubmatchIndex(reply, -1)
	if len(matches) == 0 {
		return reply, nil
	}
	var items []bus.MediaItem
	var out strings.Builder
	cursor := 0
	for _, m := range matches {
		path := reply[m[4]:m[5]]

		// Remote URLs: keep the markdown ref intact so the IM client
		// can render its own preview. Don't strip, don't fetch.
		if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
			continue
		}

		var bytes []byte
		var filename string

		if strings.HasPrefix(path, "data:") {
			b, name, err := decodeDataURL(path)
			if err != nil {
				// Common case: LLM hallucinates a data URL with a
				// truncated/abbreviated base64 ("...", placeholders,
				// random fake bytes). Expected — log at Debug and
				// rely on the workspace fallback to still deliver
				// the real file. Don't spam Warn for this.
				head := path
				if len(head) > 80 {
					head = head[:80] + "…"
				}
				slog.Debug("data URL decode failed (LLM-fabricated bytes are expected — workspace fallback covers it)",
					"agent", agentID, "error", err, "len", len(path), "head", head)
			} else {
				bytes = b
				filename = name
			}
		} else if ws != nil {
			key := strings.TrimPrefix(path, "/workspace/")
			key = strings.TrimPrefix(key, "workspace/")
			key = strings.TrimPrefix(key, "/")
			if key != "" {
				rc, err := ws.Get(ctx, agentID, sessionID, key)
				if err != nil {
					slog.Warn("split media: workspace get failed", "agent", agentID, "session", sessionID, "key", key, "error", err)
				} else {
					data, rerr := io.ReadAll(rc)
					rc.Close()
					if rerr != nil {
						slog.Warn("split media: read failed", "key", key, "error", rerr)
					} else {
						bytes = data
						filename = filepath.Base(key)
					}
				}
			}
		}

		if len(bytes) > 0 {
			items = append(items, bus.MediaItem{Filename: filename, Bytes: bytes})
		}

		// Strip the `![alt](src)` either way — leaving an unresolvable
		// ref in the chat body just shows raw markdown / a base64 blob.
		out.WriteString(reply[cursor:m[0]])
		cursor = m[1]
		// Drop the trailing newline after the image ref if one
		// follows — keeps the body tidy when the LLM put the ref on
		// its own line.
		if cursor < len(reply) && reply[cursor] == '\n' {
			cursor++
		}
	}
	out.WriteString(reply[cursor:])
	return strings.TrimSpace(out.String()), items
}

// decodeDataURL parses `data:image/png;base64,...` style URLs into raw
// bytes. Returns (bytes, suggested filename, error). Extension is
// derived from the MIME so IMs that sniff content-type by filename
// (Telegram does for documents) get a sensible default.
func decodeDataURL(dataURL string) ([]byte, string, error) {
	if !strings.HasPrefix(dataURL, "data:") {
		return nil, "", fmt.Errorf("not a data URL")
	}
	rest := dataURL[len("data:"):]
	commaIdx := strings.IndexByte(rest, ',')
	if commaIdx < 0 {
		return nil, "", fmt.Errorf("data URL missing payload")
	}
	meta := rest[:commaIdx]
	payload := rest[commaIdx+1:]
	mimeType := "application/octet-stream"
	isBase64 := false
	for _, part := range strings.Split(meta, ";") {
		switch {
		case part == "base64":
			isBase64 = true
		case strings.Contains(part, "/"):
			mimeType = part
		}
	}
	var raw []byte
	if isBase64 {
		// LLMs frequently soft-wrap long base64 payloads, putting
		// whitespace mid-string that stock StdEncoding rejects with
		// "illegal base64 data". Strip whitespace before decode and
		// fall through alternative alphabets / paddings so the
		// agent's markdown survives any of the common variants:
		// standard, URL-safe, with or without padding.
		clean := stripWhitespace(payload)
		decoded, err := decodeBase64Tolerant(clean)
		if err != nil {
			return nil, "", fmt.Errorf("base64 decode: %w", err)
		}
		raw = decoded
	} else {
		// URL-encoded text payload — rare for images but handle it.
		u, err := url.QueryUnescape(payload)
		if err != nil {
			return nil, "", fmt.Errorf("url unescape: %w", err)
		}
		raw = []byte(u)
	}
	return raw, "media" + mimeExt(mimeType), nil
}

func stripWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func decodeBase64Tolerant(s string) ([]byte, error) {
	// Try the std-alphabet variants first (LLM-emitted base64 almost
	// always uses `+/`). URL-alphabet variants are kept as long-shot
	// fallbacks but listed last so the reported error is the
	// std-alphabet failure (much more informative — URL alphabets
	// fail at byte 0 the moment they hit a `/` character, which is
	// useless for diagnosis).
	candidates := []struct {
		name string
		enc  *base64.Encoding
	}{
		{"std", base64.StdEncoding},
		{"raw_std", base64.RawStdEncoding},
		{"url", base64.URLEncoding},
		{"raw_url", base64.RawURLEncoding},
	}
	var errs []string
	for _, c := range candidates {
		if data, err := c.enc.DecodeString(s); err == nil {
			return data, nil
		} else {
			errs = append(errs, c.name+": "+err.Error())
		}
	}
	return nil, fmt.Errorf("all base64 encodings failed (%s)", strings.Join(errs, "; "))
}

// appendRecentWorkspaceImages lists the session's workspace and
// attaches every image file modified at or after `turnStart`. This is
// the IM-side guarantee that "if image-tool wrote a file this turn,
// the user receives it" — independent of whether the LLM's reply
// markdown referenced it correctly (broken data URLs, missing refs,
// hallucinated filenames all bypass this path).
//
// Filter rules:
//   - extension is one of .png/.jpg/.jpeg/.webp/.gif/.svg
//   - ModTime >= turnStart (use a 1-second back-buffer for stores
//     whose mtime granularity is coarse — better to over-send than
//     drop a borderline file)
//   - filename not already in `existing` (dedupe)
//
// Logs counts at every filter stage so a future "no image attached"
// report can be diagnosed from logs alone.
func appendRecentWorkspaceImages(ctx context.Context, ws workspace.Store, agentID, sessionID string, turnStart time.Time, existing []bus.MediaItem) []bus.MediaItem {
	if ws == nil {
		return existing
	}
	objs, err := ws.List(ctx, agentID, sessionID)
	if err != nil {
		slog.Warn("workspace list failed for media fallback",
			"agent", agentID, "session", sessionID, "error", err)
		return existing
	}

	have := make(map[string]bool, len(existing))
	for _, it := range existing {
		have[it.Filename] = true
	}

	// 1-second back-buffer: some store backends round mtime to
	// whole seconds, which can leave a file written 0.4s into the
	// turn with a mtime stamp 0.6s before turnStart.
	cutoff := turnStart.Add(-1 * time.Second)

	imageCount := 0
	recentCount := 0
	attached := 0
	for _, obj := range objs {
		if !looksLikeImage(obj.Path) {
			continue
		}
		imageCount++
		if obj.ModTime.Before(cutoff) {
			continue
		}
		recentCount++
		base := filepath.Base(obj.Path)
		if have[base] {
			continue
		}
		rc, gerr := ws.Get(ctx, agentID, sessionID, obj.Path)
		if gerr != nil {
			slog.Warn("workspace get failed for media fallback",
				"path", obj.Path, "error", gerr)
			continue
		}
		data, rerr := io.ReadAll(rc)
		rc.Close()
		if rerr != nil || len(data) == 0 {
			continue
		}
		existing = append(existing, bus.MediaItem{Filename: base, Bytes: data})
		have[base] = true
		attached++
	}
	slog.Info("workspace media fallback",
		"agent", agentID, "session", sessionID,
		"total_objs", len(objs), "images_total", imageCount,
		"images_recent", recentCount, "attached", attached,
		"turn_start", turnStart.Format(time.RFC3339Nano))
	return existing
}

func looksLikeImage(p string) bool {
	ext := strings.ToLower(filepath.Ext(p))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".svg":
		return true
	}
	return false
}

// mimeExt picks a filename extension from a MIME type — minimal table
// covering what image-tool / replicate / OpenAI image gen actually
// emit. Falls back to .bin for anything unknown.
func mimeExt(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "image/svg+xml":
		return ".svg"
	}
	return ".bin"
}
