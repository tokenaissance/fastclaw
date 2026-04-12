package setup

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"

	"github.com/fastclaw-ai/fastclaw/internal/config"
)

// --- Agent Management ---

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.loadUserConfig(r)
	if err != nil {
		jsonResponse(w, http.StatusOK, []any{})
		return
	}
	resolved := config.ResolveAgentsForUser(cfg, config.UserIDFromContext(r.Context()))
	var agents []map[string]any
	for _, ra := range resolved {
		soul := ""
		soulPath := filepath.Join(ra.Workspace, "SOUL.md")
		if data, readErr := os.ReadFile(soulPath); readErr == nil {
			soul = string(data)
		}
		agents = append(agents, map[string]any{
			"id":                ra.ID,
			"model":             ra.Model,
			"workspace":         ra.Workspace,
			"maxTokens":         ra.MaxTokens,
			"temperature":       ra.Temperature,
			"maxToolIterations": ra.MaxToolIterations,
			"thinking":          ra.Thinking,
			"soul":              soul,
		})
	}
	if agents == nil {
		agents = []map[string]any{}
	}
	jsonResponse(w, http.StatusOK, agents)
}

func (s *Server) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Soul  string `json:"soul"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	if req.ID == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "id is required"})
		return
	}

	cfg, err := s.loadUserConfig(r)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	// Add agent to config
	cfg.Agents.List = append(cfg.Agents.List, config.AgentEntry{
		ID:    req.ID,
		Model: req.Model,
	})

	if err := s.saveUserConfig(r, cfg); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	// Create workspace under the default user's directory.
	userDir, _ := userDirForRequest(r)
	agentDir := filepath.Join(userDir, "agents", req.ID, "agent")
	for _, dir := range []string{agentDir, filepath.Join(agentDir, "memory"), filepath.Join(agentDir, "sessions"), filepath.Join(agentDir, "skills")} {
		os.MkdirAll(dir, 0o755)
	}
	if req.Soul != "" {
		os.WriteFile(filepath.Join(agentDir, "SOUL.md"), []byte(req.Soul), 0o644)
	}
	agentCfg := config.AgentFileConfig{Model: req.Model}
	agentData, _ := json.MarshalIndent(agentCfg, "", "  ")
	os.WriteFile(filepath.Join(agentDir, "agent.json"), agentData, 0o644)

	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Model string `json:"model"`
		Soul  string `json:"soul"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}

	cfg, err := s.loadUserConfig(r)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	found := false
	for i, entry := range cfg.Agents.List {
		if entry.ID == id {
			if req.Model != "" {
				cfg.Agents.List[i].Model = req.Model
			}
			found = true
			break
		}
	}
	if !found {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "agent not found"})
		return
	}

	if err := s.saveUserConfig(r, cfg); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	// Update workspace files under the default user's directory.
	userDir, _ := userDirForRequest(r)
	agentDir := filepath.Join(userDir, "agents", id, "agent")
	if req.Soul != "" {
		os.WriteFile(filepath.Join(agentDir, "SOUL.md"), []byte(req.Soul), 0o644)
	}
	if req.Model != "" {
		agentCfg := config.AgentFileConfig{Model: req.Model}
		agentData, _ := json.MarshalIndent(agentCfg, "", "  ")
		os.WriteFile(filepath.Join(agentDir, "agent.json"), agentData, 0o644)
	}

	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cfg, err := s.loadUserConfig(r)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	newList := make([]config.AgentEntry, 0, len(cfg.Agents.List))
	for _, entry := range cfg.Agents.List {
		if entry.ID != id {
			newList = append(newList, entry)
		}
	}
	cfg.Agents.List = newList

	if err := s.saveUserConfig(r, cfg); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}
