package setup

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/config"
)

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	homeDir, err := config.HomeDir()
	if err != nil {
		jsonResponse(w, http.StatusOK, map[string]any{
			"configured": false,
			"running":    false,
		})
		return
	}
	configPath := filepath.Join(homeDir, "fastclaw.json")
	_, statErr := os.Stat(configPath)
	configured := statErr == nil

	resp := map[string]any{
		"configured": configured,
		"running":    s.agentProvider != nil,
		"port":       s.port,
		"agents":     []any{},
		"channels":   []any{},
		"provider":   nil,
		"uptime":     "",
	}

	if s.agentProvider != nil {
		resp["uptime"] = formatDuration(time.Since(s.startedAt))
		var agentList []map[string]string
		for _, ag := range s.agentProvider.AllAgents() {
			agentList = append(agentList, map[string]string{
				"id": ag.Name(),
			})
		}
		resp["agents"] = agentList
	}

	// Load config for provider/channel/agent details
	if configured {
		cfg, loadErr := config.Load()
		if loadErr == nil {
			// Provider info
			for name, prov := range cfg.Providers {
				maskedKey := maskAPIKey(prov.APIKey)
				resp["provider"] = map[string]string{
					"name":   name,
					"model":  cfg.Agents.Defaults.Model,
					"apiBase": prov.APIBase,
					"apiKey": maskedKey,
				}
				break // use first provider
			}

			// Channel info
			var channelList []map[string]string
			for chType, ch := range cfg.Channels {
				if !ch.Enabled {
					continue
				}
				entry := map[string]string{"type": chType}
				channelList = append(channelList, entry)
			}
			if len(channelList) > 0 {
				resp["channels"] = channelList
			}

			// Agent info with model details
			if s.agentProvider == nil {
				// Not running - get agent list from config
				resolved := config.ResolveAgents(cfg)
				var agentList []map[string]string
				for _, ra := range resolved {
					agentList = append(agentList, map[string]string{
						"id":        ra.ID,
						"model":     ra.Model,
						"workspace": ra.Workspace,
					})
				}
				resp["agents"] = agentList
			} else {
				// Running - enrich with model info from config
				resolved := config.ResolveAgents(cfg)
				modelMap := make(map[string]string)
				wsMap := make(map[string]string)
				for _, ra := range resolved {
					modelMap[ra.ID] = ra.Model
					wsMap[ra.ID] = ra.Workspace
				}
				var agentList []map[string]string
				for _, ag := range s.agentProvider.AllAgents() {
					agentList = append(agentList, map[string]string{
						"id":        ag.Name(),
						"model":     modelMap[ag.Name()],
						"workspace": wsMap[ag.Name()],
					})
				}
				resp["agents"] = agentList
			}
		}
	}

	jsonResponse(w, http.StatusOK, resp)
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		jsonResponse(w, http.StatusOK, map[string]any{"error": "no config found"})
		return
	}

	// Mask API keys
	masked := *cfg
	masked.Providers = make(map[string]config.ProviderConfig)
	for k, v := range cfg.Providers {
		v.APIKey = maskAPIKey(v.APIKey)
		masked.Providers[k] = v
	}

	jsonResponse(w, http.StatusOK, masked)
}

type testProviderRequest struct {
	APIBase string `json:"apiBase"`
	APIKey  string `json:"apiKey"`
	Model   string `json:"model"`
}

func (s *Server) handleTestProvider(w http.ResponseWriter, r *http.Request) {
	var req testProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}

	// Make a simple chat completion call to test the provider
	payload := map[string]any{
		"model": req.Model,
		"messages": []map[string]string{
			{"role": "user", "content": "Say hi in one word."},
		},
		"max_tokens": 10,
	}
	body, _ := json.Marshal(payload)

	httpReq, err := http.NewRequestWithContext(r.Context(), "POST", req.APIBase+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(respBody))})
		return
	}

	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

type chatRequest struct {
	AgentID string `json:"agentId"`
	Message string `json:"message"`
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if s.agentProvider == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{
			"error": "gateway is not running",
		})
		return
	}

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
		return
	}

	ag := s.agentProvider.AgentByID(req.AgentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}

	reply := ag.HandleWebChat(r.Context(), req.Message)
	jsonResponse(w, http.StatusOK, map[string]any{"response": reply})
}

type saveConfigRequest struct {
	Provider        string `json:"provider"`
	APIBase         string `json:"apiBase"`
	APIKey          string `json:"apiKey"`
	Model           string `json:"model"`
	TelegramEnabled bool   `json:"telegramEnabled"`
	TelegramToken   string `json:"telegramToken"`
	Port            int    `json:"port"`
	AgentName       string `json:"agentName"`
	Personality     string `json:"personality"`
}

func (s *Server) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	var req saveConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}

	// Build config
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"default": {
				APIKey:  req.APIKey,
				APIBase: req.APIBase,
			},
		},
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Model:             req.Model,
				MaxTokens:         8192,
				Temperature:       0.7,
				MaxToolIterations: 20,
			},
			List: []config.AgentEntry{
				{ID: req.AgentName},
			},
		},
		Channels: map[string]config.ChannelConfig{},
		Bindings: []config.Binding{
			{
				AgentID: req.AgentName,
				Match:   config.Match{Channel: "telegram"},
			},
		},
	}

	// Auto-generate a gateway auth token
	cfg.Gateway = config.GatewayCfg{
		Port: req.Port,
		Auth: config.GatewayAuth{
			Token: generateRandomToken(32),
		},
	}

	if req.TelegramEnabled && req.TelegramToken != "" {
		cfg.Channels["telegram"] = config.ChannelConfig{
			Enabled:  true,
			BotToken: req.TelegramToken,
		}
	}

	// Ensure home dir exists
	homeDir, err := config.HomeDir()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	// Write config
	configPath := filepath.Join(homeDir, "fastclaw.json")
	data, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	// Create agent workspace
	agentDir := filepath.Join(homeDir, "agents", req.AgentName, "agent")
	dirs := []string{
		agentDir,
		filepath.Join(agentDir, "memory"),
		filepath.Join(agentDir, "sessions"),
		filepath.Join(agentDir, "skills"),
	}
	for _, dir := range dirs {
		os.MkdirAll(dir, 0o755)
	}

	// Write bootstrap files
	bootstrapFiles := map[string]string{
		"AGENTS.md":    "# Agent Capabilities\n\nDescribe what this agent can do.\n",
		"IDENTITY.md":  fmt.Sprintf("# Identity\n\nYou are %s, a FastClaw AI agent.\n", req.AgentName),
		"USER.md":      "# User\n\nInformation about the user you serve.\n",
		"TOOLS.md":     "# Tools\n\nAdditional tool usage instructions.\n",
		"BOOTSTRAP.md": "# Bootstrap\n\nStartup instructions loaded on every conversation.\n",
		"HEARTBEAT.md": "# Heartbeat\n\nPeriodic check-in instructions.\n",
		"MEMORY.md":    "# Memory\n\nLong-term memory for this agent.\n",
	}
	if req.Personality != "" {
		bootstrapFiles["SOUL.md"] = fmt.Sprintf("# Soul\n\n%s\n", req.Personality)
	} else {
		bootstrapFiles["SOUL.md"] = "# Soul\n\nYour personality and behavioral guidelines.\n"
	}

	for filename, content := range bootstrapFiles {
		path := filepath.Join(agentDir, filename)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			os.WriteFile(path, []byte(content), 0o644)
		}
	}

	// Write agent.json
	agentCfg := config.AgentFileConfig{Model: req.Model}
	agentData, _ := json.MarshalIndent(agentCfg, "", "  ")
	agentJSONPath := filepath.Join(agentDir, "agent.json")
	if _, err := os.Stat(agentJSONPath); os.IsNotExist(err) {
		os.WriteFile(agentJSONPath, agentData, 0o644)
	}

	slog.Info("config saved", "path", configPath, "agent", req.AgentName)

	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})

	// Signal that config is ready
	if s.onConfig != nil {
		go s.onConfig(cfg)
	}
}

// --- Config Update ---

func (s *Server) handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
	var incoming map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}

	cfg, err := config.Load()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	// Merge providers
	if raw, ok := incoming["providers"]; ok {
		var providers map[string]config.ProviderConfig
		if json.Unmarshal(raw, &providers) == nil {
			for k, v := range providers {
				if cfg.Providers == nil {
					cfg.Providers = make(map[string]config.ProviderConfig)
				}
				existing := cfg.Providers[k]
				if v.APIBase != "" {
					existing.APIBase = v.APIBase
				}
				if v.APIKey != "" && !strings.Contains(v.APIKey, "****") {
					existing.APIKey = v.APIKey
				}
				cfg.Providers[k] = existing
			}
		}
	}

	// Merge agents defaults
	if raw, ok := incoming["agents"]; ok {
		var agentUpdate struct {
			Defaults struct {
				Model string `json:"model"`
			} `json:"defaults"`
		}
		if json.Unmarshal(raw, &agentUpdate) == nil {
			if agentUpdate.Defaults.Model != "" {
				cfg.Agents.Defaults.Model = agentUpdate.Defaults.Model
			}
		}
	}

	// Merge storage
	if raw, ok := incoming["storage"]; ok {
		var storage config.StorageCfg
		if json.Unmarshal(raw, &storage) == nil {
			cfg.Storage = storage
		}
	}

	// Merge hooks
	if raw, ok := incoming["hooks"]; ok {
		var hooks config.HooksCfg
		if json.Unmarshal(raw, &hooks) == nil {
			cfg.Hooks = hooks
		}
	}

	if err := saveConfigFile(cfg); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func jsonResponse(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func maskAPIKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "****" + key[len(key)-4:]
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	hours := int(d.Hours())
	mins := int(d.Minutes()) % 60
	if hours < 24 {
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	days := hours / 24
	hours = hours % 24
	return fmt.Sprintf("%dd %dh", days, hours)
}

// generateRandomToken generates a cryptographically random hex token.
func generateRandomToken(length int) string {
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		// Fallback: this should never happen
		return "fastclaw-default-token"
	}
	return hex.EncodeToString(b)
}

// saveConfigFile persists the config to ~/.fastclaw/fastclaw.json.
func saveConfigFile(cfg *config.Config) error {
	homeDir, err := config.HomeDir()
	if err != nil {
		return err
	}
	configPath := filepath.Join(homeDir, "fastclaw.json")
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, data, 0o644)
}
