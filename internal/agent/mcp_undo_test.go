package agent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/agent/tools"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

func TestUndoMarkerRoundTrip(t *testing.T) {
	text, err := appendUndoMarker("quandora: removed.", mcpUndoPayload{
		Action: "add", ServerName: "quandora",
		Config: &config.MCPServerConfig{Type: "http", URL: "https://mcp.quandora.ai/quant", OAuthResource: "https://mcp.quandora.ai/quant", Scopes: []string{"factor_mining:status"}},
	})
	if err != nil {
		t.Fatalf("append marker: %v", err)
	}
	p, ok := extractUndoPayload(text)
	if !ok {
		t.Fatalf("extract marker from %q", text)
	}
	if !hasUndoMarker(text) {
		t.Fatalf("hasUndoMarker(%q) = false; want true", text)
	}
	if p.Action != "add" || p.ServerName != "quandora" || p.Config.URL != "https://mcp.quandora.ai/quant" || len(p.Config.Scopes) != 1 {
		t.Fatalf("payload = %+v", p)
	}
	if _, ok := extractUndoPayload("quandora: removed."); ok {
		t.Fatal("plain text must not parse as undo payload")
	}
	if hasUndoMarker("quandora: removed.") {
		t.Fatal("plain text must not be treated as a journaled mcp mutation")
	}
}

// TestMcpUndoReplaysLatestFromTraceE2E drives `mcp undo` through the real
// sqlite store + session_events trace: add a, add b, record tool_results,
// then undo twice in LIFO order (b first, then a), then error on empty.
func TestMcpUndoReplaysLatestFromTraceE2E(t *testing.T) {
	db := openAgentMcpStore(t)
	defer db.Close()
	ctx := t.Context()

	rc := config.ResolvedAgent{ID: "agent-undo-1", UserID: "owner-undo"}
	seedAgentRow(t, db, rc)
	ag := newUndoAgent(t, db, "chat-1")
	fn := mcpToolFnWithAgent(&oauth.Bootstrap{}, rc, rc.UserID, ag)

	// add a, then add b — record each tool_result as the loop would.
	outA := mustCall(t, fn, map[string]any{"action": "add", "serverName": "a", "url": "https://a.example/mcp"})
	recordToolResult(t, db, rc, "chat-1", "t-a", outA)
	outB := mustCall(t, fn, map[string]any{"action": "add", "serverName": "b", "url": "https://b.example/mcp"})
	recordToolResult(t, db, rc, "chat-1", "t-b", outB)

	// LIFO: first undo removes b.
	out, err := callToolJSON(t, fn, map[string]any{"action": "undo"})
	if err != nil {
		t.Fatalf("undo 1: %v", err)
	}
	if !strings.Contains(out, "b: removed") {
		t.Fatalf("undo 1 output = %q", out)
	}
	servers, err := db.ListMCPServers(ctx, rc.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, ok := servers["b"]; ok {
		t.Fatal("b should be undone first (LIFO)")
	}
	if _, ok := servers["a"]; !ok {
		t.Fatal("a should still exist after first undo")
	}

	// Second undo removes a (the next older operation).
	out, err = callToolJSON(t, fn, map[string]any{"action": "undo"})
	if err != nil {
		t.Fatalf("undo 2: %v", err)
	}
	if !strings.Contains(out, "a: removed") {
		t.Fatalf("undo 2 output = %q", out)
	}
	servers, err = db.ListMCPServers(ctx, rc.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(servers) != 0 {
		t.Fatalf("servers after two undos = %+v; want empty", servers)
	}

	// Nothing left to undo.
	if _, err := callToolJSON(t, fn, map[string]any{"action": "undo"}); err == nil || !strings.Contains(err.Error(), "no un-replayed add/remove") {
		t.Fatalf("undo 3 err = %v; want no-recorded error", err)
	}
}

// TestMcpUndoRemoveReaddsOriginalEntryE2E validates the "inverse produced
// at the application site" contract: remove's output carries the full
// deleted entry, and undo replays it verbatim (url + oauthResource +
// scopes), restoring the exact declaration before the next undo removes it.
func TestMcpUndoRemoveReaddsOriginalEntryE2E(t *testing.T) {
	db := openAgentMcpStore(t)
	defer db.Close()
	ctx := t.Context()

	rc := config.ResolvedAgent{ID: "agent-undo-2", UserID: "owner-undo"}
	seedAgentRow(t, db, rc)
	ag := newUndoAgent(t, db, "chat-1")
	fn := mcpToolFnWithAgent(&oauth.Bootstrap{}, rc, rc.UserID, ag)

	addOut := mustCall(t, fn, map[string]any{
		"action": "add", "serverName": "quandora",
		"url": "https://mcp.quandora.ai/quant", "oauthResource": "https://mcp.quandora.ai/quant",
		"scopes": []string{"factor_mining:status", "strategy:runs.read"},
	})
	recordToolResult(t, db, rc, "chat-1", "t-add", addOut)
	removeOut := mustCall(t, fn, map[string]any{"action": "remove", "serverName": "quandora"})
	recordToolResult(t, db, rc, "chat-1", "t-remove", removeOut)

	// The remove result itself carries the full inverse (config snapshot).
	p, ok := extractUndoPayload(removeOut)
	if !ok || p.Action != "add" || p.Config == nil || p.Config.Scopes == nil {
		t.Fatalf("remove output must carry add-inverse payload: %q", removeOut)
	}

	// Undo the remove: re-adds the exact entry.
	out, err := callToolJSON(t, fn, map[string]any{"action": "undo"})
	if err != nil {
		t.Fatalf("undo remove: %v", err)
	}
	if !strings.Contains(out, "quandora: registered") {
		t.Fatalf("undo remove output = %q", out)
	}
	servers, err := db.ListMCPServers(ctx, rc.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	q := servers["quandora"]
	if q.URL != "https://mcp.quandora.ai/quant" || q.OAuthResource != "https://mcp.quandora.ai/quant" || len(q.Scopes) != 2 {
		t.Fatalf("re-added quandora = %+v; want exact original entry", q)
	}

	// Second undo replays the original add's inverse: removes it again.
	if _, err := callToolJSON(t, fn, map[string]any{"action": "undo"}); err != nil {
		t.Fatalf("undo add: %v", err)
	}
	servers, err = db.ListMCPServers(ctx, rc.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(servers) != 0 {
		t.Fatalf("servers after full undo = %+v; want empty", servers)
	}
}

// TestMcpUndoIsScopedToCurrentSession pins direction A: an operation
// recorded in another session of the same agent must NOT be reachable from
// the current session — deleting that other session can therefore never
// silently break the current session's LIFO chain.
func TestMcpUndoIsScopedToCurrentSession(t *testing.T) {
	db := openAgentMcpStore(t)
	defer db.Close()
	ctx := t.Context()

	rc := config.ResolvedAgent{ID: "agent-undo-3", UserID: "owner-undo"}
	seedAgentRow(t, db, rc)

	// Session 1 adds "a" (recorded in chat-1); session 2 adds "b"
	// (recorded in chat-2) — b is NEWER but belongs to another session.
	ag1 := newUndoAgent(t, db, "chat-1")
	fn1 := mcpToolFnWithAgent(&oauth.Bootstrap{}, rc, rc.UserID, ag1)
	outA := mustCall(t, fn1, map[string]any{"action": "add", "serverName": "a", "url": "https://a.example/mcp"})
	recordToolResult(t, db, rc, "chat-1", "t-a", outA)

	ag2 := newUndoAgent(t, db, "chat-2")
	fn2 := mcpToolFnWithAgent(&oauth.Bootstrap{}, rc, rc.UserID, ag2)
	outB := mustCall(t, fn2, map[string]any{"action": "add", "serverName": "b", "url": "https://b.example/mcp"})
	recordToolResult(t, db, rc, "chat-2", "t-b", outB)

	// undo in chat-1 must not touch b (other session) and should undo a.
	out, err := callToolJSON(t, fn1, map[string]any{"action": "undo"})
	if err != nil {
		t.Fatalf("session-scoped undo: %v", err)
	}
	if !strings.Contains(out, "a: removed") {
		t.Fatalf("undo output = %q; want a removed in chat-1", out)
	}
	servers, err := db.ListMCPServers(ctx, rc.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, ok := servers["a"]; ok {
		t.Fatal("a should be undone from chat-1")
	}
	if _, ok := servers["b"]; !ok {
		t.Fatal("b (chat-2 op) must survive undo from chat-1")
	}
}

func openAgentMcpStore(t *testing.T) *store.DBStore {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "agent-mcp-undo.db")
	st, err := store.New(&store.StorageConfig{Type: "sqlite", DSN: dsn, AutoMigrate: true}, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	db, ok := st.(*store.DBStore)
	if !ok {
		t.Fatalf("store is %T, want *store.DBStore", st)
	}
	return db
}

func seedAgentRow(t *testing.T, db *store.DBStore, rc config.ResolvedAgent) {
	t.Helper()
	now := time.Now().UTC()
	if err := db.SaveAgent(t.Context(), &store.AgentRecord{
		ID: rc.ID, UserID: rc.UserID, Name: "undo-test",
		Config: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
}

func newUndoAgent(t *testing.T, db *store.DBStore, sessionKey string) *Agent {
	t.Helper()
	reg := tools.NewRegistry(t.TempDir(), t.TempDir())
	reg.SetSessionID(sessionKey)
	return &Agent{dataStore: db, registry: reg, mcpConfigNotify: func(string, string) {}}
}

func recordToolResult(t *testing.T, db *store.DBStore, rc config.ResolvedAgent, sessionKey, id, result string) {
	t.Helper()
	blob, _ := json.Marshal(map[string]any{"id": id, "name": "mcp", "result": result})
	if _, err := db.AppendSessionEvent(t.Context(), rc.UserID, rc.ID, sessionKey, "tool_result", blob); err != nil {
		t.Fatalf("append tool_result: %v", err)
	}
}

func mustCall(t *testing.T, fn func(context.Context, json.RawMessage) (string, error), in map[string]any) string {
	t.Helper()
	out, err := callToolJSON(t, fn, in)
	if err != nil {
		t.Fatalf("tool call %v: %v", in, err)
	}
	return out
}

// failingCursorStore embeds the real store and fails only the undo cursor
// write, so the "applied but not recorded" path can be exercised end to
// end without faking the whole Store interface.
type failingCursorStore struct {
	store.Store
}

func (f *failingCursorStore) SetConfigValue(ctx context.Context, kind, scope, scopeID, name, value string) error {
	return errors.New("cursor write failed (injected)")
}

// TestMcpUndoCursorWriteFailureIsExplicitE2E covers the fail-loud cursor
// path: the inverse is applied, the cursor write fails, and undo must say
// so explicitly instead of silently pretending the record was consumed.
func TestMcpUndoCursorWriteFailureIsExplicitE2E(t *testing.T) {
	db := openAgentMcpStore(t)
	defer db.Close()

	rc := config.ResolvedAgent{ID: "agent-undo-cursor", UserID: "owner-undo"}
	seedAgentRow(t, db, rc)

	// Record the mutation through the real store first.
	ag := newUndoAgent(t, db, "chat-1")
	fn := mcpToolFnWithAgent(&oauth.Bootstrap{}, rc, rc.UserID, ag)
	out := mustCall(t, fn, map[string]any{"action": "add", "serverName": "a", "url": "https://a.example/mcp"})
	recordToolResult(t, db, rc, "chat-1", "t-a", out)

	// Undo through a store whose cursor write always fails.
	agFail := &Agent{
		dataStore:       &failingCursorStore{Store: db},
		registry:        ag.registry,
		mcpConfigNotify: func(string, string) {},
	}
	fnFail := mcpToolFnWithAgent(&oauth.Bootstrap{}, rc, rc.UserID, agFail)
	_, err := callToolJSON(t, fnFail, map[string]any{"action": "undo"})
	if err == nil || !strings.Contains(err.Error(), "failed to record the cursor") {
		t.Fatalf("undo with failing cursor err = %v; want explicit cursor-failure", err)
	}

	// The inverse WAS applied (server removed) — the error reports the
	// degraded state rather than pretending nothing happened.
	servers, err := db.ListMCPServers(t.Context(), rc.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, ok := servers["a"]; ok {
		t.Fatal("inverse should have been applied even though the cursor write failed")
	}
}
