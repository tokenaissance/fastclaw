package agent

// Scheme-A e2e at the wiring seam: the bearer option built by
// mcpOAuthManagerOptions must carry the session actor into the token
// provider, and the provider must refuse a foreign actor BEFORE any HTTP
// request reaches the OAuth-protected MCP server. Owner sessions keep the
// full connect + tool-list flow.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/mcp"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/port"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/usecase"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// gateTokens is a minimal in-memory TokenStore for the wiring test.
type gateTokens struct{ t *domain.OAuthTokens }

func (g *gateTokens) Save(_ context.Context, _ string, t *domain.OAuthTokens) error {
	g.t = t
	return nil
}
func (g *gateTokens) Load(_ context.Context, _ string) (*domain.OAuthTokens, error) {
	if g.t == nil {
		return nil, port.ErrNotFound
	}
	return g.t, nil
}
func (g *gateTokens) Delete(_ context.Context, _ string) error {
	g.t = nil
	return nil
}

func TestMCPOAuthOwnerGateE2E(t *testing.T) {
	var serverCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverCalls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer at-owner" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(mcpRPCResponse(map[string]any{"tools": []map[string]any{
			{"name": "quant", "description": "query", "inputSchema": map[string]any{"type": "object"}},
		}}))
	}))
	defer srv.Close()

	rc := config.ResolvedAgent{
		ID:     "agent-1",
		UserID: "owner-1",
		MCPServers: map[string]config.MCPServerConfig{
			"quandora": {Type: "http", URL: srv.URL, OAuthResource: srv.URL},
		},
	}
	tokens := &gateTokens{t: &domain.OAuthTokens{
		AccessToken:  "at-owner",
		RefreshToken: "rt-owner",
		ExpiresAt:    time.Now().UTC().Add(time.Hour),
	}}
	ob := &oauth.Bootstrap{Provider: &usecase.TokenProvider{Tokens: tokens}}

	// Visitor session: agent attached into a foreign UserSpace. The gate
	// must refuse before any HTTP request — no unauthenticated probe, no
	// owner-token fetch, no tool list.
	visitorMgr := mcp.NewManager(rc.MCPServers, mcpOAuthManagerOptions(ob, rc, "visitor-1")...)
	if visitorMgr.HasTools() {
		t.Fatal("visitor session must not receive OAuth-protected MCP tools")
	}
	if got := serverCalls.Load(); got != 0 {
		t.Fatalf("visitor session reached the MCP server %d time(s); want 0 (gate must fire before HTTP)", got)
	}

	// Owner session: full connect succeeds and tools register.
	ownerMgr := mcp.NewManager(rc.MCPServers, mcpOAuthManagerOptions(ob, rc, "owner-1")...)
	if !ownerMgr.HasTools() {
		t.Fatal("owner session should receive OAuth-protected MCP tools")
	}
	defs := ownerMgr.ToolDefs()
	if len(defs) != 1 || defs[0].Name != "mcp_quandora_quant" {
		t.Fatalf("owner tool defs = %+v, want mcp_quandora_quant", defs)
	}
	if got := serverCalls.Load(); got < 2 {
		t.Fatalf("owner session made %d server call(s); want >=2 (initialize + tools/list)", got)
	}

	// Static-header servers are untouched: no option, same behavior as
	// before the gate (no OAuth gate, no bearer injection).
	plainSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			http.Error(w, "unexpected auth", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(mcpRPCResponse(map[string]any{"tools": []map[string]any{
			{"name": "echo", "description": "echo", "inputSchema": map[string]any{"type": "object"}},
		}}))
	}))
	defer plainSrv.Close()
	plainMgr := mcp.NewManager(map[string]config.MCPServerConfig{
		"plain": {Type: "http", URL: plainSrv.URL, Headers: map[string]string{"X-Static": "v"}},
	})
	if !plainMgr.HasTools() {
		t.Fatal("static-header server with no OAuth resource must still connect")
	}
}

// TestManagerStampsSessionActorOnAgent pins the framework-layer contract:
// the Manager's UserSpace user (owner or visitor) is what lands in
// mcpActorUserID — rc.UserID keeps the agent owner. The pair is what the
// scheme-A gate compares, so an agent attached into a foreign UserSpace
// carries the visitor as actor while the owner still owns the credential.
func TestManagerStampsSessionActorOnAgent(t *testing.T) {
	rc := func(home string) config.ResolvedAgent {
		return config.ResolvedAgent{
			ID: "agent-1", UserID: "owner-1",
			Home: home, Workspace: home + "/ws",
		}
	}

	ownerM, err := NewManager([]config.ResolvedAgent{rc(t.TempDir())}, nil, bus.New(), WithUserID("owner-1"))
	if err != nil {
		t.Fatalf("owner NewManager: %v", err)
	}
	if ag := ownerM.AgentByID("agent-1"); ag == nil || ag.mcpActorUserID != "owner-1" {
		t.Fatalf("owner manager actor = %+v, want owner-1", ag)
	}

	visitorM, err := NewManager([]config.ResolvedAgent{rc(t.TempDir())}, nil, bus.New(), WithUserID("visitor-1"))
	if err != nil {
		t.Fatalf("visitor NewManager: %v", err)
	}
	if ag := visitorM.AgentByID("agent-1"); ag == nil || ag.mcpActorUserID != "visitor-1" {
		t.Fatalf("visitor manager actor = %+v, want visitor-1", ag)
	}
}

// TestMCPOAuthOwnerGateRealStoreE2E drives the whole chain a deployed
// daemon would: the process singleton bootstrap (real AES-GCM file store),
// the scheme-A gate in the token provider, and the real MCP manager wiring.
// An authorized owner session can connect and call tools; a visitor session
// attached to the same agent is refused before a single HTTP request.
func TestMCPOAuthOwnerGateRealStoreE2E(t *testing.T) {
	secret := "test-secret-for-scheme-a-e2e"

	// Boot the process singleton exactly like cmd/fastclaw does — with a
	// DB (sqlite here, postgres in multi-instance production). Deployed
	// single instances also store via DB, never the file fallback.
	dsn := filepath.Join(t.TempDir(), "oauth.db")
	st, err := store.New(&store.StorageConfig{Type: "sqlite", DSN: dsn, AutoMigrate: true}, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	db, ok := st.(*store.DBStore)
	if !ok {
		t.Fatalf("store is %T, want *store.DBStore", st)
	}
	defer db.Close()
	if _, err := oauth.Init(secret, oauth.Options{DB: db}); err != nil {
		t.Fatalf("oauth.Init: %v", err)
	}
	ob := oauth.Global()
	if ob == nil {
		t.Fatal("oauth.Global() is nil after Init")
	}

	// Simulate a completed authorization: persist the encrypted owner
	// token through the real DB store the bootstrap assembled.
	key := domain.StoreKey("owner-1", "agent-1", "quandora")
	if err := ob.Tokens.Save(t.Context(), key, &domain.OAuthTokens{
		AccessToken:  "at-owner",
		RefreshToken: "rt-owner",
		ExpiresAt:    time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("seed encrypted token: %v", err)
	}

	var serverCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverCalls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer at-owner" {
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
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{},
			}))
		case "tools/list":
			json.NewEncoder(w).Encode(mcpRPCResponse(map[string]any{"tools": []map[string]any{
				{"name": "quant", "description": "query", "inputSchema": map[string]any{"type": "object"}},
			}}))
		case "tools/call":
			json.NewEncoder(w).Encode(mcpRPCResponse(map[string]any{
				"content": []map[string]any{{"type": "text", "text": "quandora-ok"}},
			}))
		default:
			json.NewEncoder(w).Encode(mcpRPCResponse(map[string]any{}))
		}
	}))
	defer srv.Close()

	rc := config.ResolvedAgent{
		ID:     "agent-1",
		UserID: "owner-1",
		MCPServers: map[string]config.MCPServerConfig{
			"quandora": {Type: "http", URL: srv.URL, OAuthResource: srv.URL},
		},
	}

	// Visitor (foreign UserSpace attach): refused before HTTP.
	visitorMgr := mcp.NewManager(rc.MCPServers, mcpOAuthManagerOptions(ob, rc, "visitor-1")...)
	if visitorMgr.HasTools() {
		t.Fatal("visitor session must not receive OAuth-protected MCP tools")
	}
	if got := serverCalls.Load(); got != 0 {
		t.Fatalf("visitor reached the MCP server %d time(s); want 0", got)
	}

	// Owner: real store lookup succeeds, connect + tools/list + tools/call
	// all carry the owner bearer token.
	ownerMgr := mcp.NewManager(rc.MCPServers, mcpOAuthManagerOptions(ob, rc, "owner-1")...)
	if !ownerMgr.HasTools() {
		t.Fatal("owner session should receive OAuth-protected MCP tools")
	}
	out, err := ownerMgr.CallTool(context.Background(), "mcp_quandora_quant", json.RawMessage(`{"query":"x"}`))
	if err != nil {
		t.Fatalf("owner tool call: %v", err)
	}
	if out != "quandora-ok" {
		t.Fatalf("owner tool result = %q, want quandora-ok", out)
	}
	if got := serverCalls.Load(); got < 3 {
		t.Fatalf("owner made %d server calls; want >=3 (initialize + tools/list + tools/call)", got)
	}

	// Agent tool surface (mcpToolFn) rides the same real store + gate:
	// refresh on the fresh seeded token is a no-op (no server calls),
	// check performs a real initialize + tools/list with the owner bearer,
	// and a visitor is refused for both before any HTTP request.
	ownerFn := mcpToolFn(ob, rc, "owner-1")
	beforeRefresh := serverCalls.Load()
	rawRefresh, _ := json.Marshal(map[string]string{"action": "refresh", "serverName": "quandora"})
	out, err = ownerFn(t.Context(), rawRefresh)
	if err != nil {
		t.Fatalf("owner mcp refresh: %v", err)
	}
	if !strings.Contains(out, "token refreshed") {
		t.Fatalf("owner mcp refresh output = %q, want token refreshed", out)
	}
	if got := serverCalls.Load(); got != beforeRefresh {
		t.Fatalf("mcp refresh on fresh token made %d server call(s); want 0", got-beforeRefresh)
	}

	beforeCheck := serverCalls.Load()
	rawCheck, _ := json.Marshal(map[string]string{"action": "check", "serverName": "quandora"})
	out, err = ownerFn(t.Context(), rawCheck)
	if err != nil {
		t.Fatalf("owner mcp check: %v", err)
	}
	if !strings.Contains(out, "quant") || !strings.Contains(out, "connected and authorized") {
		t.Fatalf("owner mcp check output = %q, want connected + quant", out)
	}
	if got := serverCalls.Load(); got < beforeCheck+2 {
		t.Fatalf("mcp check made %d server call(s); want >=2 (initialize + tools/list)", got-beforeCheck)
	}

	afterCheck := serverCalls.Load()
	visitorFn := mcpToolFn(ob, rc, "visitor-1")
	for _, action := range []string{"check", "refresh"} {
		raw, _ := json.Marshal(map[string]string{"action": action, "serverName": "quandora"})
		if _, err := visitorFn(t.Context(), raw); err == nil || !strings.Contains(err.Error(), "only the agent owner") {
			t.Fatalf("visitor mcp %s error = %v, want owner-only", action, err)
		}
	}
	if got := serverCalls.Load(); got != afterCheck {
		t.Fatalf("visitor mcp actions reached the MCP server %d time(s); want 0", got-afterCheck)
	}
}

// TestMcpToolCheckRealStoreRotatesE2E drives the full daemon seam with a
// stale stored credential: discovery + refresh at the real provider over
// HTTP, rotated token persisted through the real encrypted SQLite store,
// then check's initialize + tools/list succeed with the new bearer. After
// rotation a refresh action is a no-op, and a visitor never reaches either
// the token endpoint or the MCP server.
func TestMcpToolCheckRealStoreRotatesE2E(t *testing.T) {
	secret := "test-secret-stale-rotation-e2e"
	dsn := filepath.Join(t.TempDir(), "oauth-rot.db")
	st, err := store.New(&store.StorageConfig{Type: "sqlite", DSN: dsn, AutoMigrate: true}, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	db, ok := st.(*store.DBStore)
	if !ok {
		t.Fatalf("store is %T, want *store.DBStore", st)
	}
	defer db.Close()
	if _, err := oauth.Init(secret, oauth.Options{DB: db}); err != nil {
		t.Fatalf("oauth.Init: %v", err)
	}
	ob := oauth.Global()
	if ob == nil {
		t.Fatal("oauth.Global() is nil after Init")
	}

	// Seed the registration row the refresh use case resolves, plus a
	// STALE credential (refresh required before any request can carry a
	// valid bearer).
	if err := ob.Regs.Save(t.Context(), "quandora", "https://app.example/oauth/mcp/cb",
		&domain.ClientRegistration{ClientID: "cid-rot", RedirectURIs: []string{"https://app.example/oauth/mcp/cb"}}); err != nil {
		t.Fatalf("seed registration: %v", err)
	}
	key := domain.StoreKey("owner-1", "agent-1", "quandora")
	if err := ob.Tokens.Save(t.Context(), key, &domain.OAuthTokens{
		AccessToken:  "at-old",
		RefreshToken: "rt-old",
		ExpiresAt:    time.Now().UTC().Add(-time.Hour), // stale
		Scopes:       []string{"factor_mining:*"},
	}); err != nil {
		t.Fatalf("seed stale token: %v", err)
	}

	var tokenCalls, mcpCalls atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/.well-known/oauth-authorization-server":
			json.NewEncoder(w).Encode(map[string]any{
				"issuer":                 srv.URL,
				"authorization_endpoint": srv.URL + "/oauth/authorize",
				"token_endpoint":         srv.URL + "/oauth/token",
			})
		case r.URL.Path == "/oauth/token":
			tokenCalls.Add(1)
			if err := r.ParseForm(); err != nil {
				http.Error(w, "bad form", http.StatusBadRequest)
				return
			}
			if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "rt-old" {
				http.Error(w, "unexpected grant", http.StatusBadRequest)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "at-new",
				"refresh_token": "rt-new",
				"expires_in":    7200,
				"scope":         "factor_mining:*",
			})
		default: // MCP endpoint
			mcpCalls.Add(1)
			if got := r.Header.Get("Authorization"); got != "Bearer at-new" {
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
		}
	}))
	defer srv.Close()

	rc := config.ResolvedAgent{
		ID:     "agent-1",
		UserID: "owner-1",
		MCPServers: map[string]config.MCPServerConfig{
			"quandora": {Type: "http", URL: srv.URL + "/mcp", OAuthResource: srv.URL + "/quant"},
		},
	}
	ownerFn := mcpToolFn(ob, rc, "owner-1")

	// Owner check with a stale token: discovery + exactly one refresh
	// rotation, then initialize + tools/list with the rotated bearer.
	rawCheck, _ := json.Marshal(map[string]string{"action": "check", "serverName": "quandora"})
	out, err := ownerFn(t.Context(), rawCheck)
	if err != nil {
		t.Fatalf("owner check with stale token: %v", err)
	}
	if !strings.Contains(out, "connected and authorized") || !strings.Contains(out, "quant") {
		t.Fatalf("owner check output = %q, want connected + quant", out)
	}
	if got := tokenCalls.Load(); got != 1 {
		t.Fatalf("token endpoint calls = %d, want exactly 1 rotation", got)
	}
	if got := mcpCalls.Load(); got < 2 {
		t.Fatalf("mcp calls = %d, want >=2 (initialize + tools/list)", got)
	}
	stored, err := ob.Tokens.Load(t.Context(), key)
	if err != nil || stored.AccessToken != "at-new" {
		t.Fatalf("stored token after check = %+v (err %v), want rotated at-new", stored, err)
	}

	// The same owner's refresh action on the now-fresh token is a no-op:
	// no second token-endpoint call.
	rawRefresh, _ := json.Marshal(map[string]string{"action": "refresh", "serverName": "quandora"})
	if out, err = ownerFn(t.Context(), rawRefresh); err != nil {
		t.Fatalf("owner refresh after rotation: %v", err)
	}
	if !strings.Contains(out, "token refreshed") {
		t.Fatalf("owner refresh output = %q, want token refreshed", out)
	}
	if got := tokenCalls.Load(); got != 1 {
		t.Fatalf("token endpoint calls after no-op refresh = %d, want still 1", got)
	}

	// Visitor never reaches either endpoint.
	after := tokenCalls.Load() + mcpCalls.Load()
	visitorFn := mcpToolFn(ob, rc, "visitor-1")
	for _, action := range []string{"check", "refresh"} {
		raw, _ := json.Marshal(map[string]string{"action": action, "serverName": "quandora"})
		if _, err := visitorFn(t.Context(), raw); err == nil || !strings.Contains(err.Error(), "only the agent owner") {
			t.Fatalf("visitor mcp %s error = %v, want owner-only", action, err)
		}
	}
	if got := tokenCalls.Load() + mcpCalls.Load(); got != after {
		t.Fatalf("visitor actions reached provider %d time(s); want 0", got-after)
	}
}

// mcpRPCResponse mirrors the internal JSON-RPC envelope shape expected by
// the MCP HTTP client (same fields, test-local so we don't reach into mcp
// internals).
func mcpRPCResponse(result map[string]any) struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
} {
	raw, _ := json.Marshal(result)
	return struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Result  json.RawMessage `json:"result"`
	}{JSONRPC: "2.0", ID: 1, Result: raw}
}
