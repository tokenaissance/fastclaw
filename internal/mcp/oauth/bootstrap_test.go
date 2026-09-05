package oauth

// Bootstrap-level guarantees for the storage-selection contract:
//  1. When a DB is passed (the only deployed shape — sqlite single
//     instance or postgres multi instance), Init assembles the SQL stores,
//     never the file-backed fallback;
//  2. a single instance persists credentials through its own DB across a
//     restart (close + reopen the same sqlite file);
//  3. the Home directory stays empty — no file-store artifacts are
//     produced while DB storage is in use.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/adapter"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/port"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

func openSQLite(t *testing.T, dsn string) *store.DBStore {
	t.Helper()
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

func TestInitUsesDBStoresForSingleInstanceAndSurvivesRestart(t *testing.T) {
	const secret = "bootstrap-test-secret"
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "single.db")
	home := t.TempDir()

	db := openSQLite(t, dsn)
	b, err := Init(secret, Options{Home: home, DB: db})
	if err != nil {
		t.Fatalf("Init (first boot): %v", err)
	}
	// DB present → SQL stores, not file stores.
	if _, ok := b.Tokens.(*adapter.DBTokenStore); !ok {
		t.Fatalf("Tokens store = %T, want *adapter.DBTokenStore", b.Tokens)
	}
	if _, ok := b.Pending.(*adapter.DBPendingStore); !ok {
		t.Fatalf("Pending store = %T, want *adapter.DBPendingStore", b.Pending)
	}
	if _, ok := b.Regs.(*adapter.DBRegistrationStore); !ok {
		t.Fatalf("Regs store = %T, want *adapter.DBRegistrationStore", b.Regs)
	}

	key := domain.StoreKey("u1", "a1", "quandora")
	now := time.Now().UTC()
	if err := b.Tokens.Save(ctx, key, &domain.OAuthTokens{
		AccessToken: "at-1", RefreshToken: "rt-1", ExpiresAt: now.Add(time.Hour), Issuer: "https://mcp.example.test",
	}); err != nil {
		t.Fatalf("save token: %v", err)
	}
	if err := b.Pending.Save(ctx, &domain.PendingAuth{
		State: "st1", CodeVerifier: "verifier-1", Scopes: []string{"quant"},
		UserID: "u1", AgentID: "a1", ServerName: "quandora",
		ServerURL: "https://mcp.example.test/quant", CallbackURL: "https://app.example.com/oauth/mcp/x/callback",
		CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}); err != nil {
		t.Fatalf("save pending: %v", err)
	}

	// Home stays empty: DB storage must not leak file-store artifacts.
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatalf("read home: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("Home contains %d entries; want 0 (file store fallback must not be used)", len(entries))
	}

	// Restart: close the handle and reopen the same sqlite file as a new
	// process would — token and pending both survive.
	if err := db.Close(); err != nil {
		t.Fatalf("close first db: %v", err)
	}
	db2 := openSQLite(t, dsn)
	defer db2.Close()
	b2, err := Init(secret, Options{Home: t.TempDir(), DB: db2})
	if err != nil {
		t.Fatalf("Init (restart): %v", err)
	}

	tok, err := b2.Tokens.Load(ctx, key)
	if err != nil {
		t.Fatalf("load token after restart: %v", err)
	}
	if tok.AccessToken != "at-1" || tok.RefreshToken != "rt-1" {
		t.Fatalf("token after restart = %+v, want at-1/rt-1", tok)
	}
	p, err := b2.Pending.Take(ctx, "st1")
	if err != nil {
		t.Fatalf("take pending after restart: %v", err)
	}
	if p.CodeVerifier != "verifier-1" || p.UserID != "u1" {
		t.Fatalf("pending after restart = %+v, want verifier-1/u1", p)
	}
	// Take is single-use across the restart boundary too.
	if _, err := b2.Pending.Take(ctx, "st1"); !errors.Is(err, port.ErrNotFound) {
		t.Fatalf("second Take error = %v, want ErrNotFound", err)
	}
}
