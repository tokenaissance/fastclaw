package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/agent/goal"
	"github.com/fastclaw-ai/fastclaw/internal/agent/tools"
	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
	"github.com/fastclaw-ai/fastclaw/internal/session"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/usage"
	"github.com/fastclaw-ai/fastclaw/internal/workspace"
)

// providerForAgent picks an LLM provider for a single agent. Resolution:
//
//  1. Parse `rc.Model` as "<providerKey>/<modelId>".
//  2. Look up `rc.Providers[providerKey]`. `Providers` is the merged view
//     (global ← per-agent DB config), so agent-exclusive providers shadow global
//     ones with the same key.
//  3. Fall back to the shared provider (the one the Manager/UserSpace
//     picked from global defaults) so old deployments without per-agent
//     providers keep working.
//
// This is what makes per-agent credentials real at runtime — each agent
// builds its own provider.Provider from its own API key+base, not the
// user-space-wide one.
func providerForAgent(rc config.ResolvedAgent, shared provider.Provider) provider.Provider {
	key, _ := provider.SplitProviderModel(rc.Model)
	if key == "" {
		// A bare model name still works, but the credentials — and therefore
		// the upstream account/group — are the *shared* provider's. That is how
		// a model nobody misspelled answers "No available channel for model X
		// under group Y": the name was right, the account was not the one the
		// operator had in mind. Say it once per build instead of leaving the
		// 503 to be decoded by hand.
		slog.Warn("agent model has no provider prefix — requests use the shared provider",
			"agent", rc.ID, "model", rc.Model,
			"hint", "write the model as <providerKey>/<modelId> to pin a provider row from this agent's Providers map")
		return shared
	}
	if pc, ok := rc.Providers[key]; ok && pc.APIKey != "" {
		return provider.NewProvider(pc.APIKey, pc.APIBase, pc.APIType)
	}
	if _, ok := rc.Providers[key]; ok {
		slog.Warn("agent model names a provider row without an API key — requests use the shared provider",
			"agent", rc.ID, "model", rc.Model, "providerKey", key)
	} else {
		slog.Warn("agent model names an unknown provider — requests use the shared provider",
			"agent", rc.ID, "model", rc.Model, "providerKey", key,
			"knownProviderKeys", strings.Join(providerKeys(rc.Providers), ","))
	}
	return shared
}

// providerKeys returns the configured provider keys, sorted so the log line is
// stable (and diffable in tests).
func providerKeys(m map[string]config.ProviderConfig) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ManagerOption configures optional Manager behavior.
type ManagerOption func(*managerOpts)

type managerOpts struct {
	sessionStore    session.SessionStore
	memoryStore     MemoryStore
	workspaceStore  workspace.Store
	dataStore       store.Store
	meter           usage.Meter
	quotaStore      usage.QuotaStore
	userID          string
	globalSkillsCfg config.SkillsCfg
	mcpConfigNotify func(userID, agentID string)
	sessionLease    SessionLease
	// messageBus is where a fired goal continuation is published. Kept because the goal watchdog
	// (Manager.SweepStalledGoals) needs the same bus the agents publish onto.
	messageBus *bus.MessageBus
	// skillsLearnerCfg is the resolved `skillsLearner` namespace for the user
	// space this Manager builds agents for. Threaded through the same way as
	// privacyCfg: without it the row is writable and read by nobody, because
	// its only reader used to live in NewAgentWithFullCfg, a constructor with
	// no callers.
	skillsLearnerCfg config.SkillsLearnerCfg
	// memoryCfg is the resolved `memory` namespace (system ← user scope). It is
	// the default layer for auto-persist: the per-agent rc.AutoPersist override
	// is stamped over it at construction, and `everyNTurns` / `model` have no
	// per-agent counterpart at all, so this is the only way an operator can set
	// the distill cadence or give it its own model.
	memoryCfg config.MemoryCfg
	// privacyCfg is the resolved privacy settings (privacy.piiScrubbing.enabled)
	// for the user space this Manager builds agents for. Every agent it builds —
	// and every agent whose provider it swaps in on hot-reload — gets the
	// redaction wrapper when the switch is on (see Agent.setProvider).
	privacyCfg config.PrivacyCfg
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

// WithMeter installs the admin-level token meter on every agent so each
// provider.Chat / ChatStream call records into token_usage_daily. Omit
// to disable metering (tests, single-user dev runs).
func WithMeter(m usage.Meter) ManagerOption {
	return func(o *managerOpts) { o.meter = m }
}

// WithQuotaStore installs per-user billing quota enforcement on every
// agent. The agent loop checks the owner's quota before processing a
// turn — when exceeded, the user gets a friendly rejection and no LLM
// tokens are burned. Omit to disable quota enforcement.
func WithQuotaStore(qs usage.QuotaStore) ManagerOption {
	return func(o *managerOpts) { o.quotaStore = qs }
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

// WithPrivacy threads the user space's resolved privacy settings
// (privacy.piiScrubbing.enabled) into every agent the Manager builds. Without
// it the switch has no reader on this path: the row would be writable in the
// admin UI, readable back, and do nothing — which is what it did until the
// change register's row 49, because its only reader lived in
// NewAgentWithFullCfg, a constructor with no callers.
func WithPrivacy(cfg config.PrivacyCfg) ManagerOption {
	return func(o *managerOpts) { o.privacyCfg = cfg }
}

// WithSkillsLearner threads the user space's resolved `skillsLearner` settings
// into every agent the Manager builds, so the background skill extractor is
// reachable on the production path at all. Its construction must happen *here*
// (after the registry's skill routing is configured) rather than in the
// constructor: the learner writes through the skills namespace's single writer,
// which needs the per-user bucket and the workspace store to be wired first.
func WithSkillsLearner(cfg config.SkillsLearnerCfg) ManagerOption {
	return func(o *managerOpts) { o.skillsLearnerCfg = cfg }
}

// WithMemory threads the user space's resolved `memory` settings into every
// agent the Manager builds — the system ← user scope default for auto-persist,
// which the per-agent `agents.defaults.autoPersist` override then stamps over
// (nil = inherit, false = veto). Without it that row had the same defect the
// privacy row had before row 49 and the skillsLearner row before row 51: it was
// writable and readable but read by nobody, because its only reader lived in
// NewAgentWithFullCfg, a constructor no production path calls.
func WithMemory(cfg config.MemoryCfg) ManagerOption {
	return func(o *managerOpts) { o.memoryCfg = cfg }
}

// WithSessionLease installs the cross-replica turn lease on every agent the
// Manager builds. Omit it and each agent gets NopSessionLease: single-instance
// installs keep today's behaviour instead of failing closed on a component
// they never had (mirrors WithWorkspaceStore / WithDataStore).
func WithSessionLease(l SessionLease) ManagerOption {
	return func(o *managerOpts) { o.sessionLease = l }
}

// WithMCPConfigNotify wires the gateway's per-agent reload notify into
// every agent so the `mcp add/remove` tool can persist agent MCP config
// and invalidate caches across replicas.
func WithMCPConfigNotify(fn func(userID, agentID string)) ManagerOption {
	return func(o *managerOpts) { o.mcpConfigNotify = fn }
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
	// The watchdog publishes through the same bus the agents do (see SweepStalledGoals).
	m.opts.messageBus = mb

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
	// newAgentWithActor stamps the Manager's user (m.uid) as the session
	// actor so OAuth-protected MCP servers are owner-gated (scheme A):
	// an agent attached into a foreign UserSpace carries the visitor as
	// actor while rc.UserID stays the agent owner, so the token provider
	// refuses the visitor before reading the owner's credential.
	ag := newAgentWithActor(rc, providerForAgent(rc, prov), mb, homeDir, m.opts.globalSkillsCfg, m.uid, m.opts.privacyCfg, m.opts.memoryCfg)
	ag.SetOwnerUserID(m.uid)
	// Per-user skills bucket: chat-time `skills/...` writes route to
	// ~/.fastagent/users/<uid>/, where SkillsLoader's "personal" layer
	// also scans (see SkillsLoader.WithUserID). Set userID on the
	// registry up front (the systemFileStore branch below also sets
	// it, but only when memoryStore is wired — without this hoist a
	// non-cloud install would store-mirror skills under agentID
	// instead of the per-user owner key, splitting the same skill's
	// content between two store namespaces). Skipped on legacy /
	// single-user installs where m.uid is empty — file.go falls back
	// to systemRoot (agent home) so existing skill bundles still work.
	if m.uid != "" {
		ag.registry.SetOwnerUserID(m.uid)
		if base := userSkillsRootDir(m.uid); base != "" {
			ag.registry.SetUserSkillsRoot(base)
		}
	}
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
		// Tag the chatter (m.uid) for per-user files (USER.md /
		// MEMORY.md) and the agent's owner (rc.UserID) for identity
		// files (SOUL.md / IDENTITY.md / BOOTSTRAP.md / ...). Without
		// the second call, the agent's BOOTSTRAP flow would write
		// SOUL/IDENTITY/BOOTSTRAP under the chatter and the Customize
		// page (keyed on the agent owner) would never see them.
		ag.registry.SetOwnerUserID(m.uid)
		ag.registry.SetAgentOwnerUserID(rc.UserID)
		// Owner-uploaded knowledge base: registered only when the store
		// can search it. The prompt's knowledge index section tells the
		// model when to call it (large corpora); small corpora are
		// injected in full and the tool goes unused.
		if searcher, ok := m.opts.memoryStore.(tools.KnowledgeSearcher); ok {
			tools.RegisterKnowledgeSearch(ag.registry, searcher)
		}
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
	if m.opts.sessionLease != nil {
		ag.sessionLease = m.opts.sessionLease
	}
	if m.opts.dataStore != nil {
		// Cron tools need the relational store to persist scheduled
		// jobs; the closure also reads channel/chatID off the registry
		// at execute time (bindSession stamps them per-turn) so the
		// fired message routes back to the originating chat.
		tools.RegisterCronTools(ag.registry, m.opts.dataStore, rc.ID)
		// set_timezone persists the chatter's IANA timezone into scope
		// prefs — the same rows the system-prompt date line and cron
		// scheduling resolve through. Needs the relational store, so it
		// rides the same guard as cron.
		tools.RegisterTimezoneTool(ag.registry, m.opts.dataStore)
		tools.RegisterPreferenceTool(ag.registry, m.opts.dataStore)
		// /goal feature: token-accounting hook + update_goal tool, all
		// keyed on the agent's owner (set above by SetOwnerUserID).
		// Same dataStore guard as cron because both features need the
		// relational store; agents without one degrade quietly.
		ag.WireGoals(m.opts.dataStore)
		// Stamp on Agent too so runtime checks (e.g. the autoPersist
		// cadence gate that counts session_messages instead of relying
		// on an in-memory counter that restart-clears) can hit the
		// store directly without re-plumbing through Manager.
		ag.dataStore = m.opts.dataStore
		ag.mcpConfigNotify = m.opts.mcpConfigNotify
		// Date line in the chatter's timezone — needs dataStore for the
		// scope-prefs lookup, hence wired here and re-applied by
		// ReloadWorkspaceFiles after every ctxBuilder rebuild.
		ag.ctxBuilder.SetTimezoneResolver(ag.chatterLocation)
	}
	// Stamp agentID even when no workspaceStore is wired (single-user
	// local mode), so usage metering can record per-agent rollups.
	ag.agentID = rc.ID
	if m.opts.meter != nil {
		ag.SetMeter(m.opts.meter)
		tools.RegisterBillingTools(ag.registry, m.opts.meter, m.opts.quotaStore)
	}
	if m.opts.quotaStore != nil {
		ag.SetQuotaStore(m.opts.quotaStore)
	}
	// Background skills learner (`skillsLearner` namespace). Last, because it
	// needs the wiring above: the registry must already know the per-user skill
	// bucket and the workspace store before the learner can write through it,
	// and the provider must already have gone through setProvider so the
	// extraction call is redacted like every other call site.
	ag.enableSkillsLearner(m.opts.skillsLearnerCfg)
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

// AddAgentWithSkillsCfg is AddAgent + a one-shot skills cfg override that
// replaces m.opts.globalSkillsCfg for just this build. EnsureAgent (which
// injects a foreign agent into a different user's UserSpace) uses this so
// the SkillsLoader closure baked into the new agent picks up the agent's
// own agent-scope skill env (e.g. image-tool's REPLICATE_API_TOKEN) — the
// caller's UserSpace cfg doesn't carry it because the agent isn't owned
// by the caller.
//
// The override is local: m.opts.globalSkillsCfg is restored before
// returning so the next AddAgent on the same manager goes back to the
// caller's own cfg. Held under no extra lock — callers (UserSpace.
// EnsureAgent) already serialize via sp.mu.
func (m *Manager) AddAgentWithSkillsCfg(rc config.ResolvedAgent, prov provider.Provider, mb *bus.MessageBus, cfg config.SkillsCfg) error {
	if _, exists := m.agents[rc.ID]; exists {
		return fmt.Errorf("agent %q already exists", rc.ID)
	}
	prev := m.opts.globalSkillsCfg
	m.opts.globalSkillsCfg = cfg
	m.agents[rc.ID] = m.buildAgent(rc, prov, mb)
	m.opts.globalSkillsCfg = prev
	slog.Info("agent added dynamically with override skills cfg", "id", rc.ID, "model", rc.Model)
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

// SweepStalledGoals re-fires the continuation of a goal whose chain has gone quiet.
//
// A goal advances through PostTurn hooks only — something has to fire the next continuation and
// there is no timer anywhere (`goal.TryFireContinuation` has three callers, all of them turn
// boundaries). So anything that keeps a hook from firing leaves the row `active` with nobody
// scheduled to move it: a failed turn (until row 78 landed), a killed pod, a lost event, a failed
// state write. Production measured 86 minutes of silence on 2026-09-28 before the user typed
// "continue" themselves — that is the symptom this sweep removes.
//
// Conservative by construction:
//   - only goals untouched for `staleAfter` are considered (`updated_at` moves on every model call,
//     so a live turn keeps its own row fresh);
//   - a goal is skipped while its agent has ANY turn in flight — that turn owns the chain and will
//     fire the hook itself;
//   - `TryFireContinuation` re-reads the row and re-checks every gate (active, routing), so a race
//     with a live turn costs at most one extra message, which the session's own admission defers.
func (m *Manager) SweepStalledGoals(ctx context.Context, staleAfter time.Duration) {
	if m.opts.dataStore == nil || m.opts.messageBus == nil {
		return
	}
	stale, err := m.opts.dataStore.ListStaleActiveGoals(ctx, time.Now().UTC().Add(-staleAfter))
	if err != nil {
		slog.Warn("goal watchdog: listing stalled goals failed", "error", err)
		return
	}
	for _, g := range stale {
		ag := m.agents[g.AgentID]
		if ag == nil || ag.TurnInFlight() {
			continue
		}
		slog.Info("goal watchdog: re-firing a stalled continuation",
			"agent", g.AgentID, "session", g.SessionKey, "goal", g.ID,
			"stale_for", time.Since(g.UpdatedAt).Truncate(time.Second))
		goal.TryFireContinuation(ctx, m.opts.dataStore, m.opts.messageBus, g.AgentID, g.SessionKey)
	}
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

// AnyTurnInFlight reports whether any of this manager's agents has a turn
// running or waiting for the slot.
func (m *Manager) AnyTurnInFlight() bool {
	if m == nil {
		return false
	}
	for _, ag := range m.All() {
		if ag.TurnInFlight() {
			return true
		}
	}
	return false
}

// CloseMCPClients releases every agent's MCP clients: a stdio server is a
// subprocess and a standing notification stream is a goroutine plus a
// connection, so both outlive the agent object unless something hands them
// back. Called when the owning user space is released, never per turn
// (docs 10 §3.4).
func (m *Manager) CloseMCPClients() {
	if m == nil {
		return
	}
	for _, ag := range m.All() {
		if ag != nil && ag.mcpMgr != nil {
			ag.mcpMgr.Close()
		}
	}
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
// Agents with their own per-agent provider override (per-agent providers
// in the DB config shadowing the shared one) keep their dedicated provider — this call
// only affects agents that were using the shared instance.
func (m *Manager) UpdateProvider(prov provider.Provider) {
	for _, ag := range m.agents {
		// setProvider, not a raw assignment: a swapped-in provider has to
		// pass through the same piiScrubbing decision as the one built at
		// construction, or a settings reload would silently unship the
		// redaction (the shape row 49 removed).
		ag.setProvider(prov)
	}
}

// UpdateProviderResolved is like UpdateProvider but aware of per-agent
// provider overrides. For each agent it rebuilds the provider using the
// same rule NewManager applied at construction: agent-level `providers`
// in the DB config shadow the shared fallback.
func (m *Manager) UpdateProviderResolved(shared provider.Provider, resolved []config.ResolvedAgent) {
	byID := make(map[string]config.ResolvedAgent, len(resolved))
	for _, rc := range resolved {
		byID[rc.ID] = rc
	}
	for id, ag := range m.agents {
		if rc, ok := byID[id]; ok {
			ag.setProvider(providerForAgent(rc, shared))
		} else {
			ag.setProvider(shared)
		}
	}
}
