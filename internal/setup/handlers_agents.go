package setup

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/workspace"
)

// Agent name is a public identifier used in URLs and bot handles, so it must
// be URL-safe and globally unique. 3–32 chars, lowercase letters/digits/hyphens,
// cannot start with a hyphen.
var agentNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,31}$`)

const defaultBootstrap = `This is your first conversation with the user. Your SOUL.md, IDENTITY.md, and USER.md are all empty — you have no name, no personality, and no information about the user yet.

On the user's first message (regardless of what it is), do NOT answer it directly. Instead:

1. Briefly greet the user and let them know you're a fresh agent that needs a bit of setup.
2. Ask 2-3 short questions in a warm, conversational tone (not a form):
   - What should the user call you? (your agent name)
   - How should you address the user? (their preferred name)
   - What role / personality do they want you to have?

When the user answers, use the write_file tool to save each piece of information in exactly one file. **Do not repeat the same sentence or phrase across files** — each file has a strictly different purpose:

- ` + "`IDENTITY.md`" + ` — the agent's NAME and ROLE. One or two short lines, no personality or tone words. Example:
  "# Identity\n\nYour name is {agent_name}. You are a {role — e.g. podcast creation assistant}."

- ` + "`SOUL.md`" + ` — HOW the agent behaves: tone, communication style, quirks, values, what it cares about. **Must NOT** repeat the name or the role from IDENTITY.md. Describes behavior, not identity. Example if the user said "be playful and blunt":
  "# Soul\n\n- Tone: playful and blunt — skip pleasantries, say what you think.\n- Keep replies short; ask clarifying questions instead of guessing.\n- Prefer concrete examples over abstract advice."
  If the user only gave a role ("厉害的助理", "啥都能干") without any tone/personality hints, do NOT paraphrase the role into SOUL.md — instead write sensible defaults (e.g. concise, helpful, low-ceremony tone) or briefly ask one follow-up question about how they'd like you to talk.

- ` + "`USER.md`" + ` — facts about the USER only. Example:
  "# User\n\nPreferred name: {user_name}." (add any other details they share, like role or working language)

After saving all three, overwrite this BOOTSTRAP.md with a single blank line via write_file so the setup instructions stop appearing in future conversations. Then acknowledge the setup and proceed with whatever the user originally asked (if anything actionable).

Keep the exchange short. Don't lecture, don't dump a list of options — ask, listen, write files, move on.
`

// --- Agent Management ---

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.loadUserConfig(r)
	if err != nil {
		cfg = &config.Config{}
	}
	caller := callerFrom(r)
	// In multi-pod / cloud deploys not every agent has a directory on the
	// pod that's serving this request — the canonical list lives in the
	// dataStore. Supplement filesystem discovery with store IDs so the
	// admin UI shows every agent regardless of which pod wrote it.
	var storeAgentIDs []string
	if s.dataStore != nil {
		if records, lerr := s.dataStore.ListAgents(r.Context()); lerr == nil {
			for _, ar := range records {
				storeAgentIDs = append(storeAgentIDs, ar.ID)
			}
		}
	}
	resolved := config.ResolveAgentsWithExtra(cfg, "", storeAgentIDs)
	var agents []map[string]any
	for _, ra := range resolved {
		// API-key callers only see agents bound to their key; admins see all.
		if !s.canAccessAgent(caller, ra.ID) {
			continue
		}
		soul := ""
		if data, readErr := s.readIdentityFile(r.Context(), ra.ID, "SOUL.md"); readErr == nil {
			soul = string(data)
		}
		owner := ""
		if s.agentBindings != nil {
			owner = s.agentBindings.OwnerOf(ra.ID)
		}
		agents = append(agents, map[string]any{
			"id":                ra.ID,
			"model":             ra.Model,
			"home":              ra.Home,
			"workspace":         ra.Workspace,
			"maxTokens":         ra.MaxTokens,
			"temperature":       ra.Temperature,
			"maxToolIterations": ra.MaxToolIterations,
			"thinking":          ra.Thinking,
			"soul":              soul,
			"apiKeyId":          owner, // "" means admin-owned
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
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	if req.ID == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "agent name is required"})
		return
	}
	if !agentNameRE.MatchString(req.ID) {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "agent name must be 3–32 chars, lowercase letters, digits or hyphens, and cannot start with a hyphen"})
		return
	}

	// Check for duplicate by checking if the home dir already exists.
	homePath, _ := config.AgentHomeDir(req.ID)
	if _, err := os.Stat(homePath); err == nil {
		jsonResponse(w, http.StatusConflict, map[string]any{"ok": false, "error": fmt.Sprintf("agent name %q is already taken", req.ID)})
		return
	}

	// Create the agent's home (identity, sessions, memory, skills).
	for _, dir := range []string{homePath, filepath.Join(homePath, "memory"), filepath.Join(homePath, "sessions"), filepath.Join(homePath, "skills")} {
		os.MkdirAll(dir, 0o755)
	}

	// Create the separate workspace dir for agent-generated user content.
	if workPath, err := config.AgentWorkspaceDir(req.ID); err == nil {
		os.MkdirAll(workPath, 0o755)
	}

	// Identity files go through the Store so Postgres-backed deployments
	// keep agent state consistent across pods. FileStore also goes through
	// this path and writes to the same filesystem layout, so file-mode
	// deployments see no behavior change.
	_ = s.writeIdentityFile(r.Context(), req.ID, "SOUL.md", nil) // empty; agent fills during BOOTSTRAP
	_ = s.writeIdentityFile(r.Context(), req.ID, "BOOTSTRAP.md", []byte(defaultBootstrap))
	agentCfg := config.AgentFileConfig{Model: req.Model}
	agentData, _ := json.MarshalIndent(agentCfg, "", "  ")
	_ = s.writeIdentityFile(r.Context(), req.ID, "agent.json", agentData)

	// Record the agent in the Store so other pods can discover it without a
	// shared filesystem. In file mode this is a cheap extra write; in
	// Postgres mode it is how multi-pod deployments see each others' agents.
	if s.dataStore != nil {
		_ = s.dataStore.SaveAgent(r.Context(), &store.AgentRecord{
			ID:        req.ID,
			Name:      req.ID,
			Model:     req.Model,
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		})
	}

	// Auto-bind the new agent to the caller's API key. Admin-created agents
	// stay unbound (admin-owned by default). This is what makes API-key
	// holders able to create their own agents without seeing others'.
	caller := callerFrom(r)
	if caller.Kind == callerAPIKey && s.agentBindings != nil {
		s.agentBindings.Bind(req.ID, caller.APIKeyID)
		if err := s.agentBindings.Save(); err != nil {
			slog.Warn("failed to save agent binding", "agent", req.ID, "apiKey", caller.APIKeyID, "error", err)
		}
	}

	// Load the new agent into the running gateway so chat requests see it.
	if s.agentProvider != nil {
		if err := s.agentProvider.ReloadAgents(); err != nil {
			slog.Warn("failed to reload agents after create", "id", req.ID, "error", err)
		}
	}

	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.canAccessAgent(callerFrom(r), id) {
		forbid(w, id)
		return
	}
	var req struct {
		Model     string                            `json:"model"`
		Soul      string                            `json:"soul"`
		Skills    *config.SkillsConfig              `json:"skills,omitempty"`
		Providers *map[string]config.ProviderConfig `json:"providers,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}

	// Update home files directly.
	homePath, _ := config.AgentHomeDir(id)
	if _, err := os.Stat(homePath); err != nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "agent not found"})
		return
	}

	if req.Soul != "" {
		_ = s.writeIdentityFile(r.Context(), id, "SOUL.md", []byte(req.Soul))
	}

	// Model / skills / providers live in agent.json. Read-modify-write so
	// updating one field doesn't clobber the others (prior implementation
	// overwrote the whole file when only `model` changed, silently dropping
	// skills / tools / MCP config).
	if req.Model != "" || req.Skills != nil || req.Providers != nil {
		var agentCfg config.AgentFileConfig
		if existing, rerr := s.readIdentityFile(r.Context(), id, "agent.json"); rerr == nil && len(existing) > 0 {
			_ = json.Unmarshal(existing, &agentCfg) // tolerate malformed — start fresh
		}
		if req.Model != "" {
			agentCfg.Model = req.Model
		}
		if req.Skills != nil {
			agentCfg.Skills = *req.Skills
		}
		if req.Providers != nil {
			// Whole-map replace (incl. empty-map to clear all per-agent
			// providers). Callers that want surgical update should merge
			// on the client side and PUT the full map.
			agentCfg.Providers = *req.Providers
		}
		agentData, _ := json.MarshalIndent(agentCfg, "", "  ")
		_ = s.writeIdentityFile(r.Context(), id, "agent.json", agentData)
	}

	if s.agentProvider != nil {
		if err := s.agentProvider.ReloadAgents(); err != nil {
			slog.Warn("failed to reload agents after update", "id", id, "error", err)
		}
	}

	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// handleGetAgentConfig returns the raw agent.json contents for a single
// agent. Used by the admin UI's per-agent model / skills pages, which need
// current per-agent overrides (not the merged resolved config).
func (s *Server) handleGetAgentConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.canAccessAgent(callerFrom(r), id) {
		forbid(w, id)
		return
	}
	homePath, _ := config.AgentHomeDir(id)
	if _, err := os.Stat(homePath); err != nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "agent not found"})
		return
	}
	var agentCfg config.AgentFileConfig
	if data, rerr := s.readIdentityFile(r.Context(), id, "agent.json"); rerr == nil && len(data) > 0 {
		_ = json.Unmarshal(data, &agentCfg)
	}
	jsonResponse(w, http.StatusOK, agentCfg)
}

// systemFileNames is the allowlist of agent metadata files that the admin UI
// can read and write through the system-files endpoints. Keeping it closed
// ensures arbitrary paths can't be written into the agent home directory.
var systemFileNames = map[string]bool{
	"SOUL.md":      true,
	"IDENTITY.md":  true,
	"USER.md":      true,
	"BOOTSTRAP.md": true,
	"MEMORY.md":    true,
	"HEARTBEAT.md": true,
	"AGENTS.md":    true,
	"TOOLS.md":     true,
}

// handleGetAgentSystemFile reads one of the agent's identity/metadata files
// (SOUL.md, IDENTITY.md, ...) from its home dir and returns {content: "..."}.
// This is the read side of the admin UI's Files editor.
func (s *Server) handleGetAgentSystemFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := r.PathValue("name")
	if !s.canAccessAgent(callerFrom(r), id) {
		forbid(w, id)
		return
	}
	if !systemFileNames[name] {
		http.Error(w, "unknown system file", http.StatusBadRequest)
		return
	}
	data, err := s.readIdentityFile(r.Context(), id, name)
	if err != nil {
		// Treat not-found as empty (frontend reads every tab on load).
		if os.IsNotExist(err) || isStoreNotFound(err) {
			jsonResponse(w, http.StatusOK, map[string]any{"content": ""})
			return
		}
		http.Error(w, "read file", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"content": string(data)})
}

// handlePutAgentSystemFile writes content to one of the agent's metadata files.
// Agent is reloaded so the new content takes effect for the next chat turn.
func (s *Server) handlePutAgentSystemFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := r.PathValue("name")
	if !s.canAccessAgent(callerFrom(r), id) {
		forbid(w, id)
		return
	}
	if !systemFileNames[name] {
		http.Error(w, "unknown system file", http.StatusBadRequest)
		return
	}
	var req struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	if err := s.writeIdentityFile(r.Context(), id, name, []byte(req.Content)); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if s.agentProvider != nil {
		if err := s.agentProvider.ReloadAgents(); err != nil {
			slog.Warn("failed to reload agents after system-file write", "id", id, "file", name, "error", err)
		}
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAgentFileList returns the list of objects currently in the agent's
// workspace. Used by the chat UI to show a "produced files" panel under
// the final reply bubble — the frontend diffs a pre-turn snapshot against
// the post-turn list to find files the turn just created or updated.
// Identity files (SOUL.md, USER.md, …) are excluded — those live in the
// system store, not the workspace, and shouldn't be offered as downloads.
func (s *Server) handleAgentFileList(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.canAccessAgent(callerFrom(r), id) {
		forbid(w, id)
		return
	}
	if s.workspaceStore == nil {
		// Legacy filesystem-only setups can't list through the store API;
		// returning an empty list is fine — the UI just hides the panel.
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"files": []any{}})
		return
	}
	objs, err := s.workspaceStore.List(r.Context(), id)
	if err != nil {
		http.Error(w, "list workspace: "+err.Error(), http.StatusInternalServerError)
		return
	}
	type fileItem struct {
		Path    string `json:"path"`
		Size    int64  `json:"size"`
		ModTime int64  `json:"modTime"`
	}
	files := make([]fileItem, 0, len(objs))
	for _, o := range objs {
		files = append(files, fileItem{Path: o.Path, Size: o.Size, ModTime: o.ModTime.UnixMilli()})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"files": files})
}

// handleAgentFile serves a file from the agent's workspace directory for
// inline preview or download. The path is sanitized and must resolve inside
// the workspace root — any attempt to escape is rejected with 403.
// Add ?download=1 to force a download (Content-Disposition: attachment).
// Goes through workspaceStore when configured — in S3 mode that's a
// 302 redirect to a signed URL, letting the browser fetch directly from S3
// without streaming the blob through the gateway pod. Falls back to direct
// filesystem for backward-compat paths that predate the store abstraction.
func (s *Server) handleAgentFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	relPath := r.PathValue("path")
	if relPath == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	if !s.canAccessAgent(callerFrom(r), id) {
		forbid(w, id)
		return
	}

	// When a WorkspaceStore is wired in, that is the canonical read path.
	if s.workspaceStore != nil {
		s.serveFileFromWorkspaceStore(w, r, id, relPath)
		return
	}

	// Legacy filesystem path (still used when no store is configured —
	// mostly embedded tests and very old setups).
	workRoot, err := config.AgentWorkspaceDir(id)
	if err != nil {
		http.Error(w, "resolve workspace", http.StatusInternalServerError)
		return
	}
	absRoot, err := filepath.Abs(workRoot)
	if err != nil {
		http.Error(w, "invalid workspace", http.StatusInternalServerError)
		return
	}
	absFull, err := filepath.Abs(filepath.Join(absRoot, relPath))
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if absFull != absRoot && !strings.HasPrefix(absFull, absRoot+string(filepath.Separator)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	info, err := os.Stat(absFull)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if info.IsDir() {
		http.Error(w, "is a directory", http.StatusBadRequest)
		return
	}

	if r.URL.Query().Get("download") == "1" {
		filename := filepath.Base(absFull)
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filename))
	}

	// http.ServeFile handles Range requests, If-Modified-Since, and content type.
	http.ServeFile(w, r, absFull)
}

// serveFileFromWorkspaceStore streams or redirects to the blob store. If the
// backend can sign URLs (S3), redirect — the browser fetches straight from
// the bucket, sparing the gateway pod bandwidth. Otherwise stream the body.
func (s *Server) serveFileFromWorkspaceStore(w http.ResponseWriter, r *http.Request, agentID, path string) {
	ctx := r.Context()
	// Try a signed URL first; browsers can handle the 302 and go direct.
	if url, err := s.workspaceStore.SignedURL(ctx, agentID, path, 10*time.Minute); err == nil {
		http.Redirect(w, r, url, http.StatusFound)
		return
	}
	info, err := s.workspaceStore.Stat(ctx, agentID, path)
	if err != nil {
		if err == workspace.ErrNotFound {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rc, err := s.workspaceStore.Get(ctx, agentID, path)
	if err != nil {
		if err == workspace.ErrNotFound {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rc.Close()
	if info.ContentType != "" {
		w.Header().Set("Content-Type", info.ContentType)
	}
	if info.Size > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size))
	}
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filepath.Base(path)))
	}
	_, _ = io.Copy(w, rc)
}

func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.canAccessAgent(callerFrom(r), id) {
		forbid(w, id)
		return
	}
	homePath, _ := config.AgentHomeDir(id)

	// Remove the entire agent home (~/.fastclaw/agents/{id}).
	parent := filepath.Dir(homePath)
	if err := os.RemoveAll(parent); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	// Remove the workspace (user-facing content) too.
	if workPath, err := config.AgentWorkspaceDir(id); err == nil {
		os.RemoveAll(workPath)
	}

	// Drop the ownership record — no point keeping a binding to an agent
	// that no longer exists.
	if s.agentBindings != nil {
		s.agentBindings.Unbind(id)
		_ = s.agentBindings.Save()
	}

	// Clean up Store rows too, otherwise other pods would keep seeing the
	// agent in ListAgents and try (and fail) to serve it.
	if s.dataStore != nil {
		if err := s.dataStore.DeleteAgent(r.Context(), id); err != nil {
			slog.Warn("store delete agent failed", "id", id, "error", err)
		}
	}

	if s.agentProvider != nil {
		if err := s.agentProvider.ReloadAgents(); err != nil {
			slog.Warn("failed to reload agents after delete", "id", id, "error", err)
		}
	}

	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}
