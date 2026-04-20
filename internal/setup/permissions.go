package setup

import (
	"net/http"

	"github.com/fastclaw-ai/fastclaw/internal/config"
)

// callerKind indicates how the request authenticated. Used by handlers to
// decide whether to apply api-key-scoped filtering.
type callerKind int

const (
	callerAdmin  callerKind = iota // admin token (or local mode with no auth)
	callerAPIKey                   // a valid API key from users.Registry
)

// caller bundles the authenticated identity for a request. Admin callers see
// everything. API-key callers only see agents bound to their key id via the
// AgentBindings registry.
type caller struct {
	Kind     callerKind
	APIKeyID string // populated when Kind == callerAPIKey
}

// callerFrom returns the caller for a request. For back-compat it reads the
// same context value the legacy userID flow populates (DefaultUserID → admin,
// anything else → API key id). Kept as a single helper so routes don't
// reimplement the rule every time.
func callerFrom(r *http.Request) caller {
	id := config.UserIDFromContext(r.Context())
	if id == "" || id == config.DefaultUserID {
		return caller{Kind: callerAdmin}
	}
	return caller{Kind: callerAPIKey, APIKeyID: id}
}

// canAccessAgent returns true when the caller is allowed to read/write the
// given agent. Admin can do anything. API-key callers can only touch agents
// bound to their id; agents with no binding are admin-only.
func (s *Server) canAccessAgent(c caller, agentID string) bool {
	if c.Kind == callerAdmin {
		return true
	}
	if s.agentBindings == nil {
		return false
	}
	return s.agentBindings.OwnerOf(agentID) == c.APIKeyID
}

// visibleAgents filters a slice of agent IDs down to what the caller is
// allowed to see. Admin sees everything; api-key callers see only bound
// agents. Preserves input ordering.
func (s *Server) visibleAgents(c caller, all []string) []string {
	if c.Kind == callerAdmin {
		return all
	}
	if s.agentBindings == nil {
		return nil
	}
	owned := map[string]bool{}
	for _, id := range s.agentBindings.AgentsOf(c.APIKeyID) {
		owned[id] = true
	}
	out := make([]string, 0, len(all))
	for _, id := range all {
		if owned[id] {
			out = append(out, id)
		}
	}
	return out
}

// forbid writes a 403 JSON response. Used by agent routes when the caller
// is authenticated but doesn't own the target agent.
func forbid(w http.ResponseWriter, agentID string) {
	jsonResponse(w, http.StatusForbidden, map[string]any{
		"ok":    false,
		"error": "agent " + agentID + " is not accessible with this API key",
	})
}
