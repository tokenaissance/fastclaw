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
	"github.com/fastclaw-ai/fastclaw/internal/auth"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/session"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/taskqueue"
	"github.com/fastclaw-ai/fastclaw/internal/usage"
	"github.com/fastclaw-ai/fastclaw/internal/users"
	"github.com/fastclaw-ai/fastclaw/internal/workspace"
)

// AgentHandle is the surface the web UI uses to talk to a running agent.
type AgentHandle interface {
	Name() string
	HandleWebChat(ctx context.Context, sessionId, text string) string
	HandleWebChatStream(ctx context.Context, sessionId, text string, imageURLs []string, events chan<- agent.ChatEvent) string
	WebChatHistory(sessionId string) []map[string]any
	WebChatSessions() []session.WebSession
	DeleteWebChatSession(sessionId string) error
	RenameWebChatSession(sessionId, title string) error
	ReloadWorkspaceFiles()
}

// AgentProvider is implemented by gateway.UserSpace's agent manager — used
// by handlers that legitimately need to enumerate the *current caller's*
// agents (resolved through the user resolver, not from a global pool).
type AgentProvider interface {
	AllAgents() []AgentHandle
	AgentByID(id string) AgentHandle
	ReloadAgents() error
}

// Server hosts the web UI + admin API. Multi-user is unconditional —
// every request must resolve to a real users.id via the auth.Resolver.
type Server struct {
	port           int
	bind           string
	gatewayCfg     *config.GatewayCfg
	userResolver   api.UserResolver
	taskQueue      *taskqueue.Queue
	apiServer      *api.Server
	authResolver   *auth.Resolver
	accounts       *users.Accounts
	apikeys        *users.APIKeys
	dataStore      store.Store
	workspaceStore workspace.Store
	usage          usage.Meter
	startedAt      time.Time
}

// NewServer creates a setup wizard server on the given port.
func NewServer(port int) *Server {
	return &Server{port: port, bind: "loopback", startedAt: time.Now()}
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

// SetTaskQueue sets the task queue for the tasks API endpoint.
func (s *Server) SetTaskQueue(tq *taskqueue.Queue) {
	s.taskQueue = tq
}

// SetAPIServer sets the OpenAI-compatible API server for /v1/* and /ws routes.
func (s *Server) SetAPIServer(apiSrv *api.Server) {
	s.apiServer = apiSrv
}

// SetUserResolver sets the per-user agent routing resolver.
func (s *Server) SetUserResolver(resolver api.UserResolver) {
	s.userResolver = resolver
}

// SetStore sets the storage backend.
func (s *Server) SetStore(st store.Store) {
	s.dataStore = st
	if st != nil {
		s.accounts, _ = users.NewAccounts(st)
		s.apikeys, _ = users.NewAPIKeys(st)
	}
}

// SetWorkspaceStore installs the blob store used for agent-generated artifacts.
func (s *Server) SetWorkspaceStore(ws workspace.Store) {
	s.workspaceStore = ws
}

// SetUsageMeter installs the per-tenant resource counter.
func (s *Server) SetUsageMeter(m usage.Meter) {
	s.usage = m
}

// SetAuth installs the auth resolver. Required.
func (s *Server) SetAuth(resolver *auth.Resolver) {
	s.authResolver = resolver
}

// authMiddleware wraps the auth.Resolver's Middleware. Required for every
// authenticated route.
func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	if s.authResolver == nil {
		return func(w http.ResponseWriter, r *http.Request) {
			jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "auth not configured"})
		}
	}
	return s.authResolver.Middleware(next)
}

// optionalAuth is the bootstrap-friendly variant for endpoints reachable
// before login (status, login, onboard).
func (s *Server) optionalAuth(next http.HandlerFunc) http.HandlerFunc {
	if s.authResolver == nil {
		return next
	}
	return s.authResolver.Optional(next)
}

// requireSuperAdmin gates handlers behind a super_admin role check.
func (s *Server) requireSuperAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.authMiddleware(auth.RequireSuperAdmin(next))
}

// Run starts the HTTP server and blocks until the context is canceled.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()

	// Health probes (unauthenticated).
	healthz := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /livez", healthz)
	mux.HandleFunc("GET /readyz", healthz)

	auth := s.authMiddleware
	opt := s.optionalAuth
	admin := s.requireSuperAdmin

	// Bootstrap / login.
	mux.HandleFunc("GET /api/status", opt(s.handleStatus))
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", auth(s.handleLogout))
	mux.HandleFunc("GET /api/me", auth(s.handleMe))
	mux.HandleFunc("POST /api/test-provider", opt(s.handleTestProvider))
	mux.HandleFunc("POST /api/onboard", s.handleOnboard)

	// Per-user config (system_settings + scoped providers/channels).
	mux.HandleFunc("GET /api/config", auth(s.handleGetConfig))
	mux.HandleFunc("POST /api/config", auth(s.handleUpdateConfig))

	// Chat
	mux.HandleFunc("POST /api/chat", auth(s.handleChat))
	mux.HandleFunc("POST /api/chat/stream", auth(s.handleChatStream))
	mux.HandleFunc("GET /api/chat/history", auth(s.handleChatHistory))
	mux.HandleFunc("GET /api/chat/sessions", auth(s.handleChatSessions))
	mux.HandleFunc("PUT /api/chat/sessions/{key}", auth(s.handleRenameSession))
	mux.HandleFunc("DELETE /api/chat/sessions/{key}", auth(s.handleDeleteSession))

	// Agents
	mux.HandleFunc("GET /api/agents", auth(s.handleListAgents))
	mux.HandleFunc("POST /api/agents", auth(s.handleCreateAgent))
	mux.HandleFunc("GET /api/agents/{id}", auth(s.handleGetAgent))
	mux.HandleFunc("PUT /api/agents/{id}", auth(s.handleUpdateAgent))
	mux.HandleFunc("GET /api/agents/{id}/config", auth(s.handleGetAgentConfig))
	mux.HandleFunc("DELETE /api/agents/{id}", auth(s.handleDeleteAgent))

	mux.HandleFunc("GET /api/agents/{id}/files", auth(s.handleAgentFileList))
	mux.HandleFunc("GET /api/agents/{id}/files/{path...}", auth(s.handleAgentFile))
	mux.HandleFunc("POST /api/agents/{id}/files", auth(s.handleAgentFileUpload))

	mux.HandleFunc("GET /api/agents/{id}/system-files/{name}", auth(s.handleGetAgentSystemFile))
	mux.HandleFunc("PUT /api/agents/{id}/system-files/{name}", auth(s.handlePutAgentSystemFile))
	mux.HandleFunc("DELETE /api/agents/{id}/system-files/{name}", auth(s.handleDeleteAgentSystemFile))

	// Skills
	mux.HandleFunc("GET /api/skills", s.handleListSkills)
	mux.HandleFunc("GET /api/skills/search", auth(s.handleSearchSkills))
	mux.HandleFunc("POST /api/skills/install", auth(s.handleInstallSkill))
	mux.HandleFunc("DELETE /api/skills/{name}", admin(s.handleDeleteSkill))
	mux.HandleFunc("GET /api/agents/{id}/skills", auth(s.handleListAgentSkills))
	mux.HandleFunc("DELETE /api/agents/{id}/skills/{name}", auth(s.handleDeleteAgentSkill))

	// Plugins (super_admin only).
	mux.HandleFunc("GET /api/plugins", admin(s.handleListPlugins))
	mux.HandleFunc("PUT /api/plugins/{id}", admin(s.handleUpdatePlugin))

	// Tools (super_admin only).
	mux.HandleFunc("GET /api/tools", admin(s.handleGetTools))
	mux.HandleFunc("PUT /api/tools", admin(s.handleSaveTools))

	// Channels (read-only list of registered channel adapters at runtime)
	mux.HandleFunc("GET /api/channels", auth(s.handleListChannels))

	// Scoped CRUD: providers + channels at system / user / agent scope.
	mux.HandleFunc("GET /api/providers", auth(s.handleListProviders))
	mux.HandleFunc("POST /api/providers", auth(s.handleCreateProvider))
	mux.HandleFunc("PUT /api/providers/{id}", auth(s.handleUpdateProvider))
	mux.HandleFunc("DELETE /api/providers/{id}", auth(s.handleDeleteProvider))
	mux.HandleFunc("POST /api/providers/{id}/test", auth(s.handleTestStoredProvider))
	mux.HandleFunc("GET /api/scoped-channels", auth(s.handleListScopedChannels))
	mux.HandleFunc("POST /api/scoped-channels", auth(s.handleCreateScopedChannel))
	mux.HandleFunc("PUT /api/scoped-channels/{id}", auth(s.handleUpdateScopedChannel))
	mux.HandleFunc("DELETE /api/scoped-channels/{id}", auth(s.handleDeleteScopedChannel))

	// Cron jobs (per-user)
	mux.HandleFunc("GET /api/cron", auth(s.handleListCronJobs))
	mux.HandleFunc("POST /api/cron", auth(s.handleCreateCronJob))
	mux.HandleFunc("PUT /api/cron/{id}", auth(s.handleUpdateCronJob))
	mux.HandleFunc("DELETE /api/cron/{id}", auth(s.handleDeleteCronJob))

	// Tasks
	mux.HandleFunc("GET /api/tasks", auth(s.handleListTasks))

	// Apikeys (per-user, with agent multi-select).
	mux.HandleFunc("GET /api/apikeys", auth(s.handleListAPIKeys))
	mux.HandleFunc("POST /api/apikeys", auth(s.handleCreateAPIKey))
	mux.HandleFunc("DELETE /api/apikeys/{id}", auth(s.handleDeleteAPIKey))
	mux.HandleFunc("POST /api/apikeys/{id}/rotate", auth(s.handleRotateAPIKey))
	mux.HandleFunc("PUT /api/apikeys/{id}/agents", auth(s.handleSetAPIKeyAgents))

	// Admin: user management (super_admin only).
	mux.HandleFunc("GET /api/admin/users", admin(s.handleAdminListUsers))
	mux.HandleFunc("POST /api/admin/users", admin(s.handleAdminCreateUser))
	mux.HandleFunc("PUT /api/admin/users/{id}", admin(s.handleAdminUpdateUser))
	mux.HandleFunc("DELETE /api/admin/users/{id}", admin(s.handleAdminDeleteUser))
	mux.HandleFunc("POST /api/admin/users/{id}/password", admin(s.handleAdminResetPassword))
	mux.HandleFunc("GET /api/admin/agents", admin(s.handleAdminListAgents))
	mux.HandleFunc("GET /api/admin/usage", admin(s.handleGetUsage))

	// OpenAI-compatible API and WebSocket gateway.
	if s.apiServer != nil {
		s.apiServer.RegisterRoutes(mux)
	}

	// Static UI files.
	webRoot, err := fs.Sub(webFS, "web")
	if err != nil {
		return fmt.Errorf("setup: embed sub: %w", err)
	}
	mux.Handle("/", spaHandler{fs: webRoot})

	var addr string
	if s.bind == "all" {
		addr = fmt.Sprintf("0.0.0.0:%d", s.port)
	} else {
		addr = fmt.Sprintf("127.0.0.1:%d", s.port)
	}
	srv := &http.Server{Addr: addr, Handler: mux}

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

// spaHandler serves the embedded Next.js UI with SPA-style fallback.
type spaHandler struct {
	fs fs.FS
}

func (h spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path != "/" && strings.HasSuffix(path, "/") {
		path = strings.TrimSuffix(path, "/")
	}
	fsPath := strings.TrimPrefix(path, "/")
	if fsPath == "" {
		fsPath = "."
	}
	if f, err := h.fs.Open(fsPath); err == nil {
		stat, statErr := f.Stat()
		f.Close()
		if statErr == nil && !stat.IsDir() {
			http.ServeFileFS(w, r, h.fs, fsPath)
			return
		}
	}
	var indexPath string
	if fsPath == "." {
		indexPath = "index.html"
	} else {
		indexPath = fsPath + "/index.html"
	}
	if f, err := h.fs.Open(indexPath); err == nil {
		f.Close()
		http.ServeFileFS(w, r, h.fs, indexPath)
		return
	}
	if strings.HasPrefix(fsPath, "agents/") {
		parts := strings.SplitN(fsPath, "/", 3)
		if len(parts) >= 3 && parts[1] != "default" {
			directFallback := "agents/default/" + parts[2]
			if f, err := h.fs.Open(directFallback); err == nil {
				stat, statErr := f.Stat()
				f.Close()
				if statErr == nil && !stat.IsDir() {
					http.ServeFileFS(w, r, h.fs, directFallback)
					return
				}
			}
			dirFallback := "agents/default/" + parts[2] + "/index.html"
			if f, err := h.fs.Open(dirFallback); err == nil {
				f.Close()
				http.ServeFileFS(w, r, h.fs, dirFallback)
				return
			}
		}
	}
	htmlPath := fsPath + ".html"
	if f, err := h.fs.Open(htmlPath); err == nil {
		f.Close()
		http.ServeFileFS(w, r, h.fs, htmlPath)
		return
	}
	http.ServeFileFS(w, r, h.fs, "index.html")
}
