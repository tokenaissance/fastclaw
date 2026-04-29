package setup

import (
	"net/http"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/usage"
)

// handleGetUsage returns the per-(day, apikey, agent, kind) counter rows.
// Wrapped by requireSuperAdmin in server.go. Filters: ?apiKey=, ?agent=,
// ?kind=tokens_in,sandbox_seconds, ?since=, ?until=.
func (s *Server) handleGetUsage(w http.ResponseWriter, r *http.Request) {
	if s.usage == nil {
		jsonResponse(w, http.StatusOK, map[string]any{"rows": []any{}})
		return
	}
	q := usage.Query{
		APIKey: r.URL.Query().Get("apiKey"),
		Agent:  r.URL.Query().Get("agent"),
	}
	if v := r.URL.Query().Get("since"); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			q.Since = t
		} else if t, err := time.Parse(time.RFC3339, v); err == nil {
			q.Since = t
		}
	}
	if v := r.URL.Query().Get("until"); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			q.Until = t
		} else if t, err := time.Parse(time.RFC3339, v); err == nil {
			q.Until = t
		}
	}
	if v := r.URL.Query().Get("kind"); v != "" {
		q.Kinds = splitKinds(v)
	}
	rows, err := s.usage.Query(r.Context(), q)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"rows": rows})
}

func splitKinds(s string) []usage.Kind {
	var out []usage.Kind
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if i > start {
				out = append(out, usage.Kind(s[start:i]))
			}
			start = i + 1
		}
	}
	return out
}
