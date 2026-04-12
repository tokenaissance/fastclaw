package setup

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/api"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/taskqueue"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

// AgentHandle is a minimal interface for interacting with an agent from the web UI.
type AgentHandle interface {
	Name() string
	HandleWebChat(ctx context.Context, sessionId, text string) string
	HandleWebChatStream(ctx context.Context, sessionId, text string, events chan<- agent.ChatEvent) string
	WebChatHistory(sessionId string) []map[string]any
	WebChatSessions() []map[string]string
}

// AgentProvider gives the server access to the running agents.
type AgentProvider interface {
	AllAgents() []AgentHandle
	AgentByID(id string) AgentHandle
}

// Server serves the setup wizard UI and handles config API endpoints.
type Server struct {
	port          int
	bind          string // "loopback" or "all"
	gatewayCfg    *config.GatewayCfg
	onConfig      func(*config.Config) // called after config is saved
	agentProvider AgentProvider
	userResolver  api.UserResolver // for per-user agent routing
	taskQueue     *taskqueue.Queue
	apiServer     *api.Server
	authToken     string           // gateway admin token (for auth middleware)
	userRegistry  *users.Registry  // cloud mode user registry
	dataStore     store.Store      // DB or file store for per-user config
	startedAt     time.Time
}

// NewServer creates a setup wizard server on the given port.
func NewServer(port int, onConfig func(*config.Config)) *Server {
	return &Server{port: port, bind: "loopback", onConfig: onConfig, startedAt: time.Now()}
}

// SetGatewayConfig sets the gateway configuration for bind address and HTTP endpoints.
func (s *Server) SetGatewayConfig(cfg *config.GatewayCfg) {
	s.gatewayCfg = cfg
	if cfg.Bind != "" {
		s.bind = cfg.Bind
	}
	if cfg.Port > 0 {
		s.port = cfg.Port
	}
}

// SetAgentProvider sets the agent provider for chat and status endpoints.
func (s *Server) SetAgentProvider(ap AgentProvider) {
	s.agentProvider = ap
}

// SetTaskQueue sets the task queue for the tasks API endpoint.
func (s *Server) SetTaskQueue(tq *taskqueue.Queue) {
	s.taskQueue = tq
}

// SetAPIServer sets the OpenAI-compatible API server for /v1/* and /ws routes.
func (s *Server) SetAPIServer(apiSrv *api.Server) {
	s.apiServer = apiSrv
}

// SetUserResolver sets the resolver for per-user agent routing in the web UI.
func (s *Server) SetUserResolver(resolver api.UserResolver) {
	s.userResolver = resolver
}

// SetStore sets the storage backend for per-user config persistence.
// When set, cloud user configs are stored in the database instead of
// the filesystem. The local user always bootstraps from filesystem.
func (s *Server) SetStore(st store.Store) {
	s.dataStore = st
}

// SetAuth configures authentication for the /api/* endpoints. In local mode
// (token="" and registry=nil) all requests are treated as the local user.
// In cloud mode, the bearer token is resolved to a user ID.
func (s *Server) SetAuth(token string, registry *users.Registry) {
	s.authToken = token
	s.userRegistry = registry
}

// userAuth is middleware that resolves the bearer token to a user ID and
// injects it into the request context. In local mode (no token configured),
// it passes through as the default user.
func (s *Server) userAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// No auth configured → local mode, default user.
		if s.authToken == "" && s.userRegistry == nil {
			r = r.WithContext(config.WithUserID(r.Context(), config.DefaultUserID))
			next(w, r)
			return
		}

		auth := r.Header.Get("Authorization")
		if auth == "" {
			jsonResponse(w, http.StatusUnauthorized, map[string]any{
				"ok": false, "error": "token required",
			})
			return
		}

		token := strings.TrimPrefix(auth, "Bearer ")
		if token == auth {
			jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "invalid auth header"})
			return
		}

		// Try cloud user registry first.
		if s.userRegistry != nil {
			if uid, ok := s.userRegistry.LookupByToken(token); ok {
				r = r.WithContext(config.WithUserID(r.Context(), uid))
				next(w, r)
				return
			}
		}
		// Try admin token → local user.
		if s.authToken != "" && token == s.authToken {
			r = r.WithContext(config.WithUserID(r.Context(), config.DefaultUserID))
			next(w, r)
			return
		}

		jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "invalid token"})
	}
}

// Run starts the HTTP server and blocks until the context is canceled
// or the setup is completed.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()

	// API routes — all wrapped with user auth so cloud users get their own
	// config/agents/sessions when they access the web UI with a bearer token.
	ua := s.userAuth
	mux.HandleFunc("GET /api/status", ua(s.handleStatus))
	mux.HandleFunc("GET /api/config", ua(s.handleGetConfig))
	mux.HandleFunc("POST /api/config", ua(s.handleUpdateConfig))
	mux.HandleFunc("POST /api/test-provider", ua(s.handleTestProvider))
	mux.HandleFunc("POST /api/save-config", ua(s.handleSaveConfig))
	mux.HandleFunc("POST /api/chat", ua(s.handleChat))
	mux.HandleFunc("POST /api/chat/stream", ua(s.handleChatStream))
	mux.HandleFunc("GET /api/chat/history", ua(s.handleChatHistory))
	mux.HandleFunc("GET /api/chat/sessions", ua(s.handleChatSessions))

	// Agent management
	mux.HandleFunc("GET /api/agents", ua(s.handleListAgents))
	mux.HandleFunc("POST /api/agents", ua(s.handleCreateAgent))
	mux.HandleFunc("PUT /api/agents/{id}", ua(s.handleUpdateAgent))
	mux.HandleFunc("DELETE /api/agents/{id}", ua(s.handleDeleteAgent))

	// Skills (global, not per-user)
	mux.HandleFunc("GET /api/skills", s.handleListSkills)
	mux.HandleFunc("DELETE /api/skills/{name}", s.handleDeleteSkill)

	// Plugins (global, not per-user)
	mux.HandleFunc("GET /api/plugins", s.handleListPlugins)
	mux.HandleFunc("PUT /api/plugins/{id}", s.handleUpdatePlugin)

	// Tasks
	mux.HandleFunc("GET /api/tasks", ua(s.handleListTasks))

	// Channels (local user only)
	mux.HandleFunc("GET /api/channels", s.handleListChannels)

	// Cron jobs
	mux.HandleFunc("GET /api/cron", ua(s.handleListCronJobs))
	mux.HandleFunc("POST /api/cron", ua(s.handleCreateCronJob))
	mux.HandleFunc("PUT /api/cron/{id}", ua(s.handleUpdateCronJob))
	mux.HandleFunc("DELETE /api/cron/{id}", ua(s.handleDeleteCronJob))

	// OpenAI-compatible API and WebSocket gateway
	if s.apiServer != nil {
		s.apiServer.RegisterRoutes(mux)
		s.apiServer.RegisterAdminRoutes(mux)
	}

	// Static files from embedded web/out
	webRoot, err := fs.Sub(webFS, "web")
	if err != nil {
		return fmt.Errorf("setup: embed sub: %w", err)
	}

	// Serve static files with SPA fallback
	mux.Handle("/", spaHandler{fs: webRoot})

	// Determine bind address
	var addr string
	if s.bind == "all" {
		addr = fmt.Sprintf("0.0.0.0:%d", s.port)
	} else {
		addr = fmt.Sprintf("127.0.0.1:%d", s.port)
	}
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	// Graceful shutdown on context cancel
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("setup: listen %s: %w", addr, err)
	}

	slog.Info("web UI running", "url", fmt.Sprintf("http://localhost:%d", s.port))
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// spaHandler serves static files, falling back to the directory's index.html
// for paths that don't match a file (to support client-side routing).
type spaHandler struct {
	fs fs.FS
}

func (h spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Strip trailing slash (except root) to normalize
	if path != "/" && strings.HasSuffix(path, "/") {
		path = strings.TrimSuffix(path, "/")
	}

	// fs.FS paths don't have leading slash
	fsPath := strings.TrimPrefix(path, "/")
	if fsPath == "" {
		fsPath = "."
	}

	// Try to open the file directly (static assets like .js, .css, .ico)
	f, err := h.fs.Open(fsPath)
	if err == nil {
		stat, statErr := f.Stat()
		f.Close()
		if statErr == nil && !stat.IsDir() {
			http.ServeFileFS(w, r, h.fs, fsPath)
			return
		}
	}

	// For route paths (/onboard, /overview, /chat), look for index.html in that dir
	var indexPath string
	if fsPath == "." {
		indexPath = "index.html"
	} else {
		indexPath = fsPath + "/index.html"
	}
	f, err = h.fs.Open(indexPath)
	if err == nil {
		f.Close()
		http.ServeFileFS(w, r, h.fs, indexPath)
		return
	}

	// Try path.html
	htmlPath := fsPath + ".html"
	f, err = h.fs.Open(htmlPath)
	if err == nil {
		f.Close()
		http.ServeFileFS(w, r, h.fs, htmlPath)
		return
	}

	// Fall back to index.html for client-side routing
	http.ServeFileFS(w, r, h.fs, "index.html")
}
