package store

import (
	"context"
	"errors"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/config"
)

func TestAgentMCPServersCRUDAndPreconditions(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	agentID := "agt_mcp_store"

	// Empty list for an agent with no declarations.
	got, err := db.ListMCPServers(ctx, agentID)
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("fresh list = %+v; want empty", got)
	}

	cfg := config.MCPServerConfig{
		Type: "http", URL: "https://mcp.quandora.ai/quant",
		OAuthResource: "https://mcp.quandora.ai/quant",
		Scopes:        []string{"factor_mining:status", "strategy:runs.read"},
		Headers:       map[string]string{"X-Test": "1"},
	}
	if err := db.AddMCPServer(ctx, agentID, "quandora", cfg); err != nil {
		t.Fatalf("add quandora: %v", err)
	}
	// add precondition: duplicate name must fail with no state change.
	if err := db.AddMCPServer(ctx, agentID, "quandora", cfg); !errors.Is(err, ErrMCPServerExists) {
		t.Fatalf("duplicate add err = %v; want ErrMCPServerExists", err)
	}

	// Different servers are independent rows (key-level commutation).
	if err := db.AddMCPServer(ctx, agentID, "plain", config.MCPServerConfig{
		Type: "http", URL: "https://plain.example/mcp",
	}); err != nil {
		t.Fatalf("add plain: %v", err)
	}

	got, err = db.ListMCPServers(ctx, agentID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("list len = %d; want 2 (%+v)", len(got), got)
	}
	q := got["quandora"]
	if q.Type != "http" || q.URL != cfg.URL || q.OAuthResource != cfg.OAuthResource {
		t.Fatalf("quandora roundtrip = %+v", q)
	}
	if len(q.Scopes) != 2 || q.Scopes[0] != "factor_mining:status" || q.Headers["X-Test"] != "1" {
		t.Fatalf("quandora scopes/headers roundtrip = %+v", q)
	}

	// remove precondition: deleting an absent server must fail.
	if err := db.DeleteMCPServer(ctx, agentID, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing err = %v; want ErrNotFound", err)
	}
	if err := db.DeleteMCPServer(ctx, agentID, "quandora"); err != nil {
		t.Fatalf("delete quandora: %v", err)
	}
	got, err = db.ListMCPServers(ctx, agentID)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if _, exists := got["quandora"]; exists {
		t.Fatal("quandora should be gone")
	}
	if _, exists := got["plain"]; !exists {
		t.Fatal("plain should survive")
	}
}

func TestAgentMCPServersReplaceWholeList(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	agentID := "agt_mcp_replace"
	add := func(name string) {
		t.Helper()
		if err := db.AddMCPServer(ctx, agentID, name, config.MCPServerConfig{
			Type: "http", URL: "https://" + name + ".example/mcp",
		}); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	add("a")
	add("b")

	// Whole-map replace: {a,b} -> {b,c}: b survives, a dropped, c added.
	if err := db.ReplaceMCPServers(ctx, agentID, map[string]config.MCPServerConfig{
		"b": {Type: "http", URL: "https://b.example/mcp"},
		"c": {Type: "http", URL: "https://c.example/mcp"},
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, err := db.ListMCPServers(ctx, agentID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("replace result len = %d; want 2 (%+v)", len(got), got)
	}
	if _, ok := got["a"]; ok {
		t.Fatalf("a survived whole-map replace: %+v", got)
	}
	if _, ok := got["b"]; !ok {
		t.Fatalf("b missing after replace: %+v", got)
	}
	if _, ok := got["c"]; !ok {
		t.Fatalf("c missing after replace: %+v", got)
	}

	// Empty map / nil clears every row (dashboard reset semantics).
	if err := db.ReplaceMCPServers(ctx, agentID, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, err = db.ListMCPServers(ctx, agentID)
	if err != nil {
		t.Fatalf("list after clear: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("after clear = %+v; want empty", got)
	}
}
