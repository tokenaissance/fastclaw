package setup

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/skills"
)

// handleInstallSkill installs a skill from skills.sh, clawhub.ai, or a
// specific GitHub repo. Body:
//
//	{
//	  "source": "skillssh" | "clawhub" | "github" | "" (auto),
//	  "name":   "<skill slug / folder name>",
//	  "repo":   "owner/repo"  (github only),
//	  "agent":  "<agent-id>"  (optional; if set, install into the agent's own
//	                           skills dir and hot-reload it; otherwise install
//	                           globally — admin only)
//	}
//
// Source precedence when source is empty: skills.sh → clawhub.
// Global installs (no `agent`) require the local/admin user — cloud users
// cannot modify shared skills through this endpoint.
func (s *Server) handleInstallSkill(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source string `json:"source"`
		Name   string `json:"name"`
		Skill  string `json:"skill"` // legacy alias for "name"
		Repo   string `json:"repo"`
		Agent  string `json:"agent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	if req.Name == "" {
		req.Name = req.Skill
	}
	if req.Name == "" && req.Repo == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "name or repo required"})
		return
	}

	// For per-agent installs, validate access against the canonical source
	// (store + filesystem) and let resolveInstallTarget lazy-create the
	// home/skills dir if this pod hasn't seen the agent yet. Without this
	// step, multi-pod deployments 403 on the non-creating pod because its
	// emptyDir doesn't contain agents/<id>/.
	if req.Agent != "" && !s.canAccessAgent(callerFrom(r), req.Agent) {
		forbid(w, req.Agent)
		return
	}
	targetDir, err := resolveInstallTarget(r, req.Agent)
	if err != nil {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	result, err := runInstall(req.Source, req.Name, req.Repo, targetDir)
	if err != nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	// Mirror the installed skill bundle to the shared object store so
	// other pods can hydrate it on their next reload. Without this step
	// the skill only exists on this pod's emptyDir, and a chat request
	// balanced to another pod wouldn't see it.
	if s.workspaceStore != nil && result != nil && result.Name != "" {
		owner := req.Agent
		if owner == "" {
			owner = skills.GlobalSkillOwner
		}
		if uerr := skills.SyncSkillUp(r.Context(), s.workspaceStore, owner, result.Name, targetDir); uerr != nil {
			slog.Warn("failed to mirror skill to object store",
				"owner", owner, "skill", result.Name, "error", uerr)
		}
	}

	// Hot-reload the target agent so the new skill is visible on the next turn.
	if req.Agent != "" && s.agentProvider != nil {
		if ag := s.agentProvider.AgentByID(req.Agent); ag != nil {
			ag.ReloadWorkspaceFiles()
		}
	}

	slog.Info("skill installed",
		"source", result.Source, "name", result.Name,
		"version", result.Version, "path", result.InstalledAt, "agent", req.Agent)
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":          true,
		"source":      result.Source,
		"name":        result.Name,
		"version":     result.Version,
		"installedAt": result.InstalledAt,
		"files":       result.FilesWritten,
	})
}

// resolveInstallTarget picks the target directory for an install and enforces
// the admin-only rule for global installs.
func resolveInstallTarget(r *http.Request, agentID string) (string, error) {
	if agentID != "" {
		// Access has already been checked by the caller against the
		// authoritative agent source (DB store + bindings). The home dir
		// may legitimately not exist yet on this pod (emptyDir in
		// multi-pod deploys) — just create it.
		homePath, err := config.AgentHomeDir(agentID)
		if err != nil {
			return "", fmt.Errorf("resolve agent home: %w", err)
		}
		dir := filepath.Join(homePath, "skills")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("create agent skills dir: %w", err)
		}
		return dir, nil
	}
	// Global install — admin/local user only.
	if config.UserIDFromContext(r.Context()) != config.DefaultUserID {
		return "", fmt.Errorf("global skills are admin-managed; pass an 'agent' id to install into one agent only")
	}
	home, err := config.HomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "skills")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// runInstall dispatches to the right skills backend. When source is empty it
// tries skills.sh then clawhub (skill-creator is a chat-level fallback, not a
// registry — the agent tool offers it when both sources miss).
func runInstall(source, name, repo, targetDir string) (*skills.Result, error) {
	switch source {
	case "github":
		if repo == "" {
			return nil, fmt.Errorf("source=github requires 'repo'")
		}
		return skills.InstallFromGitHubRepo(repo, name, targetDir)
	case "clawhub":
		return skills.InstallFromClawHub(name, targetDir)
	case "skillssh", "skills.sh":
		results, err := skills.SearchSkillsSh(name)
		if err != nil {
			return nil, err
		}
		pick := skills.PickSkillsShExact(results, name)
		if pick == nil || pick.SkillID != name {
			return nil, fmt.Errorf("skill %q not found on skills.sh", name)
		}
		return skills.InstallFromSkillsSh(*pick, targetDir)
	case "", "auto":
		if repo != "" {
			return skills.InstallFromGitHubRepo(repo, name, targetDir)
		}
		return skills.InstallAuto(name, targetDir)
	default:
		return nil, fmt.Errorf("unknown source %q", source)
	}
}

// handleSearchSkills returns search results. source=skillssh (default) hits
// https://skills.sh; source=clawhub proxies clawhub.ai's search endpoint.
// GET /api/skills/search?q=xxx&source=skillssh|clawhub
func (s *Server) handleSearchSkills(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	source := r.URL.Query().Get("source")
	if source == "" {
		source = "skillssh"
	}

	switch source {
	case "skillssh", "skills.sh":
		results, err := skills.SearchSkillsSh(query)
		if err != nil {
			jsonResponse(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		jsonResponse(w, http.StatusOK, map[string]any{"source": "skills.sh", "results": results})
	case "clawhub":
		u := fmt.Sprintf("https://clawhub.ai/api/v1/search?q=%s&limit=20", url.QueryEscape(query))
		resp, err := http.Get(u)
		if err != nil {
			jsonResponse(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	default:
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unsupported source"})
	}
}
