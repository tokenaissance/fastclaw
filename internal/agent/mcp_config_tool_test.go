package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// TestMcpToolAddRemovePersistsE2E drives `mcp add` / `mcp remove` through
// the real sqlite store: add persists the server into the agent config
// blob and fires the reload notify; conflict/invalid inputs are refused;
// remove is the exact inverse and clears the row.
func TestMcpToolAddRemovePersistsE2E(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "agent-mcp.db")
	st, err := store.New(&store.StorageConfig{Type: "sqlite", DSN: dsn, AutoMigrate: true}, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	db, ok := st.(*store.DBStore)
	if !ok {
		t.Fatalf("store is %T, want *store.DBStore", st)
	}
	defer db.Close()

	now := time.Now().UTC()
	rc := config.ResolvedAgent{ID: "agent-1", UserID: "owner-1"}
	if err := db.SaveAgent(t.Context(), &store.AgentRecord{
		ID: rc.ID, UserID: rc.UserID, Name: "test",
		Config: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	var notifyCalls int
	var notifiedUser, notifiedAgent string
	ag := &Agent{
		dataStore: db,
		mcpConfigNotify: func(userID, agentID string) {
			notifyCalls++
			notifiedUser, notifiedAgent = userID, agentID
		},
	}
	fn := mcpToolFnWithAgent(&oauth.Bootstrap{}, rc, "owner-1", ag)

	// add: static server
	out, err := callToolJSON(t, fn, map[string]any{
		"action": "add", "serverName": "plain", "url": "https://plain.example/mcp",
	})
	if err != nil {
		t.Fatalf("add static: %v", err)
	}
	if !strings.Contains(out, "registered") {
		t.Fatalf("add output = %q, want registered", out)
	}
	if notifyCalls != 1 || notifiedUser != "owner-1" || notifiedAgent != "agent-1" {
		t.Fatalf("notify = (%d,%q,%q), want (1,owner-1,agent-1)", notifyCalls, notifiedUser, notifiedAgent)
	}

	// add: OAuth server with scopes
	if _, err := callToolJSON(t, fn, map[string]any{
		"action": "add", "serverName": "quandora",
		"url":           "https://mcp.quandora.ai/quant",
		"oauthResource": "https://mcp.quandora.ai/quant",
		"scopes":        []string{"factor_mining:status"},
	}); err != nil {
		t.Fatalf("add oauth: %v", err)
	}

	servers, err := db.ListMCPServers(context.Background(), rc.ID)
	if err != nil {
		t.Fatalf("load servers: %v", err)
	}
	if got := servers["plain"].URL; got != "https://plain.example/mcp" {
		t.Fatalf("plain url = %q", got)
	}
	q := servers["quandora"]
	if q.OAuthResource != "https://mcp.quandora.ai/quant" || len(q.Scopes) != 1 || q.Scopes[0] != "factor_mining:status" {
		t.Fatalf("quandora stored = %+v", q)
	}

	// Refuse: duplicate, bad scheme, scopes without oauthResource.
	if _, err := callToolJSON(t, fn, map[string]any{
		"action": "add", "serverName": "plain", "url": "https://other.example/mcp",
	}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate add error = %v, want already exists", err)
	}
	if _, err := callToolJSON(t, fn, map[string]any{
		"action": "add", "serverName": "ftp", "url": "ftp://x/mcp",
	}); err == nil || !strings.Contains(err.Error(), "http(s)") {
		t.Fatalf("bad scheme error = %v, want http(s)", err)
	}
	if _, err := callToolJSON(t, fn, map[string]any{
		"action": "add", "serverName": "x", "url": "https://x/mcp",
		"scopes": []string{"a:b"},
	}); err == nil || !strings.Contains(err.Error(), "scopes require oauthResource") {
		t.Fatalf("scopes w/o oauth error = %v", err)
	}

	// remove is the inverse of add.
	if _, err := callToolJSON(t, fn, map[string]any{
		"action": "remove", "serverName": "quandora",
	}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	servers, err = db.ListMCPServers(context.Background(), rc.ID)
	if err != nil {
		t.Fatalf("reload after remove: %v", err)
	}
	if _, exists := servers["quandora"]; exists {
		t.Fatal("quandora should be removed")
	}
	if _, exists := servers["plain"]; !exists {
		t.Fatal("plain should still exist")
	}
	if notifyCalls != 3 {
		t.Fatalf("notify calls = %d, want 3 (2 add + 1 remove)", notifyCalls)
	}
	if _, err := callToolJSON(t, fn, map[string]any{
		"action": "remove", "serverName": "missing",
	}); err == nil || !strings.Contains(err.Error(), "not a configured") {
		t.Fatalf("remove missing error = %v", err)
	}

	// Removing the last server deletes the key entirely.
	if _, err := callToolJSON(t, fn, map[string]any{
		"action": "remove", "serverName": "plain",
	}); err != nil {
		t.Fatalf("remove last: %v", err)
	}
	servers, err = db.ListMCPServers(context.Background(), rc.ID)
	if err != nil {
		t.Fatalf("reload agent: %v", err)
	}
	if len(servers) != 0 {
		t.Fatalf("agent_mcp_servers should be empty after removing the last server, got %+v", servers)
	}
}

func callToolJSON(t *testing.T, fn func(context.Context, json.RawMessage) (string, error), in map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(in)
	return fn(context.Background(), raw)
}
