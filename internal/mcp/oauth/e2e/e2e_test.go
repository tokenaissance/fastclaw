// Package e2e exercises the OAuth client against a local mock provider
// using the REAL adapters (HTTP discovery/registration/exchange, file
// stores, loopback receiver) wired exactly like the daemon bootstrap.
// It covers the three interaction paths from the design doc: web
// callback, CLI loopback, and paste-back (via ParseCallbackURL).
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/adapter"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/port"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/usecase"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// ---------- mock provider ----------

type codeRecord struct {
	clientID    string
	redirectURI string
	challenge   string
}

type mockProvider struct {
	mu           sync.Mutex
	base         string
	clients      map[string][]string // clientID -> redirect_uris
	challenges   map[string]string   // state -> code_challenge
	codes        map[string]codeRecord
	refreshToken string // current valid refresh token (rotated on use)
	revoked      []string
	refreshCalls int
	next         int
}

func newMockProvider(t *testing.T) (*mockProvider, *httptest.Server) {
	t.Helper()
	m := &mockProvider{
		clients:    map[string][]string{},
		challenges: map[string]string{},
		codes:      map[string]codeRecord{},
	}
	srv := httptest.NewServer(http.HandlerFunc(m.handle))
	m.base = srv.URL
	return m, srv
}

func (m *mockProvider) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/.well-known/oauth-authorization-server":
		m.handleDiscovery(w)
	case r.URL.Path == "/oauth/register" && r.Method == http.MethodPost:
		m.handleRegister(w, r)
	case r.URL.Path == "/oauth/authorize" && r.Method == http.MethodGet:
		m.handleAuthorize(w, r)
	case r.URL.Path == "/oauth/token" && r.Method == http.MethodPost:
		m.handleToken(w, r)
	case r.URL.Path == "/oauth/revoke" && r.Method == http.MethodPost:
		m.handleRevoke(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (m *mockProvider) handleDiscovery(w http.ResponseWriter) {
	json.NewEncoder(w).Encode(map[string]any{
		"issuer":                           m.base,
		"authorization_endpoint":           m.base + "/oauth/authorize",
		"token_endpoint":                   m.base + "/oauth/token",
		"registration_endpoint":            m.base + "/oauth/register",
		"revocation_endpoint":              m.base + "/oauth/revoke",
		"scopes_supported":                 []string{"quant"},
		"code_challenge_methods_supported": []string{"S256"},
		"authorization_response_iss_parameter_supported": false,
	})
}

func (m *mockProvider) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	cid := fmt.Sprintf("cid-%d", m.next)
	m.clients[cid] = req.RedirectURIs
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{"client_id": cid})
}

func (m *mockProvider) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientID, redirectURI, state := q.Get("client_id"), q.Get("redirect_uri"), q.Get("state")
	challenge, method := q.Get("code_challenge"), q.Get("code_challenge_method")
	m.mu.Lock()
	defer m.mu.Unlock()
	uris, ok := m.clients[clientID]
	if !ok {
		http.Error(w, "unknown client", http.StatusBadRequest)
		return
	}
	allowed := false
	for _, u := range uris {
		if u == redirectURI {
			allowed = true
			break
		}
	}
	if !allowed {
		http.Error(w, "redirect_uri not registered", http.StatusBadRequest)
		return
	}
	if method != "S256" {
		http.Error(w, "only S256 supported", http.StatusBadRequest)
		return
	}
	m.challenges[state] = challenge
	m.next++
	code := fmt.Sprintf("code-%d", m.next)
	m.codes[code] = codeRecord{clientID: clientID, redirectURI: redirectURI, challenge: challenge}
	loc := redirectURI + "?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(state)
	http.Redirect(w, r, loc, http.StatusFound)
}

func (m *mockProvider) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	grant := r.Form.Get("grant_type")
	m.mu.Lock()
	defer m.mu.Unlock()
	switch grant {
	case "authorization_code":
		code, verifier := r.Form.Get("code"), r.Form.Get("code_verifier")
		redirectURI, clientID := r.Form.Get("redirect_uri"), r.Form.Get("client_id")
		rec, ok := m.codes[code]
		if !ok {
			http.Error(w, "code already used or unknown", http.StatusBadRequest)
			return
		}
		delete(m.codes, code) // single-use code
		if rec.clientID != clientID || rec.redirectURI != redirectURI {
			http.Error(w, "client/redirect mismatch", http.StatusBadRequest)
			return
		}
		if domain.S256Challenge(verifier) != rec.challenge {
			http.Error(w, "pkce verification failed", http.StatusBadRequest)
			return
		}
		m.refreshToken = "rt-1"
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "at-1",
			"refresh_token": m.refreshToken,
			"expires_in":    3600,
			"scope":         "quant",
		})
	case "refresh_token":
		m.refreshCalls++
		oldRT, clientID := r.Form.Get("refresh_token"), r.Form.Get("client_id")
		if _, ok := m.clients[clientID]; !ok {
			http.Error(w, "unknown client", http.StatusBadRequest)
			return
		}
		if oldRT != m.refreshToken {
			http.Error(w, "refresh token already used or unknown", http.StatusBadRequest)
			return
		}
		// Rotate: the old refresh token is invalidated.
		m.refreshToken = "rt-2"
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "at-2",
			"refresh_token": m.refreshToken,
			"expires_in":    3600,
			"scope":         "quant",
		})
	default:
		http.Error(w, "unsupported grant", http.StatusBadRequest)
	}
}

func (m *mockProvider) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.revoked = append(m.revoked, r.Form.Get("token"))
	w.WriteHeader(http.StatusOK)
}

// ---------- harness (real adapters, same assembly as bootstrap) ----------

type env struct {
	root      string
	crypt     *adapter.AESGCMCryptor
	meta      *adapter.CachingMetadataFetcher
	regs      port.ClientRegistrationStore
	pending   port.PendingAuthStore
	tokens    port.TokenStore
	start     *usecase.StartAuthorization
	complete  *usecase.CompleteAuthorization
	refresh   *usecase.RefreshToken
	provider  *usecase.TokenProvider
	revoke    *usecase.RevokeToken
	status    *usecase.Status
	serverURL string
}

func newEnv(t *testing.T, srv *httptest.Server, root string) *env {
	t.Helper()
	crypt, err := adapter.NewAESGCMCryptor("e2e-secret")
	if err != nil {
		t.Fatalf("cryptor: %v", err)
	}
	cli := srv.Client()
	meta := adapter.NewCachingMetadataFetcher(cli, time.Hour)
	regs := &adapter.FileRegistrationStore{Root: root}
	pending := &adapter.FilePendingStore{Root: root, Crypt: crypt}
	tokens := &adapter.FileTokenStore{Root: root, Crypt: crypt}
	exch := &adapter.HTTPCodeExchanger{Client: cli}
	registrar := &adapter.HTTPClientRegistrar{Client: cli}
	locks := usecase.NewRefreshLocks()
	return &env{
		root: root, crypt: crypt, meta: meta, regs: regs, pending: pending, tokens: tokens,
		start:     &usecase.StartAuthorization{Meta: meta, Registrar: registrar, Regs: regs, Pending: pending},
		complete:  &usecase.CompleteAuthorization{Meta: meta, Pending: pending, Regs: regs, Tokens: tokens, Exchange: exch},
		refresh:   &usecase.RefreshToken{Meta: meta, Regs: regs, Tokens: tokens, Exchange: exch, Locks: locks},
		provider:  &usecase.TokenProvider{Tokens: tokens, Refresh: &usecase.RefreshToken{Meta: meta, Regs: regs, Tokens: tokens, Exchange: exch, Locks: locks}},
		revoke:    &usecase.RevokeToken{Meta: meta, Regs: regs, Tokens: tokens, Exchange: exch},
		status:    &usecase.Status{Tokens: tokens},
		serverURL: srv.URL + "/quant",
	}
}

// newEnvDB builds the same assembly but with the shared SQL stores
// (sqlite in tests; Postgres in production) — the multi-instance shape.
func newEnvDB(t *testing.T, srv *httptest.Server, db *store.DBStore, locker port.DistributedLocker) *env {
	t.Helper()
	crypt, err := adapter.NewAESGCMCryptor("e2e-secret")
	if err != nil {
		t.Fatalf("cryptor: %v", err)
	}
	cli := srv.Client()
	meta := adapter.NewCachingMetadataFetcher(cli, time.Hour)
	regs := &adapter.DBRegistrationStore{DB: db.DB(), Dialect: db.Dialect()}
	pending := &adapter.DBPendingStore{DB: db.DB(), Dialect: db.Dialect(), Crypt: crypt}
	tokens := &adapter.DBTokenStore{DB: db.DB(), Dialect: db.Dialect(), Crypt: crypt}
	exch := &adapter.HTTPCodeExchanger{Client: cli}
	registrar := &adapter.HTTPClientRegistrar{Client: cli}
	locks := usecase.NewRefreshLocks()
	refresh := &usecase.RefreshToken{Meta: meta, Regs: regs, Tokens: tokens, Exchange: exch, Locks: locks, Locker: locker}
	return &env{
		root: "", crypt: crypt, meta: meta, regs: regs, pending: pending, tokens: tokens,
		start:     &usecase.StartAuthorization{Meta: meta, Registrar: registrar, Regs: regs, Pending: pending},
		complete:  &usecase.CompleteAuthorization{Meta: meta, Pending: pending, Regs: regs, Tokens: tokens, Exchange: exch},
		refresh:   refresh,
		provider:  &usecase.TokenProvider{Tokens: tokens, Refresh: refresh},
		revoke:    &usecase.RevokeToken{Meta: meta, Regs: regs, Tokens: tokens, Exchange: exch},
		status:    &usecase.Status{Tokens: tokens},
		serverURL: srv.URL + "/quant",
	}
}

// authorize follows the provider redirect one hop and returns the
// callback params (what the browser / cloud forwarding would receive).
func authorize(t *testing.T, authURL string) domain.CallbackParams {
	t.Helper()
	cli := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := cli.Get(authURL)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize status = %d, want 302", resp.StatusCode)
	}
	params, err := domain.ParseCallbackURL(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect: %v", err)
	}
	if params.Code == "" || params.State == "" {
		t.Fatalf("redirect missing code/state: %+v", params)
	}
	return params
}

func refreshInput(e *env) usecase.RefreshInput {
	return usecase.RefreshInput{
		UserID: "u1", AgentID: "a1", ServerName: "quandora", ServerURL: e.serverURL,
	}
}

// ---------- scenarios ----------

// Test1WebFlow: callbackBase path (cloud), real HTTP adapters, and the
// state/registration/pending surviving a simulated daemon restart.
func Test1WebFlowAndPersistence(t *testing.T) {
	m, srv := newMockProvider(t)
	defer srv.Close()
	root := t.TempDir()
	e := newEnv(t, srv, root)

	out, err := e.start.Execute(context.Background(), usecase.StartAuthInput{
		UserID: "u1", AgentID: "a1", ServerName: "quandora",
		ServerURL: e.serverURL, CallbackBase: srv.URL + "/oauth/mcp",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	callbackID, _ := domain.CallbackID(e.serverURL)
	wantCB := srv.URL + "/oauth/mcp/" + callbackID + "/callback"
	if out.CallbackURL != wantCB {
		t.Fatalf("callback url = %q, want %q", out.CallbackURL, wantCB)
	}
	if !strings.Contains(out.AuthURL, "code_challenge_method=S256") || !strings.Contains(out.AuthURL, "state="+out.State) {
		t.Fatalf("auth url missing PKCE/state: %s", out.AuthURL)
	}

	params := authorize(t, out.AuthURL)
	if _, err := e.complete.Execute(context.Background(), usecase.CompleteAuthInput{Callback: params}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	st, _ := e.status.Execute(context.Background(), refreshInput(e))
	if st.Status != usecase.StatusValid {
		t.Fatalf("status = %s, want authorized", st.Status)
	}
	if tok, err := e.provider.AccessToken(context.Background(), refreshInput(e)); err != nil || tok != "at-1" {
		t.Fatalf("access token = %q, err %v", tok, err)
	}

	// Simulated restart: fresh stores on the same root. Pending was
	// consumed, so replaying the state must fail even after restart.
	e2 := newEnv(t, srv, root)
	if _, err := e2.complete.Execute(context.Background(), usecase.CompleteAuthInput{Callback: params}); err == nil {
		t.Fatal("state replay after restart must fail")
	}
	// Registration must also be reused: starting again must NOT create a
	// second provider client.
	if _, err := e2.start.Execute(context.Background(), usecase.StartAuthInput{
		UserID: "u1", AgentID: "a1", ServerName: "quandora",
		ServerURL: e2.serverURL, CallbackBase: srv.URL + "/oauth/mcp",
	}); err != nil {
		t.Fatalf("restart start: %v", err)
	}
	m.mu.Lock()
	clients := len(m.clients)
	m.mu.Unlock()
	if clients != 1 {
		t.Fatalf("registered clients = %d, want 1 (reuse after restart)", clients)
	}
}

// Test2PendingSurvivesRestart: a daemon restart mid-flow must not lose
// the pending authorization.
func Test2PendingSurvivesRestart(t *testing.T) {
	m, srv := newMockProvider(t)
	defer srv.Close()
	root := t.TempDir()
	e1 := newEnv(t, srv, root)

	out, err := e1.start.Execute(context.Background(), usecase.StartAuthInput{
		UserID: "u1", AgentID: "a1", ServerName: "quandora",
		ServerURL: e1.serverURL, CallbackBase: srv.URL + "/oauth/mcp",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	params := authorize(t, out.AuthURL)

	// Restart BEFORE completing: brand-new stores must still complete.
	e2 := newEnv(t, srv, root)
	if _, err := e2.complete.Execute(context.Background(), usecase.CompleteAuthInput{Callback: params}); err != nil {
		t.Fatalf("complete after restart: %v", err)
	}
	if tok, err := e2.provider.AccessToken(context.Background(), refreshInput(e2)); err != nil || tok != "at-1" {
		t.Fatalf("access token after restart = %q, err %v", tok, err)
	}
	m.mu.Lock()
	clients := len(m.clients)
	m.mu.Unlock()
	if clients != 1 {
		t.Fatalf("registered clients = %d, want 1", clients)
	}
}

// Test3RefreshRotation: expired token triggers refresh; the old refresh
// token is invalidated by rotation; a second access is served fresh.
func Test3RefreshRotation(t *testing.T) {
	_, srv := newMockProvider(t)
	defer srv.Close()
	e := newEnv(t, srv, t.TempDir())
	ctx := context.Background()
	out, err := e.start.Execute(context.Background(), usecase.StartAuthInput{
		UserID: "u1", AgentID: "a1", ServerName: "quandora",
		ServerURL: e.serverURL, CallbackBase: srv.URL + "/oauth/mcp",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := e.complete.Execute(context.Background(), usecase.CompleteAuthInput{Callback: authorize(t, out.AuthURL)}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Force expiry so the next access triggers refresh.
	key := domain.StoreKey("u1", "a1", "quandora")
	if err := e.tokens.Save(context.Background(), key, &domain.OAuthTokens{
		AccessToken: "stale", RefreshToken: "rt-1", ExpiresAt: time.Now().UTC().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("force expire: %v", err)
	}
	tok, err := e.provider.AccessToken(context.Background(), refreshInput(e))
	if err != nil {
		t.Fatalf("refresh access: %v", err)
	}
	if tok != "at-2" {
		t.Fatalf("refreshed access = %q, want at-2", tok)
	}

	// A second access now returns the fresh token without another rotation.
	tok2, err := e.provider.AccessToken(context.Background(), refreshInput(e))
	if err != nil || tok2 != "at-2" {
		t.Fatalf("second access = %q, err %v", tok2, err)
	}

	// Rotation: using the OLD refresh token (rt-1) must now be rejected.
	if err := e.tokens.Save(ctx, key, &domain.OAuthTokens{
		AccessToken: "x", RefreshToken: "rt-1", ExpiresAt: time.Now().UTC().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("save old token: %v", err)
	}
	if _, err := e.refresh.Execute(ctx, refreshInput(e)); err == nil {
		t.Fatal("old refresh token must be rejected after rotation")
	}
}

// Test4LoopbackPath: CLI loopback receiver captures the browser redirect
// and the flow completes through the same use cases.
func Test4LoopbackPath(t *testing.T) {
	_, srv := newMockProvider(t)
	defer srv.Close()
	e := newEnv(t, srv, t.TempDir())
	ctx := context.Background()

	port, err := adapter.FreeLoopbackPort()
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	callbackID, _ := domain.CallbackID(e.serverURL)
	callbackURL := fmt.Sprintf("http://127.0.0.1:%d/callback/%s", port, callbackID)

	out, err := e.start.Execute(ctx, usecase.StartAuthInput{
		UserID: "u1", AgentID: "a1", ServerName: "quandora",
		ServerURL: e.serverURL, CallbackURL: callbackURL,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	recv, err := (&adapter.LoopbackCallbackReceiver{Port: port, CallbackID: callbackID}).Listen(ctx)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	params := authorize(t, out.AuthURL)

	// Browser follows the redirect to the loopback receiver.
	loc := callbackURL + "?code=" + url.QueryEscape(params.Code) + "&state=" + url.QueryEscape(params.State)
	resp, err := http.Get(loc)
	if err != nil {
		t.Fatalf("browser hit loopback: %v", err)
	}
	resp.Body.Close()

	select {
	case got := <-recv:
		if got.Code != params.Code || got.State != params.State {
			t.Fatalf("loopback params = %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("loopback receiver timed out")
	}
	if _, err := e.complete.Execute(ctx, usecase.CompleteAuthInput{Callback: params}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if tok, err := e.provider.AccessToken(ctx, refreshInput(e)); err != nil || tok != "at-1" {
		t.Fatalf("access token = %q, err %v", tok, err)
	}
}

// Test5Revoke: revocation hits the provider and removes local storage.
func Test5Revoke(t *testing.T) {
	m, srv := newMockProvider(t)
	defer srv.Close()
	e := newEnv(t, srv, t.TempDir())
	ctx := context.Background()
	out, err := e.start.Execute(ctx, usecase.StartAuthInput{
		UserID: "u1", AgentID: "a1", ServerName: "quandora",
		ServerURL: e.serverURL, CallbackBase: srv.URL + "/oauth/mcp",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := e.complete.Execute(ctx, usecase.CompleteAuthInput{Callback: authorize(t, out.AuthURL)}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := e.revoke.Execute(ctx, refreshInput(e)); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	st, _ := e.status.Execute(ctx, refreshInput(e))
	if st.Status != usecase.StatusNone {
		t.Fatalf("status after revoke = %s, want none", st.Status)
	}
	if _, err := os.Stat(filepath.Join(e.root, domain.StoreKey("u1", "a1", "quandora"))); !os.IsNotExist(err) {
		t.Fatal("token file should be removed after revoke")
	}
	m.mu.Lock()
	revoked := len(m.revoked)
	m.mu.Unlock()
	if revoked == 0 {
		t.Fatal("provider revoke endpoint was not called")
	}
}

// openSQLiteDBAt opens a sqlite store at an explicit DSN (shared path).
func openSQLiteDBAt(t *testing.T, dsn string) *store.DBStore {
	t.Helper()
	st, err := store.New(&store.StorageConfig{Type: "sqlite", DSN: dsn, AutoMigrate: true}, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	db, ok := st.(*store.DBStore)
	if !ok {
		t.Fatalf("store is %T, want *DBStore", st)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// Test6MultiInstanceSharedStorage: authorization starts on instance A
// and completes on instance B (shared SQL store) — the exact topology a
// load balancer creates for the web callback path.
func Test6MultiInstanceSharedStorage(t *testing.T) {
	_, srv := newMockProvider(t)
	defer srv.Close()
	dsn := filepath.Join(t.TempDir(), "shared.db")
	dbA := openSQLiteDBAt(t, dsn)
	dbB := openSQLiteDBAt(t, dsn)
	eA := newEnvDB(t, srv, dbA, nil)
	eB := newEnvDB(t, srv, dbB, nil)
	ctx := context.Background()

	out, err := eA.start.Execute(ctx, usecase.StartAuthInput{
		UserID: "u1", AgentID: "a1", ServerName: "quandora",
		ServerURL: eA.serverURL, CallbackBase: srv.URL + "/oauth/mcp",
	})
	if err != nil {
		t.Fatalf("start on A: %v", err)
	}
	params := authorize(t, out.AuthURL)

	// The callback lands on B: pending + registration must be shared.
	if _, err := eB.complete.Execute(ctx, usecase.CompleteAuthInput{Callback: params}); err != nil {
		t.Fatalf("complete on B: %v", err)
	}
	tok, err := eB.provider.AccessToken(ctx, refreshInput(eB))
	if err != nil || tok != "at-1" {
		t.Fatalf("access token on B = %q, err %v", tok, err)
	}
	// And instance A can read the same credential.
	if tok, err := eA.provider.AccessToken(ctx, refreshInput(eA)); err != nil || tok != "at-1" {
		t.Fatalf("access token on A = %q, err %v", tok, err)
	}
}

// Test7ConcurrentRefreshAcrossInstances: two instances share storage and
// race to refresh an expired token; the distributed (Redis) lock makes
// exactly one provider refresh happen and both callers succeed.
func Test7ConcurrentRefreshAcrossInstances(t *testing.T) {
	m, srv := newMockProvider(t)
	defer srv.Close()
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rc.Close()
	locker := &adapter.RedisRefreshLocker{Client: rc, Prefix: "e2e"}

	dsn := filepath.Join(t.TempDir(), "shared.db")
	dbA := openSQLiteDBAt(t, dsn)
	dbB := openSQLiteDBAt(t, dsn)
	eA := newEnvDB(t, srv, dbA, locker)
	eB := newEnvDB(t, srv, dbB, locker)
	ctx := context.Background()

	// Authorize once (via A), then expire the token on both instances.
	out, err := eA.start.Execute(ctx, usecase.StartAuthInput{
		UserID: "u1", AgentID: "a1", ServerName: "quandora",
		ServerURL: eA.serverURL, CallbackBase: srv.URL + "/oauth/mcp",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := eA.complete.Execute(ctx, usecase.CompleteAuthInput{Callback: authorize(t, out.AuthURL)}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	key := domain.StoreKey("u1", "a1", "quandora")
	expired := &domain.OAuthTokens{
		AccessToken: "stale", RefreshToken: "rt-1", ExpiresAt: time.Now().UTC().Add(-time.Minute),
	}
	if err := eA.tokens.Save(ctx, key, expired); err != nil {
		t.Fatalf("expire: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	toks := make(chan string, 2)
	for _, e := range []*env{eA, eB} {
		wg.Add(1)
		go func(ev *env) {
			defer wg.Done()
			tok, err := ev.provider.AccessToken(ctx, refreshInput(ev))
			if err != nil {
				errs <- err
				return
			}
			toks <- tok
		}(e)
	}
	wg.Wait()
	close(errs)
	close(toks)
	for err := range errs {
		t.Fatalf("concurrent access failed: %v", err)
	}
	for tok := range toks {
		if tok != "at-2" {
			t.Fatalf("access token = %q, want at-2", tok)
		}
	}
	m.mu.Lock()
	calls := m.refreshCalls
	m.mu.Unlock()
	if calls != 1 {
		t.Fatalf("provider refresh calls = %d, want exactly 1 (distributed lock)", calls)
	}
}

// ---------- Postgres real-DB e2e (env-gated) ----------
//
// The multi-instance tests above use one shared SQLite file — the same
// SQL adapters run against Postgres in production, but SQLite cannot
// prove the real server dialect. This test runs the identical
// "start on A → complete on B → both read" scenario against a real
// Postgres when explicitly enabled:
//
//	FASTAGENT_E2E_PG=1 go test ./internal/mcp/oauth/e2e/ -run Test8PostgresMultiInstanceSharedStorage
//
// The DSN is read from FASTAGENT_E2E_PG_DSN only — no implicit
// .env.development / DATABASE_URL fallback, so the test can never
// accidentally touch a development or shared database it wasn't pointed
// at. Without the opt-in flag and DSN the test skips.
const (
	pgE2EEnableEnv = "FASTAGENT_E2E_PG"
	pgE2EDSNEnv    = "FASTAGENT_E2E_PG_DSN"
)

func Test8PostgresMultiInstanceSharedStorage(t *testing.T) {
	dsn, why := resolveE2EPGDSN()
	if dsn == "" {
		t.Skip(why)
	}

	_, srv := newMockProvider(t)
	defer srv.Close()

	suffix := fmt.Sprintf("pg_%d", time.Now().UnixNano())
	userID := "u_" + suffix
	agentID := "a_" + suffix
	serverName := "q_" + suffix

	openPG := func() *store.DBStore {
		t.Helper()
		st, err := store.New(&store.StorageConfig{Type: "postgres", DSN: dsn, AutoMigrate: true}, t.TempDir())
		if err != nil {
			t.Fatalf("open postgres store: %v", err)
		}
		db, ok := st.(*store.DBStore)
		if !ok {
			t.Fatalf("store is %T, want *store.DBStore", st)
		}
		return db
	}
	dbA := openPG()
	dbB := openPG()
	t.Cleanup(func() {
		cleanupPGRows(t, []*store.DBStore{dbA, dbB}, userID, agentID, serverName)
		dbA.Close()
		dbB.Close()
	})
	if dbA.Dialect() != "postgres" || dbB.Dialect() != "postgres" {
		t.Fatalf("dialects = %q/%q, want postgres", dbA.Dialect(), dbB.Dialect())
	}

	eA := newEnvDB(t, srv, dbA, nil)
	eB := newEnvDB(t, srv, dbB, nil)
	ctx := context.Background()

	out, err := eA.start.Execute(ctx, usecase.StartAuthInput{
		UserID: userID, AgentID: agentID, ServerName: serverName,
		ServerURL: eA.serverURL, CallbackBase: srv.URL + "/oauth/mcp",
	})
	if err != nil {
		t.Fatalf("start on A (postgres): %v", err)
	}
	params := authorize(t, out.AuthURL)

	// Callback lands on B: pending + registration must be shared across
	// the real Postgres server, not just a shared SQLite file.
	if _, err := eB.complete.Execute(ctx, usecase.CompleteAuthInput{Callback: params}); err != nil {
		t.Fatalf("complete on B (postgres): %v", err)
	}
	for name, e := range map[string]*env{"B": eB, "A": eA} {
		tok, err := e.provider.AccessToken(ctx, usecase.RefreshInput{
			UserID: userID, AgentID: agentID, ServerName: serverName, ServerURL: e.serverURL,
		})
		if err != nil || tok != "at-1" {
			t.Fatalf("access token on %s = %q, err %v; want at-1", name, tok, err)
		}
	}
	// State stays single-use across the real server too.
	if _, err := eB.complete.Execute(ctx, usecase.CompleteAuthInput{Callback: params}); err == nil {
		t.Fatal("replayed callback must fail on postgres")
	}
}

// resolveE2EPGDSN returns the opt-in Postgres DSN, or "" plus a reason
// when the test should skip.
func resolveE2EPGDSN() (dsn, why string) {
	if os.Getenv(pgE2EEnableEnv) != "1" {
		return "", "set FASTAGENT_E2E_PG=1 to run the Postgres multi-instance e2e"
	}
	if v := os.Getenv(pgE2EDSNEnv); v != "" {
		return v, ""
	}
	return "", "FASTAGENT_E2E_PG=1 but FASTAGENT_E2E_PG_DSN is not set"
}

func cleanupPGRows(t *testing.T, dbs []*store.DBStore, userID, agentID, serverName string) {
	t.Helper()
	ctx := context.Background()
	key := domain.StoreKey(userID, agentID, serverName)
	for _, db := range dbs {
		_, _ = db.DB().ExecContext(ctx, "DELETE FROM mcp_oauth_clients WHERE server_name = $1", serverName)
		_, _ = db.DB().ExecContext(ctx, "DELETE FROM mcp_oauth_pending WHERE server_name = $1 OR user_id = $2", serverName, userID)
		_, _ = db.DB().ExecContext(ctx, "DELETE FROM mcp_oauth_tokens WHERE token_key = $1", key)
	}
}
