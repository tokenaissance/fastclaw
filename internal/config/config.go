package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultUserID is the user ID used in single-user/local mode.
// In cloud mode, real user IDs come from authenticated requests; in local mode
// everything is scoped under this single default user.
const DefaultUserID = "local"

type userIDKey struct{}

// WithUserID returns a new context carrying the given user ID.
func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, userIDKey{}, userID)
}

// UserIDFromContext extracts the user ID from context, falling back to
// DefaultUserID if none is set (local mode).
func UserIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return DefaultUserID
	}
	if v, ok := ctx.Value(userIDKey{}).(string); ok && v != "" {
		return v
	}
	return DefaultUserID
}

// HasUserID reports whether the context carries an explicit user ID (i.e. an
// auth middleware resolved a caller). Handlers on bootstrap endpoints that
// are reachable unauthenticated use this to gate user-scoped fields, since
// UserIDFromContext would otherwise silently return DefaultUserID.
func HasUserID(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, ok := ctx.Value(userIDKey{}).(string)
	return ok && v != ""
}

// MCPServerConfig holds configuration for a single MCP server.
type MCPServerConfig struct {
	Type    string            `json:"type"`              // "http" or "stdio"
	URL     string            `json:"url,omitempty"`     // for http
	Headers map[string]string `json:"headers,omitempty"` // for http
	Command string            `json:"command,omitempty"` // for stdio
	Args    []string          `json:"args,omitempty"`    // for stdio
	Env     map[string]string `json:"env,omitempty"`     // for stdio
}

// CronJob defines a scheduled job in the configuration.
type CronJob struct {
	Name     string `json:"name"`
	Type     string `json:"type"`     // "exact", "interval", "cron"
	Schedule string `json:"schedule"` // depends on type
	AgentID  string `json:"agentId"`
	Channel  string `json:"channel"`
	ChatID   string `json:"chatId"`
	Message  string `json:"message"`
}

// HeartbeatCfg holds heartbeat configuration.
type HeartbeatCfg struct {
	IntervalMinutes int `json:"intervalMinutes,omitempty"` // default 30
}

// StorageCfg configures the storage backend.
// Default: file-based. For cloud multi-tenant: "postgres" or "sqlite".
type StorageCfg struct {
	Type        string `json:"type,omitempty"`        // "file" (default), "postgres", "sqlite"
	DSN         string `json:"dsn,omitempty"`          // database connection string
	AutoMigrate bool   `json:"autoMigrate,omitempty"` // auto-create tables on startup
}

// ObjectStoreCfg controls the object-storage backend that holds agent-
// produced artifacts (generated PDFs/images/audio, workspace files…).
// Internally the gateway still speaks `workspace.Store`; this is the
// operator-facing config for where those bytes actually go.
//
// Type picks the provider. For hosted S3-compat providers the factory
// fills in a preset endpoint from Region / AccountID so operators don't
// need to memorise vendor URLs.
//
// Valid Type values:
//   "", "local"     — pod-local filesystem (single-host only)
//   "aws-s3"        — AWS S3
//   "cloudflare-r2" — Cloudflare R2
//   "backblaze-b2"  — Backblaze B2 S3-compat
//   "aliyun-oss"    — Aliyun OSS
//   "minio"         — Self-hosted MinIO (needs explicit S3.Endpoint)
//   "s3"            — Any other S3-compat; needs explicit S3.Endpoint
type ObjectStoreCfg struct {
	Type         string              `json:"type,omitempty"`
	Local        ObjectStoreLocalCfg `json:"local,omitempty"`
	S3           ObjectStoreS3Cfg    `json:"s3,omitempty"`
	AccountID    string              `json:"accountId,omitempty"`      // Cloudflare R2
	AliyunIntern bool                `json:"aliyunInternal,omitempty"` // prefer OSS -internal endpoint
}

// ObjectStoreLocalCfg configures the local-filesystem backend.
type ObjectStoreLocalCfg struct {
	Root string `json:"root,omitempty"` // default ~/.fastclaw/workspaces
}

// ObjectStoreS3Cfg configures any S3-compatible backend.
type ObjectStoreS3Cfg struct {
	Endpoint  string `json:"endpoint,omitempty"`
	Region    string `json:"region,omitempty"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix,omitempty"`
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
	UseSSL    bool   `json:"useSSL"`
}



// WebSearchCfg is the legacy single-provider web search config, kept for
// back-compat reading of older fastclaw.json files. New config uses
// ToolProviders + Tools below; migration is lossless and one-way (handled
// by MigrateLegacyWebSearch).
type WebSearchCfg struct {
	Provider string `json:"provider,omitempty"` // "brave"
	APIKey   string `json:"apiKey,omitempty"`
}

// ToolProviderCfg holds the credentials/endpoint for one provider (e.g.
// "exa" or "searxng"), shared across every tool category that uses it. Keys
// live here so the admin UI has one place to configure them; tool
// categories below just reference providers by name.
type ToolProviderCfg struct {
	APIKey   string            `json:"apiKey,omitempty"`
	Endpoint string            `json:"endpoint,omitempty"`
	Options  map[string]string `json:"options,omitempty"`
}

// ToolCategoryCfg chooses which provider(s) back a tool category
// (e.g. web_search, image_gen). "primary" is tried first; if it misses and
// autoFallback is true (the default), "fallbacks" are tried in order.
// References are "<provider>/<model>" strings; the "<model>" suffix is
// provider-specific (e.g. "exa/auto", "openai/gpt-image-1").
type ToolCategoryCfg struct {
	Primary      string   `json:"primary,omitempty"`
	Fallbacks    []string `json:"fallbacks,omitempty"`
	AutoFallback *bool    `json:"autoFallback,omitempty"`
}

// FallbackEnabled returns the effective autoFallback flag, defaulting to true.
func (c ToolCategoryCfg) FallbackEnabled() bool {
	if c.AutoFallback == nil {
		return true
	}
	return *c.AutoFallback
}

// Chain returns [primary, fallbacks...] with empty entries filtered out.
func (c ToolCategoryCfg) Chain() []string {
	var out []string
	if c.Primary != "" {
		out = append(out, c.Primary)
	}
	for _, f := range c.Fallbacks {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// HooksCfg configures the webhook ingress server.
type HooksCfg struct {
	Enabled bool   `json:"enabled,omitempty"`
	Token   string `json:"token,omitempty"`
	Path    string `json:"path,omitempty"`  // default "/hooks"
	Port    int    `json:"port,omitempty"`  // default 18954
}

// PluginsCfg configures the plugin system.
type PluginsCfg struct {
	Enabled bool                       `json:"enabled"`
	Paths   []string                   `json:"paths,omitempty"`
	Entries map[string]PluginEntryCfg  `json:"entries,omitempty"`
}

// PluginEntryCfg is per-plugin configuration.
type PluginEntryCfg struct {
	Enabled bool                   `json:"enabled"`
	Config  map[string]interface{} `json:"config,omitempty"`
}

// TaskQueueCfg configures the task queue.
type TaskQueueCfg struct {
	MaxConcurrent  int `json:"maxConcurrent,omitempty"`  // default 10
	TaskTimeoutSec int `json:"taskTimeoutSec,omitempty"` // default 300
}

// SandboxCfg holds sandbox configuration for an agent.
type SandboxCfg struct {
	Enabled  bool   `json:"enabled"`
	Image    string `json:"image,omitempty"`
	Policy   string `json:"policy,omitempty"`  // policy preset name
	Backend  string `json:"backend,omitempty"` // "docker" (default), "e2b"
	E2BKey   string `json:"e2bKey,omitempty"`  // E2B API key (fallback to E2B_API_KEY env)
	// IdleTTLSec is how long a sandbox may sit unused before the lifecycle
	// pool destroys it. The next tool call lazily recreates. Set to 0 to
	// disable eviction (old behavior: sandboxes live until pod shutdown).
	// Default (when Enabled && IdleTTLSec==0): 600 seconds = 10 minutes.
	IdleTTLSec int `json:"idleTTLSec,omitempty"`
}

// GatewayAuth holds authentication settings for the gateway API.
type GatewayAuth struct {
	Mode  string `json:"mode,omitempty"`  // "token" (default), "none"
	Token string `json:"token"`
}

// GatewayHTTPEndpoints controls which HTTP endpoints are enabled.
type GatewayHTTPEndpoints struct {
	ChatCompletions GatewayEndpoint `json:"chatCompletions,omitempty"`
	Agents          GatewayEndpoint `json:"agents,omitempty"`
}

// GatewayEndpoint toggles a single HTTP endpoint.
type GatewayEndpoint struct {
	Enabled bool `json:"enabled"`
}

// GatewayHTTP holds HTTP-specific gateway settings.
type GatewayHTTP struct {
	Endpoints GatewayHTTPEndpoints `json:"endpoints,omitempty"`
}

// GatewayCfg holds gateway server configuration.
type GatewayCfg struct {
	Port      int          `json:"port,omitempty"`
	Mode      string       `json:"mode,omitempty"`  // "local" (default), "cloud"
	Bind      string       `json:"bind,omitempty"`  // "loopback" (default), "all"
	Auth      GatewayAuth  `json:"auth,omitempty"`
	HTTP      GatewayHTTP  `json:"http,omitempty"`
	RateLimit RateLimitCfg `json:"rateLimit,omitempty"`
}

// RateLimitCfg controls per-user rate limiting on the HTTP API.
type RateLimitCfg struct {
	RPM int `json:"rpm,omitempty"` // requests per minute per user (0 = unlimited)
}

// MemoryCfg holds memory system configuration.
type MemoryCfg struct {
	AutoPersist AutoPersistCfg `json:"autoPersist,omitempty"`
	FTS         FTSCfg         `json:"fts,omitempty"`
}

// AutoPersistCfg controls automatic memory persistence after agent turns.
type AutoPersistCfg struct {
	Enabled     bool   `json:"enabled"`                  // default true
	EveryNTurns int    `json:"everyNTurns,omitempty"`    // default 5
	Model       string `json:"model,omitempty"`           // override model for extraction
}

// FTSCfg configures full-text search.
type FTSCfg struct {
	Enabled bool   `json:"enabled"`
	DBPath  string `json:"dbPath,omitempty"`
}

// PrivacyCfg holds privacy-related settings.
type PrivacyCfg struct {
	PIIScrubbing PIIScrubCfg `json:"piiScrubbing,omitempty"`
}

// PIIScrubCfg controls PII scrubbing before LLM calls.
type PIIScrubCfg struct {
	Enabled bool `json:"enabled"` // default false — opt-in
}

// SkillsLearnerCfg configures the skills learning loop.
type SkillsLearnerCfg struct {
	Enabled      bool   `json:"enabled"`               // default false — opt-in
	MinToolCalls int    `json:"minToolCalls,omitempty"` // default 3
	Model        string `json:"model,omitempty"`        // override model
}

// Config is the top-level configuration loaded from ~/.fastclaw/fastclaw.json.
type Config struct {
	Providers  map[string]ProviderConfig  `json:"providers"`
	Agents     AgentsConfig               `json:"agents"`
	Channels   map[string]ChannelConfig   `json:"channels"`
	Bindings   []Binding                  `json:"bindings,omitempty"`
	Teams      map[string]TeamEntry       `json:"teams,omitempty"`
	MCPServers map[string]MCPServerConfig `json:"mcpServers,omitempty"`
	CronJobs   []CronJob                  `json:"cronJobs,omitempty"`
	Heartbeat  HeartbeatCfg               `json:"heartbeat,omitempty"`
	Storage    StorageCfg                 `json:"storage,omitempty"`
	Sandbox    SandboxCfg                 `json:"sandbox,omitempty"`
	WebSearch     WebSearchCfg               `json:"webSearch,omitempty"` // legacy; migrated into ToolProviders/Tools on load
	ToolProviders map[string]ToolProviderCfg `json:"toolProviders,omitempty"`
	Tools         map[string]ToolCategoryCfg `json:"tools,omitempty"`
	// ObjectStore configures the blob backend for agent-produced artifacts
	// (AWS S3 / Cloudflare R2 / Aliyun OSS / MinIO / local filesystem).
	ObjectStore ObjectStoreCfg `json:"objectStore,omitempty"`
	Hooks         HooksCfg                   `json:"hooks,omitempty"`
	Plugins       PluginsCfg                 `json:"plugins,omitempty"`
	Gateway    GatewayCfg                 `json:"gateway,omitempty"`
	TaskQueue  TaskQueueCfg               `json:"taskQueue,omitempty"`
	Skills        SkillsCfg                  `json:"skills,omitempty"`
	Memory        MemoryCfg                  `json:"memory,omitempty"`
	Privacy       PrivacyCfg                 `json:"privacy,omitempty"`
	SkillsLearner SkillsLearnerCfg           `json:"skillsLearner,omitempty"`
}

// ModelCost holds pricing info for a model.
type ModelCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

// ModelEntry describes a single model within a provider.
type ModelEntry struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Reasoning     bool     `json:"reasoning"`
	Input         []string `json:"input"`
	Cost          ModelCost `json:"cost"`
	ContextWindow int      `json:"contextWindow"`
	MaxTokens     int      `json:"maxTokens"`
}

// ProviderConfig holds API credentials for an LLM provider.
type ProviderConfig struct {
	APIKey   string       `json:"apiKey"`
	APIBase  string       `json:"apiBase"`
	APIType  string       `json:"apiType,omitempty"`
	AuthType string       `json:"authType,omitempty"`
	Models   []ModelEntry `json:"models,omitempty"`
}

// UnmarshalJSON handles backward compatibility: reads "api" as "apiType".
func (pc *ProviderConfig) UnmarshalJSON(data []byte) error {
	type Alias ProviderConfig
	aux := &struct {
		*Alias
		API string `json:"api,omitempty"`
	}{Alias: (*Alias)(pc)}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	if pc.APIType == "" && aux.API != "" {
		pc.APIType = aux.API
	}
	return nil
}

// AgentsConfig holds agent defaults and the list of agent entries.
type AgentsConfig struct {
	Defaults AgentDefaults `json:"defaults"`
}

// AgentDefaults holds fallback values for all agents.
type AgentDefaults struct {
	Model             string  `json:"model,omitempty"`
	MaxTokens         int     `json:"maxTokens,omitempty"`
	Temperature       float64 `json:"temperature,omitempty"`
	MaxToolIterations int     `json:"maxToolIterations,omitempty"`
	Thinking          string  `json:"thinking,omitempty"`
	PolicyPreset      string  `json:"policy,omitempty"`
}

// AgentEntry is a per-agent entry — populated by merging agent.json with
// config defaults. The `workspace` field here is the agent's working dir
// for user-facing files (overrides the default ~/.fastclaw/workspaces/{id}).
// The agent's home dir (SOUL.md, sessions, memory) is always derived from
// the agent ID via AgentHomeDir.
type AgentEntry struct {
	ID                string                     `json:"id"`
	Workspace         string                     `json:"workspace,omitempty"`
	Model             string                     `json:"model,omitempty"`
	TemplateID        string                     `json:"templateId,omitempty"` // optional: inherit identity files from this template

	MaxTokens         int                        `json:"maxTokens,omitempty"`
	Temperature       float64                    `json:"temperature,omitempty"`
	MaxToolIterations int                        `json:"maxToolIterations,omitempty"`
	Skills            []string                   `json:"skills,omitempty"`
	Tools             []string                   `json:"tools,omitempty"`
	MCPServers        map[string]MCPServerConfig `json:"mcpServers,omitempty"`
	AlwaysLoadSkills  []string                   `json:"alwaysLoadSkills,omitempty"`
	Thinking          string                     `json:"thinking,omitempty"` // off, low, medium, high, adaptive
	Sandbox           SandboxCfg                 `json:"sandbox,omitempty"`
	PolicyPreset      string                     `json:"policy,omitempty"`  // "permissive", "standard", "restricted"
}

// ChannelConfig holds per-channel configuration with optional accounts.
type ChannelConfig struct {
	Enabled  bool                     `json:"enabled"`
	BotToken string                   `json:"botToken,omitempty"`
	AppToken string                   `json:"appToken,omitempty"` // for Slack Socket Mode
	Accounts map[string]AccountConfig `json:"accounts,omitempty"`
}

// AccountConfig holds account-specific overrides within a channel.
type AccountConfig struct {
	BotToken string `json:"botToken,omitempty"`
}

// Binding maps a match pattern to an agent.
type Binding struct {
	AgentID string `json:"agentId"`
	Match   Match  `json:"match"`
}

// Match defines criteria for routing a message to an agent.
type Match struct {
	Channel   string `json:"channel"`
	AccountID string `json:"accountId,omitempty"`
	Peer      *Peer  `json:"peer,omitempty"`
}

// Peer specifies peer matching criteria.
type Peer struct {
	Kind string `json:"kind,omitempty"` // "group" or "dm"
	ID   string `json:"id,omitempty"`   // specific chat/group ID
}

// AgentFileConfig is the schema for agent.json inside an agent workspace.
type AgentFileConfig struct {
	Model             string                     `json:"model,omitempty"`
	MaxTokens         int                        `json:"maxTokens,omitempty"`
	Temperature       float64                    `json:"temperature,omitempty"`
	MaxToolIterations int                        `json:"maxToolIterations,omitempty"`
	// Workspace overrides the default working directory (where the agent
	// saves user-facing files). Supports `~/` expansion. Leave empty to
	// use the default `~/.fastclaw/workspaces/{id}`.
	Workspace  string                     `json:"workspace,omitempty"`
	Skills     SkillsConfig               `json:"skills,omitempty"`
	MCPServers map[string]MCPServerConfig `json:"mcpServers,omitempty"`
	// ToolProviders overrides / supplements global credentials for specific
	// providers. Keys declared here shadow the same provider in the global
	// toolProviders map.
	ToolProviders map[string]ToolProviderCfg `json:"toolProviders,omitempty"`
	// Tools overrides the chain for specific categories (e.g. this agent
	// prefers a different primary than the global default). Categories not
	// listed here inherit the global chain.
	Tools map[string]ToolCategoryCfg `json:"tools,omitempty"`
	// Providers holds LLM provider credentials that are scoped to this
	// agent only. Entries shadow the global providers map by key name; the
	// admin UI writes agent-exclusive providers here instead of the
	// global config.
	Providers map[string]ProviderConfig `json:"providers,omitempty"`
}

// SkillsConfig controls skill loading for an agent.
type SkillsConfig struct {
	Disabled   []string `json:"disabled,omitempty"`
	AlwaysLoad []string `json:"alwaysLoad,omitempty"`
}

// SkillsCfg is the top-level skills configuration (global).
type SkillsCfg struct {
	Install    SkillsInstallCfg         `json:"install,omitempty"`
	Entries    map[string]SkillEntryCfg `json:"entries,omitempty"`
	Load       SkillsLoadCfg            `json:"load,omitempty"`
	AlwaysLoad []string                 `json:"alwaysLoad,omitempty"`
}

// SkillsInstallCfg configures skill installation behavior.
type SkillsInstallCfg struct {
	NodeManager string `json:"nodeManager,omitempty"` // npm, pnpm, bun
}

// SkillEntryCfg is per-skill configuration.
type SkillEntryCfg struct {
	Enabled bool              `json:"enabled"`
	APIKey  string            `json:"apiKey,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// SkillsLoadCfg configures extra skill directories.
type SkillsLoadCfg struct {
	ExtraDirs []string `json:"extraDirs,omitempty"`
}

// ResolvedAgent is the fully merged config for a single agent.
type ResolvedAgent struct {
	ID                string
	Home              string // agent's own directory: SOUL.md, IDENTITY.md, sessions, memory, skills
	Workspace         string // working directory where agent creates user-facing files
	Model             string
	TemplateID        string // optional: agent_id of a template whose identity files this agent inherits
	MaxTokens         int
	Temperature       float64
	MaxToolIterations int
	Thinking          string
	Skills            SkillsConfig
	MCPServers        map[string]MCPServerConfig
	Sandbox           SandboxCfg
	PolicyPreset      string
	// ToolProviders is the per-agent credentials view: it starts from the
	// global toolProviders map and merges in any overrides from agent.json.
	ToolProviders map[string]ToolProviderCfg
	// Tools is the per-agent view of category→chain config (primary +
	// fallbacks), with agent.json entries shadowing the global map.
	Tools map[string]ToolCategoryCfg
	// Providers is the per-agent LLM-provider credentials view: starts from
	// the global providers map and merges in any overrides from agent.json.
	// Lets each agent pin its own API key + base URL for models that only
	// it should see (e.g. a personal OpenRouter account).
	Providers map[string]ProviderConfig
}

// TeamEntry defines a team of agents with group chat behavior settings.
type TeamEntry struct {
	Agents        []string `json:"agents"`
	DefaultAgent  string   `json:"defaultAgent,omitempty"`
	GroupBehavior string   `json:"groupBehavior,omitempty"` // "mention-only" (default) or "default-agent"
}

// TeamConfig is the schema for team.json.
type TeamConfig struct {
	Name    string            `json:"name"`
	Agents  []string          `json:"agents"`
	Routing map[string]string `json:"routing"`
}

// HomeDir returns the FastClaw root directory — `$FASTCLAW_HOME` if set,
// otherwise `~/.fastclaw`. The env override exists so multiple FastClaw
// instances can run side-by-side (one per agent product) without
// fighting over the same ~/.fastclaw — useful during local debugging
// of imgany / copyweb / podlm in parallel without Docker.
func HomeDir() (string, error) {
	if h := os.Getenv("FASTCLAW_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".fastclaw"), nil
}

// UserDir returns ~/.fastclaw (kept for backward compat, ignores userID).
func UserDir(userID ...string) (string, error) {
	return HomeDir()
}

// EnsureUserDir creates ~/.fastclaw and ~/.fastclaw/agents if needed.
func EnsureUserDir(userID ...string) (string, error) {
	home, err := HomeDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(home, "agents"), 0o755); err != nil {
		return "", err
	}
	return home, nil
}

// AgentHomeDir returns ~/.fastclaw/agents/{agentID}/agent — the agent's own
// directory where its identity files (SOUL.md, IDENTITY.md), sessions, memory
// and skills live. Think of it as the agent's "home".
func AgentHomeDir(agentID string) (string, error) {
	home, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "agents", agentID, "agent"), nil
}

// AgentWorkspaceDir returns ~/.fastclaw/workspaces/{agentID} — the agent's
// working directory for user-facing artifacts (files it creates for or with
// the user). Separate from AgentHomeDir so identity files and user work
// don't share a directory.
func AgentWorkspaceDir(agentID string) (string, error) {
	home, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "workspaces", agentID), nil
}

func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}

// UserConfigPath returns the full path to the config file for the given user.
func UserConfigPath(userID string) (string, error) {
	dir, err := UserDir(userID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "fastclaw.json"), nil
}

// Load reads and parses the global config from ~/.fastclaw/fastclaw.json.
// Falls back to the legacy per-user path for backwards compatibility.
func Load() (*Config, error) {
	home, err := HomeDir()
	if err != nil {
		return nil, err
	}
	globalPath := filepath.Join(home, "fastclaw.json")
	if _, err := os.Stat(globalPath); err == nil {
		return loadConfigFile(globalPath)
	}
	// Fallback: legacy per-user path
	return LoadForUser(DefaultUserID)
}

// LoadOrEmpty returns the parsed config if a fastclaw.json exists, or an
// empty Config (with defaults applied) when it doesn't. Callers that run
// in cloud/K8s mode — where infra comes from env and product config is
// persisted in the DB store — should prefer this over Load() so a missing
// file doesn't break hot-reload paths.
func LoadOrEmpty() *Config {
	cfg, err := Load()
	if err == nil {
		return cfg
	}
	if !os.IsNotExist(err) && !isNotFound(err) {
		// Malformed file (not "doesn't exist") is still surfaced via a
		// fresh config — callers downstream may log it — but we never
		// block the hot-reload here.
	}
	empty := &Config{}
	applyDefaults(empty)
	return empty
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	return os.IsNotExist(err) ||
		// loadConfigFile wraps the os.ReadFile error — check for the
		// canonical "no such file or directory" substring as a fallback.
		(err.Error() != "" && strings.Contains(err.Error(), "no such file"))
}

func loadConfigFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	applyDefaults(&cfg)
	MigrateLegacyWebSearch(&cfg)
	return &cfg, nil
}

// GlobalConfigPath returns ~/.fastclaw/fastclaw.json.
func GlobalConfigPath() (string, error) {
	home, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "fastclaw.json"), nil
}

// LoadForUser reads and parses ~/.fastclaw/users/{userID}/fastclaw.json.
func LoadForUser(userID string) (*Config, error) {
	if err := MigrateLegacyLayout(); err != nil {
		return nil, fmt.Errorf("migrate legacy layout: %w", err)
	}

	configPath, err := UserConfigPath(userID)
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}

	return loadConfigFile(configPath)
}

// ApplyDefaults fills in zero-valued knobs on Agents.Defaults. Idempotent
// — safe to call after every cfg mutation (loadConfigFile already does;
// gateway.New repeats it after the store overlay so a stored config that
// omits MaxToolIterations doesn't end up at 0 and immediately trip the
// "max tool iterations reached" guard on the very first turn).
func ApplyDefaults(cfg *Config) {
	if cfg.Agents.Defaults.MaxTokens == 0 {
		cfg.Agents.Defaults.MaxTokens = 8192
	}
	if cfg.Agents.Defaults.Temperature == 0 {
		cfg.Agents.Defaults.Temperature = 0.7
	}
	if cfg.Agents.Defaults.MaxToolIterations == 0 {
		cfg.Agents.Defaults.MaxToolIterations = 20
	}
}

// applyDefaults is the unexported alias kept for the existing in-package
// callers; same logic, same guarantees.
func applyDefaults(cfg *Config) { ApplyDefaults(cfg) }

// MergedAgentConfig merges defaults with an agent entry and its workspace agent.json
// to produce a fully resolved agent config. Priority: agent.json > entry > defaults.
// Uses DefaultUserID for workspace path resolution; for cloud users call
// MergedAgentConfigForUser instead.
func (cfg *Config) MergedAgentConfig(entry AgentEntry) ResolvedAgent {
	return cfg.MergedAgentConfigForUser(entry, DefaultUserID)
}

// MergedAgentConfigForUser is like MergedAgentConfig but resolves the
// workspace path under the given user's directory.
func (cfg *Config) MergedAgentConfigForUser(entry AgentEntry, userID string) ResolvedAgent {
	home, _ := AgentHomeDir(entry.ID)
	workspace := expandPath(entry.Workspace)
	if workspace == "" {
		workspace, _ = AgentWorkspaceDir(entry.ID)
	}

	resolved := ResolvedAgent{
		ID:                entry.ID,
		Home:              home,
		Workspace:         workspace,
		Model:             cfg.Agents.Defaults.Model,
		TemplateID:        entry.TemplateID,
		MaxTokens:         cfg.Agents.Defaults.MaxTokens,
		Temperature:       cfg.Agents.Defaults.Temperature,
		MaxToolIterations: cfg.Agents.Defaults.MaxToolIterations,
		Thinking:          cfg.Agents.Defaults.Thinking,
		Sandbox:           cfg.Sandbox,
		PolicyPreset:      cfg.Agents.Defaults.PolicyPreset,
	}

	// Layer 2: per-agent entry overrides
	if entry.Model != "" {
		resolved.Model = entry.Model
	}
	if entry.MaxTokens > 0 {
		resolved.MaxTokens = entry.MaxTokens
	}
	if entry.Temperature > 0 {
		resolved.Temperature = entry.Temperature
	}
	if entry.MaxToolIterations > 0 {
		resolved.MaxToolIterations = entry.MaxToolIterations
	}
	if entry.Thinking != "" {
		resolved.Thinking = entry.Thinking
	}
	if entry.Sandbox.Enabled {
		resolved.Sandbox = entry.Sandbox
	}
	if entry.PolicyPreset != "" {
		resolved.PolicyPreset = entry.PolicyPreset
	}

	// Start with global MCP servers
	if len(cfg.MCPServers) > 0 {
		resolved.MCPServers = make(map[string]MCPServerConfig, len(cfg.MCPServers))
		for k, v := range cfg.MCPServers {
			resolved.MCPServers[k] = v
		}
	}

	// Seed LLM providers from global — agent.json entries below shadow by key.
	if len(cfg.Providers) > 0 {
		resolved.Providers = make(map[string]ProviderConfig, len(cfg.Providers))
		for k, v := range cfg.Providers {
			resolved.Providers[k] = v
		}
	}

	// Seed tool config from the global config. agent.json (layer 3) overlays
	// individual providers/categories below, so agents inherit shared keys
	// until they explicitly shadow one.
	if len(cfg.ToolProviders) > 0 {
		resolved.ToolProviders = make(map[string]ToolProviderCfg, len(cfg.ToolProviders))
		for k, v := range cfg.ToolProviders {
			resolved.ToolProviders[k] = v
		}
	}
	if len(cfg.Tools) > 0 {
		resolved.Tools = make(map[string]ToolCategoryCfg, len(cfg.Tools))
		for k, v := range cfg.Tools {
			resolved.Tools[k] = v
		}
	}

	// Layer 3: agent.json in the agent's home dir (not workspace — that's
	// the user-facing content dir). Agent-level overrides shadow global.
	agentJSON := filepath.Join(home, "agent.json")
	if data, readErr := os.ReadFile(agentJSON); readErr == nil {
		var fileCfg AgentFileConfig
		if jsonErr := json.Unmarshal(data, &fileCfg); jsonErr == nil {
			if fileCfg.Model != "" {
				resolved.Model = fileCfg.Model
			}
			if fileCfg.MaxTokens > 0 {
				resolved.MaxTokens = fileCfg.MaxTokens
			}
			if fileCfg.Temperature > 0 {
				resolved.Temperature = fileCfg.Temperature
			}
			if fileCfg.MaxToolIterations > 0 {
				resolved.MaxToolIterations = fileCfg.MaxToolIterations
			}
			resolved.Skills = fileCfg.Skills

			// Merge per-agent MCP servers (agent-level overrides global)
			for k, v := range fileCfg.MCPServers {
				if resolved.MCPServers == nil {
					resolved.MCPServers = make(map[string]MCPServerConfig)
				}
				resolved.MCPServers[k] = v
			}

			// Merge per-agent LLM providers (whole-entry replace by key).
			for name, pc := range fileCfg.Providers {
				if resolved.Providers == nil {
					resolved.Providers = make(map[string]ProviderConfig)
				}
				resolved.Providers[name] = pc
			}

			// Merge per-agent tool providers (whole-entry replace: a provider
			// declared at the agent level replaces the global one entirely —
			// keys don't deep-merge, which avoids surprising half-populated
			// credentials).
			for name, pc := range fileCfg.ToolProviders {
				if resolved.ToolProviders == nil {
					resolved.ToolProviders = make(map[string]ToolProviderCfg)
				}
				resolved.ToolProviders[name] = pc
			}
			// Merge per-agent tool categories. An agent declaring
			// tools.web_search fully replaces the global web_search chain;
			// categories it doesn't mention still inherit the global chain.
			for cat, tc := range fileCfg.Tools {
				if resolved.Tools == nil {
					resolved.Tools = make(map[string]ToolCategoryCfg)
				}
				resolved.Tools[cat] = tc
			}
		}
	}

	return resolved
}

// ResolveAgents produces resolved agent configs from config.agents.list.
// If no agents are listed, creates a single "default" agent.
// ResolveAgents discovers agents from the filesystem (~/.fastclaw/agents/)
// and merges each one's config with the global defaults.
func ResolveAgents(cfg *Config) []ResolvedAgent {
	return ResolveAgentsForUser(cfg, "")
}

// ResolveAgentsForUser is like ResolveAgents (userID kept for backward compat, ignored).
func ResolveAgentsForUser(cfg *Config, userID string) []ResolvedAgent {
	return ResolveAgentsWithExtra(cfg, userID, nil)
}

// ResolveAgentsWithExtra merges filesystem-discovered agents with an extra
// list provided by the caller (e.g. agent IDs read from the Store in
// Postgres-backed deployments). De-duplicates by ID; filesystem entries win
// on overlap since they carry agent.json overrides. Used by the gateway to
// build a consistent agent list across pods whether they have a local
// filesystem copy or not.
func ResolveAgentsWithExtra(cfg *Config, userID string, extra []string) []ResolvedAgent {
	entries := DiscoverAgents()
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		seen[e.ID] = true
	}
	for _, id := range extra {
		if id != "" && !seen[id] {
			entries = append(entries, AgentEntry{ID: id})
			seen[id] = true
		}
	}
	if len(entries) == 0 {
		// Still empty — the very first run on a fresh pod; keep the
		// historical default so the UI doesn't break before the user has
		// created anything.
		entries = []AgentEntry{{ID: "default"}}
	}

	agents := make([]ResolvedAgent, 0, len(entries))
	for _, entry := range entries {
		agents = append(agents, cfg.MergedAgentConfigForUser(entry, userID))
	}
	return agents
}

// DiscoverAgents scans ~/.fastclaw/agents/ for agent directories.
// Each subdirectory with an agent/ subfolder is treated as an agent.
func DiscoverAgents() []AgentEntry {
	home, err := HomeDir()
	if err != nil {
		return nil
	}
	agentsDir := filepath.Join(home, "agents")
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		return nil
	}

	var agents []AgentEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		agentID := e.Name()
		wsDir := filepath.Join(agentsDir, agentID, "agent")
		if _, err := os.Stat(wsDir); err != nil {
			continue
		}

		entry := AgentEntry{ID: agentID}

		// Read agent.json if exists for model override etc.
		if data, err := os.ReadFile(filepath.Join(wsDir, "agent.json")); err == nil {
			json.Unmarshal(data, &entry)
			entry.ID = agentID // ensure ID matches directory name
		}

		agents = append(agents, entry)
	}
	return agents
}

// LoadTeam reads a team.json file.
func LoadTeam(path string) (*TeamConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tc TeamConfig
	if err := json.Unmarshal(data, &tc); err != nil {
		return nil, err
	}
	return &tc, nil
}
