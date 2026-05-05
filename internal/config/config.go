// Package config holds runtime configuration types and ctx user-id plumbing.
//
// There is no fastclaw.json. Bootstrap settings (port, bind, storage DSN,
// sandbox backend) come from FASTCLAW_* env vars; user-facing config (providers,
// channels, agents, etc.) lives in the database. The Config struct here is
// the in-memory snapshot the gateway assembles at boot from those sources;
// callers never read it from disk.
package config

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type userIDKey struct{}

// WithUserID stamps a resolved user_id onto ctx. Auth middleware does this
// after validating a session cookie or apikey; nothing else should.
func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, userIDKey{}, userID)
}

// UserIDFromContext extracts the resolved user_id, or "" if none.
//
// There is no DefaultUserID fallback. Code paths that reach the store
// without a real user_id are bugs — the auth middleware should have 401'd
// the request, the cron tick should have read the job's owner from the
// row, the channel ingress should have resolved the credential. Catch
// these in development by panicking on store calls with empty user_id.
func UserIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(userIDKey{}).(string); ok {
		return v
	}
	return ""
}

// MustUserIDFromContext returns the resolved user_id or an error. Use this
// at handler boundaries where missing identity is a 500-level bug rather
// than a normal flow.
func MustUserIDFromContext(ctx context.Context) (string, error) {
	uid := UserIDFromContext(ctx)
	if uid == "" {
		return "", errors.New("config: request context has no user_id (auth middleware bug)")
	}
	return uid, nil
}

// MCPServerConfig holds configuration for a single MCP server.
type MCPServerConfig struct {
	Type    string            `json:"type"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// CronJob defines a scheduled job loaded into the gateway's runtime.
type CronJob struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Schedule    string `json:"schedule"`
	OwnerUserID string `json:"ownerUserId,omitempty"`
	AgentID     string `json:"agentId"`
	Channel     string `json:"channel"`
	ChatID      string `json:"chatId"`
	Message     string `json:"message"`
}

// HeartbeatCfg holds heartbeat configuration.
type HeartbeatCfg struct {
	IntervalMinutes int `json:"intervalMinutes,omitempty"`
}

// StorageCfg mirrors the bootstrap storage block so existing code that reads it
// off Config keeps working without an extra parameter plumbed through.
type StorageCfg struct {
	Type        string `json:"type,omitempty"`
	DSN         string `json:"dsn,omitempty"`
	AutoMigrate bool   `json:"autoMigrate,omitempty"`
}

// ObjectStoreCfg controls the object-storage backend.
type ObjectStoreCfg struct {
	Type         string              `json:"type,omitempty"`
	Local        ObjectStoreLocalCfg `json:"local,omitempty"`
	S3           ObjectStoreS3Cfg    `json:"s3,omitempty"`
	AccountID    string              `json:"accountId,omitempty"`
	AliyunIntern bool                `json:"aliyunInternal,omitempty"`
}

type ObjectStoreLocalCfg struct {
	Root string `json:"root,omitempty"`
}

type ObjectStoreS3Cfg struct {
	Endpoint  string `json:"endpoint,omitempty"`
	Region    string `json:"region,omitempty"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix,omitempty"`
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
	UseSSL    bool   `json:"useSSL"`
}

// ToolProviderCfg holds credentials/endpoint for one provider (e.g. "exa").
type ToolProviderCfg struct {
	APIKey   string            `json:"apiKey,omitempty"`
	Endpoint string            `json:"endpoint,omitempty"`
	Options  map[string]string `json:"options,omitempty"`
}

// ToolCategoryCfg chooses which provider(s) back a tool category.
type ToolCategoryCfg struct {
	Primary      string   `json:"primary,omitempty"`
	Fallbacks    []string `json:"fallbacks,omitempty"`
	AutoFallback *bool    `json:"autoFallback,omitempty"`
}

func (c ToolCategoryCfg) FallbackEnabled() bool {
	if c.AutoFallback == nil {
		return true
	}
	return *c.AutoFallback
}

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
	Path    string `json:"path,omitempty"`
	Port    int    `json:"port,omitempty"`
}

type PluginsCfg struct {
	Enabled bool                      `json:"enabled"`
	Paths   []string                  `json:"paths,omitempty"`
	Entries map[string]PluginEntryCfg `json:"entries,omitempty"`
}

type PluginEntryCfg struct {
	Enabled bool                   `json:"enabled"`
	Config  map[string]interface{} `json:"config,omitempty"`
}

type TaskQueueCfg struct {
	MaxConcurrent  int `json:"maxConcurrent,omitempty"`
	TaskTimeoutSec int `json:"taskTimeoutSec,omitempty"`
}

// SandboxCfg holds sandbox configuration for an agent.
type SandboxCfg struct {
	Enabled    bool   `json:"enabled"`
	Image      string `json:"image,omitempty"`
	Policy     string `json:"policy,omitempty"`
	Backend    string `json:"backend,omitempty"`
	E2BKey     string `json:"e2bKey,omitempty"`
	Network    string `json:"network,omitempty"`
	IdleTTLSec int    `json:"idleTTLSec,omitempty"`
}

// GatewayAuth is now a thin shell — the authoritative auth state lives in
// the users table (cookie session) and apikeys table (bearer). Token here
// is unused at runtime; kept on the struct so existing JSON serializations
// remain compatible while the field is migrated out of callers.
type GatewayAuth struct {
	Mode  string `json:"mode,omitempty"`
	Token string `json:"token,omitempty"`
}

type GatewayHTTPEndpoints struct {
	ChatCompletions GatewayEndpoint `json:"chatCompletions,omitempty"`
	Agents          GatewayEndpoint `json:"agents,omitempty"`
}

type GatewayEndpoint struct {
	Enabled bool `json:"enabled"`
}

type GatewayHTTP struct {
	Endpoints GatewayHTTPEndpoints `json:"endpoints,omitempty"`
}

// GatewayCfg holds gateway server configuration. The legacy "mode" field
// is gone — multi-user is unconditional.
type GatewayCfg struct {
	Port      int          `json:"port,omitempty"`
	Bind      string       `json:"bind,omitempty"`
	Auth      GatewayAuth  `json:"auth,omitempty"`
	HTTP      GatewayHTTP  `json:"http,omitempty"`
	RateLimit RateLimitCfg `json:"rateLimit,omitempty"`
}

type RateLimitCfg struct {
	RPM int `json:"rpm,omitempty"`
}

type MemoryCfg struct {
	AutoPersist AutoPersistCfg `json:"autoPersist,omitempty"`
	FTS         FTSCfg         `json:"fts,omitempty"`
}

type AutoPersistCfg struct {
	Enabled     bool   `json:"enabled"`
	EveryNTurns int    `json:"everyNTurns,omitempty"`
	Model       string `json:"model,omitempty"`
}

type FTSCfg struct {
	Enabled bool   `json:"enabled"`
	DBPath  string `json:"dbPath,omitempty"`
}

type PrivacyCfg struct {
	PIIScrubbing PIIScrubCfg `json:"piiScrubbing,omitempty"`
}

type PIIScrubCfg struct {
	Enabled bool `json:"enabled"`
}

type SkillsLearnerCfg struct {
	Enabled      bool   `json:"enabled"`
	MinToolCalls int    `json:"minToolCalls,omitempty"`
	Model        string `json:"model,omitempty"`
}

// Config is the in-memory runtime snapshot. The gateway assembles this at
// boot by reading FASTCLAW_* env vars + database (system_settings, providers,
// channels, agents). Callers never serialize it back out — DB tables are
// the persistent source of truth.
type Config struct {
	Providers     map[string]ProviderConfig  `json:"providers"`
	Agents        AgentsConfig               `json:"agents"`
	Channels      map[string]ChannelConfig   `json:"channels"`
	Bindings      []Binding                  `json:"bindings,omitempty"`
	Teams         map[string]TeamEntry       `json:"teams,omitempty"`
	MCPServers    map[string]MCPServerConfig `json:"mcpServers,omitempty"`
	CronJobs      []CronJob                  `json:"cronJobs,omitempty"`
	Heartbeat     HeartbeatCfg               `json:"heartbeat,omitempty"`
	Storage       StorageCfg                 `json:"storage,omitempty"`
	Sandbox       SandboxCfg                 `json:"sandbox,omitempty"`
	ToolProviders map[string]ToolProviderCfg `json:"toolProviders,omitempty"`
	Tools         map[string]ToolCategoryCfg `json:"tools,omitempty"`
	ObjectStore   ObjectStoreCfg             `json:"objectStore,omitempty"`
	Hooks         HooksCfg                   `json:"hooks,omitempty"`
	Plugins       PluginsCfg                 `json:"plugins,omitempty"`
	Gateway       GatewayCfg                 `json:"gateway,omitempty"`
	TaskQueue     TaskQueueCfg               `json:"taskQueue,omitempty"`
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

type ModelEntry struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Reasoning     bool      `json:"reasoning"`
	Input         []string  `json:"input"`
	Cost          ModelCost `json:"cost"`
	ContextWindow int       `json:"contextWindow"`
	MaxTokens     int       `json:"maxTokens"`
}

// ProviderConfig holds API credentials for an LLM provider — used both as
// the JSON shape inside agents.config and as the resolved per-(scope, name)
// view assembled by the providers resolver.
type ProviderConfig struct {
	APIKey   string       `json:"apiKey"`
	APIBase  string       `json:"apiBase"`
	APIType  string       `json:"apiType,omitempty"`
	AuthType string       `json:"authType,omitempty"`
	Models   []ModelEntry `json:"models,omitempty"`
}

// UnmarshalJSON handles a long-deprecated `api` alias for `apiType`.
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

type AgentsConfig struct {
	Defaults AgentDefaults `json:"defaults"`
}

type AgentDefaults struct {
	Model             string  `json:"model,omitempty"`
	MaxTokens         int     `json:"maxTokens,omitempty"`
	Temperature       float64 `json:"temperature,omitempty"`
	MaxToolIterations int     `json:"maxToolIterations,omitempty"`
	Thinking          string  `json:"thinking,omitempty"`
	PolicyPreset      string  `json:"policy,omitempty"`
}

// AgentEntry is the in-memory shape of one agent row, used during
// resolution. UserID is the owning account (mirrors agents.user_id).
// Per-agent model overrides aren't carried here — they live in the
// configs table at scope=agent and are merged in via scope.SettingInto
// during userspace load.
type AgentEntry struct {
	ID                string                     `json:"id"`
	UserID            string                     `json:"userId,omitempty"`
	Workspace         string                     `json:"workspace,omitempty"`
	MaxTokens         int                        `json:"maxTokens,omitempty"`
	Temperature       float64                    `json:"temperature,omitempty"`
	MaxToolIterations int                        `json:"maxToolIterations,omitempty"`
	Skills            []string                   `json:"skills,omitempty"`
	Tools             []string                   `json:"tools,omitempty"`
	MCPServers        map[string]MCPServerConfig `json:"mcpServers,omitempty"`
	AlwaysLoadSkills  []string                   `json:"alwaysLoadSkills,omitempty"`
	Thinking          string                     `json:"thinking,omitempty"`
	Sandbox           SandboxCfg                 `json:"sandbox,omitempty"`
	PolicyPreset      string                     `json:"policy,omitempty"`
}

// ChannelConfig holds per-channel runtime configuration. Built by the
// channels scope resolver from system/user/agent rows.
type ChannelConfig struct {
	Enabled  bool                     `json:"enabled"`
	BotToken string                   `json:"botToken,omitempty"`
	AppToken string                   `json:"appToken,omitempty"`
	Accounts map[string]AccountConfig `json:"accounts,omitempty"`
}

type AccountConfig struct {
	BotToken string `json:"botToken,omitempty"`
	// BaseURL is the per-account API base used by adapters whose
	// upstream isn't a fixed hostname (e.g. WeChat iLink hands out a
	// region-specific baseurl on QR confirmation). Empty for
	// Telegram/Discord/Slack — they all hit fixed endpoints.
	BaseURL string `json:"baseUrl,omitempty"`
	// UserID is an extra account-scoped identifier some adapters need
	// alongside BotToken (WeChat iLink's `ilink_user_id`, used as the
	// X-WECHAT-UIN seed and for typing/getconfig calls). Empty when
	// not applicable.
	UserID string `json:"userId,omitempty"`
}

type Binding struct {
	AgentID string `json:"agentId"`
	Match   Match  `json:"match"`
}

type Match struct {
	Channel   string `json:"channel"`
	AccountID string `json:"accountId,omitempty"`
	Peer      *Peer  `json:"peer,omitempty"`
}

type Peer struct {
	Kind string `json:"kind,omitempty"`
	ID   string `json:"id,omitempty"`
}

// AgentFileConfigLoader is the indirection point for layer-3 agent config.
// Gateway boot wires it to read from agents.config rows in the DB.
var AgentFileConfigLoader func(agentID, home string) (AgentFileConfig, bool) = defaultAgentFileConfigLoader

func defaultAgentFileConfigLoader(_, home string) (AgentFileConfig, bool) {
	if home == "" {
		return AgentFileConfig{}, false
	}
	data, err := os.ReadFile(filepath.Join(home, "agent.json"))
	if err != nil {
		return AgentFileConfig{}, false
	}
	var cfg AgentFileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return AgentFileConfig{}, false
	}
	return cfg, true
}

// AgentFileConfig is the schema for an agent's per-row override JSON
// (agents.config column). Per-agent providers/channels live in their own
// scoped DB tables and are NOT persisted here.
type AgentFileConfig struct {
	Model             string                     `json:"model,omitempty"`
	MaxTokens         int                        `json:"maxTokens,omitempty"`
	Temperature       float64                    `json:"temperature,omitempty"`
	MaxToolIterations int                        `json:"maxToolIterations,omitempty"`
	Workspace         string                     `json:"workspace,omitempty"`
	Skills            SkillsConfig               `json:"skills,omitempty"`
	MCPServers        map[string]MCPServerConfig `json:"mcpServers,omitempty"`
	ToolProviders     map[string]ToolProviderCfg `json:"toolProviders,omitempty"`
	Tools             map[string]ToolCategoryCfg `json:"tools,omitempty"`
	Providers         map[string]ProviderConfig  `json:"providers,omitempty"`
}

type SkillsConfig struct {
	Disabled   []string `json:"disabled,omitempty"`
	AlwaysLoad []string `json:"alwaysLoad,omitempty"`
}

type SkillsCfg struct {
	Install      SkillsInstallCfg                    `json:"install,omitempty"`
	Entries      map[string]SkillEntryCfg            `json:"entries,omitempty"`
	AgentEntries map[string]map[string]SkillEntryCfg `json:"agentEntries,omitempty"`
	Load         SkillsLoadCfg                       `json:"load,omitempty"`
	AlwaysLoad   []string                            `json:"alwaysLoad,omitempty"`
}

type SkillsInstallCfg struct {
	NodeManager string `json:"nodeManager,omitempty"`
}

type SkillEntryCfg struct {
	Enabled bool              `json:"enabled"`
	APIKey  string            `json:"apiKey,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

type SkillsLoadCfg struct {
	ExtraDirs []string `json:"extraDirs,omitempty"`
}

// ResolvedAgent is the fully merged config for a single agent.
type ResolvedAgent struct {
	ID                string
	UserID            string
	Home              string
	Workspace         string
	Model             string
	MaxTokens         int
	Temperature       float64
	MaxToolIterations int
	Thinking          string
	Skills            SkillsConfig
	MCPServers        map[string]MCPServerConfig
	Sandbox           SandboxCfg
	PolicyPreset      string
	ToolProviders     map[string]ToolProviderCfg
	Tools             map[string]ToolCategoryCfg
	Providers         map[string]ProviderConfig
}

type TeamEntry struct {
	Agents        []string `json:"agents"`
	DefaultAgent  string   `json:"defaultAgent,omitempty"`
	GroupBehavior string   `json:"groupBehavior,omitempty"`
}

type TeamConfig struct {
	Name    string            `json:"name"`
	Agents  []string          `json:"agents"`
	Routing map[string]string `json:"routing"`
}

// HomeDir returns the FastClaw root directory (default ~/.fastclaw).
// Holds the sqlite db, sandbox roots, and FS-materialized agent caches.
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

// AgentHomeDir returns ~/.fastclaw/agents/{agentID}/agent — the FS cache
// directory the runtime materializes agent identity files into. agents.id
// is globally unique so no user namespace is needed.
func AgentHomeDir(agentID string) (string, error) {
	if agentID == "" {
		return "", errors.New("config.AgentHomeDir: agentID is required")
	}
	home, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "agents", agentID, "agent"), nil
}

// AgentWorkspaceDir returns the agent's working directory for user-facing
// artifacts: ~/.fastclaw/workspaces/<agent_id>/. agents.id is globally
// unique so no user namespace is needed; per-session sub-directories are
// added by the workspace store at write time (see workspace.LocalFS).
func AgentWorkspaceDir(agentID string) (string, error) {
	if agentID == "" {
		return "", errors.New("config.AgentWorkspaceDir: agentID is required")
	}
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

// ApplyDefaults fills in zero-valued knobs on Agents.Defaults.
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

// MergedAgentConfig merges defaults with an agent entry to produce a fully
// resolved agent config.
func (cfg *Config) MergedAgentConfig(entry AgentEntry) ResolvedAgent {
	home, _ := AgentHomeDir(entry.ID)
	workspace := expandPath(entry.Workspace)
	if workspace == "" {
		workspace, _ = AgentWorkspaceDir(entry.ID)
	}

	resolved := ResolvedAgent{
		ID:                entry.ID,
		UserID:            entry.UserID,
		Home:              home,
		Workspace:         workspace,
		Model:             cfg.Agents.Defaults.Model,
		MaxTokens:         cfg.Agents.Defaults.MaxTokens,
		Temperature:       cfg.Agents.Defaults.Temperature,
		MaxToolIterations: cfg.Agents.Defaults.MaxToolIterations,
		Thinking:          cfg.Agents.Defaults.Thinking,
		Sandbox:           cfg.Sandbox,
		PolicyPreset:      cfg.Agents.Defaults.PolicyPreset,
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

	if len(cfg.MCPServers) > 0 {
		resolved.MCPServers = make(map[string]MCPServerConfig, len(cfg.MCPServers))
		for k, v := range cfg.MCPServers {
			resolved.MCPServers[k] = v
		}
	}
	if len(cfg.Providers) > 0 {
		resolved.Providers = make(map[string]ProviderConfig, len(cfg.Providers))
		for k, v := range cfg.Providers {
			resolved.Providers[k] = v
		}
	}
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

	if fileCfg, ok := AgentFileConfigLoader(entry.ID, home); ok {
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
		for k, v := range fileCfg.MCPServers {
			if resolved.MCPServers == nil {
				resolved.MCPServers = make(map[string]MCPServerConfig)
			}
			resolved.MCPServers[k] = v
		}
		for k, v := range fileCfg.Providers {
			if resolved.Providers == nil {
				resolved.Providers = make(map[string]ProviderConfig)
			}
			resolved.Providers[k] = v
		}
		for k, v := range fileCfg.ToolProviders {
			if resolved.ToolProviders == nil {
				resolved.ToolProviders = make(map[string]ToolProviderCfg)
			}
			resolved.ToolProviders[k] = v
		}
		for k, v := range fileCfg.Tools {
			if resolved.Tools == nil {
				resolved.Tools = make(map[string]ToolCategoryCfg)
			}
			resolved.Tools[k] = v
		}
	}

	return resolved
}

// ResolveAgents builds resolved agent configs from a list of entries.
// Source-of-truth lookup happens in the caller (DB ListAgents); this
// function only does the merge.
func ResolveAgents(cfg *Config, entries []AgentEntry) []ResolvedAgent {
	out := make([]ResolvedAgent, 0, len(entries))
	for _, e := range entries {
		if e.ID == "" {
			continue
		}
		out = append(out, cfg.MergedAgentConfig(e))
	}
	return out
}

// LoadTeam reads a team.json file from the FS skills bundle.
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
