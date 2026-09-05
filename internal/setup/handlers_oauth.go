package setup

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"

	"github.com/fastclaw-ai/fastclaw/internal/auth"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/usecase"
)

// oauthDisabled reports whether MCP OAuth is wired in this process.
func (s *Server) oauthDisabled(w http.ResponseWriter) bool {
	if s.mcpOAuth != nil {
		return false
	}
	jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "mcp oauth is not configured (FASTAGENT_OAUTH_SECRET)"})
	return true
}

// handleMcpOAuthStart initiates an authorization flow for the caller's
// agent + server. Returns the provider auth URL the SPA should redirect
// to, plus the one-shot state (useful for debugging / status display).
func (s *Server) handleMcpOAuthStart(w http.ResponseWriter, r *http.Request) {
	if s.oauthDisabled(w) {
		return
	}
	var req struct {
		AgentID      string   `json:"agentId"`
		ServerName   string   `json:"serverName"`
		ServerURL    string   `json:"serverUrl"`
		CallbackURL  string   `json:"callbackUrl"`
		CallbackBase string   `json:"callbackBase"`
		Scopes       []string `json:"scopes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if req.AgentID == "" || req.ServerName == "" || req.ServerURL == "" || (req.CallbackURL == "" && req.CallbackBase == "") {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "agentId, serverName, serverUrl and callbackUrl/callbackBase are required"})
		return
	}
	ownerID, ok := s.oauthAgentOwner(r, req.AgentID)
	if !ok {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "agent not accessible"})
		return
	}
	out, err := s.mcpOAuth.Start.Execute(r.Context(), usecase.StartAuthInput{
		UserID:       ownerID,
		AgentID:      req.AgentID,
		ServerName:   req.ServerName,
		ServerURL:    req.ServerURL,
		CallbackURL:  req.CallbackURL,
		CallbackBase: req.CallbackBase,
		Scopes:       req.Scopes,
	})
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "authUrl": out.AuthURL, "state": out.State, "callbackUrl": out.CallbackURL})
}

// handleMcpOAuthCallback completes an authorization from a forwarded
// callback (the cloud proxies the provider redirect here with the admin
// key; the state is single-use so replay is impossible).
func (s *Server) handleMcpOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if s.oauthDisabled(w) {
		return
	}
	var req struct {
		Code  string `json:"code"`
		State string `json:"state"`
		Iss   string `json:"iss"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	out, err := s.mcpOAuth.Complete.Execute(r.Context(), usecase.CompleteAuthInput{
		Callback: domain.CallbackParams{Code: req.Code, State: req.State, Issuer: req.Iss},
	})
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.notifyAgentChanged(out.UserID, out.AgentID)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// handleMcpOAuthPublicCallback is the self-hosted redirect landing:
// provider -> GET /oauth/mcp/{callbackID}/callback?code=...&state=...
// The path binds the callback to a specific server (mix-up protection).
// Session-free by design — security rests on single-use state + PKCE.
func (s *Server) handleMcpOAuthPublicCallback(w http.ResponseWriter, r *http.Request) {
	if s.oauthDisabled(w) {
		http.Redirect(w, r, "/?mcp=oauth-disabled", http.StatusFound)
		return
	}
	// callbackID only helps the provider-side redirect_uri match; the
	// state itself binds to the pending auth, so we don't need to map
	// the id back to a server here.
	params, err := domain.ParseCallbackURL(r.URL.String())
	if err != nil {
		http.Redirect(w, r, "/?mcp=invalid-callback", http.StatusFound)
		return
	}
	out, err := s.mcpOAuth.Complete.Execute(r.Context(), usecase.CompleteAuthInput{Callback: params})
	if err != nil {
		slog.Warn("mcp oauth public callback rejected", "error", err)
		http.Redirect(w, r, "/?mcp=denied", http.StatusFound)
		return
	}
	s.notifyAgentChanged(out.UserID, out.AgentID)
	http.Redirect(w, r, "/?mcp=authorized", http.StatusFound)
}

// handleMcpOAuthStatus reports whether an agent+server is authorized.
func (s *Server) handleMcpOAuthStatus(w http.ResponseWriter, r *http.Request) {
	if s.oauthDisabled(w) {
		return
	}
	q := r.URL.Query()
	agentID, serverName := q.Get("agentId"), q.Get("serverName")
	if agentID == "" || serverName == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "agentId and serverName are required"})
		return
	}
	ownerID, ok := s.oauthAgentOwner(r, agentID)
	if !ok {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "agent not accessible"})
		return
	}
	out, err := s.mcpOAuth.Status.Execute(r.Context(), usecase.RefreshInput{
		UserID: ownerID, AgentID: agentID, ServerName: serverName,
	})
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok": true, "status": out.Status, "scopes": out.Scopes, "issuer": out.Issuer, "expiresAt": out.ExpiresAt,
	})
}

// handleMcpOAuthServers lists the agent's OAuth-protected MCP servers
// declared in agent_mcp_servers (one row per server), each with the local
// authorization status the cloud console renders. Never returns token
// material. Owner or platform admin only — same boundary as start/revoke.
func (s *Server) handleMcpOAuthServers(w http.ResponseWriter, r *http.Request) {
	if s.oauthDisabled(w) {
		return
	}
	agentID := r.URL.Query().Get("agentId")
	if agentID == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "agentId is required"})
		return
	}
	ownerID, ok := s.oauthAgentOwner(r, agentID)
	if !ok {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "agent not accessible"})
		return
	}
	rec, err := s.dataStore.GetAgent(r.Context(), agentID)
	if err != nil || rec == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "agent not found"})
		return
	}

	configured, err := s.dataStore.ListMCPServers(r.Context(), agentID)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	names := make([]string, 0, len(configured))
	for name := range configured {
		names = append(names, name)
	}
	sort.Strings(names)

	servers := make([]map[string]any, 0, len(names))
	for _, name := range names {
		cfg := configured[name]
		if cfg.OAuthResource == "" {
			continue // static-header servers have nothing to authorize
		}
		out, serr := s.mcpOAuth.Status.Execute(r.Context(), usecase.RefreshInput{
			UserID: ownerID, AgentID: agentID, ServerName: name, ServerURL: cfg.OAuthResource,
		})
		if serr != nil {
			slog.Warn("mcp oauth servers: status lookup failed", "agent", agentID, "server", name, "error", serr)
			continue
		}
		servers = append(servers, map[string]any{
			"serverName": name,
			"url":        cfg.OAuthResource,
			"status":     string(out.Status),
			"scopes":     out.Scopes,
			"issuer":     out.Issuer,
			"expiresAt":  out.ExpiresAt,
		})
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "servers": servers})
}

// handleMcpOAuthRevoke revokes and deletes the stored credential.
func (s *Server) handleMcpOAuthRevoke(w http.ResponseWriter, r *http.Request) {
	if s.oauthDisabled(w) {
		return
	}
	var req struct {
		AgentID    string `json:"agentId"`
		ServerName string `json:"serverName"`
		ServerURL  string `json:"serverUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	ownerID, ok := s.oauthAgentOwner(r, req.AgentID)
	if !ok {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "agent not accessible"})
		return
	}
	if err := s.mcpOAuth.Revoke.Execute(r.Context(), usecase.RefreshInput{
		UserID: ownerID, AgentID: req.AgentID, ServerName: req.ServerName, ServerURL: req.ServerURL,
	}); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.notifyAgentChanged(ownerID, req.AgentID)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// oauthAgentOwner validates that the caller is allowed to bind OAuth
// credentials to agentID and returns the agent's OWNING user id
// (agents.user_id). Credentials are stored under the owner so the agent
// loop (which reads tokens with rc.UserID = owner) finds them.
//
// Only the agent owner or a platform admin may authorize a provider for
// an agent. Public-agent visitors are deliberately excluded: an outsider
// must not bind their provider account to someone else's agent.
func (s *Server) oauthAgentOwner(r *http.Request, agentID string) (string, bool) {
	if agentID == "" {
		return "", false
	}
	ident, ok := auth.FromContext(r.Context())
	if !ok {
		return "", false
	}
	// OAuth start/revoke are mutating, ownership-scoped actions; actAs
	// browsing is read-only by contract.
	if ident.IsActingAs() || !ident.CanAccessAgent(agentID) {
		return "", false
	}
	rec, err := s.dataStore.GetAgent(r.Context(), agentID)
	if err != nil || rec == nil {
		return "", false
	}
	if rec.UserID == "" || (ident.UserID != rec.UserID && !ident.CanAdminPlatform()) {
		return "", false
	}
	return rec.UserID, true
}

// handleMcpOAuthRefresh manually refreshes a credential (ops). Never
// returns token material — only the new expiry.
func (s *Server) handleMcpOAuthRefresh(w http.ResponseWriter, r *http.Request) {
	if s.oauthDisabled(w) {
		return
	}
	var req struct {
		UserID     string `json:"userId"`
		AgentID    string `json:"agentId"`
		ServerName string `json:"serverName"`
		ServerURL  string `json:"serverUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if req.UserID == "" || req.AgentID == "" || req.ServerName == "" || req.ServerURL == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "userId, agentId, serverName and serverUrl are required"})
		return
	}
	tokens, err := s.mcpOAuth.Refresh.Execute(r.Context(), usecase.RefreshInput{
		UserID: req.UserID, AgentID: req.AgentID, ServerName: req.ServerName, ServerURL: req.ServerURL,
	})
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "expiresAt": tokens.ExpiresAt})
}

// notifyAgentChanged drops the affected (user, agent) caches everywhere:
// local InvalidateAgent (owner's space + any admin-attached space holding
// the agent), then per-user DB epoch bump (no-Redis replicas poll it) and
// per-user Redis broadcast (instant). Never a system-wide reload — one
// tenant's authorization must not flush other tenants' caches.
func (s *Server) notifyAgentChanged(userID, agentID string) {
	if userID == "" || agentID == "" {
		return
	}
	s.invalidateAgent(agentID)
	if b, ok := s.userResolver.(interface{ BumpAgentReloadEpoch(userID string) error }); ok {
		if err := b.BumpAgentReloadEpoch(userID); err != nil {
			slog.Warn("bump agent reload epoch after mcp oauth", "user", userID, "error", err)
		}
	}
	if b, ok := s.userResolver.(interface{ BroadcastAgentReload(userID string) error }); ok {
		if err := b.BroadcastAgentReload(userID); err != nil {
			slog.Warn("broadcast agent reload after mcp oauth", "user", userID, "error", err)
		}
	}
}
