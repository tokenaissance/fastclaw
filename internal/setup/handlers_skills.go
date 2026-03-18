package setup

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/config"
)

// --- Skills ---

func (s *Server) handleListSkills(w http.ResponseWriter, r *http.Request) {
	homeDir, err := config.HomeDir()
	if err != nil {
		jsonResponse(w, http.StatusOK, []any{})
		return
	}

	skillsDir := filepath.Join(homeDir, "skills")
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		jsonResponse(w, http.StatusOK, []any{})
		return
	}

	var skills []map[string]string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		desc := ""
		skillPath := filepath.Join(skillsDir, name, "SKILL.md")
		if data, readErr := os.ReadFile(skillPath); readErr == nil {
			lines := strings.SplitN(string(data), "\n", 3)
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line != "" && !strings.HasPrefix(line, "#") {
					desc = line
					break
				}
			}
		}
		skills = append(skills, map[string]string{
			"name":        name,
			"description": desc,
			"location":    filepath.Join(skillsDir, name),
			"type":        "skill",
		})
	}
	if skills == nil {
		jsonResponse(w, http.StatusOK, []any{})
		return
	}
	jsonResponse(w, http.StatusOK, skills)
}

func (s *Server) handleDeleteSkill(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	homeDir, err := config.HomeDir()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	skillPath := filepath.Join(homeDir, "skills", name)
	if err := os.RemoveAll(skillPath); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}
