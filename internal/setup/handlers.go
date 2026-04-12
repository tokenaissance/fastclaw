package setup

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

// loadUserConfig reads the config for the user identified by the request
// context. For the local user it always reads from filesystem (bootstrap).
// For cloud users it tries the Store first (DB-backed), falling back to
// filesystem, and auto-provisions if nothing exists.
func (s *Server) loadUserConfig(r *http.Request) (*config.Config, error) {
	userID := config.UserIDFromContext(r.Context())

	// Try loading from filesystem first (works for both local and file-backed cloud).
	cfg, err := config.LoadForUser(userID)
	if err == nil {
		return cfg, nil
	}

	// Auto-provision if missing.
	if os.IsNotExist(unwrapPathError(err)) {
		if provErr := users.ProvisionWorkspace(userID); provErr != nil {
			return nil, fmt.Errorf("auto-provision user %s: %w", userID, provErr)
		}
		return config.LoadForUser(userID)
	}
	return nil, err
}

func unwrapPathError(err error) error {
	for err != nil {
		if pe, ok := err.(*os.PathError); ok {
			return pe.Err
		}
		err = errors.Unwrap(err)
	}
	return nil
}

// saveUserConfig persists the config for the request's user.
func (s *Server) saveUserConfig(r *http.Request, cfg *config.Config) error {
	userID := config.UserIDFromContext(r.Context())
	if _, err := config.EnsureUserDir(userID); err != nil {
		return err
	}
	configPath, err := config.UserConfigPath(userID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, data, 0o644)
}

// userDir returns the workspace directory for the request's user.
func userDirForRequest(r *http.Request) (string, error) {
	return config.UserDir(config.UserIDFromContext(r.Context()))
}

// resolveAgent finds an agent for the current request's user. For the local
// user it uses the preloaded agentProvider; for cloud users it lazily loads
// their UserSpace via the resolver.
func (s *Server) resolveAgent(r *http.Request, agentID string) AgentHandle {
	userID := config.UserIDFromContext(r.Context())

	// Cloud user → resolve from userResolver.
	if userID != config.DefaultUserID && s.userResolver != nil {
		space, err := s.userResolver.UserSpaceFor(userID)
		if err != nil {
			return nil
		}
		ag := space.Agents.AgentByID(agentID)
		if ag == nil {
			return nil
		}
		return ag
	}

	// Local user → use preloaded provider.
	if s.agentProvider != nil {
		return s.agentProvider.AgentByID(agentID)
	}
	return nil
}

// resolveAllAgents returns all agents for the current request's user.
func (s *Server) resolveAllAgents(r *http.Request) []AgentHandle {
	userID := config.UserIDFromContext(r.Context())

	if userID != config.DefaultUserID && s.userResolver != nil {
		space, err := s.userResolver.UserSpaceFor(userID)
		if err != nil {
			return nil
		}
		all := space.Agents.All()
		result := make([]AgentHandle, len(all))
		for i, ag := range all {
			result[i] = ag
		}
		return result
	}

	if s.agentProvider != nil {
		return s.agentProvider.AllAgents()
	}
	return nil
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	configPath, err := config.UserConfigPath(config.UserIDFromContext(r.Context()))
	if err != nil {
		jsonResponse(w, http.StatusOK, map[string]any{
			"configured": false,
			"running":    false,
		})
		return
	}
	_, statErr := os.Stat(configPath)
	configured := statErr == nil

	userID := config.UserIDFromContext(r.Context())
	isAdmin := userID == config.DefaultUserID && s.authToken != ""

	resp := map[string]any{
		"configured": configured,
		"running":    s.agentProvider != nil,
		"port":       s.port,
		"agents":     []any{},
		"channels":   []any{},
		"provider":   nil,
		"uptime":     "",
		"userId":     userID,
		"isAdmin":    isAdmin,
	}

	resp["uptime"] = formatDuration(time.Since(s.startedAt))
	allAgents := s.resolveAllAgents(r)
	if len(allAgents) > 0 {
		var agentList []map[string]string
		for _, ag := range allAgents {
			agentList = append(agentList, map[string]string{
				"id": ag.Name(),
			})
		}
		resp["agents"] = agentList
	}

	// Load config for provider/channel/agent details
	if configured {
		cfg, loadErr := s.loadUserConfig(r)
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
				resolved := config.ResolveAgentsForUser(cfg, userID)
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
				resolved := config.ResolveAgentsForUser(cfg, userID)
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
	cfg, err := s.loadUserConfig(r)
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
	APIBase  string `json:"apiBase"`
	APIKey   string `json:"apiKey"`
	Model    string `json:"model"`
	APIType  string `json:"apiType"`
	AuthType string `json:"authType"`
}

func (s *Server) handleTestProvider(w http.ResponseWriter, r *http.Request) {
	var req testProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}

	base := strings.TrimRight(req.APIBase, "/")
	var testURL string
	var method string
	var body io.Reader

	if req.APIType == "anthropic-messages" {
		// Anthropic Messages API: base + /v1/messages
		testURL = base + "/v1/messages"
		method = "POST"
		model := req.Model
		if model == "" {
			model = "claude-sonnet-4-20250514"
		}
		payload := fmt.Sprintf(`{"model":"%s","messages":[{"role":"user","content":"hi"}]}`, model)
		body = strings.NewReader(payload)
	} else {
		// OpenAI-compatible: send a minimal chat completion to verify API key
		testURL = base + "/chat/completions"
		method = "POST"
		model := req.Model
		if model == "" {
			model = "gpt-4o-mini"
		}
		payload := fmt.Sprintf(`{"model":"%s","messages":[{"role":"user","content":"hi"}]}`, model)
		body = strings.NewReader(payload)
	}

	httpReq, err := http.NewRequestWithContext(r.Context(), method, testURL, body)
	if err != nil {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "url": testURL})
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// Set auth headers based on API type
	if req.APIType == "anthropic-messages" {
		if req.APIKey != "" {
			httpReq.Header.Set("x-api-key", req.APIKey)
		}
		httpReq.Header.Set("anthropic-version", "2023-06-01")
	} else {
		if req.APIKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)
		}
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "url": testURL})
		return
	}
	defer resp.Body.Close()

	// For chat/messages endpoints, 200 means success.
	// 401/403 means bad API key, other errors are connectivity issues.
	if resp.StatusCode == http.StatusOK {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "url": testURL})
		return
	}

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	errMsg := fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(respBody))

	// 401/403 = auth failure, clearly report it
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": "Authentication failed. Please check your API Key.", "url": testURL})
		return
	}

	jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": errMsg, "url": testURL})
}

type chatRequest struct {
	AgentID   string `json:"agentId"`
	SessionID string `json:"sessionId"`
	Message   string `json:"message"`
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
		return
	}

	ag := s.resolveAgent(r, req.AgentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}

	reply := ag.HandleWebChat(r.Context(), req.SessionID, req.Message)
	jsonResponse(w, http.StatusOK, map[string]any{"response": reply})
}

func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	ag := s.resolveAgent(r, req.AgentID)
	if ag == nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	events := make(chan agent.ChatEvent, 32)

	// Run agent in background
	go func() {
		defer close(events)
		ag.HandleWebChatStream(r.Context(), req.SessionID, req.Message, events)
	}()

	for evt := range events {
		data, _ := json.Marshal(evt)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	// Send final done event (in case agent returned without emitting done)
	fmt.Fprintf(w, "data: {\"type\":\"done\"}\n\n")
	flusher.Flush()
}

func (s *Server) handleChatHistory(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agentId")
	sessionID := r.URL.Query().Get("sessionId")
	if agentID == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "agentId required"})
		return
	}

	ag := s.resolveAgent(r, agentID)
	if ag == nil {
		jsonResponse(w, http.StatusOK, []any{})
		return
	}

	history := ag.WebChatHistory(sessionID)
	if history == nil {
		history = []map[string]any{}
	}
	jsonResponse(w, http.StatusOK, history)
}

func (s *Server) handleChatSessions(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agentId")
	if agentID == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "agentId required"})
		return
	}

	ag := s.resolveAgent(r, agentID)
	if ag == nil {
		jsonResponse(w, http.StatusOK, []any{})
		return
	}

	sessions := ag.WebChatSessions()
	if sessions == nil {
		sessions = []map[string]string{}
	}
	jsonResponse(w, http.StatusOK, sessions)
}

type saveConfigRequest struct {
	Provider        string `json:"provider"`
	ProviderName    string `json:"providerName"`
	APIBase         string `json:"apiBase"`
	APIKey          string `json:"apiKey"`
	APIType         string `json:"apiType"`
	AuthType        string `json:"authType"`
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

	// Normalize agent name to slug: lowercase, spaces/underscores to hyphens
	agentID := strings.ToLower(strings.TrimSpace(req.AgentName))
	agentID = strings.ReplaceAll(agentID, " ", "-")
	agentID = strings.ReplaceAll(agentID, "_", "-")

	// Determine provider key
	providerKey := req.Provider
	if req.Provider == "custom" && req.ProviderName != "" {
		providerKey = strings.ToLower(strings.TrimSpace(req.ProviderName))
		providerKey = strings.ReplaceAll(providerKey, " ", "-")
	}

	// Build config
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			providerKey: {
				APIKey:   req.APIKey,
				APIBase:  req.APIBase,
				APIType:  req.APIType,
				AuthType: req.AuthType,
			},
		},
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Model:             providerKey + "/" + req.Model,
				MaxTokens:         8192,
				Temperature:       0.7,
				MaxToolIterations: 20,
			},
			List: []config.AgentEntry{
				{ID: agentID},
			},
		},
		Channels: map[string]config.ChannelConfig{},
		Bindings: []config.Binding{},
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
		cfg.Bindings = append(cfg.Bindings, config.Binding{
			AgentID: agentID,
			Match:   config.Match{Channel: "telegram"},
		})
	}

	// Ensure the default user's directory exists.
	userDir, err := config.EnsureUserDir(config.DefaultUserID)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	// Write config
	configPath := filepath.Join(userDir, "fastclaw.json")
	data, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	// Create agent workspace
	agentDir := filepath.Join(userDir, "agents", agentID, "agent")
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
		"IDENTITY.md":  fmt.Sprintf("# Identity\n\nYou are %s, a FastClaw AI agent.\n", req.AgentName),  // use display name
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

	slog.Info("config saved", "path", configPath, "agent", agentID)

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

	cfg, err := s.loadUserConfig(r)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	// Replace providers (supports add, update, and delete)
	if raw, ok := incoming["providers"]; ok {
		var providers map[string]config.ProviderConfig
		if json.Unmarshal(raw, &providers) == nil {
			oldProviders := cfg.Providers
			cfg.Providers = make(map[string]config.ProviderConfig)
			for k, v := range providers {
				p := config.ProviderConfig{
					APIBase:  v.APIBase,
					APIType:  v.APIType,
					AuthType: v.AuthType,
					Models:   v.Models,
				}
				if v.APIKey != "" && !strings.Contains(v.APIKey, "****") {
					p.APIKey = v.APIKey
				} else if old, exists := oldProviders[k]; exists {
					p.APIKey = old.APIKey
				}
				cfg.Providers[k] = p
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

	if err := s.saveUserConfig(r, cfg); err != nil {
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

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	if s.taskQueue == nil {
		jsonResponse(w, http.StatusOK, []any{})
		return
	}

	tasks := s.taskQueue.RecentTasks(50)
	result := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		entry := map[string]any{
			"id":        t.ID,
			"agentId":   t.AgentID,
			"chatKey":   t.ChatKey,
			"status":    string(t.Status),
			"createdAt": t.CreatedAt.Format(time.RFC3339),
		}
		if t.StartedAt != nil && t.DoneAt != nil {
			entry["duration"] = t.DoneAt.Sub(*t.StartedAt).Milliseconds()
		}
		if t.Error != nil {
			entry["error"] = t.Error.Error()
		}
		result = append(result, entry)
	}
	jsonResponse(w, http.StatusOK, result)
}

// saveConfigFile persists the config to ~/.fastclaw/users/local/fastclaw.json.
func saveConfigFile(cfg *config.Config) error {
	if _, err := config.EnsureUserDir(config.DefaultUserID); err != nil {
		return err
	}
	configPath, err := config.UserConfigPath(config.DefaultUserID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, data, 0o644)
}
