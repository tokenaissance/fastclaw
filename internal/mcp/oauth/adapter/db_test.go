package adapter

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/port"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// openSharedDB opens a sqlite DB and returns TWO independent handles to
// the same file — the minimal "two gateway instances sharing storage"
// simulation.
func openSharedDB(t *testing.T) (*store.DBStore, *store.DBStore) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "shared.db")
	open := func() *store.DBStore {
		st, err := store.New(&store.StorageConfig{Type: "sqlite", DSN: dsn, AutoMigrate: true}, t.TempDir())
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		db, ok := st.(*store.DBStore)
		if !ok {
			t.Fatalf("store is %T, want *DBStore", st)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	return open(), open()
}

func newCrypt(t *testing.T) *AESGCMCryptor {
	t.Helper()
	c, err := NewAESGCMCryptor("db-test-secret")
	if err != nil {
		t.Fatalf("cryptor: %v", err)
	}
	return c
}

func TestDBTokenStoreSharedAcrossInstances(t *testing.T) {
	a, b := openSharedDB(t)
	crypt := newCrypt(t)
	sa := &DBTokenStore{DB: a.DB(), Dialect: a.Dialect(), Crypt: crypt}
	sb := &DBTokenStore{DB: b.DB(), Dialect: b.Dialect(), Crypt: crypt}
	ctx := context.Background()
	key := "oauth/u1/a1/quandora.json"

	if err := sa.Save(ctx, key, &domain.OAuthTokens{
		AccessToken: "at", RefreshToken: "rt", ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("save on A: %v", err)
	}
	got, err := sb.Load(ctx, key) // read on B
	if err != nil {
		t.Fatalf("load on B: %v", err)
	}
	if got.AccessToken != "at" || got.RefreshToken != "rt" {
		t.Fatalf("unexpected tokens: %+v", got)
	}

	// Ciphertext on disk must not leak plaintext.
	var raw []byte
	if err := a.DB().QueryRowContext(ctx, "SELECT ciphertext FROM mcp_oauth_tokens WHERE token_key = ?", key).Scan(&raw); err != nil {
		t.Fatalf("query raw: %v", err)
	}
	if strings.Contains(string(raw), "at") || strings.Contains(string(raw), "rt") {
		t.Fatal("plaintext token found in DB")
	}

	if err := sb.Delete(ctx, key); err != nil {
		t.Fatalf("delete on B: %v", err)
	}
	if _, err := sa.Load(ctx, key); err != port.ErrNotFound {
		t.Fatalf("load after delete: got %v, want ErrNotFound", err)
	}
}

func TestDBPendingStoreSharedSingleUse(t *testing.T) {
	a, b := openSharedDB(t)
	crypt := newCrypt(t)
	sa := &DBPendingStore{DB: a.DB(), Dialect: a.Dialect(), Crypt: crypt}
	sb := &DBPendingStore{DB: b.DB(), Dialect: b.Dialect(), Crypt: crypt}
	ctx := context.Background()
	p, err := domain.NewPendingAuth("u1", "a1", "quandora", "https://mcp.quandora.ai/quant", "https://app.example.com/cb", []string{"quant"}, domain.CallbackSpecific)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}

	if err := sa.Save(ctx, p); err != nil {
		t.Fatalf("save on A: %v", err)
	}
	got, err := sb.Take(ctx, p.State) // consume on B
	if err != nil {
		t.Fatalf("take on B: %v", err)
	}
	if got.UserID != "u1" || got.CodeVerifier != p.CodeVerifier {
		t.Fatalf("unexpected pending: %+v", got)
	}
	// Single-use across instances: second take must fail.
	if _, err := sa.Take(ctx, p.State); err != port.ErrNotFound {
		t.Fatalf("second take: got %v, want ErrNotFound", err)
	}
}

func TestDBPendingStoreCountActiveAndTTL(t *testing.T) {
	a, _ := openSharedDB(t)
	crypt := newCrypt(t)
	s := &DBPendingStore{DB: a.DB(), Dialect: a.Dialect(), Crypt: crypt}
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		p, err := domain.NewPendingAuth("u1", "a1", "quandora", "https://mcp.quandora.ai/quant", "https://app.example.com/cb", nil, domain.CallbackSpecific)
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		if i == 2 {
			p.ExpiresAt = time.Now().UTC().Add(-time.Minute) // expired
		}
		if err := s.Save(ctx, p); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	n, err := s.CountActive(ctx, "u1")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("active = %d, want 2 (expired lazily purged)", n)
	}
	other, _ := domain.NewPendingAuth("u2", "a2", "quandora", "https://mcp.quandora.ai/quant", "https://app.example.com/cb2", nil, domain.CallbackSpecific)
	if err := s.Save(ctx, other); err != nil {
		t.Fatalf("save other: %v", err)
	}
	n, _ = s.CountActive(ctx, "u1")
	if n != 2 {
		t.Fatalf("u1 active = %d, want 2", n)
	}
	n, _ = s.CountActive(ctx, "u2")
	if n != 1 {
		t.Fatalf("u2 active = %d, want 1", n)
	}
}

func TestDBRegistrationStoreShared(t *testing.T) {
	a, b := openSharedDB(t)
	sa := &DBRegistrationStore{DB: a.DB(), Dialect: a.Dialect()}
	sb := &DBRegistrationStore{DB: b.DB(), Dialect: b.Dialect()}
	ctx := context.Background()
	reg := &domain.ClientRegistration{ClientID: "cid-9", RedirectURIs: []string{"https://app.example.com/cb"}, TokenEndpointAuthMethod: "none"}

	if err := sa.Save(ctx, "quandora", "https://app.example.com/cb", reg); err != nil {
		t.Fatalf("save on A: %v", err)
	}
	got, err := sb.Get(ctx, "quandora", "https://app.example.com/cb")
	if err != nil {
		t.Fatalf("get on B: %v", err)
	}
	if got.ClientID != "cid-9" {
		t.Fatalf("client id = %q", got.ClientID)
	}
	any, err := sb.GetAny(ctx, "quandora")
	if err != nil || any.ClientID != "cid-9" {
		t.Fatalf("getany: %+v, %v", any, err)
	}
}

// TestSQLPlaceholderDialects pins the dialect-aware placeholder mapping
// shared by every SQL store: Postgres uses $N, SQLite uses "?". The same
// adapters run against both — "shared DB instances" is not Postgres-only.
func TestSQLPlaceholderDialects(t *testing.T) {
	for n := 1; n <= 3; n++ {
		if got, want := ph("postgres", n), fmt.Sprintf("$%d", n); got != want {
			t.Fatalf("postgres placeholder %d = %q, want %q", n, got, want)
		}
		if got, want := ph("sqlite", n), "?"; got != want {
			t.Fatalf("sqlite placeholder %d = %q, want %q", n, got, want)
		}
	}
	// Unknown dialect degrades to the sqlite form, never to $N.
	if got, want := ph("", 2), "?"; got != want {
		t.Fatalf("default placeholder = %q, want %q", got, want)
	}
}
