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

func newTestPending(t *testing.T, root string, crypt *AESGCMCryptor, expiresIn time.Duration) *domain.PendingAuth {
	t.Helper()
	p, err := domain.NewPendingAuth("u1", "a1", "quandora", "https://mcp.quandora.ai/quant", "https://app.example.com/oauth/mcp/abc/callback", []string{"quant"}, domain.CallbackSpecific)
	if err != nil {
		t.Fatalf("NewPendingAuth: %v", err)
	}
	if expiresIn != 0 {
		p.ExpiresAt = time.Now().UTC().Add(expiresIn)
	}
	return p
}

func TestFilePendingStorePersistsAcrossInstances(t *testing.T) {
	root := t.TempDir()
	crypt, _ := NewAESGCMCryptor("s")
	p := newTestPending(t, root, crypt, time.Minute)

	if err := (&FilePendingStore{Root: root, Crypt: crypt}).Save(context.Background(), p); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Plaintext state/verifier must never hit disk.
	raw, _ := os.ReadFile(filepath.Join(root, "oauth", "pending.json"))
	if strings.Contains(string(raw), p.State) || strings.Contains(string(raw), p.CodeVerifier) {
		t.Fatal("pending file contains plaintext state or verifier")
	}

	// Brand-new store instance = daemon restart; state must survive.
	got, err := (&FilePendingStore{Root: root, Crypt: crypt}).Take(context.Background(), p.State)
	if err != nil {
		t.Fatalf("Take after restart: %v", err)
	}
	if got.State != p.State || got.CodeVerifier != p.CodeVerifier || got.UserID != "u1" {
		t.Fatalf("unexpected pending: %+v", got)
	}

	// Single-use: second Take must fail.
	if _, err := (&FilePendingStore{Root: root, Crypt: crypt}).Take(context.Background(), p.State); err != port.ErrNotFound {
		t.Fatalf("second Take: got %v, want ErrNotFound", err)
	}
}

func TestFilePendingStoreTTLExpiry(t *testing.T) {
	root := t.TempDir()
	crypt, _ := NewAESGCMCryptor("s")
	p := newTestPending(t, root, crypt, -time.Minute) // already expired
	if err := (&FilePendingStore{Root: root, Crypt: crypt}).Save(context.Background(), p); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := (&FilePendingStore{Root: root, Crypt: crypt}).Take(context.Background(), p.State); err != port.ErrNotFound {
		t.Fatalf("expired Take: got %v, want ErrNotFound", err)
	}
}
