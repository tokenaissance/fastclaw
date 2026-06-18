package usage_test

import (
	"context"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/usage"
)

func TestMemQuotaStoreCRUD(t *testing.T) {
	ctx := context.Background()
	qs := usage.NewMemQuotaStore()

	// No quota → error
	if _, err := qs.GetQuota(ctx, "u_1"); err == nil {
		t.Fatal("expected error for missing quota")
	}

	// Set quota
	q := &usage.Quota{UserID: "u_1", MonthlyTokenLimit: 5_000_000, MonthlyRequestLimit: 10_000, ResetDay: 1}
	if err := qs.SetQuota(ctx, q); err != nil {
		t.Fatalf("set: %v", err)
	}

	// Get it back
	got, err := qs.GetQuota(ctx, "u_1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.MonthlyTokenLimit != 5_000_000 || got.MonthlyRequestLimit != 10_000 {
		t.Errorf("got %+v", got)
	}

	// Update
	q.MonthlyTokenLimit = 10_000_000
	if err := qs.SetQuota(ctx, q); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ = qs.GetQuota(ctx, "u_1")
	if got.MonthlyTokenLimit != 10_000_000 {
		t.Errorf("updated limit = %d, want 10M", got.MonthlyTokenLimit)
	}

	// Delete
	if err := qs.DeleteQuota(ctx, "u_1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := qs.GetQuota(ctx, "u_1"); err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestSQLQuotaStoreCRUD(t *testing.T) {
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "test.db") + "?_pragma=foreign_keys(1)"
	st, err := store.NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ctx := context.Background()
	qs := usage.NewSQLQuotaStore(st.DB(), "sqlite")

	// No quota → error
	if _, err := qs.GetQuota(ctx, "u_1"); err == nil {
		t.Fatal("expected error for missing quota")
	}

	// Set
	q := &usage.Quota{UserID: "u_1", MonthlyTokenLimit: 5_000_000, ResetDay: 15}
	if err := qs.SetQuota(ctx, q); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := qs.GetQuota(ctx, "u_1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.MonthlyTokenLimit != 5_000_000 || got.ResetDay != 15 {
		t.Errorf("got %+v", got)
	}

	// Upsert
	q.MonthlyTokenLimit = 8_000_000
	if err := qs.SetQuota(ctx, q); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, _ = qs.GetQuota(ctx, "u_1")
	if got.MonthlyTokenLimit != 8_000_000 {
		t.Errorf("upserted limit = %d, want 8M", got.MonthlyTokenLimit)
	}

	// Delete
	if err := qs.DeleteQuota(ctx, "u_1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := qs.GetQuota(ctx, "u_1"); err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestCheckQuotaAllowed(t *testing.T) {
	ctx := context.Background()
	qs := usage.NewMemQuotaStore()
	meter := usage.NewMemMeter()

	// No quota → allowed
	status, err := usage.CheckQuota(ctx, qs, meter, "u_1")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !status.Allowed {
		t.Fatal("expected allowed when no quota set")
	}

	// Set quota with high limit
	qs.SetQuota(ctx, &usage.Quota{UserID: "u_1", MonthlyTokenLimit: 1_000_000, ResetDay: 1})

	// Record some usage (well under limit)
	meter.RecordTokens(ctx, "u_1", "agent1", "s1", "anthropic", "sonnet",
		usage.Tokens{Input: 1000, Output: 500})

	status, err = usage.CheckQuota(ctx, qs, meter, "u_1")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !status.Allowed {
		t.Fatal("expected allowed when under limit")
	}
	if status.TokensUsed != 1500 {
		t.Errorf("tokensUsed = %d, want 1500", status.TokensUsed)
	}
}

func TestCheckQuotaExceeded(t *testing.T) {
	ctx := context.Background()
	qs := usage.NewMemQuotaStore()
	meter := usage.NewMemMeter()

	// Set a very low token limit
	qs.SetQuota(ctx, &usage.Quota{UserID: "u_1", MonthlyTokenLimit: 100, ResetDay: 1})

	// Burn past it
	meter.RecordTokens(ctx, "u_1", "agent1", "s1", "anthropic", "sonnet",
		usage.Tokens{Input: 80, Output: 30})

	status, err := usage.CheckQuota(ctx, qs, meter, "u_1")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if status.Allowed {
		t.Fatal("expected NOT allowed when over token limit")
	}
	if status.TokensUsed != 110 {
		t.Errorf("tokensUsed = %d, want 110", status.TokensUsed)
	}
}

func TestCheckQuotaRequestLimitExceeded(t *testing.T) {
	ctx := context.Background()
	qs := usage.NewMemQuotaStore()
	meter := usage.NewMemMeter()

	// Unlimited tokens but only 2 requests
	qs.SetQuota(ctx, &usage.Quota{UserID: "u_1", MonthlyRequestLimit: 2, ResetDay: 1})

	meter.RecordTokens(ctx, "u_1", "a", "s1", "", "m", usage.Tokens{Input: 1})
	meter.RecordTokens(ctx, "u_1", "a", "s2", "", "m", usage.Tokens{Input: 1})

	status, _ := usage.CheckQuota(ctx, qs, meter, "u_1")
	if status.Allowed {
		t.Fatal("expected NOT allowed when request limit hit")
	}
	if status.RequestsUsed != 2 {
		t.Errorf("requestsUsed = %d, want 2", status.RequestsUsed)
	}
}

func TestTotalsForUser(t *testing.T) {
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "test.db") + "?_pragma=foreign_keys(1)"
	st, err := store.NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ctx := context.Background()
	m := usage.NewSQLMeter(st.DB(), "sqlite")

	m.RecordTokens(ctx, "alice", "a1", "s1", "anthropic", "sonnet", usage.Tokens{Input: 100, Output: 50})
	m.RecordTokens(ctx, "alice", "a2", "s2", "anthropic", "sonnet", usage.Tokens{Input: 200, Output: 80})
	m.RecordTokens(ctx, "bob", "a1", "s3", "anthropic", "sonnet", usage.Tokens{Input: 999, Output: 999})

	r := usage.LastN(1)
	tot, err := m.TotalsForUser(ctx, "alice", r)
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	if tot.Input != 300 || tot.Output != 130 {
		t.Errorf("alice totals = %+v, want in=300 out=130", tot)
	}
	if tot.Requests != 2 {
		t.Errorf("alice requests = %d, want 2", tot.Requests)
	}
}

func TestDailyForUser(t *testing.T) {
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "test.db") + "?_pragma=foreign_keys(1)"
	st, err := store.NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ctx := context.Background()
	m := usage.NewSQLMeter(st.DB(), "sqlite")

	m.RecordTokens(ctx, "alice", "a1", "s1", "anthropic", "sonnet", usage.Tokens{Input: 100, Output: 50})
	m.RecordTokens(ctx, "alice", "a1", "s2", "anthropic", "sonnet", usage.Tokens{Input: 200, Output: 80})
	m.RecordTokens(ctx, "bob", "a1", "s3", "anthropic", "sonnet", usage.Tokens{Input: 999, Output: 999})

	r := usage.LastN(1)
	daily, err := m.DailyForUser(ctx, "alice", r)
	if err != nil {
		t.Fatalf("daily: %v", err)
	}
	if len(daily) != 1 {
		t.Fatalf("daily len = %d, want 1 (one day, one agent+model combo)", len(daily))
	}
	row := daily[0]
	if row.AgentID != "a1" || row.InputTokens != 300 || row.OutputTokens != 130 {
		t.Errorf("daily row = %+v", row)
	}
}
