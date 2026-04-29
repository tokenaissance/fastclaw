package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fastclaw-ai/fastclaw/internal/provider"
	"github.com/fastclaw-ai/fastclaw/internal/sandbox"
	"github.com/fastclaw-ai/fastclaw/internal/workspace"
)

// ToolFunc is a function that executes a tool with JSON arguments and returns a result string.
type ToolFunc func(ctx context.Context, args json.RawMessage) (string, error)

// ToolSource indicates where a tool was registered from.
type ToolSource int

const (
	SourceBuiltin ToolSource = iota // built-in tool
	SourceMCP                       // MCP server tool
	SourcePlugin                    // plugin-provided tool
)

// Registry holds all registered tools.
type Registry struct {
	tools       map[string]registeredTool
	sandboxRoot string // if non-empty, file tools reject paths outside this dir
	executor    sandbox.Executor // if non-nil, all file+exec tools route through this
	// File tool roots. systemRoot is the agent metadata dir (SOUL.md etc.);
	// userRoot is where user-facing artifacts go. A relative path whose base
	// matches a known system filename routes to systemRoot; everything else
	// goes to userRoot.
	systemRoot string
	userRoot   string
	// workspaceStore is the optional durable blob store for agent-generated
	// artifacts. When set, write_file / read_file / list_dir route through
	// it for paths that would otherwise land under userRoot. Identity files
	// (systemRoot) stay on the filesystem because the runtime context
	// builder still reads them via the separate small-state Store.
	workspaceStore workspace.Store
	agentID        string
	// sessionID scopes workspace.Store reads/writes so concurrent sessions
	// of the same agent don't collide on `report.md` etc. Set per-turn by
	// the agent loop via SetSessionID; an empty value falls back to
	// agent-shared scope (admin uploads, fixtures, tests).
	sessionID string
	// systemFileStore is the optional durable store for identity files
	// (SOUL.md, IDENTITY.md, USER.md, MEMORY.md, ...). In cloud/K8s
	// deployments Server.readIdentityFile / writeIdentityFile go through
	// Postgres via Store.{Get,Save}WorkspaceFile so the admin UI sees
	// the same content across pods. Without this hook the agent's own
	// write_file tool would write SOUL.md etc. to pod-local disk and
	// never be visible from the UI — so we route identity writes here
	// when set.
	systemFileStore SystemFileStore
	// userID is the chatter — passed through to systemFileStore so
	// chat-time writes (write_file on SOUL.md / USER.md / MEMORY.md)
	// land in that user's per-user override row, not the shared
	// template. Set once at agent boot via SetOwnerUserID.
	userID string
	// sandboxRequired is the runtime contract: when true, the exec tool
	// MUST refuse to fall through to the host shell — even if sbCfg
	// wasn't set at agent construction (cfg.Sandbox.Enabled was false at
	// boot but the user later flipped it on, or attachSandboxToAgents
	// wired a pool to this agent because a *sibling* agent wanted
	// sandbox). Without this, a `pool.Get()` failure during bindSession
	// silently falls through to host execution and the user sees a
	// confusing "sh: python: command not found" instead of a clear
	// "sandbox required but unavailable" error.
	sandboxRequired bool
	// envProvider + skillDirs cache the skill-env injection wiring set
	// at agent boot via RegisterExecWithSkillEnv so a later
	// SetExecutor (per-session) can re-register the sandboxed exec
	// closure WITH env injection. Without this, the sandboxed exec
	// runs every skill in a bare env and FAL_KEY / REPLICATE_API_TOKEN
	// never reach the container — skills always think no provider is
	// configured.
	envProvider SkillEnvProvider
	skillDirs   []string
}

// SystemFileStore is the narrow slice of the DB store that write_file /
// read_file need to keep identity files (SOUL.md, IDENTITY.md, …) in
// sync across pods. Matches the shape of agent.MemoryStore (and
// store.Store) intentionally so existing adapters can be reused. userID
// is the chatter — chat-time writes land in that user's per-user
// override row so they don't clobber the shared template.
type SystemFileStore interface {
	GetWorkspaceFile(ctx context.Context, agentID, userID, filename string) ([]byte, error)
	SaveWorkspaceFile(ctx context.Context, agentID, userID, filename string, data []byte) error
}

// SetWorkspaceStore installs a workspace store on the registry. File tools
// called with paths destined for userRoot will be redirected to the store
// (keyed by agentID). Pass both non-empty or the registry stays in pure
// filesystem mode. Safe to call before or after registerBuiltins.
func (r *Registry) SetWorkspaceStore(ws workspace.Store, agentID string) {
	r.workspaceStore = ws
	r.agentID = agentID
}

// SetSystemFileStore installs a durable store for identity files so the
// agent's write_file / read_file tools share a single source of truth
// with the admin UI (Customize page). Also records agentID so the store
// calls work even when SetWorkspaceStore isn't configured. Pass store=nil
// to disable and fall back to filesystem.
func (r *Registry) SetSystemFileStore(s SystemFileStore, agentID string) {
	r.systemFileStore = s
	if agentID != "" {
		r.agentID = agentID
	}
}

// SetOwnerUserID records the chatter so identity-file writes go through
// the systemFileStore tagged with the right user_id (per-user override),
// not the shared template. Set once at agent boot from the UserSpace's
// owner; current architecture has one agent per user, so this is fixed
// for the lifetime of the registry.
func (r *Registry) SetOwnerUserID(userID string) {
	r.userID = userID
}

// SetSandboxRequired flips the exec tool's host-shell fallback off. Call
// with true whenever the runtime decides this agent must run inside a
// sandbox executor (e.g., user enabled cfg.Sandbox after boot, so
// attachSandboxToAgents wired a pool). With this set, the exec tool's
// `useSandbox` check fires even when the agent was constructed with
// sbCfg=nil, so a missing executor surfaces as an explicit error
// instead of leaking onto the host shell.
func (r *Registry) SetSandboxRequired(required bool) {
	r.sandboxRequired = required
}

// SetSessionID scopes the registry's workspace.Store calls (write_file /
// read_file / list_dir) to a single chat session. The agent loop calls
// this at the top of each turn with msg.ChatID. An empty session falls
// back to the agent-shared scope (no session isolation).
func (r *Registry) SetSessionID(sessionID string) {
	r.sessionID = sessionID
}

type registeredTool struct {
	def    provider.Tool
	fn     ToolFunc
	source ToolSource
}

// NewRegistry creates a new tool registry with built-in tools.
// NewRegistry creates a Registry whose file tools route relative paths between
// two roots: system files (SOUL.md, IDENTITY.md, etc.) land in systemRoot;
// everything else lands in userRoot. Passing the same value for both gives
// the legacy single-root behavior.
func NewRegistry(systemRoot, userRoot string) *Registry {
	r := &Registry{
		tools:      make(map[string]registeredTool),
		systemRoot: systemRoot,
		userRoot:   userRoot,
	}
	r.registerBuiltins()
	return r
}

// Register adds a tool to the registry (as a built-in tool).
func (r *Registry) Register(name, description string, parameters interface{}, fn ToolFunc) {
	r.RegisterFrom(name, description, parameters, fn, SourceBuiltin)
}

// RegisterFrom adds a tool to the registry with an explicit source.
// Plugin-sourced tools can override built-in tools with the same name.
func (r *Registry) RegisterFrom(name, description string, parameters interface{}, fn ToolFunc, source ToolSource) {
	r.tools[name] = registeredTool{
		def: provider.Tool{
			Type: "function",
			Function: provider.ToolFunction{
				Name:        name,
				Description: description,
				Parameters:  parameters,
			},
		},
		fn:     fn,
		source: source,
	}
}

// HasBuiltin returns true if a built-in tool with the given name exists.
func (r *Registry) HasBuiltin(name string) bool {
	t, ok := r.tools[name]
	return ok && t.source == SourceBuiltin
}

// GetFunc returns the ToolFunc for a tool by name, or nil if not found.
func (r *Registry) GetFunc(name string) ToolFunc {
	t, ok := r.tools[name]
	if !ok {
		return nil
	}
	return t.fn
}

// Definitions returns all tool definitions for the LLM.
func (r *Registry) Definitions() []provider.Tool {
	defs := make([]provider.Tool, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, t.def)
	}
	return defs
}

// Execute runs a tool by name with the given arguments.
func (r *Registry) Execute(ctx context.Context, name string, args string) (string, error) {
	tool, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("unknown tool: %s", name)
	}

	result, err := tool.fn(ctx, json.RawMessage(args))
	if err != nil {
		return result + "\n[Analyze the error above and try a different approach.]", err
	}
	return result, nil
}

// SetSandboxConfig updates the exec tool to use sandbox mode.
func (r *Registry) SetSandboxConfig(sbCfg *SandboxConfig) {
	registerExecWithSandbox(r, sbCfg)
}

// SetSandboxRoot restricts the file tools (read_file, write_file, list_dir)
// to paths under root. Absolute paths outside the root and relative paths
// that traverse above it are rejected. When root is empty (default), no
// restriction is applied — this is the local single-user mode. In cloud
// mode the root is typically set to the user's directory
// (~/.fastclaw/users/{userID}).
func (r *Registry) SetSandboxRoot(root string) {
	r.sandboxRoot = root
}

// SetExecutor attaches a sandbox Executor. When set, read_file, write_file,
// list_dir, and exec are ALL forwarded to the executor instead of operating
// on the host filesystem. This is the mode used for cloud deployments where
// each user gets an isolated container/VM with their own runtime + files.
func (r *Registry) SetExecutor(ex sandbox.Executor) {
	r.executor = ex
	// Re-register built-in tools to use the executor.
	registerSandboxedFile(r, ex)
	registerSandboxedExec(r, ex)
}

func (r *Registry) registerBuiltins() {
	registerExec(r)
	registerFile(r)
	registerMessage(r)
}
