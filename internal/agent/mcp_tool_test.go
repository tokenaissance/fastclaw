package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/port"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/usecase"
)

type toolMeta struct{}

func (toolMeta) Fetch(_ context.Context, _ string) (*domain.DiscoveryMetadata, error) {
	return &domain.DiscoveryMetadata{
		Issuer:                "https://as.example",
		AuthorizationEndpoint: "https://as.example/oauth/authorize",
		TokenEndpoint:         "https://as.example/oauth/token",
		RegistrationEndpoint:  "https://as.example/oauth/register",
		RevocationEndpoint:    "https://as.example/oauth/revoke",
		ScopesSupported:       []string{"factor_mining:*"},
	}, nil
}

type toolRegistrar struct{}

func (toolRegistrar) Register(_ context.Context, _ string, redirectURIs []string) (*domain.ClientRegistration, error) {
	return &domain.ClientRegistration{ClientID: "cid-1", RedirectURIs: redirectURIs, TokenEndpointAuthMethod: "none"}, nil
}

type toolRegStore struct{}

func (toolRegStore) Get(_ context.Context, _, _ string) (*domain.ClientRegistration, error) {
	return nil, port.ErrNotFound
}
func (toolRegStore) GetAny(_ context.Context, _ string) (*domain.ClientRegistration, error) {
	return nil, port.ErrNotFound
}
func (toolRegStore) Save(_ context.Context, _, _ string, _ *domain.ClientRegistration) error {
	return nil
}

type toolPending struct{}

func (toolPending) Save(_ context.Context, _ *domain.PendingAuth) error { return nil }
func (toolPending) Take(_ context.Context, _ string) (*domain.PendingAuth, error) {
	return nil, port.ErrNotFound
}
func (toolPending) CountActive(_ context.Context, _ string) (int, error) { return 0, nil }

type toolTokens struct{}

func (toolTokens) Save(_ context.Context, _ string, _ *domain.OAuthTokens) error { return nil }
func (toolTokens) Load(_ context.Context, _ string) (*domain.OAuthTokens, error) {
	return nil, port.ErrNotFound
}
func (toolTokens) Delete(_ context.Context, _ string) error { return nil }

func testToolBootstrap() *oauth.Bootstrap {
	start := &usecase.StartAuthorization{
		Meta: toolMeta{}, Registrar: toolRegistrar{},
		Regs: toolRegStore{}, Pending: toolPending{}, MaxPendingPerUser: 5,
	}
	return &oauth.Bootstrap{
		Start:  start,
		Status: &usecase.Status{Tokens: toolTokens{}},
	}
}

func testToolRC() config.ResolvedAgent {
	return config.ResolvedAgent{
		ID:     "agent-1",
		UserID: "owner-1",
		MCPServers: map[string]config.MCPServerConfig{
			"quandora": {
				Type: "http", URL: "https://mcp.quandora.ai/quant",
				OAuthResource: "https://mcp.quandora.ai/quant",
				Scopes:        []string{"factor_mining:*"},
			},
			"plain": {Type: "http", URL: "https://plain.example/mcp"},
		},
	}
}

func callTool(t *testing.T, fn func(context.Context, json.RawMessage) (string, error), action, server string) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"action": action, "serverName": server})
	return fn(context.Background(), raw)
}

func TestMcpToolOwnerGate(t *testing.T) {
	ob := testToolBootstrap()
	rc := testToolRC()
	fn := mcpToolFn(ob, rc, "visitor-1")

	for _, action := range []string{"login", "status", "check", "refresh", "logout", "add", "remove", "undo"} {
		if _, err := callTool(t, fn, action, "quandora"); err == nil || !strings.Contains(err.Error(), "only the agent owner") {
			t.Fatalf("%s visitor error = %v, want owner-only", action, err)
		}
	}
}

// TestMcpToolsNilDependenciesFailCleanly guards the whole mcp action
// surface against nil bootstrap / nil agent handlers: every action must
// return a typed error, never panic, even when invoked with degraded
// wiring (mcpToolFn passes a nil *Agent).
func TestMcpToolsNilDependenciesFailCleanly(t *testing.T) {
	rc := testToolRC()
	fn := mcpToolFn(nil, rc, "owner-1") // nil OAuth bootstrap + nil agent
	for _, action := range []string{"login", "status", "check", "refresh", "logout", "add", "remove", "undo"} {
		if _, err := callTool(t, fn, action, "quandora"); err == nil {
			t.Fatalf("%s with nil dependencies must error, got success", action)
		}
	}
}

func TestMcpToolLogin(t *testing.T) {
	rc := testToolRC()
	fn := mcpToolFn(testToolBootstrap(), rc, "owner-1")

	// Missing host callback base → clear guidance error, no URL.
	t.Setenv("FASTAGENT_OAUTH_CALLBACK_BASE", "")
	if _, err := callTool(t, fn, "login", "quandora"); err == nil || !strings.Contains(err.Error(), "FASTAGENT_OAUTH_CALLBACK_BASE") {
		t.Fatalf("login without base error = %v, want callback-base hint", err)
	}

	// With host base → returns authorization URL carrying the callback.
	t.Setenv("FASTAGENT_OAUTH_CALLBACK_BASE", "https://app.example.com/oauth/mcp")
	out, err := callTool(t, fn, "login", "quandora")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	authLine := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "https://") {
			authLine = line
			break
		}
	}
	if authLine == "" {
		t.Fatalf("no auth URL returned: %s", out)
	}
	u, err := url.Parse(authLine)
	if err != nil {
		t.Fatalf("parse auth URL: %v", err)
	}
	if got := u.Query().Get("redirect_uri"); !strings.HasPrefix(got, "https://app.example.com/oauth/mcp/") {
		t.Fatalf("redirect_uri should carry host callback base, got %q", got)
	}
	if !strings.HasPrefix(u.String(), "https://as.example/oauth/authorize") {
		t.Fatalf("auth URL should point at provider authorize endpoint, got: %s", out)
	}

	// Static server (no oauthResource) cannot be logged in.
	if _, err := callTool(t, fn, "login", "plain"); err == nil {
		t.Fatal("login on static server must fail")
	}
}

func TestMcpToolStatusAndLogout(t *testing.T) {
	rc := testToolRC()
	owner := "owner-1"
	fn := mcpToolFn(testToolBootstrap(), rc, owner)

	out, err := callTool(t, fn, "status", "")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "quandora: none") || strings.Contains(out, "plain") {
		t.Fatalf("status should list only OAuth servers, got: %s", out)
	}

	// logout requires serverName; visitor already covered by the gate test.
	if _, err := callTool(t, fn, "logout", ""); err == nil {
		t.Fatal("logout without serverName must fail")
	}
	if _, err := callTool(t, fn, "logout", "plain"); err == nil {
		t.Fatal("logout on static server must fail")
	}
}

// mcpMockMCPServer serves a minimal OAuth-protected MCP endpoint: every
// request must carry the expected bearer token, initialize returns the
// protocol handshake, and tools/list returns one tool.
func mcpMockMCPServer(t *testing.T, calls *atomic.Int32, wantToken string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer "+wantToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			json.NewEncoder(w).Encode(mcpRPCResponse(map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{},
			}))
		default:
			json.NewEncoder(w).Encode(mcpRPCResponse(map[string]any{"tools": []map[string]any{
				{"name": "quant", "description": "query", "inputSchema": map[string]any{"type": "object"}},
			}}))
		}
	}))
}

func TestMcpToolCheck(t *testing.T) {
	var calls atomic.Int32
	srv := mcpMockMCPServer(t, &calls, "at-owner")
	defer srv.Close()

	tokens := &gateTokens{t: &domain.OAuthTokens{
		AccessToken:  "at-owner",
		RefreshToken: "rt-owner",
		ExpiresAt:    time.Now().UTC().Add(time.Hour),
		Scopes:       []string{"factor_mining:*"},
	}}
	ob := &oauth.Bootstrap{
		Status:   &usecase.Status{Tokens: tokens},
		Provider: &usecase.TokenProvider{Tokens: tokens},
	}
	rc := config.ResolvedAgent{
		ID:     "agent-1",
		UserID: "owner-1",
		MCPServers: map[string]config.MCPServerConfig{
			"quandora": {Type: "http", URL: srv.URL, OAuthResource: srv.URL},
			"plain":    {Type: "http", URL: "https://plain.example/mcp"},
		},
	}
	fn := mcpToolFn(ob, rc, "owner-1")

	// Authorized owner: check performs initialize + tools/list with the
	// bearer token and reports the tool it found.
	out, err := callTool(t, fn, "check", "quandora")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !strings.Contains(out, "connected and authorized") || !strings.Contains(out, "quant") {
		t.Fatalf("check output = %s, want connected + tool name", out)
	}
	if got := calls.Load(); got < 2 {
		t.Fatalf("check made %d server calls; want >=2 (initialize + tools/list)", got)
	}

	// No stored credential: check refuses before any HTTP request.
	before := calls.Load()
	empty := &oauth.Bootstrap{
		Status:   &usecase.Status{Tokens: &gateTokens{}},
		Provider: &usecase.TokenProvider{Tokens: &gateTokens{}},
	}
	fnEmpty := mcpToolFn(empty, rc, "owner-1")
	if _, err := callTool(t, fnEmpty, "check", "quandora"); err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("check without token error = %v, want not-authorized guidance", err)
	}
	if got := calls.Load(); got != before {
		t.Fatalf("unauthenticated check reached the MCP server %d time(s); want 0", got-before)
	}

	// Static-header server is not an OAuth server: same refusal as login.
	if _, err := callTool(t, fn, "check", "plain"); err == nil || !strings.Contains(err.Error(), "not a configured OAuth MCP server") {
		t.Fatalf("check on static server error = %v, want not-oauth refusal", err)
	}
	if _, err := callTool(t, fn, "check", ""); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("check without serverName error = %v, want required", err)
	}
}

func TestMcpToolRefresh(t *testing.T) {
	tokens := &gateTokens{t: &domain.OAuthTokens{
		AccessToken:  "at-owner",
		RefreshToken: "rt-owner",
		ExpiresAt:    time.Now().UTC().Add(time.Hour),
		Scopes:       []string{"factor_mining:*"},
	}}
	ob := &oauth.Bootstrap{
		Status:  &usecase.Status{Tokens: tokens},
		Refresh: &usecase.RefreshToken{Tokens: tokens, Locks: usecase.NewRefreshLocks()},
	}
	rc := testToolRC()
	fn := mcpToolFn(ob, rc, "owner-1")

	// Fresh token: refresh is a no-op through the real use case (no
	// network, no rotation) and reports the current expiry.
	out, err := callTool(t, fn, "refresh", "quandora")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !strings.Contains(out, "token refreshed") || !strings.Contains(out, "(1 scopes)") {
		t.Fatalf("refresh output = %s, want token refreshed + scope count", out)
	}

	// No stored credential: clear guidance, no panic on nil exchange.
	empty := &gateTokens{}
	obEmpty := &oauth.Bootstrap{
		Status:  &usecase.Status{Tokens: empty},
		Refresh: &usecase.RefreshToken{Tokens: empty, Locks: usecase.NewRefreshLocks()},
	}
	fnEmpty := mcpToolFn(obEmpty, rc, "owner-1")
	if _, err := callTool(t, fnEmpty, "refresh", "quandora"); err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("refresh without token error = %v, want not-authorized guidance", err)
	}

	// serverName is required and static servers are refused.
	if _, err := callTool(t, fn, "refresh", ""); err == nil {
		t.Fatal("refresh without serverName must fail")
	}
	if _, err := callTool(t, fn, "refresh", "plain"); err == nil {
		t.Fatal("refresh on static server must fail")
	}
}

// rotateMeta/rotateRegs/rotateExchange are minimal fake ports for the
// RefreshToken use case: discovery is static, registrations exist, and
// Exchange.Refresh rotates the pair without network.
type rotateMeta struct{}

func (rotateMeta) Fetch(_ context.Context, _ string) (*domain.DiscoveryMetadata, error) {
	return &domain.DiscoveryMetadata{
		Issuer:        "https://as.example",
		TokenEndpoint: "https://as.example/oauth/token",
	}, nil
}

type rotateRegs struct{}

func (rotateRegs) Get(_ context.Context, _, _ string) (*domain.ClientRegistration, error) {
	return nil, port.ErrNotFound
}
func (rotateRegs) GetAny(_ context.Context, _ string) (*domain.ClientRegistration, error) {
	return &domain.ClientRegistration{ClientID: "cid-rot", RedirectURIs: []string{"https://as.example/cb"}}, nil
}
func (rotateRegs) Save(_ context.Context, _, _ string, _ *domain.ClientRegistration) error {
	return nil
}

type rotateExchange struct{ calls int }

func (x *rotateExchange) Refresh(_ context.Context, _ string, _ *domain.OAuthTokens, _ string, _ *domain.ClientRegistration) (*domain.OAuthTokens, error) {
	x.calls++
	return &domain.OAuthTokens{
		AccessToken:  "at-new",
		RefreshToken: "rt-new",
		ExpiresAt:    time.Now().UTC().Add(2 * time.Hour),
		Scopes:       []string{"factor_mining:*"},
	}, nil
}
func (rotateExchange) Exchange(context.Context, string, string, string, string, string, *domain.ClientRegistration) (*domain.OAuthTokens, error) {
	return nil, nil // unused in these tests
}
func (rotateExchange) Revoke(context.Context, string, string, string, string) error { return nil }

// staleTokenBootstrap wires a real RefreshToken use case over a gateTokens
// store seeded by the caller, with fake discovery/registration/exchange.
func staleTokenBootstrap(tokens *gateTokens) (*oauth.Bootstrap, *rotateExchange) {
	ex := &rotateExchange{}
	refresh := &usecase.RefreshToken{
		Meta: rotateMeta{}, Regs: rotateRegs{}, Tokens: tokens,
		Exchange: ex, Locks: usecase.NewRefreshLocks(),
	}
	return &oauth.Bootstrap{
		Status:   &usecase.Status{Tokens: tokens},
		Provider: &usecase.TokenProvider{Tokens: tokens, Refresh: refresh},
		Refresh:  refresh,
	}, ex
}

// TestMcpToolCheckRotatesStaleToken pins the check-vs-status distinction:
// a locally "expired" credential is not a dead end — check drives the same
// pre-expiry refresh the real MCP client would, then verifies the rotated
// token against the server (initialize + tools/list).
func TestMcpToolCheckRotatesStaleToken(t *testing.T) {
	var calls atomic.Int32
	srv := mcpMockMCPServer(t, &calls, "at-new") // server only accepts the rotated token
	defer srv.Close()

	tokens := &gateTokens{t: &domain.OAuthTokens{
		AccessToken:  "at-old",
		RefreshToken: "rt-old",
		ExpiresAt:    time.Now().UTC().Add(-time.Hour), // stale → refresh on access
	}}
	ob, ex := staleTokenBootstrap(tokens)
	rc := config.ResolvedAgent{
		ID:     "agent-1",
		UserID: "owner-1",
		MCPServers: map[string]config.MCPServerConfig{
			"quandora": {Type: "http", URL: srv.URL, OAuthResource: srv.URL},
		},
	}
	fn := mcpToolFn(ob, rc, "owner-1")

	out, err := callTool(t, fn, "check", "quandora")
	if err != nil {
		t.Fatalf("check with stale token: %v", err)
	}
	if !strings.Contains(out, "connected and authorized") || !strings.Contains(out, "quant") {
		t.Fatalf("check output = %s, want connected + quant", out)
	}
	if ex.calls != 1 {
		t.Fatalf("refresh exchange calls = %d, want 1", ex.calls)
	}
	if tokens.t == nil || tokens.t.AccessToken != "at-new" {
		t.Fatalf("stored access token after check = %+v, want rotated at-new", tokens.t)
	}
	if got := calls.Load(); got < 2 {
		t.Fatalf("check made %d server calls; want >=2 (initialize + tools/list)", got)
	}
}

// TestMcpToolRefreshRotatesStaleToken covers the refresh action's rotation
// path: stale store row → one exchange call → rotated pair persisted and
// the new expiry reported (token material never returned).
func TestMcpToolRefreshRotatesStaleToken(t *testing.T) {
	tokens := &gateTokens{t: &domain.OAuthTokens{
		AccessToken:  "at-old",
		RefreshToken: "rt-old",
		ExpiresAt:    time.Now().UTC().Add(-time.Hour),
	}}
	ob, ex := staleTokenBootstrap(tokens)
	rc := testToolRC()
	fn := mcpToolFn(ob, rc, "owner-1")

	out, err := callTool(t, fn, "refresh", "quandora")
	if err != nil {
		t.Fatalf("refresh with stale token: %v", err)
	}
	if !strings.Contains(out, "token refreshed") || !strings.Contains(out, "(1 scopes)") {
		t.Fatalf("refresh output = %s, want token refreshed + rotated scope count", out)
	}
	if ex.calls != 1 {
		t.Fatalf("refresh exchange calls = %d, want 1", ex.calls)
	}
	if tokens.t == nil || tokens.t.AccessToken != "at-new" {
		t.Fatalf("stored access token after refresh = %+v, want rotated at-new", tokens.t)
	}
	if strings.Contains(out, "at-new") || strings.Contains(out, "rt-new") {
		t.Fatalf("refresh output leaked token material: %s", out)
	}
}

// TestMcpToolSchemaActions guards the advertised action surface so the
// tool description, schema enum, and docs stay in sync.
func TestMcpToolSchemaActions(t *testing.T) {
	props, ok := mcpToolSchema["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema missing properties")
	}
	action, ok := props["action"].(map[string]any)
	if !ok {
		t.Fatal("schema missing action property")
	}
	enum, ok := action["enum"].([]string)
	if !ok {
		t.Fatal("action enum is not a []string")
	}
	want := []string{"login", "status", "check", "refresh", "logout", "add", "remove", "undo"}
	if !reflect.DeepEqual(enum, want) {
		t.Fatalf("action enum = %v, want %v", enum, want)
	}
}
