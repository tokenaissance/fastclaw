package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// TestStoreFirstAgentFileLoaderReadsMCPServersFromTable pins the layer-3
// overlay: agent builds read mcpServers from agent_mcp_servers (one row
// per server), never from a stale mcpServers object in agents.config.
func TestStoreFirstAgentFileLoaderReadsMCPServersFromTable(t *testing.T) {
	db := openEpochStore(t)
	defer db.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	agentID := "agt_mcp_loader"
	if err := db.SaveAgent(ctx, &store.AgentRecord{
		ID: agentID, UserID: "u_loader", Name: "x",
		Config: map[string]interface{}{
			"description": "legacy blob",
			"mcpServers": map[string]any{
				"legacy": map[string]any{"type": "http", "url": "https://legacy.example/mcp"},
			},
		},
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	oauthURL := "https://mcp.quandora.ai/quant"
	if err := db.AddMCPServer(ctx, agentID, "quandora", config.MCPServerConfig{
		Type: "http", URL: oauthURL, OAuthResource: oauthURL,
	}); err != nil {
		t.Fatalf("seed table server: %v", err)
	}

	loader := makeStoreFirstAgentFileLoader(db)
	cfg, ok := loader(agentID, "")
	if !ok {
		t.Fatal("loader returned nothing for a seeded agent")
	}
	if len(cfg.MCPServers) != 1 {
		t.Fatalf("cfg.MCPServers = %+v; want only the table row", cfg.MCPServers)
	}
	if _, hasLegacy := cfg.MCPServers["legacy"]; hasLegacy {
		t.Fatalf("stale agents.config mcpServers leaked into layer-3: %+v", cfg.MCPServers)
	}
	if q := cfg.MCPServers["quandora"]; q.URL != oauthURL || q.OAuthResource != oauthURL {
		t.Fatalf("quandora = %+v", q)
	}
}

// TestStoreFirstAgentFileLoaderEmptyJSONConfigStillReadsTable covers the
// regression where an agent with an empty agents.config (nil/empty JSON)
// but rows in agent_mcp_servers was skipped entirely — pure-`mcp add`
// agents would never register their tools.
func TestStoreFirstAgentFileLoaderEmptyJSONConfigStillReadsTable(t *testing.T) {
	db := openEpochStore(t)
	defer db.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	agentID := "agt_mcp_loader_empty"
	if err := db.SaveAgent(ctx, &store.AgentRecord{
		ID: agentID, UserID: "u_loader", Name: "empty-json",
		Config: nil, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	oauthURL := "https://mcp.quandora.ai/quant"
	if err := db.AddMCPServer(ctx, agentID, "quandora", config.MCPServerConfig{
		Type: "http", URL: oauthURL, OAuthResource: oauthURL,
	}); err != nil {
		t.Fatalf("seed table server: %v", err)
	}

	loader := makeStoreFirstAgentFileLoader(db)
	cfg, ok := loader(agentID, "")
	if !ok {
		t.Fatal("loader returned nothing for an agent with only table rows")
	}
	if q := cfg.MCPServers["quandora"]; q.URL != oauthURL {
		t.Fatalf("table row not surfaced: %+v", cfg.MCPServers)
	}
}
