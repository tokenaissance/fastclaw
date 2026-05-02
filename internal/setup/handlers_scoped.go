package setup

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/fastclaw-ai/fastclaw/internal/auth"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/scope"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

// Scope-aware CRUD for providers + channels (and a generic settings
// endpoint). Authorization:
//
//   scope=system → super_admin only
//   scope=user   → super_admin OR scopeId == caller's user_id
//   scope=agent  → super_admin OR caller owns the agent
//
// All four routes share the same gating helper so the rules stay aligned.

// authorizeScope returns true if the request is allowed to read or write
// rows at (scope, scopeID). Mutating callers should additionally check
// `requireWritable` (which already rejects super_admin actAs mode).
func (s *Server) authorizeScope(w http.ResponseWriter, r *http.Request, sc, scopeID string) bool {
	ident, ok := auth.FromContext(r.Context())
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
		return false
	}
	if ident.Role == users.RoleSuperAdmin {
		return true
	}
	switch sc {
	case scope.System:
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "super_admin required for system scope"})
		return false
	case scope.User:
		if scopeID != ident.UserID {
			jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "cannot manage other users' configs"})
			return false
		}
		// app_user accounts are end-users provisioned by a downstream
		// app — they shouldn't be able to redirect their LLM provider
		// or fork channel bindings out from under the calling app.
		// Reads are still allowed (so the agent runtime can see what
		// the upstream stack configured for them); only mutating
		// callers reach this path via requireWritable, but we hard-
		// reject up front to be unambiguous.
		if ident.Role == users.RoleAppUser {
			jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "app_user cannot manage user-scope configs"})
			return false
		}
		return true
	case scope.Agent:
		// Must own the agent. We do an inexpensive store lookup to verify.
		if s.dataStore == nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "store not configured"})
			return false
		}
		rec, err := s.dataStore.GetAgent(r.Context(), scopeID)
		if err != nil || rec == nil || rec.UserID != ident.UserID {
			jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "agent not yours"})
			return false
		}
		return true
	default:
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid scope"})
		return false
	}
}

// scopeFromQuery reads the scope/scopeId query parameters with sensible
// defaults: missing scope falls through to the caller's user scope so a
// regular user's `GET /api/providers` returns "their" providers.
func scopeFromQuery(r *http.Request) (string, string) {
	sc := r.URL.Query().Get("scope")
	scopeID := r.URL.Query().Get("scopeId")
	if sc == "" {
		ident, _ := auth.FromContext(r.Context())
		if ident.Role == users.RoleSuperAdmin {
			sc = scope.System
		} else {
			sc = scope.User
			scopeID = ident.UserID
		}
	}
	return sc, scopeID
}

// --- Providers ---

func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	sc, scopeID := scopeFromQuery(r)
	if !s.authorizeScope(w, r, sc, scopeID) {
		return
	}
	rows, err := s.dataStore.ListConfigs(r.Context(), store.KindProvider, sc, scopeID)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		pc := config.ProviderConfig{}
		if blob, err := json.Marshal(r.Data); err == nil {
			_ = json.Unmarshal(blob, &pc)
		}
		out = append(out, map[string]any{
			"id":        r.ID,
			"scope":     r.Scope,
			"scopeId":   r.ScopeID,
			"name":      r.Name,
			"apiBase":   pc.APIBase,
			"apiKey":    maskAPIKey(pc.APIKey),
			"apiType":   pc.APIType,
			"authType":  pc.AuthType,
			"models":    pc.Models,
			"updatedAt": r.UpdatedAt,
		})
	}
	jsonResponse(w, http.StatusOK, map[string]any{"providers": out, "scope": sc, "scopeId": scopeID})
}

type writeProviderRequest struct {
	Scope    string              `json:"scope"`
	ScopeID  string              `json:"scopeId"`
	Name     string              `json:"name"`
	APIBase  string              `json:"apiBase"`
	APIKey   string              `json:"apiKey"`
	APIType  string              `json:"apiType"`
	AuthType string              `json:"authType"`
	Models   []config.ModelEntry `json:"models,omitempty"`
}

func (s *Server) handleCreateProvider(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	var req writeProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if req.Name == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "name required"})
		return
	}
	sc, scopeID := req.Scope, req.ScopeID
	if sc == "" {
		sc, scopeID = scopeFromQuery(r)
	}
	if !s.authorizeScope(w, r, sc, scopeID) {
		return
	}
	pcfg := config.ProviderConfig{
		APIBase:  req.APIBase,
		APIKey:   req.APIKey,
		APIType:  req.APIType,
		AuthType: req.AuthType,
		Models:   req.Models,
	}
	if err := scope.SaveProvider(r.Context(), s.dataStore, sc, scopeID, req.Name, pcfg); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateScope(sc, scopeID)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleUpdateProvider(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	rec, err := s.dataStore.GetConfig(r.Context(), id)
	if err != nil || rec == nil || rec.Kind != store.KindProvider {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	if !s.authorizeScope(w, r, rec.Scope, rec.ScopeID) {
		return
	}
	var req writeProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	pc := config.ProviderConfig{}
	if blob, err := json.Marshal(rec.Data); err == nil {
		_ = json.Unmarshal(blob, &pc)
	}
	// Patch — preserve apiKey when caller sent the masked sentinel.
	if req.APIBase != "" {
		pc.APIBase = req.APIBase
	}
	if req.APIKey != "" && !isMaskedSecret(req.APIKey) {
		pc.APIKey = req.APIKey
	}
	if req.APIType != "" {
		pc.APIType = req.APIType
	}
	if req.AuthType != "" {
		pc.AuthType = req.AuthType
	}
	// `models` is sent as the full desired set on every PUT (the dialog
	// is the source of truth) — overwrite even when the array is empty so
	// "remove last model" actually persists.
	if req.Models != nil {
		pc.Models = req.Models
	}
	if err := scope.SaveProvider(r.Context(), s.dataStore, rec.Scope, rec.ScopeID, rec.Name, pc); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateScope(rec.Scope, rec.ScopeID)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	rec, err := s.dataStore.GetConfig(r.Context(), id)
	if err != nil || rec == nil || rec.Kind != store.KindProvider {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	if !s.authorizeScope(w, r, rec.Scope, rec.ScopeID) {
		return
	}
	if err := s.dataStore.DeleteConfig(r.Context(), id); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateScope(rec.Scope, rec.ScopeID)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// --- Channels ---

func (s *Server) handleListScopedChannels(w http.ResponseWriter, r *http.Request) {
	sc, scopeID := scopeFromQuery(r)
	if !s.authorizeScope(w, r, sc, scopeID) {
		return
	}
	rows, err := s.dataStore.ListConfigs(r.Context(), store.KindChannel, sc, scopeID)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		cc := config.ChannelConfig{}
		if blob, err := json.Marshal(r.Data); err == nil {
			_ = json.Unmarshal(blob, &cc)
		}
		out = append(out, map[string]any{
			"id":            r.ID,
			"scope":         r.Scope,
			"scopeId":       r.ScopeID,
			"type":          r.Name,
			"enabled":       r.Enabled,
			"botToken":      maskAPIKey(cc.BotToken),
			"appToken":      maskAPIKey(cc.AppToken),
			"credentialKey": r.CredentialKey,
			"updatedAt":     r.UpdatedAt,
		})
	}
	jsonResponse(w, http.StatusOK, map[string]any{"channels": out, "scope": sc, "scopeId": scopeID})
}

type writeChannelRequest struct {
	Scope         string `json:"scope"`
	ScopeID       string `json:"scopeId"`
	Type          string `json:"type"`
	Enabled       bool   `json:"enabled"`
	BotToken      string `json:"botToken"`
	AppToken      string `json:"appToken,omitempty"`
	CredentialKey string `json:"credentialKey,omitempty"`
}

// credentialKeyFor derives a stable lookup handle from the channel's
// credentials. For bot-token channels we use the last 12 chars (matches
// how Telegram / Discord bot tokens are recognizable); falls back to the
// caller-supplied key when it's already populated.
func credentialKeyFor(channelType, botToken, callerKey string) string {
	if callerKey != "" {
		return callerKey
	}
	if botToken == "" {
		return ""
	}
	if len(botToken) <= 12 {
		return botToken
	}
	return botToken[len(botToken)-12:]
}

func (s *Server) handleCreateScopedChannel(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	var req writeChannelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if req.Type == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "type required"})
		return
	}
	sc, scopeID := req.Scope, req.ScopeID
	if sc == "" {
		sc, scopeID = scopeFromQuery(r)
	}
	if !s.authorizeScope(w, r, sc, scopeID) {
		return
	}
	credKey := credentialKeyFor(req.Type, req.BotToken, req.CredentialKey)
	if err := s.assertChannelCredentialUnique(r, req.Type, credKey, ""); err != nil {
		jsonResponse(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	cc := config.ChannelConfig{
		Enabled:  req.Enabled,
		BotToken: req.BotToken,
		AppToken: req.AppToken,
	}
	if err := scope.SaveChannel(r.Context(), s.dataStore, sc, scopeID, req.Type, credKey, req.Enabled, cc); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateScope(sc, scopeID)
	if req.Enabled {
		if rec, _ := s.dataStore.LookupChannelByCredential(r.Context(), req.Type, credKey); rec != nil {
			s.hotRegisterChannel(*rec)
		}
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleUpdateScopedChannel(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	rec, err := s.dataStore.GetConfig(r.Context(), id)
	if err != nil || rec == nil || rec.Kind != store.KindChannel {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	if !s.authorizeScope(w, r, rec.Scope, rec.ScopeID) {
		return
	}
	var req writeChannelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	cc := config.ChannelConfig{}
	if blob, err := json.Marshal(rec.Data); err == nil {
		_ = json.Unmarshal(blob, &cc)
	}
	if req.BotToken != "" && !isMaskedSecret(req.BotToken) {
		cc.BotToken = req.BotToken
	}
	if req.AppToken != "" && !isMaskedSecret(req.AppToken) {
		cc.AppToken = req.AppToken
	}
	enabled := req.Enabled
	credKey := credentialKeyFor(rec.Name, cc.BotToken, req.CredentialKey)
	if credKey != rec.CredentialKey {
		if err := s.assertChannelCredentialUnique(r, rec.Name, credKey, rec.ID); err != nil {
			jsonResponse(w, http.StatusConflict, map[string]any{"error": err.Error()})
			return
		}
	}
	if err := scope.SaveChannel(r.Context(), s.dataStore, rec.Scope, rec.ScopeID, rec.Name, credKey, enabled, cc); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateScope(rec.Scope, rec.ScopeID)
	if enabled {
		if updated, _ := s.dataStore.LookupChannelByCredential(r.Context(), rec.Name, credKey); updated != nil {
			s.hotRegisterChannel(*updated)
		}
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteScopedChannel(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	rec, err := s.dataStore.GetConfig(r.Context(), id)
	if err != nil || rec == nil || rec.Kind != store.KindChannel {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	if !s.authorizeScope(w, r, rec.Scope, rec.ScopeID) {
		return
	}
	if err := s.dataStore.DeleteConfig(r.Context(), id); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateScope(rec.Scope, rec.ScopeID)
	// Best-effort: stop the bot adapter from receiving outbound routes.
	// We don't know its accountID without decoding rec, so derive from
	// the row we just looked up (rec is still valid here).
	cc := decodeChannelConfigFromRecord(rec)
	for accountID := range cc.Accounts {
		s.hotUnregisterChannel(rec.Name, accountID)
	}
	if len(cc.Accounts) == 0 {
		s.hotUnregisterChannel(rec.Name, "")
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// decodeChannelConfigFromRecord — local mirror of gateway.decodeChannelConfig
// so this package doesn't need to import gateway.
func decodeChannelConfigFromRecord(rec *store.ConfigRecord) config.ChannelConfig {
	cc := config.ChannelConfig{Enabled: rec.Enabled}
	if blob, err := json.Marshal(rec.Data); err == nil && len(blob) > 0 {
		_ = json.Unmarshal(blob, &cc)
	}
	cc.Enabled = rec.Enabled
	return cc
}

// assertChannelCredentialUnique enforces the soft uniqueness invariant the
// schema doesn't have a DB constraint for: two rows with the same
// (kind="channel", credential_key) would race in the inbound dispatcher.
// excludeID lets the update path skip the row being mutated.
func (s *Server) assertChannelCredentialUnique(r *http.Request, channelType, credKey, excludeID string) error {
	if credKey == "" {
		return nil
	}
	existing, err := s.dataStore.LookupChannelByCredential(r.Context(), channelType, credKey)
	if err != nil {
		return nil // not-found is fine; other errors get bubbled by the caller
	}
	if existing == nil || existing.ID == excludeID {
		return nil
	}
	return errors.New("another channel row already uses this credential")
}

// invalidateScope drops cached UserSpaces affected by a scope-level
// write. System changes touch every loaded space; user changes touch one.
func (s *Server) invalidateScope(sc, scopeID string) {
	if s.userResolver == nil {
		return
	}
	type globalInvalidator interface{ ReloadAgents() error }
	type userInvalidator interface{ InvalidateUser(string) }
	switch sc {
	case scope.System:
		if r, ok := s.userResolver.(globalInvalidator); ok {
			_ = r.ReloadAgents()
		}
	case scope.User:
		if r, ok := s.userResolver.(userInvalidator); ok {
			r.InvalidateUser(scopeID)
		}
	case scope.Agent:
		// Agent-scoped writes affect the owning user. Find the agent's
		// owner and drop that user's cached UserSpace.
		ctx := context.Background()
		if all, err := s.dataStore.ListAllAgents(ctx); err == nil {
			for _, ar := range all {
				if ar.ID == scopeID {
					if r, ok := s.userResolver.(userInvalidator); ok {
						r.InvalidateUser(ar.UserID)
					}
					return
				}
			}
		}
	}
}

// hotRegisterChannel asks the gateway to start the channel adapter for
// `rec` immediately. Best-effort — no-op when the resolver doesn't
// implement the hook (e.g. in tests with a stub resolver).
func (s *Server) hotRegisterChannel(rec store.ConfigRecord) {
	if s.userResolver == nil {
		return
	}
	type chanRegistrar interface {
		RegisterChannelFromConfig(rec store.ConfigRecord) error
	}
	if r, ok := s.userResolver.(chanRegistrar); ok {
		if err := r.RegisterChannelFromConfig(rec); err != nil {
			// Don't fail the request — the row is saved, the next
			// process restart will pick it up. But surface the error
			// in logs so an obviously-broken bot token is debuggable.
			slog.Warn("hot-register channel failed", "type", rec.Name, "error", err)
		}
	}
}

// hotUnregisterChannel — paired with hotRegisterChannel for delete paths.
func (s *Server) hotUnregisterChannel(channelType, accountID string) {
	if s.userResolver == nil {
		return
	}
	type chanUnregistrar interface {
		UnregisterChannel(channelType, accountID string)
	}
	if r, ok := s.userResolver.(chanUnregistrar); ok {
		r.UnregisterChannel(channelType, accountID)
	}
}
