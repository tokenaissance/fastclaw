package adapter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/port"
)

func TestFileTokenStorePersistsAcrossInstances(t *testing.T) {
	root := t.TempDir()
	crypt, _ := NewAESGCMCryptor("s")
	tok := &domain.OAuthTokens{
		AccessToken:  "at-1",
		RefreshToken: "rt-1",
		ExpiresAt:    time.Now().UTC().Add(time.Hour),
		Scopes:       []string{"quant"},
		Issuer:       "https://mcp.quandora.ai",
	}
	key := "oauth/u1/a1/quandora.json"

	if err := (&FileTokenStore{Root: root, Crypt: crypt}).Save(context.Background(), key, tok); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// A brand-new store instance (daemon restart) must read it back.
	got, err := (&FileTokenStore{Root: root, Crypt: crypt}).Load(context.Background(), key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.AccessToken != "at-1" || got.RefreshToken != "rt-1" || len(got.Scopes) != 1 {
		t.Fatalf("unexpected tokens: %+v", got)
	}

	// File must be 0600 and the plaintext must not appear on disk.
	path := filepath.Join(root, filepath.FromSlash(key))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %o, want 600", info.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "at-1") {
		t.Fatal("plaintext token found on disk")
	}

	if _, err := (&FileTokenStore{Root: root, Crypt: crypt}).Load(context.Background(), "oauth/nope.json"); err != port.ErrNotFound {
		t.Fatalf("missing key: got %v, want ErrNotFound", err)
	}
	if err := (&FileTokenStore{Root: root, Crypt: crypt}).Delete(context.Background(), key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("file should be gone after Delete")
	}
}
