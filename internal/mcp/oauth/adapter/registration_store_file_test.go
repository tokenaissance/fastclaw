package adapter

import (
	"context"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/port"
)

func TestFileRegistrationStorePersistsAcrossInstances(t *testing.T) {
	root := t.TempDir()
	reg := &domain.ClientRegistration{
		ClientID:                "cid-1",
		RedirectURIs:            []string{"https://app.example.com/oauth/mcp/abc/callback"},
		TokenEndpointAuthMethod: "none",
	}
	if err := (&FileRegistrationStore{Root: root}).Save(context.Background(), "quandora", "https://app.example.com/oauth/mcp/abc/callback", reg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// New instance = restart; registration must be reused, not re-registered.
	got, err := (&FileRegistrationStore{Root: root}).Get(context.Background(), "quandora", "https://app.example.com/oauth/mcp/abc/callback")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ClientID != "cid-1" {
		t.Fatalf("client id = %q, want cid-1", got.ClientID)
	}

	// Same server + different callback must be a separate client entry.
	if _, err := (&FileRegistrationStore{Root: root}).Get(context.Background(), "quandora", "http://127.0.0.1:9999/callback/xyz"); err != port.ErrNotFound {
		t.Fatalf("different callback: got %v, want ErrNotFound", err)
	}
	any, err := (&FileRegistrationStore{Root: root}).GetAny(context.Background(), "quandora")
	if err != nil {
		t.Fatalf("GetAny: %v", err)
	}
	if any.ClientID != "cid-1" {
		t.Fatalf("GetAny client id = %q", any.ClientID)
	}
}
