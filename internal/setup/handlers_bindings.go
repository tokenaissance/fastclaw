package setup

import (
	"encoding/json"
	"net/http"
	"os"

	"github.com/fastclaw-ai/fastclaw/internal/config"
)

// handleBindAgent sets (or replaces) the API key owner of an agent. Admin
// only — this is how the admin delegates an existing agent to a key holder.
//
//	POST /api/agents/{id}/binding  {"apiKeyId": "<id>"}
//
// An empty "apiKeyId" unbinds (agent returns to admin-only).
func (s *Server) handleBindAgent(w http.ResponseWriter, r *http.Request) {
	if callerFrom(r).Kind != callerAdmin {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "admin only"})
		return
	}
	if s.agentBindings == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "bindings registry not initialized"})
		return
	}
	agentID := r.PathValue("id")
	// Confirm the agent actually exists on disk so we don't record dangling
	// bindings that surprise future lookups.
	homePath, _ := config.AgentHomeDir(agentID)
	if _, err := os.Stat(homePath); err != nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "agent not found"})
		return
	}
	var req struct {
		APIKeyID string `json:"apiKeyId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	// Validate the target api key exists (empty = unbind is always fine).
	if req.APIKeyID != "" && s.userRegistry != nil {
		if _, ok := s.userRegistry.Get(req.APIKeyID); !ok {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unknown apiKeyId"})
			return
		}
	}
	s.agentBindings.Bind(agentID, req.APIKeyID)
	if err := s.agentBindings.Save(); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "apiKeyId": req.APIKeyID})
}

// handleListBindings returns the full agent → api key map. Admin only;
// api-key callers can already see their agents via GET /api/agents.
func (s *Server) handleListBindings(w http.ResponseWriter, r *http.Request) {
	if callerFrom(r).Kind != callerAdmin {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "admin only"})
		return
	}
	bindings := map[string]string{}
	if s.agentBindings != nil {
		bindings = s.agentBindings.All()
	}
	jsonResponse(w, http.StatusOK, map[string]any{"bindings": bindings})
}
