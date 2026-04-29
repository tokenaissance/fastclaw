package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/channels"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/cron"
	"github.com/fastclaw-ai/fastclaw/internal/skills"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
	"github.com/fsnotify/fsnotify"
)

// startConfigWatcher watches the config file and workspace files for changes,
// triggering a hot-reload when modifications are detected.
func (g *Gateway) startConfigWatcher(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Error("failed to create file watcher", "error", err)
		return
	}
	defer watcher.Close()

	// Watch the default user's config directory for fastclaw.json changes.
	configPath, err := config.UserConfigPath(config.DefaultUserID)
	if err != nil {
		slog.Error("cannot determine config dir for watcher", "error", err)
		return
	}
	configDir := filepath.Dir(configPath)

	if err := watcher.Add(configDir); err != nil {
		slog.Error("failed to watch config dir", "dir", configDir, "error", err)
		return
	}
	slog.Info("config watcher started", "path", configDir)

	// Also watch agent home directories for SOUL.md, AGENTS.md, etc.
	for _, ag := range g.agents.All() {
		hPath := ag.HomePath()
		if hPath != "" {
			if err := watcher.Add(hPath); err != nil {
				slog.Warn("failed to watch agent home", "path", hPath, "error", err)
			}
		}
	}

	// Debounce: wait for writes to settle before reloading
	var debounceTimer *time.Timer
	var debounceMu sync.Mutex

	for {
		select {
		case <-ctx.Done():
			return

		case event, ok := <-watcher.Events:
			if !ok {
				return
			}

			// Only react to writes and creates
			if !event.Has(fsnotify.Write) && !event.Has(fsnotify.Create) {
				continue
			}

			filename := filepath.Base(event.Name)

			// Determine what changed
			isConfig := filename == "fastclaw.json"
			isWorkspaceFile := isWatchedWorkspaceFile(filename)

			if !isConfig && !isWorkspaceFile {
				continue
			}

			slog.Info("file change detected", "file", event.Name, "op", event.Op.String())

			// Debounce: many editors write multiple events per save
			debounceMu.Lock()
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.AfterFunc(500*time.Millisecond, func() {
				if isConfig {
					g.reloadConfig()
				} else if isWorkspaceFile {
					g.reloadWorkspaceFile(event.Name, filename)
				}
			})
			debounceMu.Unlock()

		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			slog.Error("file watcher error", "error", err)
		}
	}
}

// isWatchedWorkspaceFile returns true if this is a file we hot-reload.
func isWatchedWorkspaceFile(filename string) bool {
	switch filename {
	case "SOUL.md", "AGENTS.md", "IDENTITY.md", "TOOLS.md",
		"BOOTSTRAP.md", "HEARTBEAT.md", "MEMORY.md", "USER.md",
		"agent.json":
		return true
	}
	return false
}

// reloadConfig reloads the main config file and applies changes.
func (g *Gateway) reloadConfig() {
	slog.Info("hot-reloading config...")

	// Tolerate missing fastclaw.json in cloud mode (config comes from env +
	// DB). Env overlay keeps infra fields correct.
	newCfg := config.LoadOrEmpty()
	config.LoadEnv().ApplyToConfig(newCfg)

	// Mirror gateway.New's DB overlay: post-#7cdc49b the store is the
	// source of truth for product fields (providers, agents.defaults,
	// channels, sandbox via mergeCloudConfig). Without this overlay a
	// settings-page save lands in the DB but the in-memory cfg keeps the
	// boot-time view, so sandbox toggles / provider rotations don't take
	// effect until restart.
	if g.store != nil {
		if gc, err := g.store.GetConfig(context.Background()); err == nil && gc != nil && len(gc.Data) > 0 {
			if blob, merr := json.Marshal(gc.Data); merr == nil {
				var stored config.Config
				if uerr := json.Unmarshal(blob, &stored); uerr == nil {
					mergeCloudConfig(newCfg, &stored)
				}
			}
		}
	}
	config.ApplyDefaults(newCfg)

	// 1. Update LLM provider if changed
	g.reloadProvider(newCfg)

	// 2. Update agent configs (model, temperature, etc.)
	g.reloadAgents(newCfg)

	// 3. Sandbox: bootstrap a pool if onboarding just turned sandbox on,
	//    and enroll any newly-added agents into an existing pool.
	g.reloadSandbox(newCfg)

	// 4. Update bindings
	g.mu.Lock()
	g.bindings = newCfg.Bindings
	g.config = newCfg
	g.mu.Unlock()

	// 5. Update cron jobs
	g.reloadCron(newCfg)

	// 6. Update teams and group context
	g.reloadTeams(newCfg)

	slog.Info("hot-reload complete ✅")
}

// reloadSandbox handles sandbox-related changes during hot-reload.
//
// Cases:
//   - Pool exists, sandbox config materially changed (image, backend,
//     network, e2b key, idle TTL, enabled toggle) → tear the pool down
//     and fall through to the rebuild path. The Settings page exposes
//     these fields to the user, so edits have to take effect without a
//     process restart; the pre-existing "rare enough, restart fixes it"
//     assumption stopped holding once the UI grew Sandbox controls.
//   - Pool exists, no material change → enroll any newly-added agents
//     into the existing pool (Get is keyed on agentID, idempotent).
//   - No pool yet, fresh config wants sandbox → bootstrap via
//     attachSandboxToAgents (this is the path the onboard wizard hits:
//     gateway booted with no fastclaw.json, then user enabled sandbox).
//   - No pool, no sandbox wanted → attachSandboxToAgents falls back to
//     path-only restriction so newly-added agents still can't escape.
func (g *Gateway) reloadSandbox(newCfg *config.Config) {
	g.mu.RLock()
	pool := g.localSpace.SandboxPool
	var oldSandbox config.SandboxCfg
	if g.config != nil {
		oldSandbox = g.config.Sandbox
	}
	g.mu.RUnlock()

	if pool != nil && sandboxRebuildNeeded(oldSandbox, newCfg.Sandbox) {
		slog.Info("sandbox config changed, tearing down pool",
			"oldBackend", oldSandbox.Backend, "newBackend", newCfg.Sandbox.Backend,
			"oldImage", oldSandbox.Image, "newImage", newCfg.Sandbox.Image,
			"oldEnabled", oldSandbox.Enabled, "newEnabled", newCfg.Sandbox.Enabled)
		pool.CloseAll()
		// Drop the pool reference from every agent so an in-flight tool
		// call doesn't keep using a now-torn-down lazyExecutor. The
		// rebuild path below re-wires either a new pool or nil.
		for _, ag := range g.agents.All() {
			ag.SetSandboxPool(nil)
		}
		g.mu.Lock()
		g.localSpace.SandboxPool = nil
		g.mu.Unlock()
		pool = nil
	}

	if pool != nil {
		// Pool already exists from boot. New agents added via this reload
		// just need a pool reference; per-(agent,session) executors are
		// fetched lazily by the loop on the first tool call of a chat.
		for _, ag := range g.agents.All() {
			ag.SetSandboxPool(pool)
		}
		return
	}

	// Mirror reloadAgents: agents live in the store, not in newCfg, so a
	// bare ResolveAgents(newCfg) returns an empty list and
	// attachSandboxToAgents never sees Sandbox.Enabled — the pool stays
	// nil and the freshly-onboarded agent gets no executor wired.
	// Without the store load, the very first chat after onboarding fails
	// with "sandbox required but no executor available" until the user
	// restarts the gateway (gateway.New's own attachSandboxToAgents runs
	// over store-loaded agents and recovers).
	var storeAgents []config.AgentEntry
	if g.store != nil {
		if records, err := g.store.ListAgents(context.Background()); err == nil {
			for _, ar := range records {
				storeAgents = append(storeAgents, config.AgentEntry{ID: ar.ID, Model: ar.Model})
			}
		}
	}
	resolved := config.ResolveAgentsWithExtra(newCfg, "", storeAgents)
	newPool := attachSandboxToAgents(config.DefaultUserID, resolved, g.agents, g.workspace)
	if newPool != nil {
		g.mu.Lock()
		g.localSpace.SandboxPool = newPool
		g.mu.Unlock()
		slog.Info("hot-reload: sandbox pool created")
	}
}

// sandboxRebuildNeeded reports whether a sandbox config change is severe
// enough to require tearing down the live pool. Tracks every field that
// NewDockerExecutorPool / NewE2BExecutorPool / NewLifecyclePool capture
// at construction — changes to any of them won't take effect on a
// running pool, so we have to recreate it.
func sandboxRebuildNeeded(old, new config.SandboxCfg) bool {
	return old.Enabled != new.Enabled ||
		old.Backend != new.Backend ||
		old.Image != new.Image ||
		old.Network != new.Network ||
		old.E2BKey != new.E2BKey ||
		old.IdleTTLSec != new.IdleTTLSec
}

// resolveProviderCfg picks the active provider config from a Config.
func resolveProviderCfg(cfg *config.Config) config.ProviderConfig {
	var pc config.ProviderConfig
	defaultModel := cfg.Agents.Defaults.Model
	if parts := strings.SplitN(defaultModel, "/", 2); len(parts) == 2 {
		if p, ok := cfg.Providers[parts[0]]; ok {
			return p
		}
	}
	for _, key := range []string{"default", "openai", "openrouter"} {
		if p, ok := cfg.Providers[key]; ok {
			return p
		}
	}
	for _, p := range cfg.Providers {
		return p
	}
	return pc
}

// reloadProvider updates the LLM provider if API key/base/type changed.
func (g *Gateway) reloadProvider(newCfg *config.Config) {
	newProvCfg := resolveProviderCfg(newCfg)

	g.mu.RLock()
	oldCfg := g.config
	g.mu.RUnlock()

	oldProvCfg := resolveProviderCfg(oldCfg)

	if newProvCfg.APIKey != oldProvCfg.APIKey || newProvCfg.APIBase != oldProvCfg.APIBase || newProvCfg.APIType != oldProvCfg.APIType {
		llm := provider.NewProvider(newProvCfg.APIKey, newProvCfg.APIBase, newProvCfg.APIType)
		// Pass the resolved-agent list so per-agent overrides survive the
		// hot-reload (otherwise UpdateProvider would clobber agent-specific
		// providers with the shared fallback).
		resolved := config.ResolveAgents(newCfg)
		g.agents.UpdateProviderResolved(llm, resolved)
		slog.Info("hot-reload: provider updated", "apiBase", newProvCfg.APIBase, "apiType", newProvCfg.APIType)
	}
}

// reloadAgents adds new agents, updates configs on existing ones, and removes
// agents that no longer have a workspace on disk or in the DB store.
//
// Multi-pod invariant: agents created on pod A land in the DB via
// Store.SaveAgent, but the home directory is mkdir'd on pod A's local
// emptyDir only. On pod B this function needs to (a) still discover the
// agent (via the store agent list) and (b) lazy-create its home dir so
// subsequent filesystem reads on pod B succeed. `ResolveAgentsWithExtra`
// + ensureAgentHome below handle exactly that.
func (g *Gateway) reloadAgents(newCfg *config.Config) {
	var storeAgents []config.AgentEntry
	if g.store != nil {
		if records, err := g.store.ListAgents(context.Background()); err == nil {
			for _, ar := range records {
				storeAgents = append(storeAgents, config.AgentEntry{ID: ar.ID, Model: ar.Model})
			}
		}
	}
	resolved := config.ResolveAgentsWithExtra(newCfg, "", storeAgents)
	seen := make(map[string]bool, len(resolved))
	for _, rc := range resolved {
		seen[rc.ID] = true
		ensureAgentHome(rc)
		if g.workspace != nil {
			if err := skills.HydrateSkillsDown(
				context.Background(), g.workspace, rc.ID, filepath.Join(rc.Home, "skills"),
			); err != nil {
				slog.Warn("hot-reload: skill hydrate failed", "agent", rc.ID, "error", err)
			}
		}
		ag := g.agents.AgentByID(rc.ID)
		if ag == nil {
			if err := g.agents.AddAgent(rc, g.localSpace.Provider, g.bus); err != nil {
				slog.Error("hot-reload: failed to add agent", "id", rc.ID, "error", err)
			} else {
				slog.Info("hot-reload: new agent added", "id", rc.ID, "model", rc.Model)
			}
			continue
		}
		ag.UpdateConfig(rc)
		slog.Info("hot-reload: agent config updated", "id", rc.ID, "model", rc.Model)
	}
	// Remove agents that disappeared from both disk and the store.
	for _, existing := range g.agents.All() {
		if !seen[existing.Name()] {
			g.agents.RemoveAgent(existing.Name())
		}
	}
}

// ensureAgentHome idempotently creates the standard agent home layout
// (home + memory/sessions/skills subdirs) on the local filesystem. Safe
// to call on every reload — mkdirall is no-op if the dirs exist. This is
// what lets a pod that *didn't* handle the original create request still
// serve requests for that agent after discovering it via the store.
func ensureAgentHome(rc config.ResolvedAgent) {
	if rc.Home == "" {
		return
	}
	for _, dir := range []string{
		rc.Home,
		filepath.Join(rc.Home, "memory"),
		filepath.Join(rc.Home, "sessions"),
		filepath.Join(rc.Home, "skills"),
	} {
		_ = os.MkdirAll(dir, 0o755)
	}
}

// ReloadAgents is the public entrypoint used by the HTTP API after a
// config write via the web UI (settings page, onboarding wizard, agent
// CRUD). The name is historical — it now runs the FULL hot-reload
// pipeline (provider, agents, sandbox, bindings, cron, teams), not just
// agents. We kept the name to avoid touching every caller; treat it as
// "reload everything from disk + env + DB".
//
// Calling reloadConfig (instead of just reloadAgents) is what makes a
// settings-page sandbox toggle take effect without a restart: the
// pipeline calls reloadSandbox which bootstraps the executor pool when
// the new cfg flips Enabled on.
func (g *Gateway) ReloadAgents() error {
	g.reloadConfig()
	return nil
}

// reloadCron updates the cron scheduler with new jobs.
func (g *Gateway) reloadCron(newCfg *config.Config) {
	var cronJobs []cron.Job
	for _, cj := range newCfg.CronJobs {
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
	g.scheduler.UpdateJobs(cronJobs)
	slog.Info("hot-reload: cron jobs updated", "count", len(cronJobs))
}

// reloadTeams updates team config and group context.
func (g *Gateway) reloadTeams(newCfg *config.Config) {
	teams := newCfg.Teams
	if teams == nil {
		teams = make(map[string]config.TeamEntry)
	}
	g.mu.Lock()
	g.teams = teams
	g.mu.Unlock()

	// Refresh group context for agents in teams
	for _, team := range teams {
		for _, agentID := range team.Agents {
			ag := g.agents.AgentByID(agentID)
			if ag == nil {
				continue
			}
			var teammates []string
			for _, otherID := range team.Agents {
				if otherID != agentID {
					if uname, ok := g.botUsernames[otherID]; ok {
						teammates = append(teammates, "@"+uname)
					} else {
						teammates = append(teammates, otherID)
					}
				}
			}
			if botUname, ok := g.botUsernames[agentID]; ok {
				ag.SetGroupContext(&agent.GroupContext{
					BotUsername: botUname,
					Teammates:  teammates,
				})
			}
		}
	}
}

// reloadWorkspaceFile handles changes to agent identity files (SOUL.md, etc.)
// stored in the agent's home directory.
func (g *Gateway) reloadWorkspaceFile(fullPath, filename string) {
	dir := filepath.Dir(fullPath)

	// Find which agent owns this home dir
	for _, ag := range g.agents.All() {
		if ag.HomePath() == dir {
			ag.ReloadWorkspaceFiles()
			slog.Info("hot-reload: agent home file updated", "agent", ag.Name(), "file", filename)
			return
		}
	}
	slog.Warn("hot-reload: changed file doesn't match any agent home", "path", fullPath)
}

// Minimal channel hot-reload: new channels require restart,
// but we can update existing channel configs.
func (g *Gateway) reloadChannels(newCfg *config.Config) {
	// For now, log which channels changed. Full channel hot-reload
	// (adding/removing Telegram bots) requires restart.
	for name, chCfg := range newCfg.Channels {
		if !chCfg.Enabled {
			continue
		}
		slog.Info("hot-reload: channel config noted (restart needed for new channels)", "channel", name)
	}
}

// registerChannelsHot would add new channels at runtime.
// Currently not implemented — new channels require restart.
func registerChannelsHot(cfg *config.Config, mb *bus.MessageBus, chanMgr *channels.Manager) error {
	_ = cfg
	_ = mb
	_ = chanMgr
	return nil
}
