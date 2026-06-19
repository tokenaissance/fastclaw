package store

import (
	"context"
	"testing"
	"time"
)

// e2e for commit 8ed4f74 "fix(store): use dialect-aware placeholders in
// ListSessionsPaginated for PostgreSQL".
//
// Cloud zero-impact rationale: this is a one-line store-layer dialect fix.
// ListSessionsPaginated is reached via the FastAgent /api/chats +
// /api/admin/chats handlers — endpoints with NO Cloud consumer (ROUTE_ACCESS
// has no /api/chats plural entry; established in the #38 review). The change
// swaps the hardcoded "?" placeholder for d.ph(i+1), which emits the same "?"
// on SQLite (behaviour byte-identical) and correct $1,$2 positional params on
// PostgreSQL (where "?" was invalid — the bug being fixed). No endpoint,
// response shape, or auth semantics change.
//
// What is pinned down:
//   - d.ph(n) emits the dialect-correct placeholder: "?" for sqlite and
//     "$n" for postgres — the exact helper whose output changed the SQL.
//   - ListSessionsPaginated with a MULTI-agent IN clause (2+ placeholders)
//     still executes correctly on the real query path — regression guard
//     that the placeholder change keeps the SQL valid and well-ordered.
//     (The #38 pagination e2e only exercised single-agent IN.)

func TestPh_DialectAwarePlaceholders(t *testing.T) {
	sqlite := &DBStore{dialect: "sqlite"}
	if got := sqlite.ph(1); got != "?" {
		t.Errorf("sqlite.ph(1) = %q, want \"?\"", got)
	}
	pg := &DBStore{dialect: "postgres"}
	for n, want := range map[int]string{1: "$1", 2: "$2", 3: "$3"} {
		if got := pg.ph(n); got != want {
			t.Errorf("postgres.ph(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestListSessionsPaginated_MultiAgentIN_CloudPathE2E(t *testing.T) {
	db, err := NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("NewDBStore: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Seed two agents' sessions. Stagger updated_at so ORDER BY updated_at
	// DESC is deterministic (modernc sqlite stores TIMESTAMP as RFC3339Nano
	// text → lexicographic ordering == chronological).
	save := func(agent, key string) {
		t.Helper()
		if err := db.SaveSession(ctx, "owner_pg", agent, key, &SessionRecord{
			Channel:  "web",
			ChatID:   key,
			Messages: []SessionMessage{{Role: "user", Content: "hello " + key}},
		}); err != nil {
			t.Fatalf("seed session %s: %v", key, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	save("agent_x", "x1")
	save("agent_y", "y1")
	save("agent_x", "x2")

	// Multi-agent IN clause: placeholders for both agents must be filled
	// correctly and the union returned newest-first.
	metas, total, err := db.ListSessionsPaginated(ctx, []string{"agent_x", "agent_y"}, 0, 10)
	if err != nil {
		t.Fatalf("ListSessionsPaginated(2 agents): %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	want := []string{"x2", "y1", "x1"}
	if len(metas) != len(want) {
		t.Fatalf("len = %d, want %d", len(metas), len(want))
	}
	for i, k := range want {
		if metas[i].Key != k {
			t.Errorf("metas[%d].Key = %q, want %q (updated_at DESC across agents)", i, metas[i].Key, k)
		}
	}
}
