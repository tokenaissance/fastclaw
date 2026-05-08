package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
	"github.com/fastclaw-ai/fastclaw/internal/sandbox"
	"github.com/fastclaw-ai/fastclaw/internal/scope"
	"github.com/fastclaw-ai/fastclaw/internal/session"
	"github.com/fastclaw-ai/fastclaw/internal/skills"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/workspace"
)

// loadAgentSkillEntries collects every agent-scope skills.entries row
// owned by this user. Mirrors the same logic in the HTTP layer; kept
// here so the runtime gateway never imports the setup handlers package.
func loadAgentSkillEntries(ctx context.Context, st store.Store, userID string) (map[string]map[string]config.SkillEntryCfg, error) {
	if st == nil {
		return nil, nil
	}
	agents, err := st.ListAgents(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]config.SkillEntryCfg{}
	for _, ar := range agents {
		rec, err := st.GetConfigByName(ctx, store.KindSetting, "", ar.ID, "skills.entries")
		if err != nil || rec == nil || len(rec.Data) == 0 {
			continue
		}
		blob, _ := json.Marshal(rec.Data)
		var entries map[string]config.SkillEntryCfg
		if json.Unmarshal(blob, &entries) == nil && len(entries) > 0 {
			out[ar.ID] = entries
		}
	}
	return out, nil
}

// ensureAgentHome idempotently creates the agent's local FS layout. Only
// `skills/` (FS-materialized SKILL.md bundles) and `memory/` (compaction
// dumps history JSONL here for audit / recovery) live on disk; identity
// files, session messages, and MEMORY.md are all in the DB.
func ensureAgentHome(rc config.ResolvedAgent) {
	if rc.Home == "" {
		return
	}
	for _, dir := range []string{
		rc.Home,
		filepath.Join(rc.Home, "skills"),
		filepath.Join(rc.Home, "memory", "logs"),
	} {
		_ = os.MkdirAll(dir, 0o755)
	}
}

// globalSkillsDirPath returns ~/.fastclaw/skills.
func globalSkillsDirPath() (string, error) {
	home, err := config.HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "skills"), nil
}

// buildSystemSandboxPool constructs the gateway-wide sandbox pool from
// the system-scope sandbox config. Returns nil when sandbox is not
// enabled at system scope (each user space then attaches no pool, and
// exec falls back to per-agent path roots).
//
// Lives at gateway scope, not per-UserSpace. The previous design built
// one pool per user, which (a) duplicated docker pools across users
// sharing the same image, and (b) left ad-hoc UserSpaces (notably the
// `app_user` identity that API-key callers get switched to) without
// any pool — those UserSpaces have zero of their own agents, so the
// per-user builder ran with `resolved=[]` and produced nil. Lazy-
// injected agents (super_admin chat, app-mode access) then ran exec
// with sandbox Enabled but no executor and surfaced "sandbox required
// but no executor available" to the user. Pulling the pool up to
// gateway scope makes the borrow path the default for every UserSpace.
func buildSystemSandboxPool(cfg config.SandboxCfg, ws workspace.Store) sandbox.ExecutorPool {
	if !cfg.Enabled {
		return nil
	}
	var inner sandbox.ExecutorPool
	home, _ := config.HomeDir()
	switch cfg.Backend {
	case "e2b":
		apiKey := cfg.E2BKey
		if apiKey == "" {
			apiKey = os.Getenv("E2B_API_KEY")
		}
		template := cfg.Image
		if template == "" {
			template = "base"
		}
		inner = sandbox.NewE2BExecutorPool(apiKey, template, home, 30*time.Minute)
		slog.Info("system sandbox executor pool created",
			"backend", "e2b", "template", template)
	default:
		policy := &sandbox.Policy{NetMode: cfg.Network}
		inner = sandbox.NewDockerExecutorPool(cfg.Image, home, policy)
		slog.Info("system sandbox executor pool created",
			"backend", "docker", "network", cfg.Network)
	}
	idle := time.Duration(cfg.IdleTTLSec) * time.Second
	if idle <= 0 {
		idle = 10 * time.Minute
	}
	lp := sandbox.NewLifecyclePool(inner, idle, 30*time.Second)
	if ws != nil {
		lp.SetWorkspace(ws)
	}
	lp.Start()
	slog.Info("system sandbox lifecycle pool enabled",
		"idleTTL", idle, "hydrate", ws != nil)
	return lp
}

// attachSandboxToAgents wires the gateway's shared sandbox pool to every
// agent in `agentMgr`. When `systemPool` is nil (sandbox disabled or
// not configured at system scope), falls back to the path-only mode:
// each agent's file tools are restricted to its own workspace dir.
//
// Pool ownership stays at the gateway: UserSpace eviction MUST NOT
// close the pool. The returned reference is the same pointer the
// gateway holds — kept on UserSpace.SandboxPool so per-request hot
// paths (EnsureAgent for lazy-injected agents) can pick it up without
// reaching back into the gateway.
func attachSandboxToAgents(
	systemPool sandbox.ExecutorPool,
	userID string,
	resolved []config.ResolvedAgent,
	agentMgr *agent.Manager,
) sandbox.ExecutorPool {
	if systemPool != nil {
		for _, ag := range agentMgr.All() {
			ag.SetSandboxPool(systemPool)
		}
		return systemPool
	}
	for _, rc := range resolved {
		if rc.Workspace == "" {
			continue
		}
		_ = os.MkdirAll(rc.Workspace, 0o755)
		if ag := agentMgr.AgentByID(rc.ID); ag != nil {
			ag.ToolRegistry().SetSandboxRoot(rc.Workspace)
		}
	}
	slog.Info("path sandbox enabled (no system pool configured)", "user", userID)
	return nil
}

// assembleConfig reads the namespaced settings rows and the scope-merged
// providers/channels for an (account, agent) and projects them into a
// runtime config.Config. Pass userID="" / agentID="" to skip those layers
// (agent boot uses the user-only view; system-only is for super_admin
// dashboards).
//
// Each setting namespace is its own configs row. assembleConfig
// reads them all in parallel-conceptually-but-serially-for-simplicity;
// the per-namespace cost is one indexed point lookup.
func assembleConfig(ctx context.Context, st store.Store, userID, agentID string) (*config.Config, error) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{},
		Channels:  map[string]config.ChannelConfig{},
	}
	if st == nil {
		return cfg, nil
	}
	if err := scope.SettingInto(ctx, st, NSAgentDefaults, userID, agentID, &cfg.Agents.Defaults); err != nil {
		return nil, err
	}
	if err := scope.SettingInto(ctx, st, NSSandbox, userID, agentID, &cfg.Sandbox); err != nil {
		return nil, err
	}
	if err := scope.SettingInto(ctx, st, NSObjectStore, userID, agentID, &cfg.ObjectStore); err != nil {
		return nil, err
	}
	if err := scope.SettingInto(ctx, st, NSHooks, userID, agentID, &cfg.Hooks); err != nil {
		return nil, err
	}
	if err := scope.SettingInto(ctx, st, NSPlugins, userID, agentID, &cfg.Plugins); err != nil {
		return nil, err
	}
	if err := scope.SettingInto(ctx, st, NSTaskQueue, userID, agentID, &cfg.TaskQueue); err != nil {
		return nil, err
	}
	if err := scope.SettingInto(ctx, st, NSToolProviders, userID, agentID, &cfg.ToolProviders); err != nil {
		return nil, err
	}
	if err := scope.SettingInto(ctx, st, NSToolCategories, userID, agentID, &cfg.Tools); err != nil {
		return nil, err
	}
	if err := scope.SettingInto(ctx, st, NSSkillsInstall, userID, agentID, &cfg.Skills.Install); err != nil {
		return nil, err
	}
	if err := scope.SettingInto(ctx, st, NSSkillsEntries, userID, agentID, &cfg.Skills.Entries); err != nil {
		return nil, err
	}
	// Per-agent skill env overrides used to live in a single user-scope
	// row keyed by agentID; they now persist as one scope=agent row each
	// at name=skills.entries (same namespace, narrower scope). Collect
	// every agent owned by this user — the agent runtime still wants
	// the keyed-by-agent map shape via cfg.Skills.AgentEntries.
	if userID != "" {
		entries, err := loadAgentSkillEntries(ctx, st, userID)
		if err != nil {
			return nil, err
		}
		if len(entries) > 0 {
			cfg.Skills.AgentEntries = entries
		}
	}
	if err := scope.SettingInto(ctx, st, NSMemory, userID, agentID, &cfg.Memory); err != nil {
		return nil, err
	}
	if err := scope.SettingInto(ctx, st, NSPrivacy, userID, agentID, &cfg.Privacy); err != nil {
		return nil, err
	}
	if err := scope.SettingInto(ctx, st, NSSkillsLearner, userID, agentID, &cfg.SkillsLearner); err != nil {
		return nil, err
	}
	if err := scope.SettingInto(ctx, st, NSHeartbeat, userID, agentID, &cfg.Heartbeat); err != nil {
		return nil, err
	}
	if err := scope.SettingInto(ctx, st, NSTeams, userID, agentID, &cfg.Teams); err != nil {
		return nil, err
	}
	if err := scope.SettingInto(ctx, st, NSBindings, userID, agentID, &cfg.Bindings); err != nil {
		return nil, err
	}
	provs, err := scope.Providers(ctx, st, userID, agentID)
	if err != nil {
		return nil, err
	}
	for k, v := range provs {
		cfg.Providers[k] = v
	}
	chs, err := scope.Channels(ctx, st, userID, agentID)
	if err != nil {
		return nil, err
	}
	for k, v := range chs {
		cfg.Channels[k] = v
	}
	return cfg, nil
}

// UserSpace holds the per-user runtime: their config snapshot, LLM
// provider, agent manager, and a sandbox pool reference. Lazy-loaded
// on first auth.
//
// SandboxPool is BORROWED from the gateway — every UserSpace shares
// the same pointer (or nil, when sandbox is disabled at system scope).
// Eviction must not call CloseAll on it; the gateway owns the
// lifecycle and tears it down once on shutdown.
type UserSpace struct {
	UserID      string
	Config      *config.Config
	Provider    provider.Provider
	Agents      *agent.Manager
	SandboxPool sandbox.ExecutorPool

	mu sync.Mutex
}

// EnsureAgent attaches an agent the user does not own to this UserSpace.
// Used by super_admin chat: the admin operates on a foreign agent under
// their own user_id namespace (sessions, memory, mem0 scope all stay
// caller-keyed) while the agent's persistent identity — system prompt,
// agent-scope config (`agents.defaults`), skills, and agent_files —
// is reused because those are agent_id-keyed in the store, not
// user_id-keyed.
//
// Idempotent: returns nil if the agent is already loaded.
func (sp *UserSpace) EnsureAgent(ctx context.Context, st store.Store, mb *bus.MessageBus, ws workspace.Store, agentID string) error {
	if sp == nil || sp.Agents == nil {
		return fmt.Errorf("EnsureAgent: nil UserSpace")
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.Agents.AgentByID(agentID) != nil {
		return nil
	}
	if st == nil {
		return fmt.Errorf("EnsureAgent: store required")
	}
	rec, err := st.GetAgent(ctx, agentID)
	if err != nil || rec == nil {
		return fmt.Errorf("EnsureAgent: agent %q not found", agentID)
	}
	resolved := config.ResolveAgents(sp.Config, []config.AgentEntry{{ID: rec.ID, UserID: rec.UserID}})
	if len(resolved) != 1 {
		return fmt.Errorf("EnsureAgent: ResolveAgents returned %d entries", len(resolved))
	}
	rc := resolved[0]
	if cfgRec, err := st.GetConfigByName(ctx, store.KindSetting, "", rc.ID, "agents.defaults"); err == nil && cfgRec != nil {
		var ovr config.AgentDefaults
		blob, _ := json.Marshal(cfgRec.Data)
		_ = json.Unmarshal(blob, &ovr)
		if ovr.Model != "" {
			rc.Model = ovr.Model
		}
		if ovr.MaxTokens > 0 {
			rc.MaxTokens = ovr.MaxTokens
		}
		if ovr.Temperature > 0 {
			rc.Temperature = ovr.Temperature
		}
		if ovr.MaxToolIterations > 0 {
			rc.MaxToolIterations = ovr.MaxToolIterations
		}
		if ovr.Thinking != "" {
			rc.Thinking = ovr.Thinking
		}
		if ovr.PolicyPreset != "" {
			rc.PolicyPreset = ovr.PolicyPreset
		}
	}
	// Overlay agent-scope providers — sp.Config.Providers carries only
	// system+user rows (assembleConfig in loadUserSpace runs with
	// agentID=""). Without this overlay, providerForAgent can't see the
	// agent's own credentials and falls back to the shared provider,
	// firing the agent's chosen model id at the wrong base URL.
	if agentProvs, err := scope.AgentScopeProviders(ctx, st, rc.ID); err == nil {
		for k, v := range agentProvs {
			if rc.Providers == nil {
				rc.Providers = make(map[string]config.ProviderConfig)
			}
			rc.Providers[k] = v
		}
	}
	ensureAgentHome(rc)
	if ws != nil {
		if err := skills.HydrateSkillsDown(ctx, ws, rc.ID, filepath.Join(rc.Home, "skills")); err != nil {
			slog.Warn("skill hydrate failed", "agent", rc.ID, "error", err)
		}
	}
	// Build a one-shot skills cfg that injects this agent's own
	// agent-scope skill env (e.g. image-tool's REPLICATE_API_TOKEN)
	// into the SkillsLoader closure the new agent will use.
	//
	// Why we can't just patch sp.Config: the manager's globalSkillsCfg
	// is captured by-value at manager-construction time and again by
	// the per-agent SkillsLoader on agent build, so patching sp.Config
	// after the fact never reaches the closure. AddAgentWithSkillsCfg
	// swaps the override only for the duration of this build.
	//
	// Symptom this fixes: web chat under the agent's owner works (the
	// owner's user-space cfg already carries the agent's skill env),
	// but API calls under an apikey/app_user that lands here silently
	// fall through to whatever keyless path the skill provides (e.g.
	// image-tool → pollinations, or "no provider configured" when
	// edit mode has no free fallback).
	//
	// Scope is deliberately tight: only the agent-scope row keyed by
	// rc.ID. We do NOT pull the owner's user-scope global skill env —
	// that would leak the owner's API keys into another user's session
	// for skills they may not even be invoking.
	skillsCfg := sp.Config.Skills
	if cfgRec, err := st.GetConfigByName(ctx, store.KindSetting, "", rc.ID, "skills.entries"); err == nil && cfgRec != nil && len(cfgRec.Data) > 0 {
		blob, _ := json.Marshal(cfgRec.Data)
		var entries map[string]config.SkillEntryCfg
		if json.Unmarshal(blob, &entries) == nil && len(entries) > 0 {
			if skillsCfg.AgentEntries == nil {
				skillsCfg.AgentEntries = map[string]map[string]config.SkillEntryCfg{}
			} else {
				// Copy-on-write: don't mutate the shared map the rest
				// of UserSpace.Config still points at.
				cp := make(map[string]map[string]config.SkillEntryCfg, len(skillsCfg.AgentEntries)+1)
				for k, v := range skillsCfg.AgentEntries {
					cp[k] = v
				}
				skillsCfg.AgentEntries = cp
			}
			skillsCfg.AgentEntries[rc.ID] = entries
		}
	}
	if err := sp.Agents.AddAgentWithSkillsCfg(rc, sp.Provider, mb, skillsCfg); err != nil {
		return fmt.Errorf("EnsureAgent: add agent: %w", err)
	}
	if sp.SandboxPool != nil {
		if ag := sp.Agents.AgentByID(rc.ID); ag != nil {
			ag.SetSandboxPool(sp.SandboxPool)
		}
	} else if rc.Workspace != "" {
		_ = os.MkdirAll(rc.Workspace, 0o755)
		if ag := sp.Agents.AgentByID(rc.ID); ag != nil {
			ag.ToolRegistry().SetSandboxRoot(rc.Workspace)
		}
	}
	slog.Info("agent injected into foreign user space",
		"caller", sp.UserID, "agent", rc.ID, "owner", rec.UserID)
	return nil
}

// loadUserSpace builds a UserSpace by:
//  1. snapshotting the system config (system_settings + system providers/
//     channels)
//  2. layering the user's own providers + channels rows on top
//  3. listing the user's agent rows from the DB
//  4. building an agent.Manager that owns those agents
//
// `systemSandboxPool` is the gateway-wide pool — borrowed, not owned,
// by the resulting UserSpace. Pass nil when sandbox is disabled at
// system scope; agents will run with path-only file roots in that
// case.
func loadUserSpace(ctx context.Context, userID string, mb *bus.MessageBus, st store.Store, ws workspace.Store, systemSandboxPool sandbox.ExecutorPool) (*UserSpace, error) {
	if userID == "" {
		return nil, fmt.Errorf("loadUserSpace: userID required")
	}
	if st == nil {
		return nil, fmt.Errorf("loadUserSpace: store required")
	}
	cfg, err := assembleConfig(ctx, st, userID, "")
	if err != nil {
		return nil, fmt.Errorf("assemble config: %w", err)
	}
	config.LoadEnv().ApplyToConfig(cfg)
	config.ApplyDefaults(cfg)

	prov := newProviderFromConfig(cfg)

	// Pull the user's agents from the DB. ResolveAgents merges in the
	// system+user defaults; per-agent overrides come from the configs
	// table via the agent-scope `agents.defaults` row that the create /
	// update agent handlers write to.
	agentRecords, err := st.ListAgents(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}

	// Public agents owned by other users are NOT loaded eagerly here —
	// they get lazy-attached via UserSpace.EnsureAgent the first time
	// the chatter hits the public-agent chat URL (see resolveAgent).
	// Sessions / memory / agent_files stay keyed by the chatter's
	// user_id so each visitor gets a private history while the
	// agent identity (SOUL/IDENTITY/skills) is shared from the
	// owner's row.

	entries := make([]config.AgentEntry, 0, len(agentRecords))
	for _, ar := range agentRecords {
		entries = append(entries, config.AgentEntry{ID: ar.ID, UserID: ar.UserID})
	}

	// Bindings used to live in their own kind=setting/name=bindings
	// row. After the configs schema refactor, channel rows carry
	// agent_id directly, so we synthesize Bindings from the channel
	// table itself — every row whose agent_id == one of this user's
	// owned agents contributes one Binding per Account in its data.
	cfg.Bindings = append(cfg.Bindings, bindingsFromChannelRows(ctx, st, userID, agentRecords)...)
	resolved := config.ResolveAgents(cfg, entries)
	for i := range resolved {
		// Layer the agent-scope agents.defaults on top of the
		// system→user merge that ResolveAgents already applied. We
		// read the agent-scope row directly (not via SettingInto
		// system+user, which would re-merge those layers and clobber
		// the user-scoped Model already in cfg.Agents.Defaults).
		//
		// Index into resolved (not range-by-value) so the writes
		// land on the slice element the manager later reads —
		// otherwise the agent-scope Model never reaches NewManager
		// and chat silently uses the system/user default.
		rc := &resolved[i]
		var agentOverride config.AgentDefaults
		if rec, err := st.GetConfigByName(ctx, store.KindSetting, "", rc.ID, "agents.defaults"); err == nil && rec != nil {
			blob, _ := json.Marshal(rec.Data)
			_ = json.Unmarshal(blob, &agentOverride)
			if agentOverride.Model != "" {
				rc.Model = agentOverride.Model
			}
			if agentOverride.MaxTokens > 0 {
				rc.MaxTokens = agentOverride.MaxTokens
			}
			if agentOverride.Temperature > 0 {
				rc.Temperature = agentOverride.Temperature
			}
			if agentOverride.MaxToolIterations > 0 {
				rc.MaxToolIterations = agentOverride.MaxToolIterations
			}
			if agentOverride.Thinking != "" {
				rc.Thinking = agentOverride.Thinking
			}
			if agentOverride.PolicyPreset != "" {
				rc.PolicyPreset = agentOverride.PolicyPreset
			}
		}
		// Same story for providers: assembleConfig was called with
		// agentID="" so cfg.Providers (now in rc.Providers) only
		// carries system+user rows. Without this, a per-agent
		// provider key (e.g. an agent-scoped OpenRouter credential)
		// is invisible to providerForAgent, which falls back to the
		// shared provider — chat fires the agent's chosen model id
		// at the wrong base URL and gets a 400 from the wrong vendor.
		if agentProvs, err := scope.AgentScopeProviders(ctx, st, rc.ID); err == nil {
			for k, v := range agentProvs {
				if rc.Providers == nil {
					rc.Providers = make(map[string]config.ProviderConfig)
				}
				rc.Providers[k] = v
			}
		}
		ensureAgentHome(*rc)
		if ws != nil {
			if err := skills.HydrateSkillsDown(
				ctx, ws, rc.ID, filepath.Join(rc.Home, "skills"),
			); err != nil {
				slog.Warn("skill hydrate failed", "agent", rc.ID, "error", err)
			}
		}
	}
	if ws != nil {
		if dir, gerr := globalSkillsDirPath(); gerr == nil {
			if err := skills.HydrateSkillsDown(
				ctx, ws, skills.GlobalSkillOwner, dir,
				agent.BundledSkillNames()...,
			); err != nil {
				slog.Warn("global skill hydrate failed", "error", err)
			}
		}
	}

	managerOpts := []agent.ManagerOption{
		agent.WithUserID(userID),
		agent.WithGlobalSkillsCfg(cfg.Skills),
		agent.WithSessionStore(session.NewStoreAdapter(st, userID)),
		agent.WithMemoryStore(agent.NewMemoryStoreAdapter(st)),
		agent.WithDataStore(st),
	}
	if ws != nil {
		managerOpts = append(managerOpts, agent.WithWorkspaceStore(ws))
	}
	agentMgr, err := agent.NewManager(resolved, prov, mb, managerOpts...)
	if err != nil {
		return nil, fmt.Errorf("create agent manager for user %q: %w", userID, err)
	}

	registerAgentToolChains(cfg, agentMgr.All())

	pool := attachSandboxToAgents(systemSandboxPool, userID, resolved, agentMgr)

	slog.Info("loaded user space", "user", userID, "agents", agentMgr.Names())

	return &UserSpace{
		UserID:      userID,
		Config:      cfg,
		Provider:    prov,
		Agents:      agentMgr,
		SandboxPool: pool,
	}, nil
}

// newProviderFromConfig picks an LLM provider for the resolved default
// model. Returns nil (with a clear log line) when nothing matches; the
// agent loop surfaces the missing-provider state as an error on the
// first turn rather than silently making bogus calls.
func newProviderFromConfig(cfg *config.Config) provider.Provider {
	defaultModel := cfg.Agents.Defaults.Model
	parts := strings.SplitN(defaultModel, "/", 2)
	if len(parts) != 2 {
		slog.Warn("no provider configured: default model is missing the '<provider>/<model>' prefix",
			"defaultModel", defaultModel, "providerCount", len(cfg.Providers))
		return nil
	}
	key := parts[0]
	p, ok := cfg.Providers[key]
	if !ok {
		slog.Warn("no provider configured: default model references a provider key that isn't in cfg.Providers",
			"key", key, "defaultModel", defaultModel,
			"availableKeys", providerKeyList(cfg.Providers))
		return nil
	}
	if p.APIKey == "" {
		slog.Warn("provider matched but its APIKey is empty",
			"key", key, "apiBase", p.APIBase)
		return nil
	}
	slog.Info("provider selected",
		"key", key, "apiBase", p.APIBase, "apiType", p.APIType,
		"defaultModel", defaultModel,
	)
	return provider.NewProvider(p.APIKey, p.APIBase, p.APIType)
}

func providerKeyList(m map[string]config.ProviderConfig) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// userSpaceRegistry is a thread-safe lazy-loaded map of user spaces. There
// are no preloaded / pinned spaces; every user is loaded on first auth and
// evicted after idleTTL of inactivity.
//
// `systemSandboxPool` is held as a borrowed reference and handed to
// each UserSpace at load time. The gateway owns its lifecycle.
type userSpaceRegistry struct {
	mu                sync.RWMutex
	spaces            map[string]*userSpaceEntry
	bus               *bus.MessageBus
	store             store.Store
	workspace         workspace.Store
	systemSandboxPool sandbox.ExecutorPool
	idleTTL           time.Duration
}

type userSpaceEntry struct {
	space    *UserSpace
	lastUsed time.Time
}

func newUserSpaceRegistry(mb *bus.MessageBus, st store.Store, ws workspace.Store, systemSandboxPool sandbox.ExecutorPool) *userSpaceRegistry {
	return &userSpaceRegistry{
		spaces:            make(map[string]*userSpaceEntry),
		bus:               mb,
		store:             st,
		workspace:         ws,
		systemSandboxPool: systemSandboxPool,
		idleTTL:           30 * time.Minute,
	}
}

func (r *userSpaceRegistry) get(userID string) (*UserSpace, bool) {
	r.mu.RLock()
	e, ok := r.spaces[userID]
	r.mu.RUnlock()
	if !ok {
		return nil, false
	}
	r.mu.Lock()
	e.lastUsed = time.Now()
	r.mu.Unlock()
	return e.space, true
}

func (r *userSpaceRegistry) getOrLoad(ctx context.Context, userID string) (*UserSpace, error) {
	if sp, ok := r.get(userID); ok {
		return sp, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.spaces[userID]; ok {
		e.lastUsed = time.Now()
		return e.space, nil
	}
	sp, err := loadUserSpace(ctx, userID, r.bus, r.store, r.workspace, r.systemSandboxPool)
	if err != nil {
		return nil, err
	}
	r.spaces[userID] = &userSpaceEntry{space: sp, lastUsed: time.Now()}
	return sp, nil
}

// invalidate drops a user's space so the next access reloads it. Used after
// admin mutations (creating an agent, rotating a provider, etc.) so the
// in-memory copy doesn't lag behind the DB.
func (r *userSpaceRegistry) invalidate(userID string) {
	r.mu.Lock()
	delete(r.spaces, userID)
	r.mu.Unlock()
}

func (r *userSpaceRegistry) all() []*UserSpace {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*UserSpace, 0, len(r.spaces))
	for _, e := range r.spaces {
		out = append(out, e.space)
	}
	return out
}

func (r *userSpaceRegistry) evictIdle() int {
	if r.idleTTL <= 0 {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := time.Now().Add(-r.idleTTL)
	evicted := 0
	for uid, e := range r.spaces {
		if e.lastUsed.Before(cutoff) {
			delete(r.spaces, uid)
			evicted++
			slog.Info("evicted idle user space", "user", uid,
				"idle", time.Since(e.lastUsed).Round(time.Second))
		}
	}
	return evicted
}

func (r *userSpaceRegistry) startEvictor(ctx context.Context) {
	if r.idleTTL <= 0 {
		return
	}
	interval := r.idleTTL / 3
	if interval < time.Minute {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n := r.evictIdle(); n > 0 {
				slog.Info("user space eviction sweep", "evicted", n, "remaining", len(r.spaces))
			}
		}
	}
}

// bindingsFromChannelRows synthesizes the (channel, accountID) →
// agentID routing table from channel rows themselves. It replaces the
// old kind=setting/name=bindings indirection: every channel row whose
// agent_id points at one of this user's agents contributes a Binding
// per Account.
//
// Pulls rows from two ownership corners that this user can route:
//   - (user_id='', agent_id=Y): the agent's "official" rows for any
//     agent Y the user owns
//   - (user_id=userID, agent_id=Y): per-(user, agent) overrides this
//     user authored on someone else's (or their own) agent
//
// Per-agent overrides live one row at a time, so we list channels for
// each agent the user can address. Granted-agent bindings stay outside
// — they live in the agent owner's space, not every grantee's.
func bindingsFromChannelRows(ctx context.Context, st store.Store, userID string, agents []store.AgentRecord) []config.Binding {
	if st == nil || len(agents) == 0 {
		return nil
	}
	var out []config.Binding
	for _, ar := range agents {
		rows, err := st.ListConfigs(ctx, store.KindChannel, "", ar.ID)
		if err == nil {
			out = append(out, expandChannelBindings(rows, ar.ID)...)
		}
		if userID != "" {
			rows, err := st.ListConfigs(ctx, store.KindChannel, userID, ar.ID)
			if err == nil {
				out = append(out, expandChannelBindings(rows, ar.ID)...)
			}
		}
	}
	return out
}

func expandChannelBindings(rows []store.ConfigRecord, agentID string) []config.Binding {
	var out []config.Binding
	for _, r := range rows {
		if !r.Enabled {
			continue
		}
		cc := config.ChannelConfig{}
		if blob, err := json.Marshal(r.Data); err == nil {
			_ = json.Unmarshal(blob, &cc)
		}
		// One Binding per account on the row; an empty Accounts map
		// means a single bot whose accountID is implicit (older
		// adapters that didn't index by username yet).
		if len(cc.Accounts) == 0 {
			out = append(out, config.Binding{
				AgentID: agentID,
				Match:   config.Match{Channel: r.Name},
			})
			continue
		}
		for accountID := range cc.Accounts {
			out = append(out, config.Binding{
				AgentID: agentID,
				Match:   config.Match{Channel: r.Name, AccountID: accountID},
			})
		}
	}
	return out
}
