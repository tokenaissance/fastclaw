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
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/adapter"
	"github.com/fastclaw-ai/fastclaw/internal/plugin"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
	coderuntime "github.com/fastclaw-ai/fastclaw/internal/runtime"
	"github.com/fastclaw-ai/fastclaw/internal/sandbox"
	"github.com/fastclaw-ai/fastclaw/internal/scope"
	"github.com/fastclaw-ai/fastclaw/internal/session"
	"github.com/fastclaw-ai/fastclaw/internal/skills"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/usage"
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
		var entries map[string]config.SkillEntryCfg
		// Resolved at agent scope through the same read model the runtime uses,
		// so a row that only exists in the configs_kv mirror is picked up here
		// rather than silently dropped.
		if err := scope.ExactSetting(ctx, st, "skills.entries", "", ar.ID, &entries); err != nil {
			continue
		}
		if len(entries) > 0 {
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

// globalSkillsDirPath returns ~/.fastagent/skills.
func globalSkillsDirPath() (string, error) {
	home, err := config.HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "skills"), nil
}

var (
	sandboxPoolOwnerOnce  sync.Once
	sandboxPoolOwnerValue string
)

// sandboxPoolOwnerID returns a per-pod identifier used as the lease owner in
// the shared sandbox_leases table. Distinct across replicas so an evicting
// pod can never destroy a sandbox another pod adopted.
func sandboxPoolOwnerID() string {
	sandboxPoolOwnerOnce.Do(func() {
		host, _ := os.Hostname()
		sandboxPoolOwnerValue = fmt.Sprintf("%s:%d", host, os.Getpid())
	})
	return sandboxPoolOwnerValue
}

// sandboxLeaseStoreFrom extracts the shared lease store from the relational
// store when it implements sandbox.SandboxLeaseStore (DBStore does). Nil
// keeps the per-pod behavior (no cross-pod sharing).
func sandboxLeaseStoreFrom(st store.Store) sandbox.SandboxLeaseStore {
	if st == nil {
		return nil
	}
	inner, ok := st.(sandbox.SandboxLeaseStore)
	if !ok {
		return nil
	}
	// envd_token is encrypted at rest with the same master secret used for
	// MCP OAuth tokens (FASTAGENT_OAUTH_SECRET). Without the secret we must
	// not persist plaintext sandbox tokens, so shared leases stay off and
	// each pod keeps its own sandbox (pre-lease behavior).
	secret := config.LoadEnv().OAuth.Secret
	if secret == "" {
		slog.Warn("shared sandbox leases disabled: FASTAGENT_OAUTH_SECRET is not set (envd_token would be stored in plaintext)")
		return nil
	}
	crypt, err := adapter.NewAESGCMCryptor(secret)
	if err != nil {
		slog.Warn("shared sandbox leases disabled: cannot build token cryptor", "error", err)
		return nil
	}
	return &store.EncryptedSandboxLeaseStore{Inner: inner, Crypt: crypt}
}

// sandboxLeaseOpts returns the shared-lease option for an E2B pool when
// both a lease store and a per-pod owner are available. Nil keeps the
// per-pod behavior (each replica creates its own sandbox). Extracted from
// buildSystemSandboxPool so the wiring decision is unit-testable without
// constructing a pool.
func sandboxLeaseOpts(leases sandbox.SandboxLeaseStore, ownerID string) *sandbox.E2BLeaseOptions {
	if leases == nil || ownerID == "" {
		return nil
	}
	return &sandbox.E2BLeaseOptions{Store: leases, Owner: ownerID}
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
func buildSystemSandboxPool(
	cfg config.SandboxCfg,
	ws workspace.Store,
	leases sandbox.SandboxLeaseStore,
	kv store.Store,
	ownerID string,
) sandbox.ExecutorPool {
	if !cfg.Enabled {
		return nil
	}
	var inner sandbox.ExecutorPool
	home, _ := config.HomeDir()
	// Prefer the per-backend image field (DockerImage / E2BTemplate /
	// BoxliteSnapshot); fall back to the legacy shared Image slot for
	// configs predating the split.
	switch cfg.Backend {
	case "e2b":
		apiKey := cfg.E2BKey
		if apiKey == "" {
			apiKey = os.Getenv("E2B_API_KEY")
		}
		template := cfg.E2BTemplate
		if template == "" {
			template = cfg.Image
		}
		if template == "" {
			template = "base"
		}
		var opts []func(*sandbox.E2BExecutorPool)
		sharedLeases := sandboxLeaseOpts(leases, ownerID)
		if sharedLeases != nil {
			opts = append(opts, sandbox.WithSandboxLeases(*sharedLeases))
		}
		inner = sandbox.NewE2BExecutorPool(apiKey, template, home, 30*time.Minute, opts...)
		slog.Info("system sandbox executor pool created",
			"backend", "e2b", "template", template, "sharedLeases", sharedLeases != nil)
	case "boxlite":
		secret := cfg.BoxliteKey
		if secret == "" {
			secret = os.Getenv("BOXLITE_API_KEY")
		}
		snapshot := cfg.BoxliteSnapshot
		if snapshot == "" {
			snapshot = cfg.Image
		}
		inner = sandbox.NewBoxliteExecutorPool(
			cfg.BoxliteURL,
			cfg.BoxlitePrefix,
			secret,
			snapshot,
			home,
			30*time.Minute,
		)
		slog.Info("system sandbox executor pool created",
			"backend", "boxlite", "image", snapshot, "url", cfg.BoxliteURL)
	default:
		image := cfg.DockerImage
		if image == "" {
			image = cfg.Image
		}
		policy := &sandbox.Policy{NetMode: cfg.Network}
		inner = sandbox.NewDockerExecutorPool(image, home, policy)
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
	// Deltas produced between turns (the idle-eviction sync) have no tool result
	// to ride, so they wait in a durable scope-keyed row instead of in this
	// process's memory (docs 09, G3).
	if kv != nil {
		lp.SetSignalStore(sandboxSignalStore{st: kv})
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
	// bindings is stored as {"list":[…]} while Config.Bindings is a slice —
	// project through the envelope (see config.BindingsPayload) instead of
	// unmarshalling the stored map into the slice, which fails and would
	// abort every load for that user. Only bindings present in the row
	// replace the synthesized ones.
	var bindingsRow config.BindingsPayload
	if err := scope.SettingInto(ctx, st, NSBindings, userID, agentID, &bindingsRow); err != nil {
		return nil, err
	}
	if len(bindingsRow.List) > 0 {
		cfg.Bindings = bindingsRow.List
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

// toolScopeRows is the tools.* view at exactly ONE (userID, agentID) scope —
// no layer merge. It is the unit every tool overlay is built from.
type toolScopeRows struct {
	categories map[string]config.ToolCategoryCfg
	providers  map[string]config.ToolProviderCfg
}

func (r toolScopeRows) empty() bool {
	return len(r.categories) == 0 && len(r.providers) == 0
}

// loadToolScopeRows reads tools.categories / tools.providers at exactly one
// (userID, agentID) scope, blob first with the mirror as fallback (same
// contract as scope.ExactSetting, which this wraps). Read errors are logged
// and treated as "no override": a broken tool row must not take down the
// UserSpace that the agent is being attached to.
func loadToolScopeRows(ctx context.Context, st store.Store, userID, agentID string) toolScopeRows {
	var out toolScopeRows
	if st == nil || (userID == "" && agentID == "") {
		return out
	}
	if err := scope.ExactSetting(ctx, st, NSToolCategories, userID, agentID, &out.categories); err != nil {
		slog.Warn("tool categories read failed", "user", userID, "agent", agentID, "error", err)
	}
	if err := scope.ExactSetting(ctx, st, NSToolProviders, userID, agentID, &out.providers); err != nil {
		slog.Warn("tool providers read failed", "user", userID, "agent", agentID, "error", err)
	}
	return out
}

// applyToolOverlay layers cats/provs on top of base and returns a copy.
// Copy-on-write: base is the UserSpace's shared snapshot and must never be
// mutated in place. Returns base unchanged (same pointer) when there is
// nothing to layer, so the common no-override case allocates nothing.
func applyToolOverlay(base *config.Config, overlays ...toolScopeRows) *config.Config {
	if base == nil {
		return base
	}
	cats := map[string]config.ToolCategoryCfg{}
	provs := map[string]config.ToolProviderCfg{}
	for _, o := range overlays {
		for k, v := range o.categories {
			cats[k] = v
		}
		for k, v := range o.providers {
			provs[k] = v
		}
	}
	if len(cats) == 0 && len(provs) == 0 {
		return base
	}
	merged := *base
	merged.Tools = mergeStringMap(base.Tools, cats)
	merged.ToolProviders = mergeStringMap(base.ToolProviders, provs)
	return &merged
}

// toolConfigForAgent returns the config view registerAgentToolChains should
// read for ONE agent: the UserSpace's own merged (system + caller-user)
// snapshot, plus every inner layer that the runtime would otherwise never
// read, in the same precedence scope.Setting uses (outer → inner, inner wins):
//
//	base                        system + the caller's own user scope
//	owner user scope            only when overlayOwner (foreign viewer whose
//	                            owner opted into shareModelConfig)
//	agent scope   ("", agentID)
//	caller+agent  (caller, agentID)
//
// Only exact-scope rows are pulled — never the owner's full merged view — so
// re-applying system rows can't clobber a caller-scope override (same rule as
// the UserScopeProviders overlay above).
//
// The agent layer is why this exists twice: `fastagent tools category set
// --agent X` writes an agent-scope row, and before this the gateway never
// read it — the row was accepted, stored, and ignored, which is exactly the
// silent failure mode that made an agent report "no web_search".
func toolConfigForAgent(ctx context.Context, st store.Store, base *config.Config, callerUserID, agentID, ownerUserID string, overlayOwner bool) *config.Config {
	if st == nil || base == nil || agentID == "" {
		return base
	}
	overlays := make([]toolScopeRows, 0, 3)
	if overlayOwner && ownerUserID != "" && ownerUserID != callerUserID {
		overlays = append(overlays, loadToolScopeRows(ctx, st, ownerUserID, ""))
	}
	overlays = append(overlays, loadToolScopeRows(ctx, st, "", agentID))
	if callerUserID != "" {
		overlays = append(overlays, loadToolScopeRows(ctx, st, callerUserID, agentID))
	}
	return applyToolOverlay(base, overlays...)
}

// toolOverlaysForAgents loads the agent-scope and per-(caller,agent) tool
// rows for many agents in two queries (one per namespace) instead of four
// point lookups per agent — loadUserSpace runs this for every agent of a
// user, so the batched form matters.
//
// Returned rows are filtered to the two layers the caller can see: the
// agent's own rows (user_id="") and the caller's own per-agent rows
// (user_id=callerUserID). Rows authored by anyone else are dropped —
// BatchGetConfigsByAgentIDs deliberately matches every user_id.
//
// The layers are applied in an explicit order (agent scope first, caller
// layer second) rather than in the order the query returned them: the batch
// query has no ORDER BY, so on Postgres the row order is storage-dependent
// and an overlapping key would otherwise resolve nondeterministically —
// disagreeing with toolConfigForAgent, which walks the layers in order.
func toolOverlaysForAgents(ctx context.Context, st store.Store, callerUserID string, agentIDs []string) map[string]toolScopeRows {
	out := make(map[string]toolScopeRows, len(agentIDs))
	if st == nil || len(agentIDs) == 0 {
		return out
	}
	catRows, err := scope.AgentScopeRows(ctx, st, NSToolCategories, agentIDs)
	if err != nil {
		slog.Warn("batch tool categories read failed", "agents", len(agentIDs), "error", err)
	}
	provRows, err := scope.AgentScopeRows(ctx, st, NSToolProviders, agentIDs)
	if err != nil {
		slog.Warn("batch tool providers read failed", "agents", len(agentIDs), "error", err)
	}
	// Layer order: the agent's own rows first, then the caller's per-agent
	// rows (inner scope wins), matching scope.Setting. Iterating layer by
	// layer — instead of trusting the query's row order — is what makes that
	// precedence deterministic; see the doc comment above.
	layerUsers := []string{""}
	if callerUserID != "" {
		layerUsers = append(layerUsers, callerUserID)
	}
	for _, pass := range []struct {
		rows []store.ConfigRecord
		cat  bool
	}{{catRows, true}, {provRows, false}} {
		for _, layerUser := range layerUsers {
			for _, r := range pass.rows {
				if r.UserID != layerUser {
					continue
				}
				layer := out[r.AgentID]
				if pass.cat {
					var m map[string]config.ToolCategoryCfg
					if jsonIntoConfig(r.Data, &m) == nil && len(m) > 0 {
						if layer.categories == nil {
							layer.categories = map[string]config.ToolCategoryCfg{}
						}
						for k, v := range m {
							layer.categories[k] = v
						}
					}
				} else {
					var m map[string]config.ToolProviderCfg
					if jsonIntoConfig(r.Data, &m) == nil && len(m) > 0 {
						if layer.providers == nil {
							layer.providers = map[string]config.ToolProviderCfg{}
						}
						for k, v := range m {
							layer.providers[k] = v
						}
					}
				}
				out[r.AgentID] = layer
			}
		}
	}
	return out
}

// jsonIntoConfig is the marshal/unmarshal hop these loaders share with
// scope.jsonInto (which is unexported): configs rows store Data as
// map[string]interface{}, the runtime wants typed structs.
func jsonIntoConfig(data map[string]interface{}, dst interface{}) error {
	blob, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return json.Unmarshal(blob, dst)
}

// mergeStringMap returns base ∪ over with `over` winning, as a fresh map so
// the caller's config snapshot is never mutated in place.
func mergeStringMap[V any](base, over map[string]V) map[string]V {
	out := make(map[string]V, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
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
	// PluginMgr is borrowed from the gateway (process-wide singleton).
	// Held here so EnsureAgent — the foreign-agent attach path — can
	// register hook plugins onto the lazy-built agent without
	// reaching back into the gateway. Nil when systemPlugins is off.
	PluginMgr *plugin.Manager
	// ProjectRuntime is borrowed from the gateway; held so EnsureAgent's
	// lazy-built agents also gain the coding-agent preview tools. Nil
	// when no runtime is configured.
	ProjectRuntime *coderuntime.Manager

	mu sync.Mutex
}

// resolveModel answers "which model will this agent actually run", given the
// layer values a caller has gathered. It exists because this contract is
// load-bearing and was previously encoded in statement order twice (the
// owner's own space in loadUserSpace, a foreign viewer's lazy attach in
// EnsureAgent) plus a prose table in docs/configs-kv-scope-adaptation.md —
// nothing failed if the order was rearranged.
//
// Order, most specific last:
//
//	base     system ← the CALLER's user row (already merged by
//	         assembleConfig/ResolveAgents, so the caller's own choice is in here)
//	ownerRow the agent OWNER's user-scope model — a foreign viewer with sharing
//	         on runs with the credentials the owner intended
//	agentRow the agent-scope model — the agent's own configuration, which wins
//	         over both user layers for everyone except the case below
//	pinRow   the VIEWER's own explicit user-scope model, pinned last for a
//	         foreign viewer ("MY tokens, MY model")
//
// An empty string means "this layer has no row"; callers pass "" for layers
// that do not apply to them (the owner's own space passes no ownerRow and no
// pinRow — its user row is already the base).
//
// Note on the pin: EnsureAgent applies it unconditionally today (it is only
// non-empty when the viewer set an explicit row), and this function preserves
// that rather than second-guessing it. Whether a NON-foreign caller should get
// the same pin is a separate product decision, not a refactor.
func resolveModel(base, ownerRow, agentRow, pinRow string) string {
	model := base
	if ownerRow != "" {
		model = ownerRow
	}
	if agentRow != "" {
		model = agentRow
	}
	if pinRow != "" {
		model = pinRow
	}
	return model
}

// readUserScopeAgentDefaults reads the (user=X, agent=”) agents.defaults
// row raw — distinct from assembleConfig, which merges system + user and
// can't tell apart "user explicitly chose the system value" from "no
// user-scope row at all". EnsureAgent uses this to detect a chatter's
// *explicit* model preference, so it can win over owner / agent-scope
// overrides for foreign agents. Returns the zero value when there's no
// row, the row's data can't be unmarshaled, or userID is empty (system
// caller — no per-user pin to honor).
func readUserScopeAgentDefaults(ctx context.Context, st store.Store, userID string) config.AgentDefaults {
	var out config.AgentDefaults
	if userID == "" || st == nil {
		return out
	}
	// The resolver is the only reader of this namespace: it owns who wins
	// between the blob and the mirror, and it applies the enabled veto (a
	// disabled row is "no explicit preference", which is the distinction this
	// function exists to make). A typed destination means the decoder parses
	// each number against the field it lands in, so the digits survive.
	_ = scope.ExactSetting(ctx, st, NSAgentDefaults, userID, "", &out)
	return out
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
	resolved := config.ResolveAgents(sp.Config, []config.AgentEntry{{ID: rec.ID, UserID: rec.UserID, Name: rec.Name}})
	if len(resolved) != 1 {
		return fmt.Errorf("EnsureAgent: ResolveAgents returned %d entries", len(resolved))
	}
	rc := resolved[0]
	// Owner-fallback layer: when the calling UserSpace isn't the agent
	// owner (super_admin, public-link viewer, apikey-shared user), pull
	// the OWNER's user-scope settings/providers so the agent runs with
	// the credentials and model the owner actually intended. Without
	// this, a viewer with no providers of their own falls through to
	// either the system shared provider (often a free-tier key that
	// runs out) or no provider at all → 429 / "no provider configured".
	// Order: viewer's resolved cfg → owner's user-scope (this block) →
	// agent-scope `agents.defaults` → agent-scope providers. Agent-
	// scope still wins, matching the precedence the owner's own
	// loadUserSpace path uses.
	//
	// shareModelConfig (agent record) gates this: default true —
	// chatters inherit the owner's keys + model selection out of the
	// box, matching the "owner already pays for the agent, they're the
	// one sharing it" mental model. Setting it to false explicitly
	// opts out, in which case the owner-fallback + agent-scope
	// overlays are skipped entirely for chatters and they see only
	// their own user-scope + system. Owner's own loadUserSpace path
	// (sp.UserID == rec.UserID) is unaffected and still gets the full
	// agent-scope overlay. The default lives in
	// setup.agentShareModelConfig — read it via the same helper here
	// (inlined to avoid a package cycle).
	//
	// Exception: when the viewer (chatter) has *explicitly* set their
	// own user-scope agents.defaults.model row, that choice wins over
	// both the owner's user-scope and the agent-scope override —
	// "MY tokens, MY model". We detect this by reading the chatter's
	// raw row directly (not the merged cfg, which can't distinguish
	// "explicit user-scope = system default" from "no user-scope row")
	// and re-pin the field after the overlay chain. Only Model is
	// pinned today — fields like MaxTokens / Temp / Thinking still
	// fall through the owner/agent overlays since the chatter doesn't
	// have UI to set them per-agent.
	chatterPin := readUserScopeAgentDefaults(ctx, st, sp.UserID)
	isForeign := rec.UserID != "" && rec.UserID != sp.UserID
	// Default true when the key is absent — keep aligned with
	// setup.agentShareModelConfig.
	shareCfg := true
	if v, ok := rec.Config["shareModelConfig"].(bool); ok {
		shareCfg = v
	}
	applyOwnerOverlays := !isForeign || shareCfg
	// The model is collected from the three layers that can carry one and
	// resolved once at the end (resolveModel), because "which layer wins" is a
	// contract this file keeps in prose in two places and previously in
	// statement order in both. The other fields stay inline: their unset
	// semantics differ per field (>0 / non-nil), so folding them into one
	// function would hide more than it explains.
	var ownerModel string
	if isForeign && applyOwnerOverlays {
		if ownerCfg, err := assembleConfig(ctx, st, rec.UserID, ""); err == nil && ownerCfg != nil {
			ovr := ownerCfg.Agents.Defaults
			ownerModel = ovr.Model
			if ovr.MaxTokens > 0 {
				rc.MaxTokens = ovr.MaxTokens
			}
			if ovr.Temperature > 0 {
				rc.Temperature = ovr.Temperature
			}
			if ovr.MaxToolIterations > 0 {
				rc.MaxToolIterations = ovr.MaxToolIterations
			}
			if ovr.MaxToolIterationContinues != nil {
				rc.MaxToolIterationContinues = *ovr.MaxToolIterationContinues
			}
			if ovr.MaxParallelToolCalls > 0 {
				rc.MaxParallelToolCalls = ovr.MaxParallelToolCalls
			}
			if ovr.Thinking != "" {
				rc.Thinking = ovr.Thinking
			}
			if ovr.PolicyPreset != "" {
				rc.PolicyPreset = ovr.PolicyPreset
			}
		}
		// Pull only the owner's user-scope provider rows (not the
		// owner's full merged view) so we don't re-apply system rows
		// over the viewer's already-merged set. Owner's user-scope
		// keys then sit between viewer's user-scope and the
		// agent-scope overlay below — same precedence the owner's
		// own UserSpace would have built.
		if ownerProvs, err := scope.UserScopeProviders(ctx, st, rec.UserID); err == nil {
			for k, v := range ownerProvs {
				if rc.Providers == nil {
					rc.Providers = make(map[string]config.ProviderConfig)
				}
				rc.Providers[k] = v
			}
		}
	}
	var agentModel string
	if applyOwnerOverlays {
		var ovr config.AgentDefaults
		// Agent-scope read through the resolver: blob or mirror, whichever
		// holds the row, and a disabled row means "no overlay here".
		if err := scope.ExactSetting(ctx, st, "agents.defaults", "", rc.ID, &ovr); err == nil {
			agentModel = ovr.Model
			if ovr.MaxTokens > 0 {
				rc.MaxTokens = ovr.MaxTokens
			}
			if ovr.Temperature > 0 {
				rc.Temperature = ovr.Temperature
			}
			if ovr.MaxToolIterations > 0 {
				rc.MaxToolIterations = ovr.MaxToolIterations
			}
			if ovr.MaxToolIterationContinues != nil {
				rc.MaxToolIterationContinues = *ovr.MaxToolIterationContinues
			}
			if ovr.MaxParallelToolCalls > 0 {
				rc.MaxParallelToolCalls = ovr.MaxParallelToolCalls
			}
			if ovr.Thinking != "" {
				rc.Thinking = ovr.Thinking
			}
			if ovr.PolicyPreset != "" {
				rc.PolicyPreset = ovr.PolicyPreset
			}
			// Keep this overlay aligned with the owner-path equivalent in
			// loadUserSpace — missing fields silently break per-agent
			// settings for chatters who lazy-attach the agent via a
			// channel binding (e.g. wechat multi-bubble hint never fires
			// because rc.SplitReplies stays nil; chatbot persona renders
			// in agent-prompt mode because rc.PromptMode stays "").
			if ovr.PromptMode != "" {
				rc.PromptMode = ovr.PromptMode
			}
			if ovr.SplitReplies != nil {
				v := *ovr.SplitReplies
				rc.SplitReplies = &v
			}
			if ovr.AutoPersist != nil {
				v := *ovr.AutoPersist
				rc.AutoPersist = &v
			}
		}
	}
	rc.Model = resolveModel(rc.Model, ownerModel, agentModel, chatterPin.Model)
	// Overlay agent-scope providers — sp.Config.Providers carries only
	// system+user rows (assembleConfig in loadUserSpace runs with
	// agentID=""). Without this overlay, providerForAgent can't see the
	// agent's own credentials and falls back to the shared provider,
	// firing the agent's chosen model id at the wrong base URL.
	//
	// Same gate as the agents.defaults overlay above: when a chatter
	// uses a foreign agent whose owner hasn't opted into sharing,
	// agent-scope provider rows stay private to the owner. The chatter
	// runs on whatever their own user-scope providers (plus system)
	// can offer.
	if applyOwnerOverlays {
		if agentProvs, err := scope.AgentScopeProviders(ctx, st, rc.ID); err == nil {
			for k, v := range agentProvs {
				if rc.Providers == nil {
					rc.Providers = make(map[string]config.ProviderConfig)
				}
				rc.Providers[k] = v
			}
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
	var entries map[string]config.SkillEntryCfg
	if err := scope.ExactSetting(ctx, st, "skills.entries", "", rc.ID, &entries); err == nil && len(entries) > 0 {
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
	// Independent of sandbox mode: hand the lazy-built agent the
	// coding-agent preview tools when a runtime is configured.
	if sp.ProjectRuntime != nil {
		if ag := sp.Agents.AgentByID(rc.ID); ag != nil {
			ag.SetProjectRuntime(sp.ProjectRuntime)
		}
	}
	// Provider-backed tool chains (web_search / web_fetch / image_gen /
	// tts). loadUserSpace wires these for every agent it builds; this
	// lazy-attach path used to skip the call entirely, so an agent
	// reached through a channel binding, a public link or an apikey
	// reported "Unknown tool: web_search" even though the owner's own
	// web chat could search with it — the chain config was in the DB
	// the whole time, just never read into a chain. Owner-scope rows
	// are layered in under the same isForeign gate as the provider
	// overlay above (tools follow the owner, like the model does).
	// The agent's own agent-scope rows and the chatter's per-agent rows
	// come from the same overlay builder loadUserSpace uses, so the two
	// attach paths can no longer disagree about which layers count.
	if ag := sp.Agents.AgentByID(rc.ID); ag != nil {
		toolCfg := toolConfigForAgent(ctx, st, sp.Config, sp.UserID, rc.ID, rec.UserID, isForeign && applyOwnerOverlays)
		registerAgentToolChains(toolCfg, []*agent.Agent{ag})
	}
	// Wire hook plugins onto the freshly-attached agent. Mirrors what
	// loadUserSpace does for owner agents — without this, hook
	// plugins would only fire for the agent's owner and never for
	// chatters who reach the agent through a foreign-attach (channel
	// binding, public link, super_admin browse).
	if sp.PluginMgr != nil {
		if ag := sp.Agents.AgentByID(rc.ID); ag != nil {
			registerHookPluginsForAgent(ctx, sp.PluginMgr, st, ag)
		}
	}
	slog.Info("agent injected into foreign user space",
		"caller", sp.UserID, "agent", rc.ID, "owner", rec.UserID)
	return nil
}

// managerOptions assembles the Manager options a user space's agents are built
// with — every rule that reaches an agent from here rather than from its own
// row. Named rather than inline in loadUserSpace for the same reason
// resolveModel is: a wiring line that exists only inside a 300-line loader has
// no witness, so nobody notices when it goes away. The piiScrubbing entry is
// the case that made this a function: it was missing entirely (the switch's
// only reader lived in agent.NewAgentWithFullCfg, a constructor with no
// callers), which made privacy.piiScrubbing.enabled a row an operator could
// turn on that did nothing at all. See
// internal/gateway/pii_scrub_cloudpath_e2e_test.go.
func managerOptions(cfg *config.Config, userID string, st store.Store, ws workspace.Store, meter usage.Meter, quotaStore usage.QuotaStore, notifyAgent func(userID, agentID string)) []agent.ManagerOption {
	opts := []agent.ManagerOption{
		agent.WithUserID(userID),
		agent.WithGlobalSkillsCfg(cfg.Skills),
		// cfg.Privacy is what assembleConfig read back from the "privacy"
		// namespace (system ← user ← agent scope). The Manager is what builds
		// — and hot-reloads — the agents, so it is the only layer that can
		// install the redaction on every provider an agent ends up holding.
		agent.WithPrivacy(cfg.Privacy),
		// cfg.SkillsLearner is the "skillsLearner" namespace (system ← user ←
		// agent scope). Same shape as the privacy entry above, and the same
		// failure if it were missing: a switch an operator can turn on whose
		// only reader was a constructor no production path calls. The Manager
		// builds the learner *after* the skill routing above it is wired, so
		// a learned SKILL.md goes through the one writer for `skills/` instead
		// of landing on a single pod's disk.
		agent.WithSkillsLearner(cfg.SkillsLearner),
		// cfg.Memory is the "memory" namespace (system ← user ← agent scope).
		// The per-agent autoPersist override is stamped over it inside the
		// constructor, so this line is what makes "no override" mean "inherit"
		// instead of "off" — and it is the only path for everyNTurns/model.
		agent.WithMemory(cfg.Memory),
		agent.WithSessionStore(session.NewStoreAdapter(st, userID)),
		agent.WithMemoryStore(agent.NewMemoryStoreAdapter(st)),
		agent.WithDataStore(st),
		// The cross-replica turn lease (docs/session-turn-integrity.md A1): admission
		// for every turn, so two replicas cannot run one session's history.
		agent.WithSessionLease(storeSessionLease{st: st}),
		agent.WithMCPConfigNotify(notifyAgent),
	}
	return opts
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
func loadUserSpace(ctx context.Context, userID string, mb *bus.MessageBus, st store.Store, ws workspace.Store, meter usage.Meter, quotaStore usage.QuotaStore, systemSandboxPool sandbox.ExecutorPool, pluginMgr *plugin.Manager, projectRuntime *coderuntime.Manager, notifyAgent func(userID, agentID string)) (*UserSpace, error) {
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
		entries = append(entries, config.AgentEntry{ID: ar.ID, UserID: ar.UserID, Name: ar.Name})
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
		if err := scope.ExactSetting(ctx, st, "agents.defaults", "", rc.ID, &agentOverride); err == nil {
			// Owner path: base is already system←user (ResolveAgents merged it
			// above), there is no owner row to layer (this IS the owner's own
			// space) and no viewer pin. So the agent row is the only overlay the
			// model can get — and it wins, which is the rule the settings page's
			// agent-context write depends on.
			rc.Model = resolveModel(rc.Model, "", agentOverride.Model, "")
			if agentOverride.MaxTokens > 0 {
				rc.MaxTokens = agentOverride.MaxTokens
			}
			if agentOverride.Temperature > 0 {
				rc.Temperature = agentOverride.Temperature
			}
			if agentOverride.MaxToolIterations > 0 {
				rc.MaxToolIterations = agentOverride.MaxToolIterations
			}
			if agentOverride.MaxToolIterationContinues != nil {
				rc.MaxToolIterationContinues = *agentOverride.MaxToolIterationContinues
			}
			if agentOverride.MaxParallelToolCalls > 0 {
				rc.MaxParallelToolCalls = agentOverride.MaxParallelToolCalls
			}
			if agentOverride.Thinking != "" {
				rc.Thinking = agentOverride.Thinking
			}
			if agentOverride.PolicyPreset != "" {
				rc.PolicyPreset = agentOverride.PolicyPreset
			}
			if agentOverride.PromptMode != "" {
				rc.PromptMode = agentOverride.PromptMode
			}
			// Per-agent WeChat split-replies — pointer semantics so
			// "absent" (no row, or row without the key) is distinct
			// from "explicitly false". Non-nil from the row means the
			// operator made a deliberate choice; nil falls through to
			// system WeChatCfg.SplitReplies later in NewAgentWithFullCfg.
			if agentOverride.SplitReplies != nil {
				v := *agentOverride.SplitReplies
				rc.SplitReplies = &v
			}
			// Per-agent autoPersist — same pointer semantics. Non-nil
			// here overrides the system/user memory.autoPersist.enabled
			// for this agent specifically. Used most by chatbot-mode
			// personas where the LLM can't write_file directly so the
			// background distill pass is the only persistence path.
			if agentOverride.AutoPersist != nil {
				v := *agentOverride.AutoPersist
				rc.AutoPersist = &v
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

	managerOpts := managerOptions(cfg, userID, st, ws, meter, quotaStore, notifyAgent)
	if ws != nil {
		managerOpts = append(managerOpts, agent.WithWorkspaceStore(ws))
	}
	if meter != nil {
		managerOpts = append(managerOpts, agent.WithMeter(meter))
	}
	if quotaStore != nil {
		managerOpts = append(managerOpts, agent.WithQuotaStore(quotaStore))
	}
	agentMgr, err := agent.NewManager(resolved, prov, mb, managerOpts...)
	if err != nil {
		return nil, fmt.Errorf("create agent manager for user %q: %w", userID, err)
	}

	// Tool chains are resolved per agent, not once for the whole space:
	// agent-scope rows written by `fastagent tools ... --agent X` are part
	// of the agent's effective config and were previously stored and then
	// ignored. Two batched queries cover every agent, and agents with no
	// overlay reuse `cfg` unchanged.
	agentIDs := make([]string, 0, len(agentMgr.All()))
	for _, ag := range agentMgr.All() {
		agentIDs = append(agentIDs, ag.Name())
	}
	agentToolOverlays := toolOverlaysForAgents(ctx, st, userID, agentIDs)
	for _, ag := range agentMgr.All() {
		registerAgentToolChains(applyToolOverlay(cfg, agentToolOverlays[ag.Name()]), []*agent.Agent{ag})
	}

	pool := attachSandboxToAgents(systemSandboxPool, userID, resolved, agentMgr)

	// Coding-agent runtime: hand every agent the preview tools. Nil when
	// no runtime is configured, in which case agents stay plain
	// assistants (no preview tools, per-chat file isolation unchanged).
	if projectRuntime != nil {
		for _, ag := range agentMgr.All() {
			ag.SetProjectRuntime(projectRuntime)
		}
	}

	// Wire hook plugins onto each agent's HookRegistry. Per-agent
	// enable comes from the configs row at (scope=agent, agent_id=X,
	// name=plugins.enabled) — falling back to the plugin manifest's
	// boot-time enabled state when there's no per-agent override.
	if pluginMgr != nil {
		for _, ag := range agentMgr.All() {
			registerHookPluginsForAgent(ctx, pluginMgr, st, ag)
		}
	}

	slog.Info("loaded user space", "user", userID, "agents", agentMgr.Names())

	return &UserSpace{
		UserID:         userID,
		Config:         cfg,
		Provider:       prov,
		Agents:         agentMgr,
		SandboxPool:    pool,
		PluginMgr:      pluginMgr,
		ProjectRuntime: projectRuntime,
	}, nil
}

// registerHookPluginsForAgent walks every running hook-type plugin
// and attaches it to ag.HookRegistry IF this agent has explicitly
// opted in via the per-agent plugins.enabled row.
//
// Default is OPT-IN: a plugin being enabled system-wide only means
// its process runs and is available to attach. Each agent must
// individually set `plugins.enabled[<id>] = true` (via the dashboard
// Plugins card or directly in the configs table) for the plugin's
// hooks to fire on its turns. System-wide enable without per-agent
// opt-in = plugin idle for that agent.
//
// Rationale: hook plugins can change agent behavior in surprising
// ways (extra messages, modified prompts, recorded conversation
// data). Default-deny avoids accidentally affecting agents the
// operator didn't intend.
//
// Idempotent at the manager level (Process is already running), but
// the HookRegistry side accumulates — call sites must not double-
// register for the same agent. Today the only call sites are
// loadUserSpace (once per UserSpace boot) and EnsureAgent (once per
// foreign attach), neither of which fires twice for the same agent.
func registerHookPluginsForAgent(ctx context.Context, pluginMgr *plugin.Manager, st store.Store, ag *agent.Agent) {
	overrides := readAgentScopePluginsEnabled(ctx, st, ag.Name())
	if len(overrides) == 0 {
		return // fast path: no opt-ins for this agent
	}
	for _, inst := range pluginMgr.HookPlugins() {
		id := inst.Manifest.ID
		// Opt-in: only attach if this agent explicitly set true.
		// Missing key or explicit false → skip.
		if !overrides[id] {
			continue
		}
		if inst.Process == nil || !inst.Process.IsRunning() {
			slog.Warn("plugin: agent opted in but plugin not running",
				"plugin", id, "agent", ag.Name())
			continue
		}
		if err := plugin.RegisterPluginHooks(ctx, pluginMgr, id, ag.HookRegistry(), ag.Name()); err != nil {
			slog.Warn("plugin: hook register failed",
				"plugin", id, "agent", ag.Name(), "error", err)
		}
	}
}

// readAgentScopePluginsEnabled reads the per-agent plugin enable
// overlay: agent scope, row name "plugins.enabled",
// data = {"<pluginID>": true|false, ...}. Missing row / missing key
// means "no override; use system default". Returns nil on lookup
// error (callers treat nil as "no overrides").
//
// The row lives under its own kind (store.KindPluginEnabled) rather than
// kind="setting": as a settings namespace its mirror keys collided with the
// "plugins" namespace's own "plugins.enabled" scalar. The read itself (blob
// first, mirror fallback) lives in scope so the HTTP layer and the runtime
// can't drift apart again.
func readAgentScopePluginsEnabled(ctx context.Context, st store.Store, agentID string) map[string]bool {
	if st == nil || agentID == "" {
		return nil
	}
	overrides, err := scope.AgentPluginEnabled(ctx, st, agentID)
	if err != nil {
		slog.Warn("agent plugin opt-in read failed", "agent", agentID, "error", err)
		return nil
	}
	return overrides
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
	mu     sync.RWMutex
	spaces map[string]*userSpaceEntry
	// retired holds dropped spaces that still owe a release. A drop is not
	// proof that nobody is using them — the write that drops a space can come
	// from inside a running turn (`mcp add` calls the reload notify from the
	// tool itself), and the MCP change path can drop the space that just
	// delivered the change — so release is deferred past releaseGrace and
	// refused while a turn is in flight (docs 10 §3.4, row 44).
	retired []retiredSpace
	// now is the clock the release sweep and the idle cutoff read. A field so
	// tests can age a space without sleeping.
	now               func() time.Time
	bus               *bus.MessageBus
	store             store.Store
	workspace         workspace.Store
	meter             usage.Meter
	quotaStore        usage.QuotaStore
	systemSandboxPool sandbox.ExecutorPool
	// pluginMgr is the shared (process-wide) plugin manager. Nil
	// when systemPlugins is disabled. Used by loadUserSpace and
	// EnsureAgent to register hook-type plugins onto each agent's
	// HookRegistry, gated by per-agent plugins.enabled config.
	pluginMgr *plugin.Manager
	idleTTL   time.Duration
	// projectRuntime is the coding-agent runtime manager, attached to
	// every agent at load time so they gain the preview tools. Nil unless
	// the gateway was given one via SetProjectRuntime; set after
	// construction (the manager is built later in boot than the
	// registry), hence the mutable field + mutex rather than a ctor arg.
	projectRuntime *coderuntime.Manager
	// notifyAgent is the gateway's per-agent reload notify (invalidate
	// local UserSpaces + DB epoch + Redis broadcast). Passed into every
	// agent.Manager so in-session `mcp add/remove` config writes take
	// effect across replicas.
	notifyAgent func(userID, agentID string)
}

// setProjectRuntime records the manager so subsequent loadUserSpace calls
// attach it. Guarded by mu since it races with concurrent getOrLoad.
func (r *userSpaceRegistry) setProjectRuntime(m *coderuntime.Manager) {
	r.mu.Lock()
	r.projectRuntime = m
	r.mu.Unlock()
}

// setSystemSandboxPool swaps the gateway-owned pool reference used for
// future UserSpace loads, then drops already-loaded spaces so active agents
// reattach to the new pool on their next request.
func (r *userSpaceRegistry) setSystemSandboxPool(p sandbox.ExecutorPool) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.systemSandboxPool = p
	evicted := len(r.spaces)
	for uid, e := range r.spaces {
		r.retireLocked(uid, e)
	}
	r.spaces = make(map[string]*userSpaceEntry)
	return evicted
}

type userSpaceEntry struct {
	space    *UserSpace
	lastUsed time.Time
}

// releaseGrace is how long a dropped user space keeps its MCP clients before
// the sweep takes them back. It exists to cover the two writes that drop a
// space from inside a turn — `mcp add` / `mcp remove` and a server's
// "my tool list changed" notification — because cutting a running turn's MCP
// clients mid-call is a user-visible failure, while a stream that stops is not.
const releaseGrace = 5 * time.Minute

// retiredSpace is a dropped space waiting for its release.
type retiredSpace struct {
	space *UserSpace
	at    time.Time
}

// Close releases the resources the space owns. The MCP clients are the ones
// with a lifetime longer than the space itself: a stdio server is a subprocess
// and a standing notification stream is a goroutine plus a connection, and
// neither is handed back by dropping the Go object that points at it.
func (sp *UserSpace) Close() {
	if sp == nil || sp.Agents == nil {
		return
	}
	sp.Agents.CloseMCPClients()
}

// AnyTurnInFlight reports whether any agent of this space has a turn running or
// waiting for the slot — i.e. whether an MCP client of it may be in use.
func (sp *UserSpace) AnyTurnInFlight() bool {
	if sp == nil || sp.Agents == nil {
		return false
	}
	return sp.Agents.AnyTurnInFlight()
}

func newUserSpaceRegistry(mb *bus.MessageBus, st store.Store, ws workspace.Store, meter usage.Meter, quotaStore usage.QuotaStore, systemSandboxPool sandbox.ExecutorPool, pluginMgr *plugin.Manager, notifyAgent func(userID, agentID string)) *userSpaceRegistry {
	return &userSpaceRegistry{
		spaces:            make(map[string]*userSpaceEntry),
		now:               time.Now,
		bus:               mb,
		store:             st,
		workspace:         ws,
		meter:             meter,
		quotaStore:        quotaStore,
		systemSandboxPool: systemSandboxPool,
		pluginMgr:         pluginMgr,
		notifyAgent:       notifyAgent,
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
	sp, err := loadUserSpace(ctx, userID, r.bus, r.store, r.workspace, r.meter, r.quotaStore, r.systemSandboxPool, r.pluginMgr, r.projectRuntime, r.notifyAgent)
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
	if e, ok := r.spaces[userID]; ok {
		r.retireLocked(userID, e)
	}
	r.mu.Unlock()
}

// retireLocked takes an entry out of the map and hands its space to the release
// list. Callers must hold r.mu.
func (r *userSpaceRegistry) retireLocked(userID string, e *userSpaceEntry) {
	if e == nil || e.space == nil {
		return
	}
	delete(r.spaces, userID)
	r.retired = append(r.retired, retiredSpace{space: e.space, at: r.clock()})
}

// clock reads the registry's clock, defaulting to the real one so a zero-value
// registry (tests) cannot panic.
func (r *userSpaceRegistry) clock() time.Time {
	if r.now == nil {
		return time.Now()
	}
	return r.now()
}

// releaseRetired closes the spaces that have been retired for at least
// releaseGrace and have no turn in flight, and returns how many it closed. The
// turn check is what makes the release safe for a long turn: a turn is the only
// thing that can be calling an MCP tool, so while one runs the clients stay.
func (r *userSpaceRegistry) releaseRetired() int {
	r.mu.Lock()
	now := r.clock()
	keep := r.retired[:0]
	var due []*UserSpace
	for _, rs := range r.retired {
		if now.Sub(rs.at) < releaseGrace || rs.space.AnyTurnInFlight() {
			keep = append(keep, rs)
			continue
		}
		due = append(due, rs.space)
	}
	r.retired = keep
	r.mu.Unlock()

	for _, sp := range due {
		sp.Close()
	}
	if len(due) > 0 {
		slog.Info("released the MCP clients of dropped user spaces",
			"released", len(due), "stillWaiting", len(keep))
	}
	return len(due)
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
	cutoff := r.clock().Add(-r.idleTTL)
	evicted := 0
	for uid, e := range r.spaces {
		if e.lastUsed.Before(cutoff) {
			r.retireLocked(uid, e)
			evicted++
			slog.Info("evicted idle user space", "user", uid,
				"idle", r.clock().Sub(e.lastUsed).Round(time.Second))
		}
	}
	return evicted
}

func (r *userSpaceRegistry) startEvictor(ctx context.Context) {
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
			// The release sweep runs even when the idle TTL is disabled: a
			// space retired by a config write still owes its clients back.
			r.releaseRetired()
			r.sweepStalledGoals(ctx)
		}
	}
}

// goalWatchdogStaleAfter is how long a goal may go untouched before the sweep re-fires its
// continuation. Ten minutes is well past a normal turn's cadence (production goal turns ran
// 2–3 minutes apart) and well short of the 86 minutes of silence the first stall cost.
const goalWatchdogStaleAfter = 10 * time.Minute

// sweepStalledGoals gives every live user space one pass of the goal watchdog
// (agent.Manager.SweepStalledGoals): a goal whose chain went quiet — a killed pod, a lost event, a
// failed state write — gets its next continuation without waiting for the user to type something.
func (r *userSpaceRegistry) sweepStalledGoals(ctx context.Context) {
	r.mu.RLock()
	spaces := make([]*UserSpace, 0, len(r.spaces))
	for _, e := range r.spaces {
		if e.space != nil && e.space.Agents != nil {
			spaces = append(spaces, e.space)
		}
	}
	r.mu.RUnlock()
	for _, sp := range spaces {
		sp.Agents.SweepStalledGoals(ctx, goalWatchdogStaleAfter)
	}
}

// bindingsFromChannelRows synthesizes the (channel, accountID) →
// agentID routing table from channel rows themselves. It replaces the
// old kind=setting/name=bindings indirection: every channel row whose
// agent_id points at an agent this user can route contributes a Binding
// per Account.
//
// Pulls rows from three ownership corners this user can route:
//   - (user_id=”, agent_id=Y): the agent's "official" rows for any
//     agent Y the user owns (legacy / pre-refactor data)
//   - (user_id=userID, agent_id=Y) where user owns Y: this user's
//     bindings on their own agent (the normal post-refactor pattern)
//   - (user_id=userID, agent_id=Z) where user does NOT own Z: this
//     user authored a channel overlay on a foreign agent (e.g. a
//     chatter binding their WeChat bot to a public agent). Without
//     this reverse-lookup, resolveChannelOwner correctly routes
//     inbound DMs to the binder's UserSpace but matchAgent finds an
//     empty Bindings list because the agent isn't in ListAgents(userID).
//     The matchAgent path then lazy-attaches the foreign agent via
//     ensureForeignAgent on first match.
//
// Granted-agent bindings without an explicit channel-row overlay stay
// outside — they live in the agent owner's space, not every grantee's.
func bindingsFromChannelRows(ctx context.Context, st store.Store, userID string, agents []store.AgentRecord) []config.Binding {
	if st == nil {
		return nil
	}
	var out []config.Binding
	covered := make(map[string]bool, len(agents))

	// Try the new channels table first — build bindings from ChannelRecords.
	hasNewRows := false
	for _, ar := range agents {
		covered[ar.ID] = true
		if chRows, err := st.ListChannels(ctx, "", ar.ID); err == nil && len(chRows) > 0 {
			hasNewRows = true
			out = append(out, expandChannelRecordBindings(chRows, ar.ID)...)
		}
		if userID != "" {
			if chRows, err := st.ListChannels(ctx, userID, ar.ID); err == nil && len(chRows) > 0 {
				hasNewRows = true
				out = append(out, expandChannelRecordBindings(chRows, ar.ID)...)
			}
		}
	}
	// Reverse-lookup from the new table: any channel this user bound
	// to an agent they don't own.
	if userID != "" {
		if allUserCh, err := st.ListAllChannels(ctx); err == nil {
			for _, ch := range allUserCh {
				if ch.UserID != userID || ch.AgentID == "" || covered[ch.AgentID] {
					continue
				}
				hasNewRows = true
				out = append(out, expandChannelRecordBindings([]store.ChannelRecord{ch}, ch.AgentID)...)
			}
		}
	}
	if hasNewRows {
		return out
	}

	// Fallback: read from configs for pre-migration installs.
	for _, ar := range agents {
		rows, err := scope.RowsAt(ctx, st, store.KindChannel, "", ar.ID)
		if err == nil {
			out = append(out, expandChannelBindings(rows, ar.ID)...)
		}
		if userID != "" {
			rows, err := scope.RowsAt(ctx, st, store.KindChannel, userID, ar.ID)
			if err == nil {
				out = append(out, expandChannelBindings(rows, ar.ID)...)
			}
		}
	}
	// Reverse-lookup: any channel row this user wrote against an agent
	// they don't own. matchAgent will lazy-attach the agent on first hit.
	if userID != "" {
		foreignRows, err := st.ListConfigsByUser(ctx, store.KindChannel, userID)
		if err == nil {
			for i := range foreignRows {
				rec := foreignRows[i]
				if rec.AgentID == "" || covered[rec.AgentID] {
					continue
				}
				out = append(out, expandChannelBindings([]store.ConfigRecord{rec}, rec.AgentID)...)
			}
		}
	}
	return out
}

// expandChannelRecordBindings builds Binding entries from ChannelRecord rows.
func expandChannelRecordBindings(rows []store.ChannelRecord, agentID string) []config.Binding {
	var out []config.Binding
	for _, r := range rows {
		if !r.Enabled {
			continue
		}
		cc := config.ChannelConfig{}
		if blob, err := json.Marshal(r.Data); err == nil {
			_ = json.Unmarshal(blob, &cc)
		}
		// Each ChannelRecord is one (type, account_id) — but the Data
		// blob may still carry an Accounts map from the migration. Use
		// the top-level AccountID as the primary binding key.
		if len(cc.Accounts) == 0 {
			out = append(out, config.Binding{
				AgentID: agentID,
				Match:   config.Match{Channel: r.Type, AccountID: r.AccountID},
			})
			continue
		}
		for accountID := range cc.Accounts {
			out = append(out, config.Binding{
				AgentID: agentID,
				Match:   config.Match{Channel: r.Type, AccountID: accountID},
			})
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
