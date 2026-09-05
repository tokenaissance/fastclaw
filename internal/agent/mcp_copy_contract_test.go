package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/agent/tools"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// TestMcpToolOutputCopyContract locks the exact model-visible mcp tool
// texts (success + failure) that get recorded into session_events. Copy
// changes therefore require an intentional golden update
// (FASTAGENT_UPDATE_GOLDEN=1 go test ...), not a silent drift.
func TestMcpToolOutputCopyContract(t *testing.T) {
	ob := testToolBootstrap()
	rc := testToolRC()
	fnNoStore := mcpToolFn(ob, rc, rc.UserID)

	type produceFn func() (string, error)
	cases := []struct {
		name  string
		probe produceFn
	}{
		{"login_no_callback_base_error", func() (string, error) {
			t.Setenv("FASTAGENT_OAUTH_CALLBACK_BASE", "")
			return callTool(t, fnNoStore, "login", "quandora")
		}},
		{"login_ok", func() (string, error) {
			t.Setenv("FASTAGENT_OAUTH_CALLBACK_BASE", "https://app.example.com/oauth/mcp")
			out, err := callTool(t, fnNoStore, "login", "quandora")
			if err == nil {
				// The provider auth URL carries dynamic nonce params; pin
				// the static instruction and keep the URL as a placeholder.
				lines := strings.Split(out, "\n")
				for i, line := range lines {
					if strings.HasPrefix(line, "https://") {
						lines[i] = "<AUTH_URL>"
					}
				}
				out = strings.Join(lines, "\n")
			}
			return out, err
		}},
		{"status_oauth_list", func() (string, error) {
			return callTool(t, fnNoStore, "status", "")
		}},
		{"status_unknown_server_error", func() (string, error) {
			return callTool(t, fnNoStore, "status", "missing")
		}},
		{"add_no_store_error", func() (string, error) {
			return callToolJSON(t, fnNoStore, map[string]any{
				"action": "add", "serverName": "s", "url": "https://s.example/mcp",
			})
		}},
		{"add_bad_url_error", func() (string, error) {
			return callToolJSON(t, fnNoStore, map[string]any{
				"action": "add", "serverName": "s", "url": "ftp://s.example/mcp",
			})
		}},
		{"add_scopes_without_oauth_error", func() (string, error) {
			return callToolJSON(t, fnNoStore, map[string]any{
				"action": "add", "serverName": "s", "url": "https://s.example/mcp",
				"scopes": []string{"a:b"},
			})
		}},
		{"undo_no_session_error", func() (string, error) {
			db := openAgentMcpStore(t)
			defer db.Close()
			seedAgentRow(t, db, rc)
			reg := newRegistryNoSession(t)
			ag := &Agent{dataStore: db, registry: reg, mcpConfigNotify: func(string, string) {}}
			return callToolJSON(t, mcpToolFnWithAgent(ob, rc, rc.UserID, ag), map[string]any{"action": "undo"})
		}},
	}

	// Store-backed add/remove/undo copy cases run on a fresh store each.
	storeCases := []struct {
		name  string
		steps func(t *testing.T, db *store.DBStore, fn func(context.Context, json.RawMessage) (string, error)) (string, error)
	}{
		{"add_ok", func(t *testing.T, db *store.DBStore, fn func(context.Context, json.RawMessage) (string, error)) (string, error) {
			return callToolJSON(t, fn, map[string]any{
				"action": "add", "serverName": "quandora",
				"url": "https://mcp.quandora.ai/quant", "oauthResource": "https://mcp.quandora.ai/quant",
				"scopes": []string{"factor_mining:*"},
			})
		}},
		{"add_duplicate_error", func(t *testing.T, db *store.DBStore, fn func(context.Context, json.RawMessage) (string, error)) (string, error) {
			first(t, fn, map[string]any{"action": "add", "serverName": "quandora", "url": "https://mcp.quandora.ai/quant"})
			return callToolJSON(t, fn, map[string]any{"action": "add", "serverName": "quandora", "url": "https://mcp.quandora.ai/quant"})
		}},
		{"remove_missing_error", func(t *testing.T, db *store.DBStore, fn func(context.Context, json.RawMessage) (string, error)) (string, error) {
			return callToolJSON(t, fn, map[string]any{"action": "remove", "serverName": "missing"})
		}},
		{"remove_ok", func(t *testing.T, db *store.DBStore, fn func(context.Context, json.RawMessage) (string, error)) (string, error) {
			first(t, fn, map[string]any{"action": "add", "serverName": "quandora", "url": "https://mcp.quandora.ai/quant", "oauthResource": "https://mcp.quandora.ai/quant"})
			return callToolJSON(t, fn, map[string]any{"action": "remove", "serverName": "quandora"})
		}},
		{"undo_no_recorded_error", func(t *testing.T, db *store.DBStore, fn func(context.Context, json.RawMessage) (string, error)) (string, error) {
			return callToolJSON(t, fn, map[string]any{"action": "undo"})
		}},
		{"undo_ok", func(t *testing.T, db *store.DBStore, fn func(context.Context, json.RawMessage) (string, error)) (string, error) {
			out := first(t, fn, map[string]any{"action": "add", "serverName": "quandora", "url": "https://mcp.quandora.ai/quant", "oauthResource": "https://mcp.quandora.ai/quant"})
			recordToolResult(t, db, rc, "chat-1", "t-add", out)
			return callToolJSON(t, fn, map[string]any{"action": "undo"})
		}},
		{"undo_exhausted_error", func(t *testing.T, db *store.DBStore, fn func(context.Context, json.RawMessage) (string, error)) (string, error) {
			out := first(t, fn, map[string]any{"action": "add", "serverName": "quandora", "url": "https://mcp.quandora.ai/quant"})
			recordToolResult(t, db, rc, "chat-1", "t-add", out)
			first(t, fn, map[string]any{"action": "undo"})
			return callToolJSON(t, fn, map[string]any{"action": "undo"})
		}},
		{"undo_state_diverged_error", func(t *testing.T, db *store.DBStore, fn func(context.Context, json.RawMessage) (string, error)) (string, error) {
			out := first(t, fn, map[string]any{"action": "add", "serverName": "quandora", "url": "https://mcp.quandora.ai/quant"})
			recordToolResult(t, db, rc, "chat-1", "t-add", out)
			if err := db.DeleteMCPServer(t.Context(), rc.ID, "quandora"); err != nil {
				t.Fatalf("delete server: %v", err)
			}
			return callToolJSON(t, fn, map[string]any{"action": "undo"})
		}},
		{"undo_cursor_failure_error", func(t *testing.T, db *store.DBStore, fn func(context.Context, json.RawMessage) (string, error)) (string, error) {
			out := first(t, fn, map[string]any{"action": "add", "serverName": "quandora", "url": "https://mcp.quandora.ai/quant"})
			recordToolResult(t, db, rc, "chat-1", "t-add", out)
			agFail := &Agent{
				dataStore:       &failingCursorStore{Store: db},
				registry:        newRegistryWithSession(t, "chat-1"),
				mcpConfigNotify: func(string, string) {},
			}
			return callToolJSON(t, mcpToolFnWithAgent(ob, rc, rc.UserID, agFail), map[string]any{"action": "undo"})
		}},
	}

	for _, tc := range storeCases {
		cases = append(cases, struct {
			name  string
			probe produceFn
		}{tc.name, func() (string, error) {
			db := openAgentMcpStore(t)
			defer db.Close()
			seedAgentRow(t, db, rc)
			ag := &Agent{
				dataStore:       db,
				registry:        newRegistryWithSession(t, "chat-1"),
				mcpConfigNotify: func(string, string) {},
			}
			fn := mcpToolFnWithAgent(ob, rc, rc.UserID, ag)
			return tc.steps(t, db, fn)
		}})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.probe()
			var text string
			if err != nil {
				text = "ERROR: " + err.Error()
			} else {
				text = "OK: " + out
			}
			checkCopyGolden(t, tc.name, text)
		})
	}
}

func checkCopyGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "mcp-copy", name+".txt")
	if os.Getenv("FASTAGENT_UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(got+"\n"), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run with FASTAGENT_UPDATE_GOLDEN=1 to create): %v", path, err)
	}
	if string(want) != got+"\n" {
		t.Errorf("copy contract drift for %s:\n--- got ---\n%s\n--- golden (%s) ---\n%s", name, got, path, string(want))
	}
}

func first(t *testing.T, fn func(context.Context, json.RawMessage) (string, error), in map[string]any) string {
	t.Helper()
	out, err := callToolJSON(t, fn, in)
	if err != nil {
		t.Fatalf("step %v failed: %v", in, err)
	}
	return out
}

func newRegistryWithSession(t *testing.T, sessionKey string) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry(t.TempDir(), t.TempDir())
	reg.SetSessionID(sessionKey)
	return reg
}

func newRegistryNoSession(t *testing.T) *tools.Registry {
	t.Helper()
	return tools.NewRegistry(t.TempDir(), t.TempDir())
}
