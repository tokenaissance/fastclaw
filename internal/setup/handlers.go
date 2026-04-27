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
	"github.com/fastclaw-ai/fastclaw/internal/session"
	"github.com/fastclaw-ai/fastclaw/internal/store"
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

	// In cloud mode the pod-local fastclaw.json is ephemeral and the
	// watcher hot-reloads from it — provisioning a stub here writes an
	// empty providers/default-agent blob that then blows away the DB-
	// hydrated config on every fresh pod. Hydrate from the DB instead.
	cloudMode := s.gatewayCfg != nil && s.gatewayCfg.Mode == "cloud"
	if cloudMode && s.dataStore != nil {
		if gc, gerr := s.dataStore.GetConfig(r.Context()); gerr == nil && gc != nil && len(gc.Data) > 0 {
			blob, merr := json.Marshal(gc.Data)
			if merr == nil {
				var stored config.Config
				if uerr := json.Unmarshal(blob, &stored); uerr == nil {
					return &stored, nil
				}
			}
		}
		// Nothing in DB yet — return a zero-value config so the setup
		// wizard can flow. Don't write anything to disk.
		return &config.Config{
			Providers: map[string]config.ProviderConfig{},
			Channels:  map[string]config.ChannelConfig{},
		}, nil
	}

	// No config on disk and no store row — return an empty cfg so callers
	// like /api/config and /api/status can answer truthfully. Pre-#5 we
	// auto-provisioned a per-user workspace + default agent here, but
	// that meant any random GET against an unknown user_id silently
	// minted a new agent (and a workspace dir) — operators saw phantom
	// agents materialize without any explicit action. Onboarding for
	// non-default users is now an explicit POST /api/agents call from
	// the application layer.
	if os.IsNotExist(unwrapPathError(err)) {
		return &config.Config{
			Providers: map[string]config.ProviderConfig{},
			Channels:  map[string]config.ChannelConfig{},
		}, nil
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

// saveUserConfig persists a UI-originated config update. Infra fields
// (storage, objectStore, gateway.auth.token, sandbox) are deliberately
// NOT overwritten — those belong to the deployment and are sourced from
// env / Secret. Letting the UI touch them would let an admin brick their
// own deployment.
//
// Everything else (providers, toolProviders, tools, agents.defaults,
// channels, plugins, cron, skills, …) goes to the DB store. The
// fastclaw.json filesystem path is the back-compat fallback used only
// when no store is wired (legacy single-user installs predating the
// DB-primary refactor); it'll go away after #5 merges the wizard.
func (s *Server) saveUserConfig(r *http.Request, cfg *config.Config) error {
	merged := *cfg
	// Preserve infra fields from whatever the caller assembled. In the
	// store-primary path we read the previous DB row; in the FS path we
	// read fastclaw.json. Either way, the UI's caller passed a Config
	// that already had infra populated (handleUpdateConfig copies it
	// from the running cfg), so this read-modify-write is mostly a
	// belt-and-suspenders against the UI accidentally clearing fields.
	if s.dataStore != nil {
		if existing, err := s.dataStore.GetConfig(r.Context()); err == nil && existing != nil && len(existing.Data) > 0 {
			if blob, err := json.Marshal(existing.Data); err == nil {
				var stored config.Config
				if err := json.Unmarshal(blob, &stored); err == nil {
					merged.Storage = stored.Storage
					merged.ObjectStore = stored.ObjectStore
					merged.Gateway = stored.Gateway
					merged.Sandbox = stored.Sandbox
				}
			}
		}
		data, err := json.MarshalIndent(&merged, "", "  ")
		if err != nil {
			return err
		}
		var rawCfg map[string]interface{}
		if err := json.Unmarshal(data, &rawCfg); err != nil {
			return err
		}
		return s.dataStore.SaveConfig(r.Context(), &store.GlobalConfig{Data: rawCfg})
	}
	// Store-less fallback (legacy FS-only mode). Same read-modify-write
	// against fastclaw.json on disk.
	if _, err := config.EnsureUserDir(); err != nil {
		return err
	}
	configPath, err := config.GlobalConfigPath()
	if err != nil {
		return err
	}
	if existingData, readErr := os.ReadFile(configPath); readErr == nil {
		var existing config.Config
		if jsonErr := json.Unmarshal(existingData, &existing); jsonErr == nil {
			merged.Storage = existing.Storage
			merged.ObjectStore = existing.ObjectStore
			merged.Gateway = existing.Gateway
			merged.Sandbox = existing.Sandbox
		}
	}
	data, err := json.MarshalIndent(&merged, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, data, 0o644)
}

// userDir returns the workspace directory for the request's user.
func userDirForRequest(r *http.Request) (string, error) {
	return config.HomeDir()
}

// configBackend names the persistence path for log lines so operators
// can tell at a glance whether a save landed in the DB or in legacy
// fastclaw.json. The store-less branch goes away once #5 merges the
// wizard into the gateway.
func configBackend(st store.Store) string {
	if st != nil {
		return "store"
	}
	return "fastclaw.json"
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
	mode := ""
	if s.gatewayCfg != nil {
		mode = s.gatewayCfg.Mode
	}

	// Post-#4, the store is the source of truth for "configured" in BOTH
	// local and cloud modes — saveUserConfig stops writing fastclaw.json
	// the moment a store is wired. The previous "mode == cloud" gate
	// was too narrow and made local-mode onboarding bounce back to
	// /onboard forever (status reports configured=false → page.tsx
	// redirects → save again → still false → loop).
	configured := false
	if s.dataStore != nil {
		if gc, err := s.dataStore.GetConfig(r.Context()); err == nil && gc != nil && len(gc.Data) > 0 {
			configured = true
		}
	}
	if !configured {
		// Back-compat fallback: legacy single-user installs that still
		// only have fastclaw.json on disk. Once they save anything via
		// the UI, the store row appears and this branch stops firing.
		if configPath, err := config.GlobalConfigPath(); err == nil {
			if _, statErr := os.Stat(configPath); statErr == nil {
				configured = true
			}
		}
	}

	// /api/status is reachable unauthenticated so the login / onboarding UI
	// can bootstrap — in that case we only return the non-sensitive public
	// fields and skip everything that would leak user-scoped data.
	authenticated := config.HasUserID(r.Context())
	userID := config.UserIDFromContext(r.Context())
	isAdmin := authenticated && userID == config.DefaultUserID && s.authToken != ""

	resp := map[string]any{
		"configured": configured,
		"running":    s.agentProvider != nil,
		"port":       s.port,
		"mode":       mode,
		"agents":     []any{},
		"channels":   []any{},
		"provider":   nil,
		"uptime":     formatDuration(time.Since(s.startedAt)),
		"isAdmin":    isAdmin,
	}
	if authenticated {
		resp["userId"] = userID
	}

	if !authenticated {
		jsonResponse(w, http.StatusOK, resp)
		return
	}

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

			// Agent info with model details. Supplement filesystem
			// discovery with DB agent IDs so admin sees agents owned by
			// other pods in a multi-replica cloud deploy.
			var storeAgentIDs []string
			if s.dataStore != nil {
				if records, lerr := s.dataStore.ListAgents(r.Context()); lerr == nil {
					for _, ar := range records {
						storeAgentIDs = append(storeAgentIDs, ar.ID)
					}
				}
			}
			resolved := config.ResolveAgentsWithExtra(cfg, userID, storeAgentIDs)
			if s.agentProvider == nil {
				// Not running - get agent list from config
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

// systemConfigured reports whether initial setup has completed. Mirrors
// the logic in handleStatus so onboarding-only endpoints can tell whether
// they're running for a first-time setup (allow unauthenticated) or for an
// already-configured deployment (require admin auth).
//
// Post-#4 the store is authoritative regardless of mode — saveUserConfig
// stops touching fastclaw.json once a store is wired. Fall back to the
// FS check only as legacy support for installs predating that change.
func (s *Server) systemConfigured(r *http.Request) bool {
	if s.dataStore != nil {
		if gc, err := s.dataStore.GetConfig(r.Context()); err == nil && gc != nil && len(gc.Data) > 0 {
			return true
		}
	}
	if configPath, err := config.GlobalConfigPath(); err == nil {
		if _, statErr := os.Stat(configPath); statErr == nil {
			return true
		}
	}
	return false
}

// requireOnboardingOrAuthed gates endpoints that the onboarding wizard must
// reach before a token exists. When the system is already configured, the
// caller must be an authenticated user — otherwise anyone could overwrite
// the deployment's provider/config from the public network.
func (s *Server) requireOnboardingOrAuthed(w http.ResponseWriter, r *http.Request) bool {
	if !s.systemConfigured(r) {
		return true
	}
	if config.HasUserID(r.Context()) {
		return true
	}
	jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "invalid token"})
	return false
}

func (s *Server) handleTestProvider(w http.ResponseWriter, r *http.Request) {
	if !s.requireOnboardingOrAuthed(w, r) {
		return
	}
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
		// max_tokens is required by the Anthropic Messages API — some
		// compat gateways (e.g. DeepSeek's /anthropic endpoint) 400 without it.
		payload := fmt.Sprintf(`{"model":"%s","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, model)
		body = strings.NewReader(payload)
	} else {
		// OpenAI-compatible: send a minimal chat completion to verify API key
		testURL = base + "/chat/completions"
		method = "POST"
		model := req.Model
		if model == "" {
			model = "gpt-4o-mini"
		}
		payload := fmt.Sprintf(`{"model":"%s","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, model)
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
		sessions = []session.WebSession{}
	}
	jsonResponse(w, http.StatusOK, sessions)
}

func (s *Server) handleRenameSession(w http.ResponseWriter, r *http.Request) {
	sessionKey := r.PathValue("key")
	if sessionKey == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "session key required"})
		return
	}

	var body struct {
		AgentID string `json:"agentId"`
		Title   string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
		return
	}
	if body.Title == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "title required"})
		return
	}

	ag := s.resolveAgent(r, body.AgentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}

	if err := ag.RenameWebChatSession(sessionKey, body.Title); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	sessionKey := r.PathValue("key")
	if sessionKey == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "session key required"})
		return
	}

	agentID := r.URL.Query().Get("agentId")
	ag := s.resolveAgent(r, agentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}

	if err := ag.DeleteWebChatSession(sessionKey); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
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
	GatewayToken    string `json:"gatewayToken"`
}

func (s *Server) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireOnboardingOrAuthed(w, r) {
		return
	}
	var req saveConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}

	// Normalize agent name
	agentID := strings.ToLower(strings.TrimSpace(req.AgentName))
	if agentID == "" {
		agentID = "default"
	}
	agentID = strings.ReplaceAll(agentID, " ", "-")
	agentID = strings.ReplaceAll(agentID, "_", "-")

	// Build global config
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{},
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				MaxTokens:         8192,
				Temperature:       0.7,
				MaxToolIterations: 20,
			},
		},
		Channels: map[string]config.ChannelConfig{},
		Bindings: []config.Binding{},
		Storage:  config.StorageCfg{Type: "file"},
		Sandbox:  config.SandboxCfg{Enabled: false},
	}

	// Provider + model (optional — can be configured later per agent)
	if req.APIKey != "" && req.Provider != "" {
		providerKey := req.Provider
		if req.Provider == "custom" && req.ProviderName != "" {
			providerKey = strings.ToLower(strings.TrimSpace(req.ProviderName))
			providerKey = strings.ReplaceAll(providerKey, " ", "-")
		}
		cfg.Providers[providerKey] = config.ProviderConfig{
			APIKey:   req.APIKey,
			APIBase:  req.APIBase,
			APIType:  req.APIType,
			AuthType: req.AuthType,
		}
		if req.Model != "" {
			cfg.Agents.Defaults.Model = providerKey + "/" + req.Model
		}
	}

	// Gateway: infra fields (port/token/mode/bind) belong to the deployment,
	// not the UI. In cloud mode they come from env; in local mode from an
	// existing fastclaw.json or UI input on first run. Either way, avoid
	// clobbering an already-loaded gateway config with UI-provided values.
	cloudMode := s.gatewayCfg != nil && s.gatewayCfg.Mode == "cloud"
	if cloudMode && s.gatewayCfg != nil {
		cfg.Gateway = *s.gatewayCfg
	} else {
		port := req.Port
		if port == 0 {
			port = 18953
		}
		gatewayToken := req.GatewayToken
		if gatewayToken == "" {
			gatewayToken = generateRandomToken(32)
		}
		cfg.Gateway = config.GatewayCfg{
			Port: port,
			Auth: config.GatewayAuth{
				Token: gatewayToken,
			},
			HTTP: config.GatewayHTTP{
				Endpoints: config.GatewayHTTPEndpoints{
					ChatCompletions: config.GatewayEndpoint{Enabled: true},
					Agents:          config.GatewayEndpoint{Enabled: true},
				},
			},
		}
	}

	// Telegram (optional)
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

	// In store-primary mode (the path post-#4) the global config lives in
	// the configs row written below — no fastclaw.json. Store-less mode
	// (the wizard process before #5 merges it into the gateway) still
	// needs the file because the next process restart reads it back to
	// learn the gateway token / storage DSN before the store is open.
	if s.dataStore == nil {
		if _, err := config.EnsureUserDir(); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		configPath, err := config.GlobalConfigPath()
		if err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		data, _ := json.MarshalIndent(cfg, "", "  ")
		if err := os.WriteFile(configPath, data, 0o644); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
	}

	// Bootstrap identity files for the new agent.
	wsFiles := map[string]string{
		"SOUL.md":      "# Soul\n\nYour personality and behavioral guidelines.\n",
		"IDENTITY.md":  fmt.Sprintf("# Identity\n\nYou are %s, a FastClaw AI agent.\n", req.AgentName),
		"USER.md":      "# User\n\nInformation about the user you serve.\n",
		"TOOLS.md":     "",
		"BOOTSTRAP.md": "",
		"HEARTBEAT.md": "",
		"MEMORY.md":    "",
		"AGENTS.md":    "",
	}
	if req.Personality != "" {
		wsFiles["SOUL.md"] = fmt.Sprintf("# %s\n\n%s\n", req.AgentName, req.Personality)
	}

	if s.dataStore != nil {
		// DB-primary path (cloud deployments): everything goes through the
		// store. No filesystem writes — pod-local FS is ephemeral anyway.
		var rawCfg map[string]interface{}
		if marshalled, err := json.Marshal(cfg); err == nil {
			_ = json.Unmarshal(marshalled, &rawCfg)
		}
		if err := s.dataStore.SaveConfig(r.Context(), &store.GlobalConfig{Data: rawCfg}); err != nil {
			slog.Warn("dataStore.SaveConfig failed", "error", err)
		}
		agentRecord := &store.AgentRecord{
			ID:    agentID,
			Name:  req.AgentName,
			Model: cfg.Agents.Defaults.Model,
		}
		if err := s.dataStore.SaveAgent(r.Context(), agentRecord); err != nil {
			slog.Warn("dataStore.SaveAgent failed", "error", err)
		}
		for filename, content := range wsFiles {
			if err := s.dataStore.SaveWorkspaceFile(r.Context(), agentID, filename, []byte(content)); err != nil {
				slog.Warn("dataStore.SaveWorkspaceFile failed", "file", filename, "error", err)
			}
		}
	} else {
		// Store-less mode (the wizard process before gateway boots, or
		// tests). Fall back to filesystem so the next gateway start can
		// load the agent from disk. This branch goes away once the wizard
		// is merged into the gateway in step #5.
		userDir, _ := config.HomeDir()
		agentDir := filepath.Join(userDir, "agents", agentID, "agent")
		for _, dir := range []string{
			agentDir,
			filepath.Join(agentDir, "memory"),
			filepath.Join(agentDir, "sessions"),
			filepath.Join(agentDir, "skills"),
		} {
			os.MkdirAll(dir, 0o755)
		}
		for filename, content := range wsFiles {
			path := filepath.Join(agentDir, filename)
			if _, err := os.Stat(path); os.IsNotExist(err) {
				os.WriteFile(path, []byte(content), 0o644)
			}
		}
	}

	// Adopt the freshly persisted gateway token in-memory so the running
	// server starts accepting it immediately. Pre-#5 the wizard process
	// got around this by exiting and letting the gateway re-read the
	// token from disk on restart; now that wizard + gateway are one
	// process, we have to update s.authToken explicitly or admin login
	// keeps reporting "Invalid admin token" because s.authToken is
	// still the empty boot value while the user types the just-saved
	// token from the UI.
	if t := cfg.Gateway.Auth.Token; t != "" && t != s.authToken {
		s.SetAuth(t, s.userRegistry)
	}

	// Refresh the running agent manager so the just-created agent is
	// usable for chat without a process restart. handleCreateAgent does
	// the same after individual create calls; handleSaveConfig has to
	// do it too because onboarding goes through here, and pre-#5 the
	// "wizard exits → gateway boot reads DB" flow used to cover this.
	if s.agentProvider != nil {
		if err := s.agentProvider.ReloadAgents(); err != nil {
			slog.Warn("failed to reload agents after save", "error", err)
		}
	}

	slog.Info("config saved", "agent", agentID,
		"backend", configBackend(s.dataStore),
		"hasProvider", len(cfg.Providers) > 0,
		"defaultModel", cfg.Agents.Defaults.Model,
	)

	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":    true,
		"token": cfg.Gateway.Auth.Token,
	})

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
			Defaults config.AgentDefaults `json:"defaults"`
		}
		if json.Unmarshal(raw, &agentUpdate) == nil {
			d := agentUpdate.Defaults
			if d.Model != "" {
				cfg.Agents.Defaults.Model = d.Model
			}
		}
	}

	// Merge sandbox (top-level)
	if raw, ok := incoming["sandbox"]; ok {
		var sandbox config.SandboxCfg
		if json.Unmarshal(raw, &sandbox) == nil {
			cfg.Sandbox = sandbox
		}
	}
	// Also handle sandbox inside agents.defaults (backwards compat)
	if raw, ok := incoming["agents"]; ok {
		var agentSandbox struct {
			Defaults struct {
				Sandbox config.SandboxCfg `json:"sandbox"`
			} `json:"defaults"`
		}
		if json.Unmarshal(raw, &agentSandbox) == nil {
			if agentSandbox.Defaults.Sandbox.Enabled || agentSandbox.Defaults.Sandbox.Backend != "" {
				cfg.Sandbox = agentSandbox.Defaults.Sandbox
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
	if _, err := config.EnsureUserDir(); err != nil {
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
