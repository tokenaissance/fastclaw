// Package store is the single persistence layer for FastClaw. The database
// is mandatory (sqlite by default; postgres for production); there is no
// file-only fallback. Every per-user table requires a real users.id row;
// callers that haven't resolved a user must 401, not invent a placeholder.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrNotFound is returned by Get* methods when the row does not exist. Use
// errors.Is(err, store.ErrNotFound) at call sites.
var ErrNotFound = errors.New("store: not found")

// Store is the unified interface for all persistent data.
//
// Tables fall into three buckets:
//   - account-scoped (users, web_sessions, apikeys): keyed by users.id
//   - agent-scoped (agents, agent_files, cron_jobs): keyed by agents.id;
//     ownership is on agents.user_id
//   - per-(user, agent) (sessions): chat history is private to one user
//   - scope-tagged (configs): rows carry (scope, scope_id, kind, name)
type Store interface {
	// --- Users ---
	CreateUser(ctx context.Context, u *UserRecord) error
	GetUser(ctx context.Context, id string) (*UserRecord, error)
	GetUserByLogin(ctx context.Context, usernameOrEmail string) (*UserRecord, error)
	GetUserByExternal(ctx context.Context, apikeyID, externalID string) (*UserRecord, error)
	ListUsers(ctx context.Context) ([]UserRecord, error)
	UpdateUser(ctx context.Context, u *UserRecord) error
	DeleteUser(ctx context.Context, id string) error
	CountUsers(ctx context.Context) (int, error)

	// --- Web sessions (login cookies) ---
	CreateWebSession(ctx context.Context, sess *WebSessionRecord) error
	GetWebSession(ctx context.Context, sid string) (*WebSessionRecord, error)
	DeleteWebSession(ctx context.Context, sid string) error
	DeleteExpiredWebSessions(ctx context.Context, before time.Time) error

	// --- API keys (per user) ---
	ListAPIKeys(ctx context.Context, userID string) ([]APIKeyRecord, error)
	GetAPIKey(ctx context.Context, id string) (*APIKeyRecord, error)
	CreateAPIKey(ctx context.Context, ak *APIKeyRecord) error
	DeleteAPIKey(ctx context.Context, id string) error
	RotateAPIKey(ctx context.Context, id, keyHash, keyPrefix string) error
	LookupAPIKeyByHash(ctx context.Context, keyHash string) (*APIKeyRecord, error)

	// --- API key ↔ agent permissions (M:N) ---
	SetAPIKeyAgents(ctx context.Context, apikeyID string, agentIDs []string) error
	ListAPIKeyAgents(ctx context.Context, apikeyID string) ([]string, error)
	APIKeyCanAccessAgent(ctx context.Context, apikeyID, agentID string) (bool, error)

	// --- Agents (atomic; agents.id is globally unique) ---
	ListAgents(ctx context.Context, ownerUserID string) ([]AgentRecord, error)
	GetAgent(ctx context.Context, agentID string) (*AgentRecord, error)
	SaveAgent(ctx context.Context, agent *AgentRecord) error
	DeleteAgent(ctx context.Context, agentID string) error
	ListAllAgents(ctx context.Context) ([]AgentRecord, error)

	// --- Sessions (per user, per agent — chat history is private) ---
	GetSession(ctx context.Context, userID, agentID, sessionKey string) (*SessionRecord, error)
	SaveSession(ctx context.Context, userID, agentID, sessionKey string, session *SessionRecord) error
	ListSessions(ctx context.Context, userID, agentID string) ([]SessionMeta, error)
	DeleteSession(ctx context.Context, userID, agentID, sessionKey string) error
	RenameSession(ctx context.Context, userID, agentID, sessionKey, title string) error

	// --- Session messages (append-only per-turn archive) ---
	//
	// Mirrors every Append into session_messages, separate from the
	// sessions.messages JSONB working set. AppendSessionMessage assigns
	// the next seq atomically inside one INSERT (COALESCE(MAX(seq),-1)+1)
	// so callers don't pass a seq. ListSessionMessages returns all rows
	// for one session in ascending seq order — that's the full history,
	// untouched by compaction. DeleteSession cascades to clean these up.
	AppendSessionMessage(ctx context.Context, userID, agentID, sessionKey string, msg SessionMessage) error
	ListSessionMessages(ctx context.Context, userID, agentID, sessionKey string) ([]SessionMessage, error)

	// --- Chat events (in-flight streaming deltas, persisted for resume) ---
	//
	// Every event the agent emits during a turn (content chunk,
	// tool_call, error, done) lands here with a per-session
	// auto-incremented seq. Clients that disconnect mid-turn (refresh,
	// network blip, mobile app backgrounded) reconnect with their
	// last-seen seq and receive the missed delta — without this the
	// agent's reply becomes invisible until the parent session row is
	// next loaded. Cleared by DeleteSession alongside session_messages.
	AppendChatEvent(ctx context.Context, userID, agentID, sessionKey, eventType string, data []byte) (int64, error)
	ListChatEventsSince(ctx context.Context, userID, agentID, sessionKey string, sinceSeq int64) ([]ChatEventRecord, error)
	LatestChatEventSeq(ctx context.Context, userID, agentID, sessionKey string) (int64, error)

	// --- Agent files ---
	//
	// SOUL.md, IDENTITY.md, MEMORY.md, AGENTS.md, BOOTSTRAP.md, etc.
	// Layered: user_id="" is the shared template (edited via the admin
	// Customize page), user_id=u_xxx is that user's personal override.
	// Read picks user-specific over template via fallback; write hits
	// the (agentID, userID, filename) row exactly.
	// GetAgentFile prefers the caller's own row, falling back to the
	// agent owner's row. Use GetAgentFileExact for a strict (agent,
	// user, filename) lookup that bypasses the overlay.
	GetAgentFile(ctx context.Context, agentID, userID, filename string) ([]byte, error)
	GetAgentFileExact(ctx context.Context, agentID, userID, filename string) ([]byte, error)
	SaveAgentFile(ctx context.Context, agentID, userID, filename string, data []byte) error
	DeleteAgentFile(ctx context.Context, agentID, userID, filename string) error
	ListAgentFiles(ctx context.Context, agentID, userID string) ([]string, error)

	// --- Scoped configs (providers / channels / settings live here) ---
	//
	// One table backs all three concept families. Each row is keyed by
	// (scope, scope_id, kind, name) and carries a JSON `data` payload.
	//
	//   kind="provider": LLM provider (name = provider key, e.g. "openai")
	//   kind="channel":  channel adapter (name = channel type, e.g. "telegram")
	//   kind="setting":  config namespace (name = "agents.defaults", "sandbox", …)
	//
	// `credential_key` is only populated for kind="channel" — it's the
	// stable lookup key the inbound dispatcher uses to find the row when a
	// message arrives. `enabled` lets a row hide an outer-scope row in the
	// merge (used by channels: an inner-scope disabled row erases the
	// outer entry).
	ListConfigs(ctx context.Context, kind, scope, scopeID string) ([]ConfigRecord, error)
	GetConfig(ctx context.Context, id string) (*ConfigRecord, error)
	GetConfigByName(ctx context.Context, kind, scope, scopeID, name string) (*ConfigRecord, error)
	SaveConfig(ctx context.Context, c *ConfigRecord) error
	DeleteConfig(ctx context.Context, id string) error
	LookupChannelByCredential(ctx context.Context, channelType, credKey string) (*ConfigRecord, error)

	// --- Cron jobs (per agent) ---
	//
	// Cron rows are owned by an agent; the executing identity is the
	// agent's user_id. List by ownerUserID joins against agents.
	ListCronJobsByOwner(ctx context.Context, ownerUserID string) ([]CronJobRecord, error)
	ListCronJobsByAgent(ctx context.Context, agentID string) ([]CronJobRecord, error)
	GetCronJob(ctx context.Context, jobID string) (*CronJobRecord, error)
	SaveCronJob(ctx context.Context, job *CronJobRecord) error
	DeleteCronJob(ctx context.Context, jobID string) error
	GetDueCronJobs(ctx context.Context, now time.Time) ([]CronJobRecord, error)
	LockCronJob(ctx context.Context, jobID, instanceID string) (bool, error)
	UpdateCronJobRun(ctx context.Context, jobID string, lastRun, nextRun time.Time) error
	// IncrementCronJobFailure atomically bumps failure_count and returns
	// the new count. Used by the scheduler when a tick can't deliver to
	// the configured channel; the caller decides whether to delete the
	// row at threshold.
	IncrementCronJobFailure(ctx context.Context, jobID string) (int, error)

	Close() error
}

// UserRecord is one row of the users table.
//
// Roles: "super_admin" | "user" are first-party humans who log in via
// password / token. "app_user" is provisioned by an api_key on behalf of
// a downstream application; for these rows APIKeyID identifies the key
// that minted them and ExternalID is the calling app's own user
// identifier (free-form). Together they give each external end-user a
// stable fastclaw user_id without anyone logging in.
type UserRecord struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"`
	DisplayName  string    `json:"displayName,omitempty"`
	Role         string    `json:"role"`   // "super_admin" | "user" | "app_user"
	Status       string    `json:"status"` // "active" | "disabled"
	APIKeyID     string    `json:"apikeyId,omitempty"`
	ExternalID   string    `json:"externalId,omitempty"`
	// AvatarURL is a self-contained data: URL ("data:image/png;base64,...")
	// stored inline to avoid a separate blob path. Cap is enforced by the
	// handler at write time (256KB by default). Empty means "no avatar"
	// — UI falls back to initials.
	AvatarURL string `json:"avatarUrl,omitempty"`
	// AgentQuota caps how many agents this user may self-create via
	// POST /api/agents. Semantics:
	//   -1 (default) — unlimited
	//    0          — self-creation forbidden (e.g. single-tenant
	//                 customers whose agent is provisioned by admin)
	//    N > 0      — at most N owned agents at once
	// Admin provisioning paths (POST /api/admin/users/{id}/agents)
	// bypass this — quota only governs caller-initiated creation.
	AgentQuota int64     `json:"agentQuota"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// WebSessionRecord backs cookie-based login state.
type WebSessionRecord struct {
	SID       string    `json:"sid"`
	UserID    string    `json:"userId"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// APIKeyRecord is one row of the apikeys table. KeyHash is SHA256(token);
// the plaintext is shown to the caller exactly once at create/rotate.
//
// Type is the key's authority tier:
//   - "admin": full platform — issues users, manages providers/models/skills
//   - "user":  the apikey owner's own resources — can create agents,
//     access every agent owned by the apikey owner (resolved at auth time)
//   - "agent": locked to the explicit list in apikey_agents — cannot
//     create agents
type APIKeyRecord struct {
	ID        string    `json:"id"`
	UserID    string    `json:"userId"`
	Name      string    `json:"name,omitempty"`
	KeyHash   string    `json:"-"`
	KeyPrefix string    `json:"keyPrefix,omitempty"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"createdAt"`
}

// AgentRecord is the persisted state for one agent. agents.id is globally
// unique; UserID is who owns the agent. The agent itself is the atomic
// unit — sessions, cron jobs, and apikey ACLs all reference agents.id
// directly, never (user_id, agent_id).
// Per-agent model overrides used to live in agents.model; they now live
// in configs as kind=setting, scope=agent, scope_id=<aid>, name=
// "agents.defaults", which is the same path system + user defaults take.
// Resolution happens in loadUserSpace via scope.SettingInto.
// IsPublic flips the "anyone with the link can chat" gate. Default
// false (private — owner-only). When true, requireAgentReadable +
// resolveAgent let any authenticated session lazy-attach the agent
// into their own UserSpace; sessions/memory/agent_files still
// partition per chatter, so only the agent identity is shared.
type AgentRecord struct {
	ID        string                 `json:"id"`
	UserID    string                 `json:"userId"`
	Name      string                 `json:"name"`
	Config    map[string]interface{} `json:"config,omitempty"`
	IsPublic  bool                   `json:"isPublic"`
	CreatedAt time.Time              `json:"createdAt"`
	UpdatedAt time.Time              `json:"updatedAt"`
}

// SessionRecord holds a conversation session.
type SessionRecord struct {
	Messages  []SessionMessage `json:"messages"`
	UpdatedAt time.Time        `json:"updatedAt"`
}

// SessionMessage is a single message in a session.
type SessionMessage struct {
	Role         string                 `json:"role"`
	Content      string                 `json:"content"`
	ContentParts interface{}            `json:"contentParts,omitempty"`
	ToolCalls    interface{}            `json:"toolCalls,omitempty"`
	ToolCallID   string                 `json:"toolCallId,omitempty"`
	Name         string                 `json:"name,omitempty"`
	Metadata     map[string]interface{} `json:"metadata,omitempty"`
	Timestamp    time.Time              `json:"timestamp"`
	Thinking     string                 `json:"thinking,omitempty"`
	RawAssistant json.RawMessage        `json:"rawAssistant,omitempty"`
}

// ChatEventRecord is one row of chat_events — a single delta the
// agent emitted during a turn. Data is opaque JSON whose shape depends
// on Type ("content", "tool_call", "error", "done", ...).
type ChatEventRecord struct {
	UserID     string    `json:"userId,omitempty"`
	AgentID    string    `json:"agentId,omitempty"`
	SessionKey string    `json:"sessionKey,omitempty"`
	Seq        int64     `json:"seq"`
	Type       string    `json:"type"`
	Data       []byte    `json:"data,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}

// SessionMeta is summary info for a session (for listing).
type SessionMeta struct {
	Key          string    `json:"key"`
	Title        string    `json:"title,omitempty"`
	MessageCount int       `json:"messageCount"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// Kinds for ConfigRecord.
const (
	KindProvider = "provider"
	KindChannel  = "channel"
	KindSetting  = "setting"
)

// Scopes for ConfigRecord.
const (
	ScopeSystem = "system"
	ScopeUser   = "user"
	ScopeAgent  = "agent"
	ScopeSkill  = "skill"
)

// ConfigRecord is one row of the configs table — the unified
// home for providers, channels, and namespaced settings.
//
//   - kind says which family this row belongs to
//   - (scope, scope_id) says who owns it
//   - name is the lookup handle inside that family (provider key,
//     channel type, or setting namespace)
//   - data is the family-specific JSON payload
//
// CredentialKey is only meaningful for kind="channel" — see
// LookupChannelByCredential.
type ConfigRecord struct {
	ID            string                 `json:"id"`
	Kind          string                 `json:"kind"`
	Scope         string                 `json:"scope"`
	ScopeID       string                 `json:"scopeId"`
	Name          string                 `json:"name"`
	Enabled       bool                   `json:"enabled"`
	CredentialKey string                 `json:"credentialKey,omitempty"`
	Data          map[string]interface{} `json:"data,omitempty"`
	CreatedAt     time.Time              `json:"createdAt"`
	UpdatedAt     time.Time              `json:"updatedAt"`
}


// CronJobRecord holds a scheduled job. agent_id is mandatory — the
// executing identity is whoever currently owns the agent (looked up via
// agents.user_id at fire time).
type CronJobRecord struct {
	ID        string     `json:"id"`
	AgentID   string     `json:"agentId"`
	Name      string     `json:"name"`
	Type      string     `json:"type"`
	Schedule  string     `json:"schedule"`
	Message   string     `json:"message"`
	Channel   string     `json:"channel"`
	ChatID    string     `json:"chatId"`
	AccountID string     `json:"accountId"`
	Timezone  string     `json:"timezone"`
	Enabled   bool       `json:"enabled"`
	LastRun   *time.Time `json:"lastRun,omitempty"`
	NextRun   *time.Time `json:"nextRun,omitempty"`
	// FailureCount is the number of consecutive fire-attempts whose
	// destination channel was missing/unreachable. UpdateCronJobRun
	// resets it to 0; IncrementCronJobFailure bumps it. The scheduler
	// deletes the row once it crosses an internal threshold.
	FailureCount int       `json:"failureCount,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
}


// StorageType identifies the storage backend.
type StorageType string

const (
	StoragePostgres StorageType = "postgres"
	StorageSQLite   StorageType = "sqlite"
)

// StorageConfig holds DB credentials. Populated from FASTCLAW_STORAGE_* env vars at boot.
type StorageConfig struct {
	Type        StorageType `json:"type"`
	DSN         string      `json:"dsn,omitempty"`
	AutoMigrate bool        `json:"autoMigrate,omitempty"`
}
