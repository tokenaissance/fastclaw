package setup

import (
	"net/http"
	"sort"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/skills"
)

type publicAgentResponse struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	AvatarURL    string `json:"avatarUrl,omitempty"`
	AuthorName   string `json:"authorName,omitempty"`
	Category     string `json:"category,omitempty"`
	InstallCount int    `json:"installCount,omitempty"`
}

type publicSkillResponse struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	AuthorName   string `json:"authorName,omitempty"`
	Category     string `json:"category,omitempty"`
	InstallCount int    `json:"installCount,omitempty"`
	Readme       string `json:"readme,omitempty"`
}

// handlePublicAgents powers the mobile Explore "Agent 广场" view. It returns
// only agents whose owner explicitly enabled the public-link toggle.
func (s *Server) handlePublicAgents(w http.ResponseWriter, r *http.Request) {
	if s.dataStore == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "store not configured"})
		return
	}

	agents, err := s.dataStore.ListPublicAgents(r.Context())
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	out := make([]publicAgentResponse, 0, len(agents))
	for _, ag := range agents {
		desc, _ := ag.Config["description"].(string)
		category, _ := ag.Config["category"].(string)
		if category == "" {
			category, _ = ag.Config["specialty"].(string)
		}
		out = append(out, publicAgentResponse{
			ID:          ag.ID,
			Name:        ag.Name,
			Description: desc,
			AvatarURL:   "/api/agents/" + ag.ID + "/files/avatar.png",
			Category:    category,
		})
	}

	jsonResponse(w, http.StatusOK, map[string]any{"agents": out})
}

// handlePublicSkills powers the mobile Explore "技能库" view. There is no
// first-party public skill catalog yet, so this exposes the existing skills.sh
// integration with a stable FastAgent-shaped response.
func (s *Server) handlePublicSkills(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		query = "agent"
	}

	results, err := skills.SearchSkillsSh(query)
	if err != nil {
		jsonResponse(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}

	sort.SliceStable(results, func(i, j int) bool {
		return results[i].Installs > results[j].Installs
	})
	if len(results) > 50 {
		results = results[:50]
	}

	out := make([]publicSkillResponse, 0, len(results))
	for _, sk := range results {
		out = append(out, publicSkillResponse{
			ID:           sk.ID,
			Name:         firstNonEmpty(sk.Name, sk.SkillID),
			Description:  sk.Source,
			AuthorName:   sourceOwner(sk.Source),
			Category:     "skills.sh",
			InstallCount: sk.Installs,
		})
	}

	jsonResponse(w, http.StatusOK, map[string]any{"skills": out})
}

func sourceOwner(source string) string {
	if idx := strings.IndexByte(source, '/'); idx > 0 {
		return source[:idx]
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
