package setup

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/auth"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/scope"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/users"
	"github.com/fastclaw-ai/fastclaw/internal/workspace"
)

// agentScopeModel reads the per-agent model override from the configs
// table — the kind=setting, scope=agent row that supersedes the
// system/user defaults when set.
func (s *Server) agentScopeModel(r *http.Request, agentID string) string {
	rec, err := s.dataStore.GetConfigByName(r.Context(), store.KindSetting, store.ScopeAgent, agentID, "agents.defaults")
	if err != nil || rec == nil {
		return ""
	}
	if v, ok := rec.Data["model"].(string); ok {
		return v
	}
	return ""
}

// saveAgentScopeModel upserts (model="") or deletes (model=="") the
// agent-scope agents.defaults row.
func (s *Server) saveAgentScopeModel(r *http.Request, agentID, model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return scope.SaveSetting(r.Context(), s.dataStore, scope.Agent, agentID, "agents.defaults", nil)
	}
	return scope.SaveSetting(r.Context(), s.dataStore, scope.Agent, agentID, "agents.defaults", map[string]interface{}{"model": model})
}

// effectiveUserID returns the resolved user_id for the request: the
// caller's own id, or — for super_admin in actAs mode — the impersonated
// user's id.
func (s *Server) effectiveUserID(r *http.Request) string {
	ident, ok := auth.FromContext(r.Context())
	if !ok {
		return ""
	}
	return ident.EffectiveUserID()
}

// requireWritable returns true if the caller may mutate, writing a 4xx
// response and false otherwise.
func (s *Server) requireWritable(w http.ResponseWriter, r *http.Request) bool {
	ident, ok := auth.FromContext(r.Context())
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
		return false
	}
	if ident.ReadOnly() {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "read-only"})
		return false
	}
	return true
}

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	uid := s.effectiveUserID(r)
	if uid == "" {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	records, err := s.dataStore.ListAgents(r.Context(), uid)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(records))
	for _, ar := range records {
		desc, _ := ar.Config["description"].(string)
		out = append(out, map[string]any{
			"id":          ar.ID,
			"name":        ar.Name,
			"description": desc,
			"model":       s.agentScopeModel(r, ar.ID),
			"avatarUrl":   "/api/agents/" + ar.ID + "/files/avatar.png",
			"createdAt":   ar.CreatedAt,
		})
	}
	jsonResponse(w, http.StatusOK, map[string]any{"agents": out})
}

type createAgentRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Model       string `json:"model,omitempty"`
}

func (s *Server) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	uid := s.effectiveUserID(r)
	var req createAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if req.Name == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "name required"})
		return
	}
	id, err := generateID("agt_")
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	rec := &store.AgentRecord{
		ID:     id,
		UserID: uid,
		Name:   req.Name,
	}
	if req.Description != "" {
		// Description lives in the agents.config JSON blob — keeps the
		// schema stable while still surfacing through GetAgentConfig and
		// the agents.config namespace settings overlay.
		rec.Config = map[string]interface{}{"description": req.Description}
	}
	if err := s.dataStore.SaveAgent(r.Context(), rec); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if req.Model != "" {
		if err := s.saveAgentScopeModel(r, id, req.Model); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	s.invalidateUser(uid)
	jsonResponse(w, http.StatusCreated, map[string]any{
		"agent": map[string]any{
			"id":     rec.ID,
			"userId": rec.UserID,
			"name":   rec.Name,
			"model":  req.Model,
			"config": rec.Config,
		},
	})
}

// requireAgentOwner returns the agent record if the caller owns it (or is
// super_admin), otherwise writes a 403/404 and returns nil.
func (s *Server) requireAgentOwner(w http.ResponseWriter, r *http.Request, agentID string) *store.AgentRecord {
	uid := s.effectiveUserID(r)
	rec, err := s.dataStore.GetAgent(r.Context(), agentID)
	if err != nil || rec == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return nil
	}
	ident, _ := auth.FromContext(r.Context())
	if rec.UserID != uid && ident.Role != users.RoleSuperAdmin {
		jsonResponse(w, http.StatusForbidden, map[string]any{"error": "not your agent"})
		return nil
	}
	return rec
}

// requireAgentReadable allows access when the caller is the owner OR the
// caller's identity is granted access via the apikey ACL (CanAccessAgent).
// This is the same gate /api/chat/history uses, so app_user requests
// proxied through an integration with X-Fastclaw-End-User can read agent
// artifacts (files, file list, zip) for sessions they own without
// 403'ing on the strict ownership check.
func (s *Server) requireAgentReadable(w http.ResponseWriter, r *http.Request, agentID string) bool {
	rec, err := s.dataStore.GetAgent(r.Context(), agentID)
	if err != nil || rec == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return false
	}
	uid := s.effectiveUserID(r)
	ident, _ := auth.FromContext(r.Context())
	if rec.UserID == uid || ident.Role == users.RoleSuperAdmin {
		return true
	}
	if ident.CanAccessAgent(agentID) {
		return true
	}
	jsonResponse(w, http.StatusForbidden, map[string]any{"error": "not your agent"})
	return false
}

func (s *Server) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	rec := s.requireAgentOwner(w, r, id)
	if rec == nil {
		return
	}
	var req struct {
		Name        string  `json:"name,omitempty"`
		Description *string `json:"description,omitempty"` // ptr so empty-string clears it
		Model       string  `json:"model,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if req.Name != "" {
		rec.Name = req.Name
	}
	if req.Description != nil {
		if rec.Config == nil {
			rec.Config = map[string]interface{}{}
		}
		if *req.Description == "" {
			delete(rec.Config, "description")
		} else {
			rec.Config["description"] = *req.Description
		}
	}
	if err := s.dataStore.SaveAgent(r.Context(), rec); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// Per-agent model override is its own configs row now. Empty string
	// means "no change" (matches the original column-write semantics);
	// to clear an existing override the caller must explicitly hit the
	// scoped settings endpoint with an empty value.
	if req.Model != "" {
		if err := s.saveAgentScopeModel(r, rec.ID, req.Model); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	s.invalidateUser(rec.UserID)
	jsonResponse(w, http.StatusOK, map[string]any{
		"agent": map[string]any{
			"id":     rec.ID,
			"userId": rec.UserID,
			"name":   rec.Name,
			"model":  s.agentScopeModel(r, rec.ID),
			"config": rec.Config,
		},
	})
}

// handleGetAgent returns the basic AgentRecord (id, name, description,
// userId) for one agent. Used by the chat header / sidebar switcher to
// resolve a display name when the agent isn't in the caller's own
// list — e.g. super_admin viewing another user's agent. Permission is
// the same as the rest of the agent endpoints: owner or super_admin.
func (s *Server) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec := s.requireAgentOwner(w, r, id)
	if rec == nil {
		return
	}
	desc, _ := rec.Config["description"].(string)
	jsonResponse(w, http.StatusOK, map[string]any{
		"agent": map[string]any{
			"id":          rec.ID,
			"name":        rec.Name,
			"description": desc,
			"userId":      rec.UserID,
			"model":       s.agentScopeModel(r, rec.ID),
			"avatarUrl":   "/api/agents/" + rec.ID + "/files/avatar.png",
			"createdAt":   rec.CreatedAt,
		},
	})
}

func (s *Server) handleGetAgentConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec := s.requireAgentOwner(w, r, id)
	if rec == nil {
		return
	}
	cfg := config.AgentFileConfig{}
	if len(rec.Config) > 0 {
		blob, _ := json.Marshal(rec.Config)
		_ = json.Unmarshal(blob, &cfg)
	}
	jsonResponse(w, http.StatusOK, cfg)
}

func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	rec := s.requireAgentOwner(w, r, id)
	if rec == nil {
		return
	}
	if err := s.dataStore.DeleteAgent(r.Context(), id); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateUser(rec.UserID)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// Agent identity / memory files — all live in agent_files, agent-scoped.
// Filename allowlist gates which files the admin UI can edit through this
// endpoint; agent-runtime tool calls go through the workspace store
// instead.
var agentSystemFileAllowlist = map[string]bool{
	"SOUL.md": true, "IDENTITY.md": true, "AGENTS.md": true,
	"BOOTSTRAP.md": true, "TOOLS.md": true, "MEMORY.md": true,
	"HEARTBEAT.md": true, "USER.md": true, "agent.json": true,
}

// systemFileUserScope returns the user_id to use for Customize page
// CRUD on system files. Every read/write is keyed by the caller's
// effective user_id; a user with no override on a given (agent_id,
// filename) gets an empty content blob from the API. The agent runtime
// transparently falls back to a local FS file at <agent_home>/<name>
// for installs that want a global default for an agent.
func (s *Server) systemFileUserScope(r *http.Request) string {
	return s.effectiveUserID(r)
}

func (s *Server) handleGetAgentSystemFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := r.PathValue("name")
	if !agentSystemFileAllowlist[name] {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "filename not allowed"})
		return
	}
	data, err := s.dataStore.GetAgentFileExact(r.Context(), id, s.systemFileUserScope(r), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			jsonResponse(w, http.StatusOK, map[string]any{"content": ""})
			return
		}
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"content": string(data)})
}

func (s *Server) handlePutAgentSystemFile(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	name := r.PathValue("name")
	if !agentSystemFileAllowlist[name] {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "filename not allowed"})
		return
	}
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := s.dataStore.SaveAgentFile(r.Context(), id, s.systemFileUserScope(r), name, []byte(body.Content)); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateUser(s.effectiveUserID(r))
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteAgentSystemFile(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	name := r.PathValue("name")
	if !agentSystemFileAllowlist[name] {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "filename not allowed"})
		return
	}
	if err := s.dataStore.DeleteAgentFile(r.Context(), id, s.systemFileUserScope(r), name); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateUser(s.effectiveUserID(r))
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// Workspace files — list / get / upload of agent-produced artifacts.
// Backed by the workspace.Store blob backend, whose layout is
//
//   workspaces/<agent_id>/<session_id>/<path>
//
// The HTTP file endpoints below operate at the agent-root level
// (sessionID="") — that's where uploads land and where ListByAgent
// returns objects across every session of that agent. The agent runtime
// passes its own sessionID for in-chat tool calls; those land under the
// session sub-prefix automatically.

func (s *Server) handleAgentFileList(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.workspaceStore == nil {
		jsonResponse(w, http.StatusOK, map[string]any{"files": []any{}})
		return
	}
	if !s.requireAgentReadable(w, r, id) {
		return
	}
	// Always List with sessionID="" so returned paths stay agent-relative
	// (e.g. "sessions/<sid>/foo.png") — the download endpoint expects that
	// shape, and filtering here is cheaper than two divergent code paths.
	objects, err := s.workspaceStore.List(r.Context(), id, "")
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	sessionID := strings.TrimSpace(r.URL.Query().Get("sessionId"))
	var prefix string
	if sessionID != "" {
		prefix = "sessions/" + sessionID + "/"
	}
	files := make([]map[string]any, 0, len(objects))
	for _, o := range objects {
		if prefix != "" && !strings.HasPrefix(o.Path, prefix) {
			continue
		}
		files = append(files, map[string]any{
			"path":    o.Path,
			"size":    o.Size,
			"modTime": o.ModTime.Unix(),
		})
	}
	jsonResponse(w, http.StatusOK, map[string]any{"files": files})
}

// handleAgentFilesZip streams a zip of every workspace file for the agent
// (or just one session when ?sessionId= is set). Files are added with
// their session-relative path so the archive layout matches what the user
// sees in the chat panel — no enclosing wrapper directory.
func (s *Server) handleAgentFilesZip(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.workspaceStore == nil {
		http.Error(w, "no workspace store", http.StatusServiceUnavailable)
		return
	}
	if !s.requireAgentReadable(w, r, id) {
		return
	}
	objects, err := s.workspaceStore.List(r.Context(), id, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sessionID := strings.TrimSpace(r.URL.Query().Get("sessionId"))
	var prefix, archiveName string
	if sessionID != "" {
		prefix = "sessions/" + sessionID + "/"
		archiveName = fmt.Sprintf("%s-%s.zip", id, sessionID)
	} else {
		archiveName = fmt.Sprintf("%s.zip", id)
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, archiveName))

	zw := zip.NewWriter(w)
	defer zw.Close()

	for _, o := range objects {
		if prefix != "" && !strings.HasPrefix(o.Path, prefix) {
			continue
		}
		entryName := o.Path
		if prefix != "" {
			entryName = strings.TrimPrefix(o.Path, prefix)
		}
		if entryName == "" {
			continue
		}
		hdr := &zip.FileHeader{
			Name:     entryName,
			Method:   zip.Deflate,
			Modified: o.ModTime,
		}
		entry, err := zw.CreateHeader(hdr)
		if err != nil {
			slog.Warn("zip: create entry failed", "agent", id, "path", o.Path, "err", err)
			return
		}
		rc, err := s.workspaceStore.Get(r.Context(), id, "", o.Path)
		if err != nil {
			slog.Warn("zip: open object failed", "agent", id, "path", o.Path, "err", err)
			continue
		}
		_, copyErr := io.Copy(entry, rc)
		rc.Close()
		if copyErr != nil {
			slog.Warn("zip: copy failed", "agent", id, "path", o.Path, "err", copyErr)
			return
		}
	}
}

func (s *Server) handleAgentFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rel := r.PathValue("path")
	if rel == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "path required"})
		return
	}
	if !s.requireAgentReadable(w, r, id) {
		return
	}
	if s.workspaceStore != nil {
		s.serveFileFromWorkspaceStore(w, r, id, rel)
		return
	}
	// Workspace store not configured — fall back to direct FS read.
	// The local FS layout mirrors the workspace store's:
	// ~/.fastclaw/workspaces/<agent_id>/<path>.
	home, err := config.HomeDir()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	root := filepath.Join(home, "workspaces", id)
	abs := filepath.Join(root, filepath.Clean("/"+rel))
	if !strings.HasPrefix(abs, root+string(os.PathSeparator)) && abs != root {
		jsonResponse(w, http.StatusForbidden, map[string]any{"error": "path escape"})
		return
	}
	http.ServeFile(w, r, abs)
}

func (s *Server) serveFileFromWorkspaceStore(w http.ResponseWriter, r *http.Request, agentID, path string) {
	rc, err := s.workspaceStore.Get(r.Context(), agentID, "", path)
	if err != nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	io.Copy(w, rc)
}

func (s *Server) handleAgentFileUpload(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	if s.workspaceStore == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "no workspace store"})
		return
	}
	if rec := s.requireAgentOwner(w, r, id); rec == nil {
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	// The chat client sends one form field "file" per attachment, so the
	// multipart payload often carries several entries under the same key.
	// r.FormFile only returns the first — iterate over MultipartForm.File
	// so multi-attach uploads land all of their files, not just one.
	headers := r.MultipartForm.File["file"]
	if len(headers) == 0 {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "no file"})
		return
	}
	// sessionId scopes the upload to the sandbox mount the agent actually
	// sees (<agent>/sessions/<sid>/). Without it, files land at the agent
	// root and list_dir on /workspace can't find them.
	sessionID := r.URL.Query().Get("sessionId")
	saved := make([]map[string]any, 0, len(headers))
	for _, h := range headers {
		fh, err := h.Open()
		if err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		data, err := io.ReadAll(fh)
		fh.Close()
		if err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if err := s.workspaceStore.Put(r.Context(), id, sessionID, h.Filename, strings.NewReader(string(data)), int64(len(data)), ""); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		saved = append(saved, map[string]any{"name": h.Filename, "size": len(data)})
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "files": saved})
}

func defaultIfEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// invalidateUser drops the user's lazy-loaded UserSpace so the next
// access reloads it from the DB. The gateway implements InvalidateUser
// behind the api.UserResolver interface.
func (s *Server) invalidateUser(userID string) {
	if userID == "" || s.userResolver == nil {
		return
	}
	if r, ok := s.userResolver.(interface{ InvalidateUser(string) }); ok {
		r.InvalidateUser(userID)
	}
	slog.Debug("invalidated user space", "user", userID)
}

// requireOwnerOrSuperAdmin guards endpoints that mutate another user's
// resources.
func (s *Server) requireOwnerOrSuperAdmin(w http.ResponseWriter, r *http.Request, ownerID string) bool {
	ident, ok := auth.FromContext(r.Context())
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return false
	}
	if ident.UserID == ownerID || ident.Role == users.RoleSuperAdmin {
		return true
	}
	jsonResponse(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
	return false
}

var _ workspace.Store = (workspace.Store)(nil)
