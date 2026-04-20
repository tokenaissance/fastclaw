package setup

import (
	"net/http"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/usage"
)

// handleGetUsage returns the per-(day, apikey, agent, kind) counter rows.
// Admin only — we currently lean on a process-local meter, so answers are
// only for this pod's activity. When the Meter is swapped for a DB-backed
// implementation the same endpoint starts returning fleet-wide totals
// with no handler changes.
//
// Query params:
//
//	?apiKey=<id>  — filter to one api key (empty = all)
//	?agent=<id>   — filter to one agent (empty = all)
//	?kind=tokens_in,sandbox_seconds,...  (empty = all known kinds)
//	?since=2026-04-01  (RFC3339 date or zero for last 30 days)
//	?until=2026-04-30  (zero for now)
func (s *Server) handleGetUsage(w http.ResponseWriter, r *http.Request) {
	if callerFrom(r).Kind != callerAdmin {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "admin only"})
		return
	}
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

// splitKinds parses "a,b,c" into []usage.Kind. Kept trivial — no
// validation of the individual tokens, so callers get whatever they
// asked for and unknown kinds quietly return no rows.
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
