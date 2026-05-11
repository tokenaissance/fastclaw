package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/codeany-ai/open-agent-sdk-go/costtracker"

	"github.com/fastclaw-ai/fastclaw/internal/agent/tools"
	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/mcp"
	"github.com/fastclaw-ai/fastclaw/internal/privacy"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
	"github.com/fastclaw-ai/fastclaw/internal/sandbox"
	"github.com/fastclaw-ai/fastclaw/internal/session"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/toolproviders"
	"github.com/fastclaw-ai/fastclaw/internal/workspace"
)

// Agent is the ReAct agent loop.
type Agent struct {
	name              string
	provider          provider.Provider
	registry          *tools.Registry
	sessions          *session.Manager
	memory            *Memory
	ctxBuilder        *ContextBuilder
	mcpMgr            *mcp.Manager
	hooks             *HookRegistry
	model                string
	maxTokens            int
	temperature          float64
	maxToolIterations    int
	maxParallelToolCalls int // 0 = unlimited
	thinking             string
	homePath          string // agent's home: SOUL.md, sessions, memory, skills
	workspacePath     string // working dir where agent creates user files
	homeDir           string // FastClaw root, ~/.fastclaw
	ownerUserID       string // the user that owns this agent (for hook namespacing)
	skillsCfg         config.SkillsConfig
	globalSkillsCfg   config.SkillsCfg
	messageBus        *bus.MessageBus
	subAgentSpawner   tools.SubAgentSpawner
	ftsStore          *store.FTSStore
	piiScrubEnabled   bool
	memoryCfg         config.MemoryCfg
	// memoryStore is the optional Store-backed source of identity files
	// (SOUL.md, IDENTITY.md, ...). Kept on the Agent so ReloadWorkspaceFiles
	// can rewire a fresh ContextBuilder to keep reading from the Store
	// instead of silently falling back to pod-local filesystem.
	memoryStore       MemoryStore
	// workspaceStore is optional; when set, SkillsLoader hydrates per-agent
	// and global skill dirs from the object store on every turn so skills
	// uploaded post-boot or on a sibling replica become visible here.
	workspaceStore workspace.Store
	skillsLearner  *SkillsLearner
	turnCount      int
	engine         *sdkEngine
	costTracker    *costtracker.Tracker
	agentID        string
	// sandboxPool is the per-user (agent + session) sandbox pool. Set
	// once at boot/hot-reload by attachSandboxToAgents; bindSession
	// pulls a session-scoped executor from it at the top of every turn
	// so concurrent sessions of the same agent get isolated containers
	// + isolated /workspace mounts.
	sandboxPool sandbox.ExecutorPool
}

// SetSandboxPool wires the per-(agent,session) executor pool. Called by
// attachSandboxToAgents on boot and by hot-reload's reloadSandbox after
// onboarding flips sandbox on. The pool is consulted by bindSession at
// the start of every chat turn — there's no eager Get at boot anymore
// because session IDs only exist once a chat starts.
//
// Also flips the context builder's sandbox flag so the system prompt's
// "Working Directory" / filesystem-layout description matches reality.
// Without this, an agent whose rc.Sandbox.Enabled=false but who got a
// pool reference (attachSandboxToAgents wires the pool to ALL agents
// once any one of them wants sandbox) ends up with exec routed through
// the container while the prompt still advertises host paths — model
// dutifully writes `/Users/.../workspaces/<id>/foo` which 404s inside
// the container. The two states must agree.
func (a *Agent) SetSandboxPool(p sandbox.ExecutorPool) {
	a.sandboxPool = p
	if a.ctxBuilder != nil {
		a.ctxBuilder.sandboxEnabled = p != nil
	}
	// Tell the tool registry sandbox is required so its host-shell exec
	// fallback refuses to run when bindSession can't bind an executor.
	// The two states (system prompt advertising /workspace + /skills,
	// exec actually using sandbox) must agree — without this, a Docker
	// daemon hiccup turns into "sh: python: command not found" on the
	// host instead of a clear "sandbox required but unavailable" error.
	if a.registry != nil {
		a.registry.SetSandboxRequired(p != nil)
	}
}

// bindSession wires per-turn session state into the tool registry: the
// session-scoped sandbox executor (when a pool is configured), the
// sessionID workspace.Store calls use to namespace artifacts, and the
// (channel, chatID) bus address so deferred-work tools (create_cron_job)
// can stamp it onto persisted rows for later replay. Called at the top
// of HandleMessage / HandleMessageStream before any tool runs.
//
// Mutating the shared registry across concurrent chats would race, but
// the current invariant is one chat-in-flight per agent — the gateway
// serializes per-agent turns. Documenting it here in case that changes.
func (a *Agent) bindSession(ctx context.Context, channel, sessionID, projectID string) {
	a.registry.SetSessionID(sessionID)
	a.registry.SetProjectID(projectID)
	a.registry.SetMessageContext(channel, sessionID)
	if a.sandboxPool == nil {
		return
	}
	ex, err := a.sandboxPool.Get(ctx, a.name, projectID, sessionID)
	if err != nil {
		// Error level (not warn) — when sandbox is required and we
		// can't bind, the next exec call will refuse with the
		// "sandboxRequired but no executor" message; log here so the
		// upstream cause (docker daemon down, image pull failed, …) is
		// captured next to the user-facing error.
		slog.Error("sandbox executor unavailable; exec will refuse host fallback",
			"agent", a.name, "session", sessionID, "error", err)
		return
	}
	a.registry.SetExecutor(ex)
}

// NewAgent creates a new Agent from a resolved config.
func NewAgent(rc config.ResolvedAgent, prov provider.Provider, mb *bus.MessageBus, homeDir string) *Agent {
	return NewAgentWithSkillsCfg(rc, prov, mb, homeDir, config.SkillsCfg{})
}

// NewAgentWithFullCfg creates a new Agent with full config support (memory, privacy, skills learner).
func NewAgentWithFullCfg(rc config.ResolvedAgent, prov provider.Provider, mb *bus.MessageBus, homeDir string, fullCfg *config.Config) *Agent {
	ag := NewAgentWithSkillsCfg(rc, prov, mb, homeDir, fullCfg.Skills)
	ag.memoryCfg = fullCfg.Memory
	ag.piiScrubEnabled = fullCfg.Privacy.PIIScrubbing.Enabled

	// Set up FTS store if configured
	if fullCfg.Memory.FTS.Enabled {
		dbPath := fullCfg.Memory.FTS.DBPath
		if dbPath == "" {
			dbPath = rc.Home + "/memory/fts.db"
		}
		if fts, err := store.NewFTSStore(dbPath); err == nil {
			if err := fts.Init(); err == nil {
				ag.ftsStore = fts
				slog.Info("FTS5 search enabled", "agent", rc.ID, "db", dbPath)
			} else {
				slog.Warn("FTS5 init failed, falling back to file scan", "error", err)
			}
		} else {
			slog.Warn("FTS5 store open failed, falling back to file scan", "error", err)
		}
	}

	// Set up skills learner if configured
	if fullCfg.SkillsLearner.Enabled {
		model := fullCfg.SkillsLearner.Model
		if model == "" {
			model = rc.Model
		}
		learnerLoader := NewSkillsLoaderWithGlobal(homeDir, rc.Home, "", rc.Skills, fullCfg.Skills)
		learnerLoader.agentID = rc.ID
		ag.skillsLearner = NewSkillsLearner(rc.Home, prov, model, learnerLoader.AllSkillDirs()...)
		if fullCfg.SkillsLearner.MinToolCalls > 0 {
			ag.skillsLearner.minToolCalls = fullCfg.SkillsLearner.MinToolCalls
		}
	}

	// Set memory auto-persist defaults
	if ag.memoryCfg.AutoPersist.EveryNTurns == 0 {
		ag.memoryCfg.AutoPersist.EveryNTurns = 5
	}

	return ag
}

// NewAgentWithSkillsCfg creates a new Agent with global skills config for env injection.
func NewAgentWithSkillsCfg(rc config.ResolvedAgent, prov provider.Provider, mb *bus.MessageBus, homeDir string, globalSkillsCfg config.SkillsCfg) *Agent {
	workspace := rc.Workspace
	if workspace == "" {
		// Fallback for callers (tests, legacy configs) that don't populate
		// Workspace — use the agent's home as a single-dir fallback.
		workspace = rc.Home
	}
	// Ensure the workspace dir exists so the first write_file doesn't fail.
	if workspace != "" {
		_ = os.MkdirAll(workspace, 0o755)
	}

	memory := NewMemory(rc.Home)
	registry := tools.NewRegistry(rc.Home, workspace)
	tools.RegisterMessage(registry, mb)
	tools.RegisterMemorySearch(registry, rc.Home)
	tools.RegisterWebFetch(registry)

	// Load skills with OpenClaw compatibility. We can't hydrate from OSS
	// here — the Agent isn't constructed yet and the manager hasn't wired
	// workspaceStore. The manager will call ReloadWorkspaceFiles after
	// wiring to refresh the summary with OSS-hosted skills, and runOnce
	// re-hydrates on every turn to pick up later uploads.
	loader := NewSkillsLoaderWithGlobal(homeDir, rc.Home, "", rc.Skills, globalSkillsCfg)
	loader.agentID = rc.ID
	skills := loader.LoadSkills()
	skillsSummary := loader.BuildSkillsSummary(skills)

	// Set up skill env injection for exec tool. Pass an sbCfg carrying
	// just the Enabled flag so the host-mode closure (used until
	// bindSession swaps in a sandboxed executor on session start) knows
	// sandbox was REQUIRED for this agent — without that signal an
	// executor-pool failure would silently fall through to /bin/sh on the
	// host, defeating the security boundary the user asked for.
	skillDirs := loader.AllSkillDirs()
	var sbCfg *tools.SandboxConfig
	if rc.Sandbox.Enabled {
		sbCfg = &tools.SandboxConfig{Enabled: true}
	}
	tools.RegisterExecWithSkillEnv(registry, sbCfg, loader.SkillEnvVars, skillDirs)

	if len(skills) > 0 {
		slog.Info("loaded skills", "agent", rc.ID, "count", len(skills))
	}

	// Set up hooks with logging
	hooks := NewHookRegistry()
	hooks.Register(BeforeModelCall, LoggingHook())
	hooks.Register(AfterModelCall, LoggingHook())
	hooks.Register(BeforeToolCall, LoggingHook())
	hooks.Register(AfterToolCall, LoggingHook())

	eng := newSDKEngine(rc.ID)

	ag := &Agent{
		name:              rc.ID,
		provider:          prov,
		registry:          registry,
		sessions:          session.NewManager(rc.Home + "/sessions"),
		memory:            memory,
		ctxBuilder:        newContextBuilderWithSandbox(rc.Home, workspace, memory, skillsSummary, rc.Thinking, rc.Sandbox.Enabled, rc.Sandbox.Backend),
		hooks:             hooks,
		model:                rc.Model,
		maxTokens:            rc.MaxTokens,
		temperature:          rc.Temperature,
		maxToolIterations:    rc.MaxToolIterations,
		maxParallelToolCalls: rc.MaxParallelToolCalls,
		thinking:             rc.Thinking,
		homePath:          rc.Home,
		workspacePath:     workspace,
		homeDir:           homeDir,
		skillsCfg:         rc.Skills,
		globalSkillsCfg:   globalSkillsCfg,
		messageBus:        mb,
		engine:            eng,
		costTracker:       eng.costTracker,
	}


	// Connect MCP servers and register their tools
	if len(rc.MCPServers) > 0 {
		mcpMgr := mcp.NewManager(rc.MCPServers)
		ag.mcpMgr = mcpMgr

		for _, td := range mcpMgr.ToolDefs() {
			toolName := td.Name
			ag.registry.Register(toolName, td.Description, td.InputSchema,
				func(ctx context.Context, args json.RawMessage) (string, error) {
					return mcpMgr.CallTool(ctx, toolName, args)
				},
			)
		}

		if mcpMgr.HasTools() {
			slog.Info("registered MCP tools", "agent", rc.ID)
		}
	}

	return ag
}

func newContextBuilderWithThinking(home string, memory *Memory, skillsSummary string, thinking string) *ContextBuilder {
	cb := NewContextBuilder(home, memory, skillsSummary)
	if thinking != "" {
		cb.SetThinking(thinking)
	}
	return cb
}

func newContextBuilderWithSandbox(home, workspace string, memory *Memory, skillsSummary string, thinking string, sandboxEnabled bool, sandboxBackend string) *ContextBuilder {
	cb := newContextBuilderWithThinking(home, memory, skillsSummary, thinking)
	cb.SetWorkspace(workspace)
	cb.sandboxEnabled = sandboxEnabled
	cb.sandboxBackend = sandboxBackend
	return cb
}

// Name returns the agent's name.
func (a *Agent) Name() string {
	return a.name
}

// HandleWebChat handles a chat message from the web UI with a session ID.
// imageURLs and params mirror the streaming variant so non-streaming
// callers (third-party apps hitting POST /api/chat) get the same
// vision + per-turn-params support as the SSE path.
//
// projectIDHint is the chat's "owning project" as carried in the URL
// (`?project=<pid>`) or chat request body. It only matters on the very
// first turn of a brand-new session: once the row exists, project_id
// stamped on it is authoritative and the hint is ignored.
func (a *Agent) HandleWebChat(ctx context.Context, sessionId, projectIDHint, userID, text string, imageURLs []string, params map[string]any) string {
	if sessionId == "" {
		sessionId = "web-ui"
	}
	if userID == "" {
		// Backward compat for unauth'd / legacy callers: keep the
		// sentinel so the per-user skills mount lands at a stable shared
		// dir instead of trying to mkdir <base>/users//skills/ (which
		// docker would happily mount over the user's whole home dir).
		userID = "web-user"
	}
	channel, accountID, chatID, projectID := a.recoverWebTriple(sessionId)
	if projectID == "" {
		projectID = projectIDHint
	}
	msg := bus.InboundMessage{
		Channel:   channel,
		AccountID: accountID,
		ChatID:    chatID,
		ProjectID: projectID,
		UserID:    userID,
		Text:      text,
		PeerKind:  "dm",
		PhotoURLs: imageURLs,
		Params:    params,
	}
	return a.HandleMessage(ctx, msg)
}

// HandleWebChatStream handles a web chat message with real-time event streaming.
// imageURLs carries any user-attached images (data URLs or fetchable HTTPS
// links) so vision-capable models receive them as image_url content parts on
// the user message. projectIDHint mirrors HandleWebChat's parameter — see
// that doc.
func (a *Agent) HandleWebChatStream(ctx context.Context, sessionId, projectIDHint, userID, text string, imageURLs []string, params map[string]any, events chan<- ChatEvent) string {
	if sessionId == "" {
		sessionId = "web-ui"
	}
	if userID == "" {
		userID = "web-user"
	}
	ctx = ContextWithChatEvents(ctx, events)
	channel, accountID, chatID, projectID := a.recoverWebTriple(sessionId)
	if projectID == "" {
		projectID = projectIDHint
	}
	msg := bus.InboundMessage{
		Channel:   channel,
		AccountID: accountID,
		ChatID:    chatID,
		ProjectID: projectID,
		UserID:    userID,
		Text:      text,
		PeerKind:  "dm",
		PhotoURLs: imageURLs,
		Params:    params,
	}
	return a.HandleMessage(ctx, msg)
}

// recoverWebTriple maps a URL `?session=` token (which can be a
// session_key for any channel, OR a legacy web chat_id) to the full
// (channel, accountID, chatID, projectID) tuple downstream callers
// need.
//
// Without recovering accountID too, an inbound web write to a
// telegram/wechat session would query Manager.Get(channel, "", chatID),
// miss the existing row (which has account_id=<bot_id>), and mint a
// brand-new session under the wrong triple — the user sees the reply
// briefly, but a refresh loads the original session's history and the
// just-written exchange vanishes.
//
// projectID is "" for loose chats and forwarded onto the inbound
// message so bindSession routes the sandbox + workspace.Store to the
// project folder.
//
// Two-step recovery:
//  1. If the token matches a session_key → look up the full triple +
//     project.
//  2. Otherwise treat it as a web chat_id (preserves the brand-new
//     "+New chat" path where the row doesn't exist yet).
func (a *Agent) recoverWebTriple(sessionId string) (channel, accountID, chatID, projectID string) {
	channel, accountID, chatID = "web", "", sessionId
	if !a.sessions.SessionExists(sessionId) {
		return
	}
	if c, acc, ci, err := a.sessions.LookupSessionTriple(sessionId); err == nil && (c != "" || ci != "") {
		channel = c
		if channel == "" {
			channel = "web"
		}
		if ci != "" {
			chatID = ci
		}
		accountID = acc
	}
	projectID = a.sessions.LookupSessionProject(sessionId)
	return
}

// home returns the agent's home (metadata) directory path.
func (a *Agent) home() string {
	return a.homePath
}

// SetGroupContext configures group chat awareness for this agent's system prompt.
func (a *Agent) SetGroupContext(gc *GroupContext) {
	a.ctxBuilder.SetGroupContext(gc)
}

// InjectGroupMessage appends a message from another bot into the session history
// without triggering an LLM call. This gives the agent awareness of what other
// bots said in the group chat.
func (a *Agent) InjectGroupMessage(ctx context.Context, msg bus.InboundMessage) {
	sess := a.sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID)
	label := msg.SenderName
	if label == "" {
		label = "Bot"
	}
	content := fmt.Sprintf("[%s]: %s", label, msg.Text)
	sess.Append(provider.Message{Role: "user", Content: content})
}

// SetSubAgentSpawner sets the sub-agent spawner for the spawn_subagent tool.
func (a *Agent) SetSubAgentSpawner(spawner tools.SubAgentSpawner) {
	a.subAgentSpawner = spawner
	tools.RegisterSubAgent(a.registry, spawner, a.name)
}

// ToolRegistry returns the agent's tool registry for external registration.
func (a *Agent) ToolRegistry() *tools.Registry {
	return a.registry
}

// SetOwnerUserID tags this agent with the owning user ID. The value is
// propagated into every HookContext so plugins like mem0 can namespace
// data per user.
func (a *Agent) SetOwnerUserID(uid string) {
	a.ownerUserID = uid
}

// HookRegistry returns the agent's hook registry for external hook registration.
func (a *Agent) HookRegistry() *HookRegistry {
	return a.hooks
}

// RegisterWebSearchChain exposes the web_search tool to this agent using a
// provider chain (primary + fallbacks). Pass nil to skip — the tool won't
// appear in the agent's tool list, so the model can't try to call it.
func (a *Agent) RegisterWebSearchChain(chain *toolproviders.Chain) {
	tools.RegisterWebSearchChain(a.registry, chain)
}

// RegisterImageGenChain exposes the image_gen tool to this agent.
func (a *Agent) RegisterImageGenChain(chain *toolproviders.Chain) {
	tools.RegisterImageGenChain(a.registry, chain)
}

// RegisterTTSChain exposes the tts tool to this agent.
func (a *Agent) RegisterTTSChain(chain *toolproviders.Chain) {
	tools.RegisterTTSChain(a.registry, chain)
}

// Sessions returns the session manager for this agent.
func (a *Agent) Sessions() *session.Manager {
	return a.sessions
}

// WebChatHistory returns chat history for a specific session — the
// name is historical; it now serves any channel because the dashboard
// surfaces all-channel chats in the sidebar.
//
// Reads from the append-only session_messages archive (via
// Session.ArchivedMessages) instead of the in-memory working set, so
// post-compaction sessions show the original conversation rather than a
// summary + last 20 turns. Falls back to the working set when no
// archive is available (file-backed mode or pre-archive sessions).
//
// sessionId may be either a canonical session_key (what
// ListWebSessions returns) or a legacy web chat_id from older URLs;
// ResolveSessionKey untangles them.
func (a *Agent) WebChatHistory(sessionId string) []map[string]any {
	if sessionId == "" {
		sessionId = "web-ui"
	}
	resolved := a.sessions.ResolveSessionKey(sessionId)
	sess := a.sessions.GetByKey(resolved)
	msgs := sess.ArchivedMessages()
	var history []map[string]any
	for _, m := range msgs {
		switch m.Role {
		case "user":
			// Multimodal user turns store text inside ContentParts and
			// leave Content empty (see HandleMessageStream's image
			// attachment path). Surface both shapes here:
			//   - text (Content fallback to joined text parts)
			//   - imageUrls (image_url parts) so the chat UI can render
			//     image thumbnails on bubbles loaded from history, not
			//     just on the live in-flight bubble.
			text := m.TextContent()
			var imageURLs []string
			for _, p := range m.ContentParts {
				if p.Type == "image_url" && p.ImageURL != nil && p.ImageURL.URL != "" {
					imageURLs = append(imageURLs, p.ImageURL.URL)
				}
			}
			if text == "" && len(imageURLs) == 0 {
				continue
			}
			entry := map[string]any{"role": "user", "content": text}
			if len(imageURLs) > 0 {
				entry["imageUrls"] = imageURLs
			}
			history = append(history, entry)
		case "assistant":
			entry := map[string]any{"role": "assistant"}
			if m.Content != "" {
				entry["content"] = m.Content
			}
			if len(m.ToolCalls) > 0 {
				var calls []map[string]string
				for _, tc := range m.ToolCalls {
					calls = append(calls, map[string]string{
						"id":        tc.ID,
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
					})
				}
				entry["toolCalls"] = calls
			}
			// Surface persisted assistant-side metadata so the UI can
			// re-render iteration-cap badges, etc. on history reload —
			// without this, the badge only ever showed on the live turn.
			if len(m.Metadata) > 0 {
				entry["metadata"] = m.Metadata
			}
			// Skip empty assistant messages (no content, no tool calls)
			if m.Content == "" && len(m.ToolCalls) == 0 {
				continue
			}
			history = append(history, entry)
		case "tool":
			entry := map[string]any{
				"role":       "tool",
				"content":    m.Content,
				"name":       m.Name,
				"toolCallId": m.ToolCallID,
			}
			if len(m.Metadata) > 0 {
				entry["metadata"] = m.Metadata
			}
			history = append(history, entry)
		}
	}
	return history
}

// WebChatSessions returns a list of web chat sessions with metadata.
func (a *Agent) WebChatSessions() []session.WebSession {
	return a.sessions.ListWebSessions()
}

// DeleteWebChatSession removes a chat session (any channel) by the URL
// token — accepts either session_key or legacy web chat_id.
func (a *Agent) DeleteWebChatSession(sessionId string) error {
	return a.sessions.DeleteSessionByID(sessionId)
}

// RenameWebChatSession sets a custom title for a chat session (any
// channel) by the URL token.
func (a *Agent) RenameWebChatSession(sessionId, title string) error {
	return a.sessions.RenameSessionByID(sessionId, title)
}

// MoveWebChatSession reassigns a chat to a different project (or
// detaches it when projectID is "") and migrates its workspace files
// from the old scope to the new one. Drives the sidebar drag-and-drop
// affordance.
//
// Order matters:
//  1. Resolve the URL token to the canonical session_key.
//  2. Read the current project_id so we know the source workspace
//     scope (loose chat = sessions/<sid>/, project chat =
//     projects/<oldPid>/<sid>/).
//  3. Release any live sandbox bound to this chat — leaving it up
//     would keep the old bind-mount referenced and the new mount
//     wouldn't take effect until eviction. Released proactively so
//     the next turn cold-starts at the new path.
//  4. Move workspace files (no-op when the source dir is empty).
//  5. Flip sessions.project_id in the store and drop the in-memory
//     Session cache so the next Get re-reads the row.
//
// Steps 4 and 5 are not atomic: a crash between them leaves the row
// pointing at the new project but files at the old path (or vice
// versa). The pending follow-up move is idempotent — re-running this
// method finishes the migration cleanly.
func (a *Agent) MoveWebChatSession(ctx context.Context, sessionId, projectID string) error {
	key := a.sessions.ResolveSessionKey(sessionId)
	if key == "" {
		return fmt.Errorf("session not found: %s", sessionId)
	}
	oldProject := a.sessions.LookupSessionProject(key)
	if oldProject == projectID {
		return nil
	}
	if a.sandboxPool != nil {
		if err := a.sandboxPool.Release(a.name, oldProject, key); err != nil {
			slog.Warn("MoveWebChatSession: sandbox release failed",
				"agent", a.name, "session", key, "error", err)
		}
	}
	if a.workspaceStore != nil {
		if err := a.workspaceStore.Move(ctx, a.name, oldProject, key, projectID, key); err != nil {
			return fmt.Errorf("workspace move: %w", err)
		}
	}
	return a.sessions.MoveSessionByID(sessionId, projectID)
}

// Model returns the agent's model name.
func (a *Agent) Model() string {
	return a.model
}

// CostTracker returns the agent's cost tracker for usage/billing queries.
func (a *Agent) CostTracker() *costtracker.Tracker {
	return a.costTracker
}

// dumpLLMRequest appends the full LLM-bound payload to a dedicated file
// when FASTCLAW_DUMP_LLM is set. Default path is ~/.fastclaw/logs/llm-dump.log
// (overridable via FASTCLAW_DUMP_LLM_FILE) — separate from gateway.log so
// the multi-thousand-line system prompt doesn't drown structured slog
// entries, and tail-able regardless of whether the gateway runs under air,
// daemon, or as a foreground process.
//
// Multi-line content is written as one block per turn (not per-line slog
// calls) so timestamps don't shred the system prompt.
func dumpLLMRequest(agentName, model string, messages []provider.Message, tools []provider.Tool) {
	if os.Getenv("FASTCLAW_DUMP_LLM") == "" {
		return
	}
	path := os.Getenv("FASTCLAW_DUMP_LLM_FILE")
	if path == "" {
		home := os.Getenv("FASTCLAW_HOME")
		if home == "" {
			if h, err := os.UserHomeDir(); err == nil {
				home = h + "/.fastclaw"
			}
		}
		if home == "" {
			return
		}
		path = home + "/logs/llm-dump.log"
	}
	_ = os.MkdirAll(filepathDir(path), 0o755)

	var b strings.Builder
	fmt.Fprintf(&b, "\n=== LLM REQUEST  ts=%s  agent=%s  model=%s  messages=%d  tools=%d ===\n",
		time.Now().Format(time.RFC3339Nano), agentName, model, len(messages), len(tools))
	for i, m := range messages {
		fmt.Fprintf(&b, "--- msg[%d] role=%s ---\n", i, m.Role)
		// Prefer Content; fall back to ContentParts for multimodal turns
		// (image_url stubs keep logs readable instead of dumping data URLs).
		content := m.Content
		if content == "" && len(m.ContentParts) > 0 {
			var pb strings.Builder
			for _, p := range m.ContentParts {
				switch p.Type {
				case "text":
					pb.WriteString(p.Text)
				case "image_url":
					pb.WriteString("[image_url]")
				default:
					fmt.Fprintf(&pb, "[%s]", p.Type)
				}
				pb.WriteString("\n")
			}
			content = pb.String()
		}
		if content != "" {
			b.WriteString(content)
			if !strings.HasSuffix(content, "\n") {
				b.WriteString("\n")
			}
		}
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&b, "[tool_call name=%s args=%s]\n", tc.Function.Name, tc.Function.Arguments)
		}
	}
	if len(tools) > 0 {
		names := make([]string, 0, len(tools))
		for _, t := range tools {
			names = append(names, t.Function.Name)
		}
		fmt.Fprintf(&b, "--- tools (%d) ---\n%s\n", len(tools), strings.Join(names, ", "))
	}
	b.WriteString("=== END LLM REQUEST ===\n")

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		// Fall back to stderr so the dump isn't silently lost.
		fmt.Fprint(os.Stderr, b.String())
		return
	}
	defer f.Close()
	_, _ = f.WriteString(b.String())
}

// filepathDir is a tiny inline helper to dodge importing path/filepath
// just for one Dir() call in this single function.
func filepathDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}

// renderClientParams turns the per-request `params` blob the API
// caller submitted into a system message that nudges the LLM to
// honor those values when calling tools. Returns "" when params is
// empty so we don't add a noise message every turn.
//
// Why a system message and not a binding into tool args:
//
//	v1 trades determinism for simplicity. Apps don't know which
//	tools the agent has — they just send a flat key/value blob, and
//	the agent owner's system prompt tells the LLM what to do with
//	each known key. LLMs are reliable at copying JSON-shaped values
//	verbatim into tool calls (the failure mode is "ignored", not
//	"corrupted"); a stronger forcing layer is a v2 problem.
//
// Output shape: a `## Client Parameters` section with the JSON
// pretty-printed in a fenced block, plus a one-liner reminding the
// model these are constraints. The header + fence are deliberate —
// LLMs honor structured params framed as a separate document
// section much more reliably than as inline prose.
func renderClientParams(params map[string]any) string {
	if len(params) == 0 {
		return ""
	}
	blob, err := json.MarshalIndent(params, "", "  ")
	if err != nil {
		return ""
	}
	// Minimal by design — one fact, no behavioural prose. Earlier
	// versions tried to nudge the model with "treat as constraints" /
	// "don't shell out" / "look at the skills section" and each one
	// opened a new literal-misread surface (the model treated `model`
	// as a directive to call that API, refused outright "no skill
	// matches", or did `ls Skills/` looking for a directory). How to
	// pick a tool / skill is the agent's regular job, fully covered
	// by the system prompt's skills section and any per-agent SOUL.md.
	// The only thing the system has to say here is "here is the data
	// the client sent" — anything more is noise.
	return "## Client Parameters\n\n" +
		"The user's client app submitted these parameters alongside " +
		"the message. Forward them to whichever tool / skill you call.\n\n" +
		"```json\n" + string(blob) + "\n```"
}

// HandleMessage processes an inbound message through the ReAct loop.
func (a *Agent) HandleMessage(ctx context.Context, msg bus.InboundMessage) string {
	// Check for slash commands first
	if result := a.handleSlashCommand(msg); result.handled {
		emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{"content": result.reply}})
		emitEvent(ctx, ChatEvent{Type: "done"})
		return result.reply
	}

	chatterUID := a.chatterUserID(msg)
	// Tag ctx so the sandbox layer can bind-mount this chatter's
	// per-user skills dir into the container at /root/.agents/skills
	// (where `npx skills add -g -y` writes). Tagging happens before
	// any sandbox.Get call below so attachments + exec inherit it.
	ctx = sandbox.WithUserID(ctx, chatterUID)
	a.refreshSkillsFromStore(chatterUID)
	sess := a.sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID)
	// Bind the registry to this chat's session so workspace.Store reads
	// + writes get session-scoped paths and (when a sandbox pool is
	// wired) the executor used by exec/read_file/list_dir is tied to a
	// session-private container.
	a.bindSession(ctx, msg.Channel, msg.ChatID, msg.ProjectID)

	// Safety net for client-aborted turns: if the loop exits with a
	// tool_use that never got its matching tool_result appended (the
	// user clicked Stop while a long-running exec was in flight, the
	// SDK returned no response for it, etc.), pad the orphan so the
	// session history stays well-formed. Without this, the tool keeps
	// rendering as a forever-spinning "running" entry on history
	// rebuild and the next turn's API call gets a 400 from Anthropic
	// for orphaned tool_use ids.
	defer padOrphanToolResults(sess)

	// Reset per-turn tool failure tracking. The web_fetch (and any
	// future tool that opts in) consults the registry's
	// PriorFailure to refuse a guaranteed-fail retry within the
	// same turn — without StartTurn here, failures from a previous
	// turn would poison legit retries the user explicitly asked for.
	a.registry.StartTurn()

	// Hook: BeforeSystemPrompt
	a.hooks.Run(ctx, &HookContext{AgentName: a.name, Point: BeforeSystemPrompt, UserID: a.ownerUserID})

	systemPrompt := a.ctxBuilder.BuildSystemPrompt()

	// Hook: AfterSystemPrompt
	a.hooks.Run(ctx, &HookContext{AgentName: a.name, Point: AfterSystemPrompt, UserID: a.ownerUserID})

	// Store the raw user message. Images may arrive via the legacy
	// PhotoURL (single, used by IM bridges) or PhotoURLs (multi, used by
	// the web chat upload path); flatten both into one content-parts
	// slice so the provider sees `[text, image, image, …]`.
	userMsg := provider.Message{Role: "user", Content: msg.Text}
	imageURLs := msg.PhotoURLs
	if msg.PhotoURL != "" {
		imageURLs = append([]string{msg.PhotoURL}, imageURLs...)
	}
	if len(imageURLs) > 0 {
		userMsg.Content = ""
		// Skip an empty leading text part — image-only sends used to
		// produce `[{text: ""}, {image_url}, …]` which some upstreams
		// reject as a content-less wire message.
		var parts []provider.ContentPart
		if msg.Text != "" {
			parts = append(parts, provider.ContentPart{Type: "text", Text: msg.Text})
		}
		for _, u := range imageURLs {
			parts = append(parts, provider.ContentPart{
				Type: "image_url", ImageURL: &provider.ImageURL{URL: u, Detail: "auto"},
			})
		}
		userMsg.ContentParts = parts
	}
	sess.Append(userMsg)

	// Context compaction: check if session messages are too large
	sessionMsgs := sess.GetMessages()
	compactResult, err := CompactMessages(sessionMsgs, a.homePath, a.provider, a.model)
	if err != nil {
		slog.Warn("compaction error", "agent", a.name, "error", err)
	}
	if compactResult != nil && compactResult.Pruned {
		// Replace session messages with compacted version
		sess.ReplaceMessages(compactResult.Messages)
		sessionMsgs = compactResult.Messages
		slog.Info("context compacted", "agent", a.name, "log_file", compactResult.LogFile)
	}

	messages := make([]provider.Message, 0, len(sessionMsgs)+2)
	messages = append(messages, provider.Message{Role: "system", Content: systemPrompt})
	if paramsMsg := renderClientParams(msg.Params); paramsMsg != "" {
		messages = append(messages, provider.Message{Role: "system", Content: paramsMsg})
	}
	messages = append(messages, sessionMsgs...)

	toolDefs := a.registry.Definitions()

	// Loop detection: track consecutive identical tool calls
	type toolCallSig struct {
		name string
		hash [32]byte
	}
	var lastSig toolCallSig
	consecutiveCount := 0
	totalToolCalls := 0
	// allFailedRounds is the count of CONSECUTIVE rounds where every
	// tool result came back as a 4xx/5xx HTTP error or an executor
	// error. This catches the "model rotates through five guessed
	// URLs that all 404" pattern that loop detection (which keys on
	// identical args) misses. After three such rounds we drop tools
	// from the next LLM call so the model is forced to produce text
	// directly instead of burning more rounds chasing dead URLs.
	allFailedRounds := 0
	const failedRoundsLimit = 3

	// ReAct loop
	for i := 0; i < a.maxToolIterations; i++ {
		slog.Info("agent loop iteration",
			"agent", a.name,
			"iteration", i+1,
			"channel", msg.Channel,
			"chat_id", msg.ChatID,
		)

		// Hook: BeforeModelCall
		hcBefore := &HookContext{AgentName: a.name, Point: BeforeModelCall, Messages: messages, ChatID: msg.ChatID, UserID: a.ownerUserID}
		a.hooks.Run(ctx, hcBefore)

		// PII scrubbing: redact sensitive data before sending to LLM
		llmMessages := messages
		if a.piiScrubEnabled {
			llmMessages = privacy.ScrubMessages(messages)
		}

		if a.provider == nil {
			slog.Error("agent has no provider configured", "agent", a.name, "model", a.model)
			noProviderMsg := "Agent is not configured with a usable LLM provider. Check that cfg.Providers contains the prefix referenced by model `" + a.model + "`."
			emitEvent(ctx, ChatEvent{Type: "error", Data: map[string]any{"message": noProviderMsg}})
			emitEvent(ctx, ChatEvent{Type: "done"})
			return noProviderMsg
		}
		// After enough consecutive rounds where every tool came back
		// as 4xx/5xx, drop tools from the next call so the model is
		// forced to produce a text answer with what it has. The
		// system message above the request makes the constraint
		// explicit so the model doesn't apologetically dangle.
		callTools := toolDefs
		if allFailedRounds >= failedRoundsLimit {
			slog.Warn("disabling tools after consecutive failed rounds",
				"agent", a.name, "failed_rounds", allFailedRounds)
			callTools = nil
			llmMessages = append(llmMessages, provider.Message{
				Role: "system",
				Content: fmt.Sprintf(
					"The last %d rounds of tool calls all failed (HTTP errors or empty results). Stop calling tools and answer the user directly with what you know — explain that authoritative sources weren't reachable and provide your best-effort response based on training knowledge, clearly marked as unverified.",
					allFailedRounds,
				),
			})
		}
		dumpLLMRequest(a.name, a.model, llmMessages, callTools)
		resp, err := a.provider.Chat(ctx, llmMessages, callTools, a.model, a.maxTokens, a.temperature)

		// Hook: AfterModelCall
		hcAfter := &HookContext{AgentName: a.name, Point: AfterModelCall, Messages: messages, Response: resp, Error: err, StartTime: hcBefore.StartTime, ChatID: msg.ChatID, UserID: a.ownerUserID}
		a.hooks.Run(ctx, hcAfter)

		if err != nil {
			slog.Error("LLM chat failed", "agent", a.name, "error", err)
			emitEvent(ctx, ChatEvent{Type: "error", Data: map[string]any{"message": err.Error()}})
			emitEvent(ctx, ChatEvent{Type: "done"})
			return "Sorry, I encountered an error processing your request."
		}

		if !resp.HasToolCalls() {
			sess.Append(provider.Message{Role: "assistant", Content: resp.Content, Thinking: resp.Thinking, Timestamp: time.Now().UnixMilli(), RawAssistant: resp.RawAssistant})
			emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{"content": resp.Content}})
			emitEvent(ctx, ChatEvent{Type: "done"})
			a.runPostTurn(ctx, messages, totalToolCalls)
			return resp.Content
		}

		// Emit assistant content before tool calls if present
		if resp.Content != "" {
			emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{"content": resp.Content}})
		}

		// Emit tool_call events
		for _, tc := range resp.ToolCalls {
			emitEvent(ctx, ChatEvent{Type: "tool_call", Data: map[string]any{
				"id":        tc.ID,
				"name":      tc.Function.Name,
				"arguments": tc.Function.Arguments,
			}})
		}

		assistantMsg := provider.Message{
			Role:         "assistant",
			Content:      resp.Content,
			ToolCalls:    resp.ToolCalls,
			Thinking:     resp.Thinking,
			Timestamp:    time.Now().UnixMilli(),
			RawAssistant: resp.RawAssistant,
		}
		sess.Append(assistantMsg)
		messages = append(messages, assistantMsg)

		// Loop detection: check before executing
		loopDetected := false
		for _, tc := range resp.ToolCalls {
			sig := toolCallSig{
				name: tc.Function.Name,
				hash: sha256.Sum256([]byte(tc.Function.Arguments)),
			}
			if sig.name == lastSig.name && sig.hash == lastSig.hash {
				consecutiveCount++
			} else {
				consecutiveCount = 1
				lastSig = sig
			}
			if consecutiveCount >= 3 {
				slog.Warn("tool loop detected", "agent", a.name, "tool", tc.Function.Name)
				warnMsg := provider.Message{
					Role:    "system",
					Content: "Loop detected: you called the same tool with the same arguments 3 times. Please try a different approach.",
				}
				sess.Append(warnMsg)
				messages = append(messages, warnMsg)
				loopDetected = true
				break
			}
		}
		if loopDetected {
			break
		}

		// Fire BeforeToolCall hooks
		for _, tc := range resp.ToolCalls {
			a.hooks.Run(ctx, &HookContext{
				AgentName: a.name,
				Point:     BeforeToolCall,
				ToolName:  tc.Function.Name,
				ToolArgs:  tc.Function.Arguments,
				UserID:    a.ownerUserID,
			})
		}

		// Apply per-round parallel cap. The LLM decides how many
		// tool calls to emit; we cap how many run concurrently this
		// round. Overflow gets a synthetic "deferred" tool_result so
		// the model sees them as resolved (no orphan tool_use ids
		// that would poison the next API request) but without
		// content — naturally re-issuing them next round when it can
		// react to the executed batch's results. Effective default
		// is 0 = unlimited; users hit specific rate-limited APIs
		// (Brave free tier 1RPS, etc.) set it to 1 / 2 to force
		// strict serial / lightly-parallel execution.
		executeCalls := resp.ToolCalls
		var deferredCalls []provider.ToolCall
		if a.maxParallelToolCalls > 0 && len(resp.ToolCalls) > a.maxParallelToolCalls {
			executeCalls = resp.ToolCalls[:a.maxParallelToolCalls]
			deferredCalls = resp.ToolCalls[a.maxParallelToolCalls:]
			slog.Info("deferring tool calls beyond parallel cap",
				"agent", a.name,
				"cap", a.maxParallelToolCalls,
				"deferred", len(deferredCalls),
			)
		}

		// Execute tools concurrently via SDK engine
		slog.Info("executing tools concurrently",
			"agent", a.name,
			"count", len(executeCalls),
		)
		results := a.engine.executeToolsConcurrently(ctx, a.registry, executeCalls, a.workspacePath)
		// Append synthetic deferred results so every original tool_use
		// id has a paired tool_result. The deferred message tells the
		// model exactly why it didn't run — it can re-issue next
		// round once it has the executed batch's results.
		for _, tc := range deferredCalls {
			results = append(results, toolCallResult{
				toolCallID: tc.ID,
				toolName:   tc.Function.Name,
				result: fmt.Sprintf(
					"Deferred — this turn's parallel-tool cap is %d, and you emitted %d. Re-issue this exact call next round if you still need it; you'll have the other tools' results to inform the decision then.",
					a.maxParallelToolCalls, len(resp.ToolCalls),
				),
			})
		}

		// Defensive backstop: if the SDK returned fewer results than tool
		// calls (and the bridge somehow didn't already pad — belt and
		// suspenders since orphan tool_use ids poison the next API request
		// with HTTP 400), synthesize a failure result so every tool_use
		// gets a paired tool_result in the conversation history.
		if len(results) < len(resp.ToolCalls) {
			padded := make([]toolCallResult, len(resp.ToolCalls))
			gotByID := make(map[string]toolCallResult, len(results))
			for _, r := range results {
				gotByID[r.toolCallID] = r
			}
			for i, tc := range resp.ToolCalls {
				if r, ok := gotByID[tc.ID]; ok {
					padded[i] = r
					continue
				}
				padded[i] = toolCallResult{
					toolCallID: tc.ID,
					toolName:   tc.Function.Name,
					result:     "tool execution did not return a result",
					err:        fmt.Errorf("missing executor response for %s", tc.ID),
				}
			}
			results = padded
		}

		// Round-level failure detection: did EVERY result come back
		// as a 4xx/5xx HTTP error or executor error? Tracked here so
		// the next iteration can decide whether to drop tools.
		roundAllFailed := len(results) > 0
		// Process results
		for idx, r := range results {
			totalToolCalls++
			tc := resp.ToolCalls[idx]
			resultContent, meta := extractToolMeta(r.result)

			// Hook: AfterToolCall
			a.hooks.Run(ctx, &HookContext{
				AgentName:  a.name,
				Point:      AfterToolCall,
				ToolName:   r.toolName,
				ToolResult: resultContent,
				Error:      r.err,
				UserID:     a.ownerUserID,
			})

			if r.err != nil {
				slog.Warn("tool execution error",
					"agent", a.name,
					"name", r.toolName,
					"error", r.err,
				)
			}

			// Classify the result: did this single call fail? Records
			// it in the registry's per-turn failure map so a later
			// retry of the same args can be short-circuited (see
			// Registry.PriorFailure / web_fetch).
			thisFailed := isFailedToolResult(r.err, resultContent)
			if thisFailed {
				summary := r.err.Error()
				if summary == "" || summary == "<nil>" {
					summary = firstNonEmptyLine(resultContent)
				}
				a.registry.RecordToolFailure(r.toolName, tc.Function.Arguments, summary)
			} else {
				// One call in this round produced a real result —
				// the round as a whole isn't "all failed".
				roundAllFailed = false
			}

			// Index in FTS if available
			if a.ftsStore != nil {
				_ = a.ftsStore.Index(a.name, msg.ChatID, "tool:"+r.toolName, resultContent, time.Now())
			}

			// Check for MEDIA: protocol in tool output
			if mediaPaths := extractMediaPaths(resultContent); len(mediaPaths) > 0 {
				a.sendMediaFiles(msg, mediaPaths)
			}

			toolMsg := provider.Message{
				Role:       "tool",
				Content:    resultContent,
				ToolCallID: tc.ID,
				Name:       r.toolName,
				Metadata:   meta,
			}
			sess.Append(toolMsg)
			messages = append(messages, toolMsg)

			evt := map[string]any{
				"id":     tc.ID,
				"name":   r.toolName,
				"result": resultContent,
			}
			if meta != nil {
				evt["metadata"] = meta
			}
			emitEvent(ctx, ChatEvent{Type: "tool_result", Data: evt})
		}
		// Update consecutive-failed-rounds tally now that the whole
		// round's results have been processed. A single non-failure
		// resets it — the model just got useful info, give it room
		// to use it.
		if roundAllFailed {
			allFailedRounds++
		} else {
			allFailedRounds = 0
		}
	}

	slog.Warn("max tool iterations reached — forcing final delivery", "agent", a.name, "max", a.maxToolIterations)
	// Forced final delivery: one more LLM call with tools disabled and a
	// nudge that tells the model to synthesize what it has. Replaces the
	// old behavior of just returning a canned warning, which left users
	// with zero deliverable after a full iteration budget got burned.
	finalMessages := append(messages, capReachedNudge(a.maxToolIterations))
	if a.piiScrubEnabled {
		finalMessages = privacy.ScrubMessages(finalMessages)
	}
	finalContent := ""
	finalResp, finalErr := a.provider.Chat(ctx, finalMessages, nil, a.model, a.maxTokens, a.temperature)
	if finalErr == nil {
		finalContent = finalResp.Content
	}
	if finalContent == "" {
		// Synthesis call itself failed or returned empty — fall back to
		// the canned line so the user still gets *something* with the
		// badge attached.
		finalContent = fmt.Sprintf("I've reached the maximum number of tool iterations (%d) and couldn't synthesize a final response. The work above represents what I gathered before hitting the limit.", a.maxToolIterations)
	}
	capMeta := iterationCapMetadata(a.maxToolIterations)
	sess.Append(provider.Message{
		Role:      "assistant",
		Content:   finalContent,
		Metadata:  capMeta,
		Timestamp: time.Now().UnixMilli(),
	})
	emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{
		"content":  finalContent,
		"metadata": capMeta,
	}})
	emitEvent(ctx, ChatEvent{Type: "done"})
	a.runPostTurn(ctx, messages, totalToolCalls)
	return finalContent
}

// isFailedToolResult is the agent loop's heuristic for "this tool
// returned nothing useful". Used both to populate the per-turn failure
// map (so a later identical call can be refused up front) and to drive
// the consecutive-failed-rounds short-circuit. We deliberately stay
// conservative — empty exec output is legit for many shell commands —
// and only flag the high-signal patterns: tool error, HTTP 4xx/5xx,
// or the `[Analyze the error above…]` envelope our wrapper appends to
// upstream failures.
func isFailedToolResult(err error, content string) bool {
	if err != nil {
		return true
	}
	c := strings.TrimSpace(content)
	if strings.HasPrefix(c, "HTTP 4") || strings.HasPrefix(c, "HTTP 5") {
		return true
	}
	if strings.Contains(c, "[Analyze the error above and try a different approach.]") {
		return true
	}
	return false
}

// firstNonEmptyLine returns the first non-empty line of s, trimmed
// and capped at 120 chars. Used to make a stash-friendly summary of a
// tool result when err.Error() is empty. (Named distinctly from
// skills.firstLine to avoid the duplicate declaration.)
func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > 120 {
			return line[:120] + "…"
		}
		return line
	}
	return ""
}

// padOrphanToolResults walks the session and appends a synthetic
// tool_result for any tool_use id from the latest assistant message that
// doesn't already have a matching tool_result. Earlier rounds aren't
// scanned — once the loop has moved past them they're already
// well-formed, otherwise the previous turn's API call would have failed.
//
// Triggered by HandleMessage's defer so a client-side Stop (or any other
// premature exit) can't leave the conversation in a state where the next
// turn's API call gets a 400 for orphan tool_use ids and the UI keeps
// spinning a "Running tools" indicator that will never resolve.
func padOrphanToolResults(sess *session.Session) {
	msgs := sess.GetMessages()
	// Walk back to the latest assistant message; if it has no tool_calls
	// or all tool_calls already have results after it, nothing to do.
	lastAssistantIdx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" && len(msgs[i].ToolCalls) > 0 {
			lastAssistantIdx = i
			break
		}
	}
	if lastAssistantIdx < 0 {
		return
	}
	resolved := make(map[string]bool)
	for _, m := range msgs[lastAssistantIdx+1:] {
		if m.Role == "tool" && m.ToolCallID != "" {
			resolved[m.ToolCallID] = true
		}
	}
	for _, tc := range msgs[lastAssistantIdx].ToolCalls {
		if resolved[tc.ID] {
			continue
		}
		slog.Warn("padding orphan tool_use with stopped result",
			"toolCallID", tc.ID, "tool", tc.Function.Name)
		sess.Append(provider.Message{
			Role:       "tool",
			ToolCallID: tc.ID,
			Name:       tc.Function.Name,
			Content:    "(stopped — execution was interrupted before the tool returned)",
		})
	}
}

// runPostTurn fires PostTurn hooks and handles auto-persist and skills learning.
func (a *Agent) runPostTurn(ctx context.Context, messages []provider.Message, toolCallCount int) {
	a.turnCount++

	// Index user/assistant messages in FTS
	if a.ftsStore != nil {
		for _, m := range messages {
			if m.Role == "user" || m.Role == "assistant" {
				_ = a.ftsStore.Index(a.name, "", m.Role, m.Content, time.Now())
			}
		}
	}

	// Fire PostTurn hooks
	a.hooks.Run(ctx, &HookContext{
		AgentName:     a.name,
		Point:         PostTurn,
		Messages:      messages,
		TurnCount:     a.turnCount,
		ToolCallCount: toolCallCount,
		Workspace:     a.homePath,
		UserID:        a.ownerUserID,
	})

	// Auto-persist memory every N turns
	if a.memoryCfg.AutoPersist.Enabled && a.turnCount%a.memoryCfg.AutoPersist.EveryNTurns == 0 {
		model := a.memoryCfg.AutoPersist.Model
		if model == "" {
			model = a.model
		}
		go AutoPersistMemory(ctx, a.memory, a.provider, model, messages)
	}

	// Skills learner
	if a.skillsLearner != nil {
		go func() {
			if err := a.skillsLearner.MaybeExtract(ctx, messages, toolCallCount); err != nil {
				slog.Debug("skills learner error", "error", err)
			}
		}()
	}
}

// HandleMessageStream processes a message through the ReAct loop and returns
// a StreamReader for the final response. Tool call iterations use non-streaming Chat;
// the final text response uses ChatStream for true SSE streaming.
func (a *Agent) HandleMessageStream(ctx context.Context, msg bus.InboundMessage) *provider.StreamReader {
	// Reuse setup logic from HandleMessage
	if result := a.handleSlashCommand(msg); result.handled {
		ch := make(chan provider.StreamChunk, 2)
		go func() {
			ch <- provider.StreamChunk{Content: result.reply, Done: true}
			close(ch)
		}()
		return provider.NewStreamReader(ch)
	}

	chatterUID := a.chatterUserID(msg)
	ctx = sandbox.WithUserID(ctx, chatterUID)
	a.refreshSkillsFromStore(chatterUID)
	sess := a.sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID)
	a.bindSession(ctx, msg.Channel, msg.ChatID, msg.ProjectID)

	// Same orphan-tool_use safety net as HandleMessage. The streaming path
	// previously lacked this, so loop detection (which appends an assistant
	// tool_use + a system warn and breaks without ever running tools) and
	// any other premature exit between sess.Append(assistantMsg) and tool
	// result append left orphaned tool_use ids in the session. The next
	// turn's API request — especially against Anthropic-compat endpoints
	// like DeepSeek's /anthropic — then 400s with "tool_use ids were found
	// without tool_result blocks immediately after".
	defer padOrphanToolResults(sess)

	a.hooks.Run(ctx, &HookContext{AgentName: a.name, Point: BeforeSystemPrompt, UserID: a.ownerUserID})
	systemPrompt := a.ctxBuilder.BuildSystemPrompt()
	a.hooks.Run(ctx, &HookContext{AgentName: a.name, Point: AfterSystemPrompt, UserID: a.ownerUserID})

	// Store raw user message — same multi-image flatten as HandleMessage.
	userMsg := provider.Message{Role: "user", Content: msg.Text}
	imageURLs := msg.PhotoURLs
	if msg.PhotoURL != "" {
		imageURLs = append([]string{msg.PhotoURL}, imageURLs...)
	}
	if len(imageURLs) > 0 {
		userMsg.Content = ""
		// Skip an empty leading text part — image-only sends used to
		// produce `[{text: ""}, {image_url}, …]` which some upstreams
		// reject as a content-less wire message.
		var parts []provider.ContentPart
		if msg.Text != "" {
			parts = append(parts, provider.ContentPart{Type: "text", Text: msg.Text})
		}
		for _, u := range imageURLs {
			parts = append(parts, provider.ContentPart{
				Type: "image_url", ImageURL: &provider.ImageURL{URL: u, Detail: "auto"},
			})
		}
		userMsg.ContentParts = parts
	}
	sess.Append(userMsg)

	sessionMsgs := sess.GetMessages()
	compactResult, err := CompactMessages(sessionMsgs, a.homePath, a.provider, a.model)
	if err != nil {
		slog.Warn("compaction error", "agent", a.name, "error", err)
	}
	if compactResult != nil && compactResult.Pruned {
		sess.ReplaceMessages(compactResult.Messages)
		sessionMsgs = compactResult.Messages
	}

	messages := make([]provider.Message, 0, len(sessionMsgs)+2)
	messages = append(messages, provider.Message{Role: "system", Content: systemPrompt})
	if paramsMsg := renderClientParams(msg.Params); paramsMsg != "" {
		messages = append(messages, provider.Message{Role: "system", Content: paramsMsg})
	}
	messages = append(messages, sessionMsgs...)

	toolDefs := a.registry.Definitions()

	type toolCallSig struct {
		name string
		hash [32]byte
	}
	var lastSig toolCallSig
	consecutiveCount := 0

	// ReAct loop - use Chat for tool iterations
	for i := 0; i < a.maxToolIterations; i++ {
		hcBefore := &HookContext{AgentName: a.name, Point: BeforeModelCall, Messages: messages, ChatID: msg.ChatID, UserID: a.ownerUserID}
		a.hooks.Run(ctx, hcBefore)

		dumpLLMRequest(a.name, a.model, messages, toolDefs)
		resp, err := a.provider.Chat(ctx, messages, toolDefs, a.model, a.maxTokens, a.temperature)

		hcAfter := &HookContext{AgentName: a.name, Point: AfterModelCall, Messages: messages, Response: resp, Error: err, StartTime: hcBefore.StartTime, ChatID: msg.ChatID, UserID: a.ownerUserID}
		a.hooks.Run(ctx, hcAfter)

		if err != nil {
			slog.Error("LLM chat failed", "agent", a.name, "error", err)
			return a.stringStream("Sorry, I encountered an error processing your request.")
		}

		if !resp.HasToolCalls() {
			// Final response - use streaming
			sr, err := a.provider.ChatStream(ctx, messages, toolDefs, a.model, a.maxTokens, a.temperature)
			if err != nil {
				slog.Error("LLM stream failed, falling back", "agent", a.name, "error", err)
				sess.Append(provider.Message{Role: "assistant", Content: resp.Content})
				return a.stringStream(resp.Content)
			}

			// Collect content in background for session storage
			outCh := make(chan provider.StreamChunk, 64)
			outReader := provider.NewStreamReader(outCh)
			go func() {
				defer close(outCh)
				var full strings.Builder
				var thinking, thinkingSig string
				var rawAssistant json.RawMessage
				for {
					chunk, ok := sr.Next()
					if !ok {
						break
					}
					if chunk.Content != "" {
						full.WriteString(chunk.Content)
					}
					if chunk.Thinking != "" {
						thinking = chunk.Thinking
					}
					if chunk.ThinkingSignature != "" {
						thinkingSig = chunk.ThinkingSignature
					}
					if len(chunk.RawAssistant) > 0 {
						rawAssistant = chunk.RawAssistant
					}
					select {
					case outCh <- chunk:
					case <-ctx.Done():
						return
					}
				}
				msg := provider.Message{Role: "assistant", Content: full.String(), Thinking: thinking}
				switch {
				case len(rawAssistant) > 0:
					// Provider already serialized the assistant message
					// in its wire format (e.g. OpenAI/DeepSeek with
					// reasoning_content). Persist verbatim so the next
					// turn replays it byte-identically — required for
					// DeepSeek thinking mode.
					msg.RawAssistant = rawAssistant
				case thinking != "":
					// Anthropic extended thinking: pack {thinking, signature}
					// as a content-block so the next turn can echo it back.
					if raw, err := json.Marshal(map[string]string{
						"type":      "thinking",
						"thinking":  thinking,
						"signature": thinkingSig,
					}); err == nil {
						msg.RawAssistant = raw
					}
				}
				sess.Append(msg)
			}()
			return outReader
		}

		// Tool calls - process concurrently via SDK engine
		assistantMsg := provider.Message{
			Role:         "assistant",
			Content:      resp.Content,
			ToolCalls:    resp.ToolCalls,
			Thinking:     resp.Thinking,
			Timestamp:    time.Now().UnixMilli(),
			RawAssistant: resp.RawAssistant,
		}
		sess.Append(assistantMsg)
		messages = append(messages, assistantMsg)

		// Loop detection
		loopDetected := false
		for _, tc := range resp.ToolCalls {
			sig := toolCallSig{
				name: tc.Function.Name,
				hash: sha256.Sum256([]byte(tc.Function.Arguments)),
			}
			if sig.name == lastSig.name && sig.hash == lastSig.hash {
				consecutiveCount++
			} else {
				consecutiveCount = 1
				lastSig = sig
			}
			if consecutiveCount >= 3 {
				slog.Warn("tool loop detected", "agent", a.name, "tool", tc.Function.Name)
				warnMsg := provider.Message{
					Role:    "system",
					Content: "Loop detected: you called the same tool with the same arguments 3 times. Please try a different approach.",
				}
				sess.Append(warnMsg)
				messages = append(messages, warnMsg)
				loopDetected = true
				break
			}
		}
		if loopDetected {
			break
		}

		// Fire BeforeToolCall hooks
		for _, tc := range resp.ToolCalls {
			a.hooks.Run(ctx, &HookContext{AgentName: a.name, Point: BeforeToolCall, ToolName: tc.Function.Name, ToolArgs: tc.Function.Arguments, UserID: a.ownerUserID})
		}

		// Execute tools concurrently via SDK engine
		results := a.engine.executeToolsConcurrently(ctx, a.registry, resp.ToolCalls, a.workspacePath)

		for idx, r := range results {
			tc := resp.ToolCalls[idx]
			resultContent, meta := extractToolMeta(r.result)
			a.hooks.Run(ctx, &HookContext{AgentName: a.name, Point: AfterToolCall, ToolName: r.toolName, ToolResult: resultContent, Error: r.err, UserID: a.ownerUserID})

			if r.err != nil {
				slog.Warn("tool execution error", "agent", a.name, "name", r.toolName, "error", r.err)
			}

			if mediaPaths := extractMediaPaths(resultContent); len(mediaPaths) > 0 {
				a.sendMediaFiles(msg, mediaPaths)
			}

			toolMsg := provider.Message{Role: "tool", Content: resultContent, ToolCallID: tc.ID, Name: r.toolName, Metadata: meta}
			sess.Append(toolMsg)
			messages = append(messages, toolMsg)
		}
	}

	slog.Warn("max tool iterations reached — streaming forced final delivery", "agent", a.name, "max", a.maxToolIterations)
	return a.streamFinalDeliveryAfterCap(ctx, messages, sess)
}

// streamFinalDeliveryAfterCap runs one extra ChatStream with tools
// disabled and a synthesis nudge, then persists the assistant message
// with iteration-cap metadata so the chat UI can badge the bubble.
// Returned StreamReader matches the contract of the normal "final
// response" branch above so callers don't need a special case.
func (a *Agent) streamFinalDeliveryAfterCap(ctx context.Context, messages []provider.Message, sess *session.Session) *provider.StreamReader {
	capMeta := iterationCapMetadata(a.maxToolIterations)
	finalMessages := append(messages, capReachedNudge(a.maxToolIterations))
	sr, err := a.provider.ChatStream(ctx, finalMessages, nil, a.model, a.maxTokens, a.temperature)
	if err != nil {
		// Streaming endpoint failed — persist+emit a fallback line
		// with the badge so the user still gets the signal.
		fallback := fmt.Sprintf("I've reached the maximum number of tool iterations (%d) and couldn't synthesize a final response. The work above represents what I gathered before hitting the limit.", a.maxToolIterations)
		sess.Append(provider.Message{Role: "assistant", Content: fallback, Metadata: capMeta, Timestamp: time.Now().UnixMilli()})
		emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{"content": fallback, "metadata": capMeta}})
		return a.stringStream(fallback)
	}

	outCh := make(chan provider.StreamChunk, 64)
	outReader := provider.NewStreamReader(outCh)
	go func() {
		defer close(outCh)
		var full strings.Builder
		var thinking, thinkingSig string
		var rawAssistant json.RawMessage
		for {
			chunk, ok := sr.Next()
			if !ok {
				break
			}
			if chunk.Content != "" {
				full.WriteString(chunk.Content)
			}
			if chunk.Thinking != "" {
				thinking = chunk.Thinking
			}
			if chunk.ThinkingSignature != "" {
				thinkingSig = chunk.ThinkingSignature
			}
			if len(chunk.RawAssistant) > 0 {
				rawAssistant = chunk.RawAssistant
			}
			select {
			case outCh <- chunk:
			case <-ctx.Done():
				return
			}
		}
		content := full.String()
		if content == "" {
			content = fmt.Sprintf("I've reached the maximum number of tool iterations (%d) and couldn't synthesize a final response. The work above represents what I gathered before hitting the limit.", a.maxToolIterations)
		}
		finalMsg := provider.Message{
			Role:      "assistant",
			Content:   content,
			Thinking:  thinking,
			Metadata:  capMeta,
			Timestamp: time.Now().UnixMilli(),
		}
		switch {
		case len(rawAssistant) > 0:
			finalMsg.RawAssistant = rawAssistant
		case thinking != "":
			if raw, err := json.Marshal(map[string]string{
				"type":      "thinking",
				"thinking":  thinking,
				"signature": thinkingSig,
			}); err == nil {
				finalMsg.RawAssistant = raw
			}
		}
		sess.Append(finalMsg)
		// Out-of-band content event so SSE subscribers + chat_events
		// archive carry the cap-reached flag — chunks themselves don't
		// have a metadata field, so we publish it once here.
		emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{
			"content":  "",
			"metadata": capMeta,
		}})
	}()
	return outReader
}

// extractToolMeta strips a FC_META prefix (if present) from a tool result and
// returns the remaining content plus the parsed metadata. Today the only
// signal is whether exec ran in a sandbox. Keeping the helper shared so all
// tool-result handoff paths emit the same shape to the frontend.
func extractToolMeta(result string) (string, map[string]any) {
	if strings.HasPrefix(result, tools.MetaSandboxPrefix) {
		return strings.TrimPrefix(result, tools.MetaSandboxPrefix), map[string]any{"sandbox": true}
	}
	return result, nil
}

// capReachedNudge is the system message we append before the forced
// final delivery turn. Spells out two things: (a) tools are disabled
// for this call so don't try, (b) deliver the structured output the
// user asked for from whatever was already gathered, marking gaps
// explicitly rather than skipping fields. The model was generally
// burning the entire budget on exploration without ever circling back
// to synthesis — surfacing the constraint explicitly is the cheapest
// nudge that produces a usable artifact.
func capReachedNudge(maxIterations int) provider.Message {
	return provider.Message{
		Role: "system",
		Content: fmt.Sprintf(
			"You've used all %d tool-call iterations available for this turn. Tools are now disabled for this final response — do not attempt to call any. Synthesize what you've already gathered into the most complete deliverable you can: if the user asked for a structured artifact (table, list, ICP summary, email drafts, etc.), produce it now from the existing tool results. For any fields you couldn't resolve, mark them as 'unknown' / 'not found' / 'partial' rather than dropping rows or skipping the structure — give the user something usable plus an honest note about what's missing. Do not apologize without delivering content.",
			maxIterations,
		),
	}
}

// iterationCapMetadata is the assistant-side metadata stamped on the
// forced final-delivery message so the UI can badge the bubble. Kept
// as a constructor so the key name stays canonical across the streaming
// and non-streaming paths.
func iterationCapMetadata(maxIterations int) map[string]any {
	return map[string]any{
		"iterationCapReached": true,
		"iterationCapValue":   maxIterations,
	}
}

// stringStream creates a StreamReader that yields a single string.
func (a *Agent) stringStream(text string) *provider.StreamReader {
	ch := make(chan provider.StreamChunk, 2)
	go func() {
		ch <- provider.StreamChunk{Content: text, Done: true}
		close(ch)
	}()
	return provider.NewStreamReader(ch)
}

// HomePath returns the agent's home directory (identity/metadata).
func (a *Agent) HomePath() string {
	return a.homePath
}

// WorkspacePath returns the agent's working directory for user-facing files.
func (a *Agent) WorkspacePath() string {
	return a.workspacePath
}

// UpdateConfig updates the agent's runtime config (model, temperature, etc.)
func (a *Agent) UpdateConfig(rc config.ResolvedAgent) {
	a.model = rc.Model
	a.maxTokens = rc.MaxTokens
	a.temperature = rc.Temperature
	a.maxToolIterations = rc.MaxToolIterations
	a.maxParallelToolCalls = rc.MaxParallelToolCalls
	// Sandbox flags drive the system prompt's "Working Directory" / "home
	// dir" description and the sandbox-capabilities block. Without this
	// propagation an agent that existed before sandbox was enabled keeps
	// telling the LLM its home is the host absolute path, even after the
	// executor itself has been swapped to Docker — model dutifully calls
	// list_dir /Users/idoubi/.fastclaw/agents/<id>/agent and 404s in the
	// container.
	a.ctxBuilder.sandboxEnabled = rc.Sandbox.Enabled
	a.ctxBuilder.sandboxBackend = rc.Sandbox.Backend
}

// chatterUserID picks the per-message chatter identity, falling back
// to the agent owner when the inbound message doesn't carry one
// (legacy channels, system-injected events, …). This is what we use
// as the per-user skills bucket key and the sandbox bind-mount target,
// so two different chatters of the same agent each see their own
// personal skill set and write installs into their own host dir.
func (a *Agent) chatterUserID(msg bus.InboundMessage) string {
	if msg.UserID != "" {
		return msg.UserID
	}
	return a.ownerUserID
}

// refreshSkillsFromStore mirrors OSS-hosted skills (global, per-agent,
// and per-user) to the local filesystem and rebuilds the skills summary
// baked into the system prompt. No-op when no workspace store is
// configured. Called at the top of every turn so a skill uploaded
// after pod start — or on a sibling replica — becomes visible here on
// the next message instead of requiring a pod restart.
//
// userID identifies whose per-user skill bucket to merge into the set;
// pass the chatter (not the agent owner) so a skill chatter A installs
// is visible only to chatter A even when both chat the same agent. Empty
// disables the per-user layer.
func (a *Agent) refreshSkillsFromStore(userID string) {
	if a.workspaceStore == nil {
		return
	}
	loader := NewSkillsLoaderWithGlobal(a.homeDir, a.homePath, "", a.skillsCfg, a.globalSkillsCfg).
		WithObjectStore(a.workspaceStore, a.agentID).
		WithUserID(userID)
	skills := loader.LoadSkills()
	a.ctxBuilder.SetSkillsSummary(loader.BuildSkillsSummary(skills))
}

// ReloadWorkspaceFiles re-reads workspace .md files (SOUL.md, AGENTS.md, etc.)
// and rebuilds the context builder.
func (a *Agent) ReloadWorkspaceFiles() {
	if a.memoryStore != nil {
		a.memory = NewMemoryWithStoreForUser(a.homePath, a.memoryStore, a.ownerUserID, a.name)
	} else {
		a.memory = NewMemory(a.homePath)
	}
	// Rebuild skills summary. When a workspace store is configured,
	// LoadSkills first hydrates global + per-agent + per-user skill dirs
	// from object storage so skills uploaded on another replica (or
	// post-boot on this one) become visible.
	loader := NewSkillsLoaderWithGlobal(a.homeDir, a.homePath, "", a.skillsCfg, a.globalSkillsCfg).
		WithUserID(a.ownerUserID)
	if a.workspaceStore != nil {
		loader.WithObjectStore(a.workspaceStore, a.agentID)
	}
	skills := loader.LoadSkills()
	skillsSummary := loader.BuildSkillsSummary(skills)
	a.ctxBuilder = NewContextBuilder(a.homePath, a.memory, skillsSummary)
	a.ctxBuilder.SetWorkspace(a.workspacePath)
	// Preserve Store-backed identity reads across reload; without this,
	// Postgres-mode pods silently fall back to pod-local filesystem.
	if a.memoryStore != nil {
		a.ctxBuilder.store = a.memoryStore
		a.ctxBuilder.agentID = a.name
	}
}

// extractMediaPaths scans tool output for MEDIA: lines and returns file paths.
// The MEDIA: protocol is used by OpenClaw skills to attach files to chat messages.
func extractMediaPaths(output string) []string {
	var paths []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "MEDIA:") {
			path := strings.TrimSpace(strings.TrimPrefix(line, "MEDIA:"))
			if path != "" {
				if _, err := os.Stat(path); err == nil {
					paths = append(paths, path)
				}
			}
		}
	}
	return paths
}

// sendMediaFiles sends extracted MEDIA: files to the outbound bus.
func (a *Agent) sendMediaFiles(msg bus.InboundMessage, mediaPaths []string) {
	if len(mediaPaths) == 0 || a.messageBus == nil {
		return
	}
	outMsg := bus.OutboundMessage{
		Channel:    msg.Channel,
		AccountID:  msg.AccountID,
		ChatID:     msg.ChatID,
		MediaPaths: mediaPaths,
	}
	select {
	case a.messageBus.Outbound <- outMsg:
	default:
		slog.Warn("outbound channel full, dropping media message", "agent", a.name)
	}
}
