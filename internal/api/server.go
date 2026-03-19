package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/config"
)

// Server handles the OpenAI-compatible API and WebSocket gateway.
type Server struct {
	agentMgr   *agent.Manager
	token      string
	gatewayCfg *config.GatewayCfg
}

// NewServer creates a new API server.
func NewServer(agentMgr *agent.Manager, token string, gatewayCfg *config.GatewayCfg) *Server {
	return &Server{
		agentMgr:   agentMgr,
		token:      token,
		gatewayCfg: gatewayCfg,
	}
}

// RegisterRoutes registers API routes on the given mux.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	// Always register WebSocket (needed for ChatClaw)
	mux.HandleFunc("/ws", s.HandleWebSocket)

	// CORS preflight for all /v1/* routes
	mux.HandleFunc("OPTIONS /v1/", s.handleCORS)

	// Chat completions endpoint
	if s.gatewayCfg == nil || s.gatewayCfg.HTTP.Endpoints.ChatCompletions.Enabled {
		mux.HandleFunc("POST /v1/chat/completions", s.authMiddleware(s.HandleChatCompletions))
	}

	// Agents list endpoint
	if s.gatewayCfg == nil || s.gatewayCfg.HTTP.Endpoints.Agents.Enabled {
		mux.HandleFunc("GET /v1/agents", s.authMiddleware(s.HandleListAgents))
	}
}

// handleCORS responds to CORS preflight requests.
func (s *Server) handleCORS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, x-openclaw-agent-id, x-openclaw-session-key")
	w.Header().Set("Access-Control-Max-Age", "86400")
	w.WriteHeader(http.StatusNoContent)
}

// HandleListAgents handles GET /v1/agents.
func (s *Server) HandleListAgents(w http.ResponseWriter, r *http.Request) {
	agents := s.buildAgentList()
	writeJSON(w, http.StatusOK, map[string]any{"agents": agents})
}

func (s *Server) buildAgentList() []map[string]string {
	all := s.agentMgr.All()

	// Also load config for model info
	cfg, _ := config.Load()
	modelMap := make(map[string]string)
	if cfg != nil {
		for _, ra := range config.ResolveAgents(cfg) {
			modelMap[ra.ID] = ra.Model
		}
	}

	agents := make([]map[string]string, 0, len(all))
	for _, ag := range all {
		model := ag.Model()
		if model == "" {
			model = modelMap[ag.Name()]
		}
		agents = append(agents, map[string]string{
			"id":    ag.Name(),
			"name":  ag.Name(),
			"model": model,
		})
	}
	return agents
}

// authMiddleware validates the Bearer token for API routes.
func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Skip auth if mode is "none" or no token configured
		if s.token == "" || (s.gatewayCfg != nil && s.gatewayCfg.Auth.Mode == "none") {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			next(w, r)
			return
		}

		auth := r.Header.Get("Authorization")
		if auth == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": map[string]string{"message": "missing Authorization header", "type": "authentication_error"},
			})
			return
		}

		token := strings.TrimPrefix(auth, "Bearer ")
		if token == auth || token != s.token {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": map[string]string{"message": "invalid token", "type": "authentication_error"},
			})
			return
		}

		// Add CORS headers
		w.Header().Set("Access-Control-Allow-Origin", "*")
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
