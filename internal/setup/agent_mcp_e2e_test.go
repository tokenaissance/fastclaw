package setup

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/config"
)

// TestAgentMCP_CloudPathE2E drives the MCP server config persistence that the
// #44 dashboard page (web/src/app/agents/[id]/mcp/page.tsx) exercises, through
// the real handlers that the Cloud proxy path hits:
//
//   - updateAgent → PATCH /api/agents/{id} (handleUpdateAgent) with a
//     mcpServers whole-map replace;
//   - getAgentConfig → GET /api/agents/{id}/config (handleGetAgentConfig),
//     which round-trips the full agent config blob including mcpServers.
//
// The dashboard saves via whole-map replace (no merge), so the contract under
// test is: PATCH writes the exact map, a second PATCH replaces (never merges),
// an empty map clears, and mcpServersReset:true clears too.
func TestAgentMCP_CloudPathE2E(t *testing.T) {
	s, uid, aid := setupFileUploadTest(t)

	patch := func(body string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPatch, "/api/agents/"+aid, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("id", aid)
		req = stampAuthAndUserID(req, uid)
		rec := httptest.NewRecorder()
		s.handleUpdateAgent(rec, req)
		return rec.Code
	}

	getConfig := func() config.AgentFileConfig {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/agents/"+aid+"/config", nil)
		req.SetPathValue("id", aid)
		req = stampAuthAndUserID(req, uid)
		rec := httptest.NewRecorder()
		s.handleGetAgentConfig(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("get config status = %d, body=%s", rec.Code, rec.Body.String())
		}
		var cfg config.AgentFileConfig
		if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
			t.Fatalf("decode config: %v", err)
		}
		return cfg
	}

	// 1. PATCH with an http + stdio pair → 200, both persist verbatim.
	body := `{"mcpServers":{"postgres":{"type":"http","url":"https://pg.example.com/mcp",
		"headers":{"Authorization":"Bearer e2e-token"}},"filesystem":{"type":"stdio",
		"command":"npx","args":["-y","@modelcontextprotocol/server-filesystem","/tmp"],
		"env":{"HOME":"/root"}}}}`
	if code := patch(body); code != http.StatusOK {
		t.Fatalf("patch mcpServers status = %d", code)
	}
	cfg := getConfig()
	if len(cfg.MCPServers) != 2 {
		t.Fatalf("mcpServers after write = %d; want 2, got %+v", len(cfg.MCPServers), cfg.MCPServers)
	}
	if pg := cfg.MCPServers["postgres"]; pg.Type != "http" || pg.URL != "https://pg.example.com/mcp" {
		t.Errorf("postgres = %+v; want http url", pg)
	} else if pg.Headers["Authorization"] != "Bearer e2e-token" {
		t.Errorf("postgres headers = %+v; want Authorization present", pg.Headers)
	}
	fs := cfg.MCPServers["filesystem"]
	if fs.Type != "stdio" || fs.Command != "npx" || len(fs.Args) != 3 || fs.Env["HOME"] != "/root" {
		t.Errorf("filesystem = %+v; want stdio npx with args+env", fs)
	}

	// 2. Whole-map replace: a second PATCH with only filesystem drops postgres.
	if code := patch(`{"mcpServers":{"filesystem":{"type":"stdio","command":"python3","args":["/opt/mcp.py"]}}}`); code != http.StatusOK {
		t.Fatalf("patch replace status = %d", code)
	}
	cfg = getConfig()
	if len(cfg.MCPServers) != 1 {
		t.Fatalf("mcpServers after replace = %d; want 1 (postgres must be dropped, not merged), got %+v", len(cfg.MCPServers), cfg.MCPServers)
	}
	if _, ok := cfg.MCPServers["postgres"]; ok {
		t.Errorf("postgres survived whole-map replace: %+v", cfg.MCPServers)
	}
	if cfg.MCPServers["filesystem"].Command != "python3" {
		t.Errorf("filesystem command = %q; want python3", cfg.MCPServers["filesystem"].Command)
	}

	// 3. Empty map {} clears the key entirely.
	if code := patch(`{"mcpServers":{}}`); code != http.StatusOK {
		t.Fatalf("patch empty status = %d", code)
	}
	if cfg := getConfig(); len(cfg.MCPServers) != 0 {
		t.Fatalf("mcpServers after {} = %+v; want cleared", cfg.MCPServers)
	}

	// 4. mcpServersReset:true clears too (dashboard reset signal).
	if code := patch(`{"mcpServers":{"postgres":{"type":"http","url":"https://pg.example.com/mcp"}}}`); code != http.StatusOK {
		t.Fatalf("patch re-add status = %d", code)
	}
	if cfg := getConfig(); len(cfg.MCPServers) != 1 {
		t.Fatalf("mcpServers after re-add = %+v; want 1", cfg.MCPServers)
	}
	if code := patch(`{"mcpServersReset":true}`); code != http.StatusOK {
		t.Fatalf("patch reset status = %d", code)
	}
	if cfg := getConfig(); len(cfg.MCPServers) != 0 {
		t.Fatalf("mcpServers after reset = %+v; want cleared", cfg.MCPServers)
	}

	// 5. An unrelated field leaves mcpServers untouched (omit semantics).
	if code := patch(`{"name":"Renamed"}`); code != http.StatusOK {
		t.Fatalf("patch name status = %d", code)
	}
	if cfg := getConfig(); len(cfg.MCPServers) != 0 {
		t.Fatalf("name-only patch clobbered mcpServers: %+v", cfg.MCPServers)
	}
}
