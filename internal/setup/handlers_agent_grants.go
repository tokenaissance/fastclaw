package setup

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/auth"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

// Agent grants ("share with another user") — owner-issued read-only
// access. The agent owner uses these endpoints to invite specific users
// to chat with their agent; grantees see the agent in their /api/agents
// list (role=viewer) but cannot mutate SOUL, skills, channels, or any
// other agent configuration. Sessions/memory remain partitioned per
// (user_id, agent_id), so each grantee gets their own private chat
// history.

type grantOut struct {
	UserID      string `json:"userId"`
	Username    string `json:"username,omitempty"`
	Email       string `json:"email,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	GrantedBy   string `json:"grantedBy,omitempty"`
	GrantedAt   string `json:"grantedAt,omitempty"`
}

// handleListAgentGrants returns the list of users currently allowed to
// chat with the agent. Owner-only.
func (s *Server) handleListAgentGrants(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.requireAgentOwner(w, r, id) == nil {
		return
	}
	rows, err := s.dataStore.ListAgentGrants(r.Context(), id)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]grantOut, 0, len(rows))
	for _, g := range rows {
		entry := grantOut{
			UserID:    g.UserID,
			GrantedBy: g.GrantedBy,
			GrantedAt: g.GrantedAt.Format("2006-01-02T15:04:05Z07:00"),
		}
		if u, err := s.dataStore.GetUser(r.Context(), g.UserID); err == nil && u != nil {
			entry.Username = u.Username
			entry.Email = u.Email
			entry.DisplayName = u.DisplayName
		}
		out = append(out, entry)
	}
	jsonResponse(w, http.StatusOK, map[string]any{"grants": out})
}

type grantCreateRequest struct {
	// Identifier is the grantee's email or username. UserID is also
	// accepted for callers that already resolved it.
	Identifier string `json:"identifier"`
	UserID     string `json:"userId,omitempty"`
}

// handleCreateAgentGrant issues a share grant. Resolves the grantee by
// email/username (or explicit user_id), upserts the grant, and
// invalidates the grantee's UserSpace so the agent shows up on their
// next request without a restart.
func (s *Server) handleCreateAgentGrant(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	rec := s.requireAgentOwner(w, r, id)
	if rec == nil {
		return
	}
	var req grantCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	req.Identifier = strings.TrimSpace(req.Identifier)
	req.UserID = strings.TrimSpace(req.UserID)
	if req.Identifier == "" && req.UserID == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "identifier or userId required"})
		return
	}

	var grantee *store.UserRecord
	if req.UserID != "" {
		u, err := s.dataStore.GetUser(r.Context(), req.UserID)
		if err != nil || u == nil {
			jsonResponse(w, http.StatusNotFound, map[string]any{"error": "user not found"})
			return
		}
		grantee = u
	} else {
		u, err := s.dataStore.GetUserByLogin(r.Context(), req.Identifier)
		if err != nil || u == nil {
			if errors.Is(err, store.ErrNotFound) {
				jsonResponse(w, http.StatusNotFound, map[string]any{"error": "no user with that email or username"})
				return
			}
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": "lookup failed"})
			return
		}
		grantee = u
	}

	if grantee.ID == rec.UserID {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "cannot share an agent with its owner"})
		return
	}
	// Don't share with app_user / disabled accounts — they're either
	// programmatic identities or not allowed in.
	if grantee.Role == users.RoleAppUser || grantee.Status == users.StatusDisabled {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "user is not eligible to receive shares"})
		return
	}

	ident, _ := auth.FromContext(r.Context())
	grantedBy := ident.UserID
	if err := s.dataStore.GrantAgent(r.Context(), rec.ID, grantee.ID, grantedBy); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// Drop the grantee's cached UserSpace so the next request rebuilds
	// it with the new shared agent loaded.
	s.invalidateUser(grantee.ID)
	jsonResponse(w, http.StatusCreated, map[string]any{
		"grant": grantOut{
			UserID:      grantee.ID,
			Username:    grantee.Username,
			Email:       grantee.Email,
			DisplayName: grantee.DisplayName,
			GrantedBy:   grantedBy,
		},
	})
}

// handleDeleteAgentGrant revokes a share. Same auth as create. Path:
// /api/agents/{id}/grants/{userId}.
func (s *Server) handleDeleteAgentGrant(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	if s.requireAgentOwner(w, r, id) == nil {
		return
	}
	granteeID := r.PathValue("userId")
	if granteeID == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "userId required"})
		return
	}
	if err := s.dataStore.RevokeAgent(r.Context(), id, granteeID); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateUser(granteeID)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}
