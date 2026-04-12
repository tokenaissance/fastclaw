package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/users"
)

// RegisterAdminRoutes adds /v1/admin/* endpoints to the mux. These are
// protected by the gateway auth token (the "admin" token) and allow
// programmatic user management in cloud mode.
func (s *Server) RegisterAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/admin/users", s.adminAuth(s.handleAdminCreateUser))
	mux.HandleFunc("GET /v1/admin/users", s.adminAuth(s.handleAdminListUsers))
	mux.HandleFunc("DELETE /v1/admin/users/{id}", s.adminAuth(s.handleAdminDeleteUser))
	mux.HandleFunc("POST /v1/admin/users/{id}/token", s.adminAuth(s.handleAdminIssueToken))
}

// adminAuth is a middleware that requires the gateway admin token (the local
// bearer token). Cloud-user bearer tokens are NOT accepted here.
func (s *Server) adminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" {
			writeUnauth(w, "admin endpoints require a gateway auth token")
			return
		}
		auth := r.Header.Get("Authorization")
		token := strings.TrimPrefix(auth, "Bearer ")
		if token == auth || token != s.token {
			writeUnauth(w, "invalid admin token")
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		next(w, r)
	}
}

func (s *Server) handleAdminCreateUser(w http.ResponseWriter, r *http.Request) {
	if s.registry == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "user management requires cloud mode (gateway.mode = \"cloud\")",
		})
		return
	}
	var req struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id is required"})
		return
	}

	u, token, err := s.registry.Add(req.ID, req.Name)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	if err := s.registry.Save(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// Provision workspace (config + default agent) so the user can
	// immediately start using the web UI and API.
	if err := users.ProvisionWorkspace(req.ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "user created but workspace provisioning failed: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"user":  u,
		"token": token,
	})
}

func (s *Server) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	if s.registry == nil {
		writeJSON(w, http.StatusOK, map[string]any{"users": []*users.User{}})
		return
	}
	list := s.registry.List()
	// Mask tokens for list endpoint.
	for _, u := range list {
		for i, t := range u.Tokens {
			if len(t) > 10 {
				u.Tokens[i] = t[:6] + "..." + t[len(t)-4:]
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": list})
}

func (s *Server) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	if s.registry == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "cloud mode required"})
		return
	}
	id := r.PathValue("id")
	if err := s.registry.Remove(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	if err := s.registry.Save(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAdminIssueToken(w http.ResponseWriter, r *http.Request) {
	if s.registry == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "cloud mode required"})
		return
	}
	id := r.PathValue("id")
	token, err := s.registry.IssueToken(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	if err := s.registry.Save(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token})
}
