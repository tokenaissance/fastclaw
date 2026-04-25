package gateway

import (
	"context"
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
	"github.com/fastclaw-ai/fastclaw/internal/session"
	"github.com/fastclaw-ai/fastclaw/internal/skills"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/workspace"
)

// globalSkillsDirPath returns ~/.fastclaw/skills — the shared skills
// directory that every agent reads as loader "Layer 2". Kept local to
// the gateway because it's only needed here; handlers resolve the same
// path independently via config.HomeDir().
func globalSkillsDirPath() (string, error) {
	home, err := config.HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "skills"), nil
}

// attachSandboxToAgents wires a sandbox Executor (E2B or Docker) to every
// agent's tool registry when any agent's SandboxCfg says Enabled. Returns
// the LifecyclePool so callers can attach it to a UserSpace for later
// eviction; returns nil when no agent wants a sandbox.
//
// This is shared between the lazy loadUserSpace path and the eager local
// space bootstrap in gateway.New — previously only the former had it, so
// admin exec calls fell back to the pod's host shell.
func attachSandboxToAgents(
	userID string,
	resolved []config.ResolvedAgent,
	agentMgr *agent.Manager,
	ws workspace.Store,
) sandbox.ExecutorPool {
	var pool sandbox.ExecutorPool
	var sandboxCfg config.SandboxCfg
	for _, rc := range resolved {
		if rc.Sandbox.Enabled {
			sandboxCfg = rc.Sandbox
			switch rc.Sandbox.Backend {
			case "e2b":
				apiKey := rc.Sandbox.E2BKey
				if apiKey == "" {
					apiKey = os.Getenv("E2B_API_KEY")
				}
				template := rc.Sandbox.Image
				if template == "" {
					template = "base"
				}
				pool = sandbox.NewE2BExecutorPool(apiKey, template, 30*time.Minute)
				slog.Info("sandbox executor pool created",
					"user", userID, "backend", "e2b", "template", template)
			default: // "docker" or empty
				userDir, _ := config.UserDir(userID)
				pool = sandbox.NewDockerExecutorPool(rc.Sandbox.Image, userDir, nil)
				slog.Info("sandbox executor pool created",
					"user", userID, "backend", "docker")
			}
			break
		}
	}
	if pool != nil {
		idle := time.Duration(sandboxCfg.IdleTTLSec) * time.Second
		if idle <= 0 {
			idle = 10 * time.Minute
		}
		lp := sandbox.NewLifecyclePool(pool, idle, 30*time.Second)
		if ws != nil {
			lp.SetWorkspace(ws)
		}
		lp.Start()
		pool = lp
		slog.Info("sandbox lifecycle pool enabled",
			"user", userID, "idleTTL", idle, "hydrate", ws != nil)
		for _, ag := range agentMgr.All() {
			ex, err := pool.Get(context.Background(), ag.Name())
			if err != nil {
				slog.Warn("sandbox executor creation failed",
					"user", userID, "agent", ag.Name(), "error", err)
				continue
			}
			ag.ToolRegistry().SetExecutor(ex)
		}
		return pool
	}
	// No sandbox backend → path-only restriction on file tools so agents
	// can't escape their own workspace dir.
	userDir, err := config.UserDir(userID)
	if err == nil {
		for _, ag := range agentMgr.All() {
			ag.ToolRegistry().SetSandboxRoot(userDir)
		}
		slog.Info("path sandbox enabled", "user", userID)
	}
	return nil
}

// UserSpace holds the per-user state that can be multiplexed inside one
// gateway process. Channels, cron, webhook and plugins remain global (bound
// to the local admin user); the HTTP API multiplexes across user spaces.
type UserSpace struct {
	UserID       string
	Config       *config.Config
	Provider     provider.Provider
	Agents       *agent.Manager
	SandboxPool  sandbox.ExecutorPool // nil in local mode / when sandbox is disabled
}

// loadUserSpace reads a user's config and instantiates their agent manager.
// The shared message bus is reused so that cross-user plumbing (typing
// indicators, outbound routing) stays in one place.
func loadUserSpace(userID string, mb *bus.MessageBus, st store.Store, ws workspace.Store) (*UserSpace, error) {
	cfg, err := config.LoadForUser(userID)
	if err != nil {
		// Cloud/K8s mode: pod-local fastclaw.json may not exist. Fall back
		// to an empty config; env overlay below still populates infra.
		if os.IsNotExist(err) || strings.Contains(err.Error(), "no such file") {
			cfg = &config.Config{}
		} else {
			return nil, fmt.Errorf("load config for user %q: %w", userID, err)
		}
	}
	// ALWAYS apply env overlay so infra fields (storage DSN, object store
	// credentials, sandbox backend / API key, ...) take effect even when a
	// per-user fastclaw.json happens to exist. This mirrors main.go's
	// startup path; previously env was applied only in the file-missing
	// branch, which made FASTCLAW_SANDBOX_BACKEND / E2B_API_KEY silently
	// no-op for admin users that had any on-disk config.
	config.LoadEnv().ApplyToConfig(cfg)

	prov := newProviderFromConfig(cfg)

	// Pull agent IDs from the DB store so pods that didn't handle the
	// original create request still see the agent on startup. For each
	// resolved agent we also make sure the home dir layout exists on this
	// pod's filesystem (idempotent MkdirAll).
	var storeAgentIDs []string
	if st != nil {
		if records, lerr := st.ListAgents(context.Background()); lerr == nil {
			for _, ar := range records {
				storeAgentIDs = append(storeAgentIDs, ar.ID)
			}
		}
	}
	resolved := config.ResolveAgentsWithExtra(cfg, userID, storeAgentIDs)
	for _, rc := range resolved {
		ensureAgentHome(rc)
		// Pull skills that other pods have installed for this agent onto
		// local disk so SkillsLoader (which scans the filesystem) sees
		// them. No-op without an object store; cheap "skip if same size"
		// inside the hydrator keeps re-runs fast.
		if ws != nil {
			if err := skills.HydrateSkillsDown(
				context.Background(), ws, rc.ID, filepath.Join(rc.Home, "skills"),
			); err != nil {
				slog.Warn("skill hydrate failed", "agent", rc.ID, "error", err)
			}
		}
	}
	// Global skills (platform-wide, owned by no single agent) follow the
	// same pattern — hydrate them into ~/.fastclaw/skills/ so any agent
	// on this pod can load them. Bundled skills (embedded in the binary,
	// re-extracted at every startup by agent.InstallBundledSkills) are
	// protected via keepLocal so an empty OSS listing never wipes them.
	if ws != nil {
		globalSkillsDir, gerr := globalSkillsDirPath()
		if gerr == nil {
			if err := skills.HydrateSkillsDown(
				context.Background(), ws, skills.GlobalSkillOwner, globalSkillsDir,
				agent.BundledSkillNames()...,
			); err != nil {
				slog.Warn("global skill hydrate failed", "error", err)
			}
		}
	}
	managerOpts := []agent.ManagerOption{agent.WithUserID(userID)}
	if st != nil {
		managerOpts = append(managerOpts,
			agent.WithSessionStore(session.NewStoreAdapter(st)),
			agent.WithMemoryStore(agent.NewMemoryStoreAdapter(st)),
		)
	}
	if ws != nil {
		managerOpts = append(managerOpts, agent.WithWorkspaceStore(ws))
	}
	agentMgr, err := agent.NewManager(resolved, prov, mb, managerOpts...)
	if err != nil {
		return nil, fmt.Errorf("create agent manager for user %q: %w", userID, err)
	}

	// SetOwnerUserID is now performed inside agent.NewManager via
	// WithUserID; the loop here was the only previous wiring point and
	// would double-tag if we kept it.
	_ = userID
	registerAgentToolChains(cfg, agentMgr.All())

	var pool sandbox.ExecutorPool

	pool = attachSandboxToAgents(userID, resolved, agentMgr, ws)

	slog.Info("loaded user space", "user", userID, "agents", agentMgr.Names())

	return &UserSpace{
		UserID:      userID,
		Config:      cfg,
		Provider:    prov,
		Agents:      agentMgr,
		SandboxPool: pool,
	}, nil
}

// newProviderFromConfig picks an LLM provider from a user's config with the
// same fallback logic Gateway.New used to use inline.
func newProviderFromConfig(cfg *config.Config) provider.Provider {
	var providerCfg config.ProviderConfig
	var matchedKey string
	defaultModel := cfg.Agents.Defaults.Model
	if parts := strings.SplitN(defaultModel, "/", 2); len(parts) == 2 {
		if p, ok := cfg.Providers[parts[0]]; ok {
			providerCfg = p
			matchedKey = parts[0]
		}
	}
	if providerCfg.APIKey == "" {
		for _, key := range []string{"default", "openai", "openrouter"} {
			if p, ok := cfg.Providers[key]; ok {
				providerCfg = p
				matchedKey = key
				break
			}
		}
	}
	if providerCfg.APIKey == "" {
		for k, p := range cfg.Providers {
			providerCfg = p
			matchedKey = k
			break
		}
	}
	slog.Info("provider selected",
		"key", matchedKey,
		"apiBase", providerCfg.APIBase,
		"apiType", providerCfg.APIType,
		"hasKey", providerCfg.APIKey != "",
		"defaultModel", defaultModel,
		"providerCount", len(cfg.Providers),
	)
	return provider.NewProvider(providerCfg.APIKey, providerCfg.APIBase, providerCfg.APIType)
}

// userSpaceRegistry is a thread-safe lazy-loaded map of user spaces owned by
// the gateway. The "local" user is preloaded at gateway startup and is NEVER
// evicted; other users are loaded on first request and evicted after an idle
// period to free memory when they stop chatting.
type userSpaceRegistry struct {
	mu        sync.RWMutex
	spaces    map[string]*userSpaceEntry
	bus       *bus.MessageBus
	store     store.Store     // optional DB store for sessions/memory
	workspace workspace.Store // optional blob store for generated artifacts
	idleTTL   time.Duration   // how long before an idle user is evicted (0 = never)
	pinned    map[string]bool // user IDs that must never be evicted (e.g. "local")
}

type userSpaceEntry struct {
	space    *UserSpace
	lastUsed time.Time
}

func newUserSpaceRegistry(mb *bus.MessageBus, st ...store.Store) *userSpaceRegistry {
	reg := &userSpaceRegistry{
		spaces:  make(map[string]*userSpaceEntry),
		bus:     mb,
		idleTTL: 30 * time.Minute,
		pinned:  make(map[string]bool),
	}
	if len(st) > 0 && st[0] != nil {
		reg.store = st[0]
	}
	return reg
}

// put stores a preloaded user space that is pinned (never evicted).
func (r *userSpaceRegistry) put(space *UserSpace) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spaces[space.UserID] = &userSpaceEntry{space: space, lastUsed: time.Now()}
	r.pinned[space.UserID] = true
}

// get returns a user space if already loaded, refreshing its last-used time.
func (r *userSpaceRegistry) get(userID string) (*UserSpace, bool) {
	r.mu.RLock()
	e, ok := r.spaces[userID]
	r.mu.RUnlock()
	if !ok {
		return nil, false
	}
	// Touch under write lock (cheap: just a time assignment).
	r.mu.Lock()
	e.lastUsed = time.Now()
	r.mu.Unlock()
	return e.space, true
}

// getOrLoad returns the user space for a given user, loading it on first
// access and refreshing the idle timer on subsequent accesses.
func (r *userSpaceRegistry) getOrLoad(userID string) (*UserSpace, error) {
	if sp, ok := r.get(userID); ok {
		return sp, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.spaces[userID]; ok {
		e.lastUsed = time.Now()
		return e.space, nil
	}

	sp, err := loadUserSpace(userID, r.bus, r.store, r.workspace)
	if err != nil {
		return nil, err
	}
	r.spaces[userID] = &userSpaceEntry{space: sp, lastUsed: time.Now()}
	return sp, nil
}

// all returns a snapshot of all loaded user spaces.
func (r *userSpaceRegistry) all() []*UserSpace {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*UserSpace, 0, len(r.spaces))
	for _, e := range r.spaces {
		out = append(out, e.space)
	}
	return out
}

// evictIdle removes user spaces that have been idle longer than idleTTL.
// Pinned spaces (e.g. "local") are never evicted. Call periodically from a
// background goroutine.
func (r *userSpaceRegistry) evictIdle() int {
	if r.idleTTL <= 0 {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := time.Now().Add(-r.idleTTL)
	evicted := 0
	for uid, e := range r.spaces {
		if r.pinned[uid] {
			continue
		}
		if e.lastUsed.Before(cutoff) {
			delete(r.spaces, uid)
			evicted++
			slog.Info("evicted idle user space", "user", uid,
				"idle", time.Since(e.lastUsed).Round(time.Second))
		}
	}
	return evicted
}

// startEvictor runs a background loop that periodically evicts idle user
// spaces. Stops when ctx is cancelled.
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
