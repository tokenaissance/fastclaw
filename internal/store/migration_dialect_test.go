package store

import (
	"strings"
	"testing"
)

// Regression guard for the workspace's AutoMigrate changes:
//   - the shared OAuth DDL must use BLOB on SQLite and BYTEA on Postgres
//     (Postgres has no BLOB type — without this split AutoMigrate fails);
//   - reload epochs live in configs_kv (design-doc contract), and no
//     stale agent_reload_epochs DDL is emitted for fresh installs;
//   - agent_mcp_servers (per-key mcpServers successor to agents.config)
//     is created on both dialects.
//
// These tests always run and need no DB server: the DDL builder is pure.
func TestMigrationSQLDialectCompatibility(t *testing.T) {
	pgSQL := joinStatements(migrationSQLForDialect("postgres"))
	sqliteSQL := joinStatements(migrationSQLForDialect("sqlite"))

	for name, got := range map[string]string{"postgres": pgSQL, "sqlite": sqliteSQL} {
		if !containsAll(got, "mcp_oauth_tokens", "mcp_oauth_pending", "mcp_oauth_clients", "configs_kv", "agent_mcp_servers") {
			t.Fatalf("%s migration missing oauth/configs_kv/agent_mcp_servers DDL", name)
		}
		if strings.Contains(got, "agent_reload_epochs") {
			t.Fatalf("%s migration still emits the retired agent_reload_epochs table", name)
		}
	}
	if !strings.Contains(pgSQL, "BYTEA") || strings.Contains(pgSQL, "BLOB") {
		t.Fatalf("postgres migration must use BYTEA, not BLOB\n%s", pgSQL)
	}
	if !strings.Contains(sqliteSQL, "BLOB") || strings.Contains(sqliteSQL, "BYTEA") {
		t.Fatalf("sqlite migration must use BLOB, not BYTEA\n%s", sqliteSQL)
	}
}

func joinStatements(stmts []string) string {
	out := ""
	for _, s := range stmts {
		out += s + "\n"
	}
	return out
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
