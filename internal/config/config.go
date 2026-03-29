package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

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


// WebSearchCfg configures web search.
type WebSearchCfg struct {
	Provider string `json:"provider,omitempty"` // "brave"
	APIKey   string `json:"apiKey,omitempty"`
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
	Enabled bool   `json:"enabled"`
	Image   string `json:"image,omitempty"`
	Policy  string `json:"policy,omitempty"` // policy preset name
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
	Port int          `json:"port,omitempty"`
	Mode string       `json:"mode,omitempty"`  // "local" (default), "public"
	Bind string       `json:"bind,omitempty"`  // "loopback" (default), "all"
	Auth GatewayAuth  `json:"auth,omitempty"`
	HTTP GatewayHTTP  `json:"http,omitempty"`
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
	WebSearch  WebSearchCfg               `json:"webSearch,omitempty"`
	Hooks      HooksCfg                   `json:"hooks,omitempty"`
	Plugins    PluginsCfg                 `json:"plugins,omitempty"`
	Gateway    GatewayCfg                 `json:"gateway,omitempty"`
	TaskQueue  TaskQueueCfg               `json:"taskQueue,omitempty"`
	Skills     SkillsCfg                  `json:"skills,omitempty"`
	Memory     MemoryCfg                  `json:"memory,omitempty"`
	Privacy    PrivacyCfg                 `json:"privacy,omitempty"`
	SkillsLearner SkillsLearnerCfg        `json:"skillsLearner,omitempty"`
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
	APIKey  string       `json:"apiKey"`
	APIBase string       `json:"apiBase"`
	API     string       `json:"api,omitempty"`
	Models  []ModelEntry `json:"models,omitempty"`
}

// AgentsConfig holds agent defaults and the list of agent entries.
type AgentsConfig struct {
	Defaults AgentDefaults `json:"defaults"`
	List     []AgentEntry  `json:"list,omitempty"`
}

// AgentDefaults holds fallback values for all agents.
type AgentDefaults struct {
	Model             string  `json:"model"`
	MaxTokens         int     `json:"maxTokens"`
	Temperature       float64 `json:"temperature"`
	MaxToolIterations int     `json:"maxToolIterations"`
	Thinking          string  `json:"thinking,omitempty"` // off, low, medium, high, adaptive
}

// AgentEntry is a per-agent entry in config.json agents.list.
type AgentEntry struct {
	ID                string                     `json:"id"`
	Workspace         string                     `json:"workspace,omitempty"`
	Model             string                     `json:"model,omitempty"`
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
	Skills            SkillsConfig               `json:"skills,omitempty"`
	MCPServers        map[string]MCPServerConfig `json:"mcpServers,omitempty"`
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

// HomeDir returns the FastClaw home directory (~/.fastclaw).
func HomeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".fastclaw"), nil
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

// Load reads and parses ~/.fastclaw/fastclaw.json.
func Load() (*Config, error) {
	homeDir, err := HomeDir()
	if err != nil {
		return nil, fmt.Errorf("get home dir: %w", err)
	}

	configPath := filepath.Join(homeDir, "fastclaw.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", configPath, err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Apply defaults
	if cfg.Agents.Defaults.Model == "" {
		cfg.Agents.Defaults.Model = "gpt-4o"
	}
	if cfg.Agents.Defaults.MaxTokens == 0 {
		cfg.Agents.Defaults.MaxTokens = 8192
	}
	if cfg.Agents.Defaults.Temperature == 0 {
		cfg.Agents.Defaults.Temperature = 0.7
	}
	if cfg.Agents.Defaults.MaxToolIterations == 0 {
		cfg.Agents.Defaults.MaxToolIterations = 20
	}

	return &cfg, nil
}

// MergedAgentConfig merges defaults with an agent entry and its workspace agent.json
// to produce a fully resolved agent config. Priority: agent.json > entry > defaults.
func (cfg *Config) MergedAgentConfig(entry AgentEntry) ResolvedAgent {
	workspace := expandPath(entry.Workspace)
	if workspace == "" {
		homeDir, _ := HomeDir()
		workspace = filepath.Join(homeDir, "agents", entry.ID, "agent")
	}

	resolved := ResolvedAgent{
		ID:                entry.ID,
		Workspace:         workspace,
		Model:             cfg.Agents.Defaults.Model,
		MaxTokens:         cfg.Agents.Defaults.MaxTokens,
		Temperature:       cfg.Agents.Defaults.Temperature,
		MaxToolIterations: cfg.Agents.Defaults.MaxToolIterations,
		Thinking:          cfg.Agents.Defaults.Thinking,
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

	// Layer 3: agent.json in workspace (highest priority)
	agentJSON := filepath.Join(workspace, "agent.json")
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
		}
	}

	return resolved
}

// ResolveAgents produces resolved agent configs from config.agents.list.
// If no agents are listed, creates a single "default" agent.
func ResolveAgents(cfg *Config) []ResolvedAgent {
	if len(cfg.Agents.List) == 0 {
		entry := AgentEntry{ID: "default"}
		return []ResolvedAgent{cfg.MergedAgentConfig(entry)}
	}

	agents := make([]ResolvedAgent, 0, len(cfg.Agents.List))
	for _, entry := range cfg.Agents.List {
		agents = append(agents, cfg.MergedAgentConfig(entry))
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
