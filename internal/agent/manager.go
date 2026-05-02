package agent

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/agent/tools"
	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
	"github.com/fastclaw-ai/fastclaw/internal/session"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/workspace"
)

// providerForAgent picks an LLM provider for a single agent. Resolution:
//
//  1. Parse `rc.Model` as "<providerKey>/<modelId>".
//  2. Look up `rc.Providers[providerKey]`. `Providers` is the merged view
//     (global ← agent.json), so agent-exclusive providers shadow global
//     ones with the same key.
//  3. Fall back to the shared provider (the one the Manager/UserSpace
//     picked from global defaults) so old deployments without per-agent
//     providers keep working.
//
// This is what makes per-agent credentials real at runtime — each agent
// builds its own provider.Provider from its own API key+base, not the
// user-space-wide one.
func providerForAgent(rc config.ResolvedAgent, shared provider.Provider) provider.Provider {
	parts := strings.SplitN(rc.Model, "/", 2)
	if len(parts) == 2 {
		if pc, ok := rc.Providers[parts[0]]; ok && pc.APIKey != "" {
			return provider.NewProvider(pc.APIKey, pc.APIBase, pc.APIType)
		}
	}
	return shared
}

// ManagerOption configures optional Manager behavior.
type ManagerOption func(*managerOpts)

type managerOpts struct {
	sessionStore    session.SessionStore
	memoryStore     MemoryStore
	workspaceStore  workspace.Store
	dataStore       store.Store
	userID          string
	globalSkillsCfg config.SkillsCfg
}

func WithSessionStore(st session.SessionStore) ManagerOption {
	return func(o *managerOpts) { o.sessionStore = st }
}

func WithMemoryStore(st MemoryStore) ManagerOption {
	return func(o *managerOpts) { o.memoryStore = st }
}

// WithUserID tags every agent the Manager loads with the owning user, so
// store-backed Memory + Session calls scope rows by user_id. UserSpace
// passes the resolved user; local-mode gateway uses config.DefaultUserID.
func WithUserID(userID string) ManagerOption {
	return func(o *managerOpts) { o.userID = userID }
}

// WithWorkspaceStore installs a durable blob store on every agent's tool
// registry so file operations (write_file / read_file / list_dir) land in
// shared storage instead of pod-local filesystem.
func WithWorkspaceStore(ws workspace.Store) ManagerOption {
	return func(o *managerOpts) { o.workspaceStore = ws }
}

// WithDataStore exposes the platform's relational store to agents. The
// cron tool needs it to persist scheduled jobs that the cron.Scheduler
// later picks up; without it create_cron_job is omitted from the
// agent's tool list and time-bound requests fall back to natural-
// language reminders in HEARTBEAT.md (which only get a lazy 30-minute
// review and are wrong for short-fuse reminders).
func WithDataStore(st store.Store) ManagerOption {
	return func(o *managerOpts) { o.dataStore = st }
}

// WithGlobalSkillsCfg propagates cfg.Skills (entries + agentEntries
// holding skill apiKey/env per skill or per (agent,skill)) into agents
// the manager constructs. Without this, buildAgent → NewAgent passes a
// zero-value SkillsCfg and SkillsLoader.SkillEnvVars sees empty
// entries — every skill runs without its configured FAL_KEY /
// REPLICATE_API_TOKEN regardless of what's saved in the DB.
func WithGlobalSkillsCfg(cfg config.SkillsCfg) ManagerOption {
	return func(o *managerOpts) { o.globalSkillsCfg = cfg }
}

// Manager loads and manages all agent instances.
type Manager struct {
	agents       map[string]*Agent
	defaultAgent *Agent
	// opts is retained so AddAgent (hot-reload after onboard / agent
	// create) can apply the same store wiring the constructor did.
	// Without this the freshly-added agent's tool registry never gets
	// SetSystemFileStore, so read_file falls through to host FS and
	// 404s on identity files (SOUL/IDENTITY/...) that live only in DB.
	opts managerOpts
	uid  string
}

// NewManager creates agents from resolved configs.
func NewManager(resolved []config.ResolvedAgent, prov provider.Provider, mb *bus.MessageBus, opts ...ManagerOption) (*Manager, error) {
	m := &Manager{
		agents: make(map[string]*Agent),
	}
	for _, o := range opts {
		o(&m.opts)
	}

	if _, err := config.HomeDir(); err != nil {
		return nil, err
	}

	m.uid = m.opts.userID
	if m.uid == "" {
		return nil, fmt.Errorf("agent.NewManager: WithUserID is required")
	}
	for _, rc := range resolved {
		ag := m.buildAgent(rc, prov, mb)
		m.agents[rc.ID] = ag

		slog.Info("loaded agent",
			"id", rc.ID,
			"model", rc.Model,
			"home", rc.Home,
			"workspace", rc.Workspace,
		)
	}

	// If only one agent, make it the default
	if len(m.agents) == 1 {
		for _, ag := range m.agents {
			m.defaultAgent = ag
		}
	}

	return m, nil
}

// buildAgent constructs an Agent and wires every store the Manager
// was configured with. Shared between NewManager's bootstrap loop and
// AddAgent's hot-reload path so a freshly-onboarded agent picks up the
// same DB-backed identity / memory / workspace plumbing.
func (m *Manager) buildAgent(rc config.ResolvedAgent, prov provider.Provider, mb *bus.MessageBus) *Agent {
	homeDir, _ := config.HomeDir()
	// Pass the global SkillsCfg through so SkillsLoader sees the
	// admin-UI-configured per-skill apiKey + env (and the per-agent
	// override map). Plain NewAgent constructs the loader with a
	// zero-value SkillsCfg, which is why FAL_KEY / REPLICATE_API_TOKEN
	// were never reaching the sandbox.
	ag := NewAgentWithSkillsCfg(rc, providerForAgent(rc, prov), mb, homeDir, m.opts.globalSkillsCfg)
	ag.SetOwnerUserID(m.uid)
	if m.opts.sessionStore != nil {
		ag.sessions = session.NewManagerWithStoreForUser(rc.Home+"/sessions", m.opts.sessionStore, m.uid, rc.ID)
	}
	if m.opts.memoryStore != nil {
		ag.memory = NewMemoryWithStoreForUser(rc.Home, m.opts.memoryStore, m.uid, rc.ID)
		ag.ctxBuilder.store = m.opts.memoryStore
		ag.ctxBuilder.agentID = rc.ID
		ag.ctxBuilder.userID = m.uid
		ag.memoryStore = m.opts.memoryStore
		// Identity files (SOUL/IDENTITY/USER/...) share the same DB
		// store as memory so write_file from the agent ends up in
		// the same rows the admin UI's Customize page reads.
		ag.registry.SetSystemFileStore(m.opts.memoryStore, rc.ID)
		// Tag identity-file writes with the chatter so write_file on
		// SOUL.md / USER.md / MEMORY.md lands in the per-user override
		// row, not the shared template the Customize page edits.
		ag.registry.SetOwnerUserID(m.uid)
	}
	if m.opts.workspaceStore != nil {
		ag.registry.SetWorkspaceStore(m.opts.workspaceStore, rc.ID)
		// Also make the store available to SkillsLoader so object-store
		// skills (global + per-agent) are hydrated on every turn. Without
		// this, pods that didn't handle the original upload will never
		// see a new skill.
		ag.workspaceStore = m.opts.workspaceStore
		ag.agentID = rc.ID
		// Refresh skills now that workspaceStore is wired — the initial
		// NewAgent pass loaded only the filesystem, missing anything that
		// lives only in OSS.
		ag.ReloadWorkspaceFiles()
	}
	if m.opts.dataStore != nil {
		// Cron tools need the relational store to persist scheduled
		// jobs; the closure also reads channel/chatID off the registry
		// at execute time (bindSession stamps them per-turn) so the
		// fired message routes back to the originating chat.
		tools.RegisterCronTools(ag.registry, m.opts.dataStore, m.uid, rc.ID)
	}
	return ag
}

// AddAgent creates and registers a new agent dynamically (for hot-reload).
func (m *Manager) AddAgent(rc config.ResolvedAgent, prov provider.Provider, mb *bus.MessageBus) error {
	if _, exists := m.agents[rc.ID]; exists {
		return fmt.Errorf("agent %q already exists", rc.ID)
	}
	m.agents[rc.ID] = m.buildAgent(rc, prov, mb)
	slog.Info("agent added dynamically", "id", rc.ID, "model", rc.Model)
	return nil
}

// RemoveAgent unregisters an agent by ID. No-op if the agent is not loaded.
func (m *Manager) RemoveAgent(id string) {
	if _, ok := m.agents[id]; !ok {
		return
	}
	delete(m.agents, id)
	if m.defaultAgent != nil && m.defaultAgent.Name() == id {
		m.defaultAgent = nil
	}
	slog.Info("agent removed dynamically", "id", id)
}

// AgentByID returns an agent by its ID.
func (m *Manager) AgentByID(id string) *Agent {
	return m.agents[id]
}

// DefaultAgent returns the default agent (set when only one agent exists).
func (m *Manager) DefaultAgent() *Agent {
	return m.defaultAgent
}

// All returns all loaded agents.
func (m *Manager) All() []*Agent {
	result := make([]*Agent, 0, len(m.agents))
	for _, ag := range m.agents {
		result = append(result, ag)
	}
	return result
}

// Names returns all agent IDs.
func (m *Manager) Names() []string {
	names := make([]string, 0, len(m.agents))
	for name := range m.agents {
		names = append(names, name)
	}
	return names
}

// UpdateProvider replaces the LLM provider for all agents (hot-reload).
// Agents with their own per-agent provider override (agent.json providers
// shadowing the shared one) keep their dedicated provider — this call
// only affects agents that were using the shared instance.
func (m *Manager) UpdateProvider(prov provider.Provider) {
	for _, ag := range m.agents {
		ag.provider = prov
	}
}

// UpdateProviderResolved is like UpdateProvider but aware of per-agent
// provider overrides. For each agent it rebuilds the provider using the
// same rule NewManager applied at construction: agent-level `providers`
// in agent.json shadow the shared fallback.
func (m *Manager) UpdateProviderResolved(shared provider.Provider, resolved []config.ResolvedAgent) {
	byID := make(map[string]config.ResolvedAgent, len(resolved))
	for _, rc := range resolved {
		byID[rc.ID] = rc
	}
	for id, ag := range m.agents {
		if rc, ok := byID[id]; ok {
			ag.provider = providerForAgent(rc, shared)
		} else {
			ag.provider = shared
		}
	}
}
