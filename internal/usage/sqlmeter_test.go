package usage_test

import (
	"context"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/usage"
)

// TestSQLMeterRecordAndQuery runs the meter end-to-end against an
// on-disk SQLite created by the real store migration. It verifies:
//   - UPSERT accumulates across multiple RecordTokens calls
//   - Totals sums correctly across the day
//   - TopAgents / TopUsers order by combined tokens desc
//   - The "system" (empty user_id) row survives recording and ranks alongside named users
func TestSQLMeterRecordAndQuery(t *testing.T) {
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "test.db") + "?_pragma=foreign_keys(1)"

	st, err := store.NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	m := usage.NewSQLMeter(st.DB(), "sqlite")
	ctx := context.Background()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	// Two calls into the same (agent, user, provider, model) bucket — must merge.
	must(m.RecordTokens(ctx, "alice", "agentA", "sess1", "anthropic-messages", "sonnet-4-6",
		usage.Tokens{Input: 100, Output: 50}))
	must(m.RecordTokens(ctx, "alice", "agentA", "sess1", "anthropic-messages", "sonnet-4-6",
		usage.Tokens{Input: 200, Output: 80, CacheRead: 30}))

	// Different agent, different user.
	must(m.RecordTokens(ctx, "bob", "agentB", "sess2", "anthropic-messages", "sonnet-4-6",
		usage.Tokens{Input: 500, Output: 1000}))

	// system row (empty user_id) — separately, an empty-provider record
	// to verify the column accepts the shared-provider sentinel.
	must(m.RecordTokens(ctx, "", "agentA", "cron-tick", "", "sonnet-4-6",
		usage.Tokens{Input: 10, Output: 5}))

	r := usage.LastN(1)
	tot, err := m.Totals(ctx, r)
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	wantIn := int64(100 + 200 + 500 + 10)
	wantOut := int64(50 + 80 + 1000 + 5)
	if tot.Input != wantIn || tot.Output != wantOut || tot.CacheRead != 30 {
		t.Errorf("totals = %+v, want in=%d out=%d cache=30", tot, wantIn, wantOut)
	}
	if tot.Requests != 4 {
		t.Errorf("request_count = %d, want 4", tot.Requests)
	}

	// Verify UPSERT collapsed the two alice calls into one row.
	var rowCount int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM token_usage_daily`).Scan(&rowCount); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rowCount != 3 {
		t.Errorf("row count = %d, want 3 (alice+bob+system)", rowCount)
	}

	agents, err := m.TopAgents(ctx, r, 10)
	if err != nil {
		t.Fatalf("top agents: %v", err)
	}
	// agentB has 1500 tokens (single call), agentA has alice's
	// 100+50+200+80+30 = 460 plus system's 10+5 = 15 = 475.
	if len(agents) != 2 || agents[0].Key != "agentB" || agents[0].Tokens != 1500 {
		t.Errorf("top agents head = %+v, want agentB=1500", agents)
	}
	if agents[1].Key != "agentA" || agents[1].Tokens != 475 {
		t.Errorf("top agents tail = %+v, want agentA=475", agents[1])
	}

	users, err := m.TopUsers(ctx, r, 10)
	if err != nil {
		t.Fatalf("top users: %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("top users len = %d, want 3 (alice/bob/system)", len(users))
	}
	if users[0].Key != "bob" || users[0].Tokens != 1500 {
		t.Errorf("top users head = %+v, want bob=1500", users[0])
	}
}

// TestSQLMeterPerUserReadback exercises the two methods added for the
// upstream billing API — TotalsForUser and DailyForUser. Both must scope
// strictly to the requested user_id, and DailyForUser must return the
// per-day/per-agent/per-model granularity GET /v1/usage renders.
func TestSQLMeterPerUserReadback(t *testing.T) {
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "test.db") + "?_pragma=foreign_keys(1)"
	st, err := store.NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	m := usage.NewSQLMeter(st.DB(), "sqlite")
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	// alice burns tokens on two different agents.
	must(m.RecordTokens(ctx, "alice", "agentA", "sess1", "anthropic-messages", "sonnet-4-6",
		usage.Tokens{Input: 100, Output: 50}))
	must(m.RecordTokens(ctx, "alice", "agentB", "sess2", "anthropic-messages", "sonnet-4-6",
		usage.Tokens{Input: 300, Output: 200}))
	// bob on the same agentA — must NOT leak into alice's readback.
	must(m.RecordTokens(ctx, "bob", "agentA", "sess3", "anthropic-messages", "sonnet-4-6",
		usage.Tokens{Input: 9999, Output: 9999}))

	r := usage.LastN(1)

	tot, err := m.TotalsForUser(ctx, "alice", r)
	if err != nil {
		t.Fatalf("TotalsForUser: %v", err)
	}
	if tot.Input != 400 || tot.Output != 250 || tot.Requests != 2 {
		t.Errorf("TotalsForUser(alice) = %+v, want in=400 out=250 req=2", tot)
	}

	daily, err := m.DailyForUser(ctx, "alice", r)
	if err != nil {
		t.Fatalf("DailyForUser: %v", err)
	}
	if len(daily) != 2 {
		t.Fatalf("DailyForUser rows = %d, want 2 (agentA + agentB)", len(daily))
	}
	byAgent := map[string]usage.DailyUsage{}
	for _, d := range daily {
		if d.Day == "" {
			t.Errorf("DailyForUser row missing Day: %+v", d)
		}
		byAgent[d.AgentID] = d
	}
	if a := byAgent["agentA"]; a.InputTokens != 100 || a.OutputTokens != 50 || a.Requests != 1 {
		t.Errorf("DailyForUser agentA = %+v, want in=100 out=50 req=1", a)
	}
	if b := byAgent["agentB"]; b.InputTokens != 300 || b.OutputTokens != 200 || b.Requests != 1 {
		t.Errorf("DailyForUser agentB = %+v, want in=300 out=200 req=1", b)
	}

	// Unknown user returns zeros, never bob's numbers.
	empty, err := m.TotalsForUser(ctx, "carol", r)
	if err != nil {
		t.Fatalf("TotalsForUser(carol): %v", err)
	}
	if empty.Input != 0 || empty.Requests != 0 {
		t.Errorf("TotalsForUser(carol) = %+v, want all zeros", empty)
	}
}

// TestSQLMeterRecordTokenLog verifies the append-only per-call log. Every
// call lands one row carrying the full token split + wall-clock duration;
// unlike token_usage_daily there is no UPSERT, so two calls are two rows.
func TestSQLMeterRecordTokenLog(t *testing.T) {
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "test.db") + "?_pragma=foreign_keys(1)"
	st, err := store.NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	m := usage.NewSQLMeter(st.DB(), "sqlite")
	ctx := context.Background()

	if err := m.RecordTokenLog(ctx, "alice", "agentA", "sess1", "anthropic-messages", "sonnet-4-6",
		usage.Tokens{Input: 100, Output: 50, CacheRead: 25, CacheCreation: 5}, 1234); err != nil {
		t.Fatalf("RecordTokenLog 1: %v", err)
	}
	if err := m.RecordTokenLog(ctx, "alice", "agentA", "sess1", "anthropic-messages", "sonnet-4-6",
		usage.Tokens{Input: 7, Output: 8}, 42); err != nil {
		t.Fatalf("RecordTokenLog 2: %v", err)
	}

	var count int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM token_usage_log`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("token_usage_log rows = %d, want 2 (append-only, no merge)", count)
	}

	var (
		userID, agentID, model       string
		in, out, cRead, cCreate, dur int64
	)
	// token_usage_log has no request_count — one row per call is the
	// append-only contract.
	if err := st.DB().QueryRow(
		`SELECT user_id, agent_id, model, input_tokens, output_tokens,
		        cache_read_tokens, cache_create_tokens, duration_ms
		 FROM token_usage_log WHERE duration_ms = 1234`,
	).Scan(&userID, &agentID, &model, &in, &out, &cRead, &cCreate, &dur); err != nil {
		t.Fatalf("scan log row: %v", err)
	}
	if userID != "alice" || agentID != "agentA" || model != "sonnet-4-6" {
		t.Errorf("log row = %s/%s/%s, want alice/agentA/sonnet-4-6", userID, agentID, model)
	}
	if in != 100 || out != 50 || cRead != 25 || cCreate != 5 || dur != 1234 {
		t.Errorf("log row = in=%d out=%d cr=%d cc=%d dur=%d, want 100/50/25/5/1234",
			in, out, cRead, cCreate, dur)
	}
}
