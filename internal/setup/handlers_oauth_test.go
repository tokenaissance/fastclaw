package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/auth"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

// oauthProvider is a minimal mock OAuth server for handler tests:
// discovery + registration + authorize + token.
type oauthProvider struct {
	srv        *httptest.Server
	clients    map[string][]string
	challenges map[string]string
	codes      map[string]string // code -> state
	next       int
}

func newOAuthProvider(t *testing.T) *oauthProvider {
	t.Helper()
	p := &oauthProvider{
		clients:    map[string][]string{},
		challenges: map[string]string{},
		codes:      map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 p.srv.URL,
			"authorization_endpoint": p.srv.URL + "/authorize",
			"token_endpoint":         p.srv.URL + "/token",
			"registration_endpoint":  p.srv.URL + "/register",
			"revocation_endpoint":    p.srv.URL + "/revoke",
			"scopes_supported":       []string{"quant"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			RedirectURIs []string `json:"redirect_uris"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		p.next++
		cid := "cid"
		p.clients[cid] = req.RedirectURIs
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"client_id": cid})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		clientID, redirectURI, state := q.Get("client_id"), q.Get("redirect_uri"), q.Get("state")
		if _, ok := p.clients[clientID]; !ok {
			http.Error(w, "client", http.StatusBadRequest)
			return
		}
		p.challenges[state] = q.Get("code_challenge")
		p.next++
		code := "code"
		p.codes[code] = state
		http.Redirect(w, r, redirectURI+"?code="+code+"&state="+state, http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		code := r.Form.Get("code")
		state, ok := p.codes[code]
		if !ok {
			http.Error(w, "code used", http.StatusBadRequest)
			return
		}
		delete(p.codes, code)
		if domain.S256Challenge(r.Form.Get("code_verifier")) != p.challenges[state] {
			http.Error(w, "pkce", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "refresh_token": "rt", "expires_in": 3600, "scope": "quant",
		})
	})
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func newOAuthTestServer(t *testing.T, p *oauthProvider) (*Server, *store.DBStore) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "oauth-handler.db")
	st, err := store.New(&store.StorageConfig{Type: "sqlite", DSN: dsn, AutoMigrate: true}, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	db, ok := st.(*store.DBStore)
	if !ok {
		t.Fatalf("store is %T, want *DBStore", st)
	}
	t.Cleanup(func() { db.Close() })

	b, err := oauth.Init("handler-test-secret", oauth.Options{DB: db})
	if err != nil {
		t.Fatalf("oauth init: %v", err)
	}
	srv := NewServer(0)
	srv.SetStore(st)
	srv.SetOAuth(b)
	return srv, db
}

func createUserAndAgent(t *testing.T, st store.Store, username string) (userID, agentID string) {
	t.Helper()
	accts, err := users.NewAccounts(st)
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	acct, err := accts.Create(context.Background(), users.CreateInput{
		Username: username, Email: username + "@test.dev", Password: "pw", Role: users.RoleUser,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	agentID = "agent-" + username
	if err := st.SaveAgent(context.Background(), &store.AgentRecord{
		ID: agentID, UserID: acct.ID, Name: username + "-agent", Config: map[string]any{},
	}); err != nil {
		t.Fatalf("save agent: %v", err)
	}
	return acct.ID, agentID
}

func withIdentity(userID, role string) context.Context {
	return auth.WithIdentity(context.Background(), auth.Identity{
		UserID: userID, Role: role, AuthMethod: "session",
	})
}

func doJSON(t *testing.T, h http.HandlerFunc, ctx context.Context, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/mcp/oauth/start", bytes.NewReader(raw)).WithContext(ctx)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestMcpOAuthStartOwnershipAndFullFlow(t *testing.T) {
	p := newOAuthProvider(t)
	srv, _ := newOAuthTestServer(t, p)
	owner, agentID := createUserAndAgent(t, srv.dataStore, "owner")
	other, _ := createUserAndAgent(t, srv.dataStore, "other")

	startBody := map[string]any{
		"agentId": agentID, "serverName": "quandora",
		"serverUrl": p.srv.URL + "/quant", "callbackBase": p.srv.URL + "/oauth/mcp",
	}

	// Owner can start.
	rec := doJSON(t, srv.handleMcpOAuthStart, withIdentity(owner, users.RoleUser), startBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner start = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		AuthURL     string `json:"authUrl"`
		State       string `json:"state"`
		CallbackURL string `json:"callbackUrl"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.AuthURL == "" || out.State == "" {
		t.Fatalf("start response: %+v err=%v", out, err)
	}

	// Non-owner is forbidden.
	rec = doJSON(t, srv.handleMcpOAuthStart, withIdentity(other, users.RoleUser), startBody)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner start = %d, want 403", rec.Code)
	}

	// Status before completion: none.
	req := httptest.NewRequest(http.MethodGet,
		"/api/mcp/oauth/status?agentId="+agentID+"&serverName=quandora", nil).
		WithContext(withIdentity(owner, users.RoleUser))
	rec = httptest.NewRecorder()
	srv.handleMcpOAuthStatus(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	// Browser goes through the provider; cloud forwards code+state.
	cli := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cli.Get(out.AuthURL)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	resp.Body.Close()
	params, err := domain.ParseCallbackURL(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect: %v", err)
	}
	rec = doJSON(t, srv.handleMcpOAuthCallback, context.Background(), map[string]any{
		"code": params.Code, "state": params.State,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("callback = %d: %s", rec.Code, rec.Body.String())
	}

	// Status now authorized.
	req = httptest.NewRequest(http.MethodGet,
		"/api/mcp/oauth/status?agentId="+agentID+"&serverName=quandora", nil).
		WithContext(withIdentity(owner, users.RoleUser))
	rec = httptest.NewRecorder()
	srv.handleMcpOAuthStatus(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status after complete = %d", rec.Code)
	}
	var st struct {
		Status string `json:"status"`
	}
	json.Unmarshal(rec.Body.Bytes(), &st)
	if st.Status != "authorized" {
		t.Fatalf("status = %q, want authorized", st.Status)
	}

	// Revoke: owner OK, non-owner forbidden.
	rec = doJSON(t, srv.handleMcpOAuthRevoke, withIdentity(other, users.RoleUser), map[string]any{
		"agentId": agentID, "serverName": "quandora", "serverUrl": p.srv.URL + "/quant",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner revoke = %d, want 403", rec.Code)
	}
	rec = doJSON(t, srv.handleMcpOAuthRevoke, withIdentity(owner, users.RoleUser), map[string]any{
		"agentId": agentID, "serverName": "quandora", "serverUrl": p.srv.URL + "/quant",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("owner revoke = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMcpOAuthDisabled(t *testing.T) {
	srv := NewServer(0) // no SetOAuth
	rec := doJSON(t, srv.handleMcpOAuthStart, withIdentity("u", users.RoleUser), map[string]any{
		"agentId": "a", "serverName": "q", "serverUrl": "https://x", "callbackBase": "https://x/oauth/mcp",
	})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("start without oauth = %d, want 503", rec.Code)
	}
}

// TestMcpOAuthServersList: the cloud console enumerates an agent's real
// OAuth-protected MCP servers through GET /api/mcp/oauth/servers. Only
// servers with oauthResource are listed, and each carries its status.
func TestMcpOAuthServersList(t *testing.T) {
	p := newOAuthProvider(t)
	srv, _ := newOAuthTestServer(t, p)
	owner, agentID := createUserAndAgent(t, srv.dataStore, "owner")
	other, _ := createUserAndAgent(t, srv.dataStore, "other")
	ctx := context.Background()

	oauthURL := p.srv.URL + "/quant"
	if err := srv.dataStore.AddMCPServer(ctx, agentID, "quandora", config.MCPServerConfig{
		Type: "http", URL: oauthURL, OAuthResource: oauthURL,
	}); err != nil {
		t.Fatalf("add quandora server: %v", err)
	}
	if err := srv.dataStore.AddMCPServer(ctx, agentID, "plain", config.MCPServerConfig{
		Type: "http", URL: "https://plain.example/mcp",
	}); err != nil {
		t.Fatalf("add plain server: %v", err)
	}

	// Authorize quandora through the real handler flow so status=authorized.
	start := doJSON(t, srv.handleMcpOAuthStart, withIdentity(owner, users.RoleUser), map[string]any{
		"agentId": agentID, "serverName": "quandora", "serverUrl": oauthURL,
		"callbackBase": p.srv.URL + "/oauth/mcp",
	})
	var out struct {
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &out); err != nil || out.AuthURL == "" {
		t.Fatalf("start response: %+v err=%v", out, err)
	}
	cli := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cli.Get(out.AuthURL)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	resp.Body.Close()
	params, err := domain.ParseCallbackURL(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect: %v", err)
	}
	cb := doJSON(t, srv.handleMcpOAuthCallback, context.Background(), map[string]any{
		"code": params.Code, "state": params.State,
	})
	if cb.Code != http.StatusOK {
		t.Fatalf("callback = %d: %s", cb.Code, cb.Body.String())
	}

	getServers := func(identity context.Context) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet,
			"/api/mcp/oauth/servers?agentId="+agentID, nil).WithContext(identity)
		r := httptest.NewRecorder()
		srv.handleMcpOAuthServers(r, req)
		return r
	}

	// Non-owner cannot enumerate.
	if r := getServers(withIdentity(other, users.RoleUser)); r.Code != http.StatusForbidden {
		t.Fatalf("non-owner servers = %d, want 403", r.Code)
	}

	r := getServers(withIdentity(owner, users.RoleUser))
	if r.Code != http.StatusOK {
		t.Fatalf("owner servers = %d: %s", r.Code, r.Body.String())
	}
	var body struct {
		OK      bool `json:"ok"`
		Servers []struct {
			ServerName string `json:"serverName"`
			URL        string `json:"url"`
			Status     string `json:"status"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode servers: %v", err)
	}
	if !body.OK || len(body.Servers) != 1 {
		t.Fatalf("servers = %+v, want exactly quandora", body.Servers)
	}
	srv0 := body.Servers[0]
	if srv0.ServerName != "quandora" || srv0.URL != oauthURL || srv0.Status != "authorized" {
		t.Fatalf("quandora entry = %+v, want authorized %s", srv0, oauthURL)
	}
}
