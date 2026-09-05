package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/port"
)

// TestTokenProviderOwnerGate (scheme A): an OAuth-protected MCP credential
// belongs to the agent owner. A session whose acting user differs must be
// refused before any store read — otherwise a visitor attached to a public
// agent would silently run every call under the owner's Quandora account.
func TestTokenProviderOwnerGate(t *testing.T) {
	ctx := context.Background()
	tokens := newFakeTokens()
	key := domain.StoreKey("owner-1", "agent-1", "quandora")
	if err := tokens.Save(ctx, key, &domain.OAuthTokens{
		AccessToken:  "at-owner",
		RefreshToken: "rt-owner",
		ExpiresAt:    time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	provider := &TokenProvider{Tokens: tokens} // Refresh nil: gate fires before any refresh

	in := RefreshInput{
		UserID: "owner-1", AgentID: "agent-1",
		ServerName: "quandora", ServerURL: "https://mcp.example.test/quant",
	}

	// Owner's own session: allowed and returns the owner token.
	in.ActorUserID = "owner-1"
	tok, err := provider.AccessToken(ctx, in)
	if err != nil {
		t.Fatalf("owner session refused: %v", err)
	}
	if tok != "at-owner" {
		t.Fatalf("owner token = %q, want at-owner", tok)
	}

	// Visitor session on the same agent: refused with the owner-only error,
	// never touching the credential store.
	in.ActorUserID = "visitor-1"
	if _, err := provider.AccessToken(ctx, in); !errors.Is(err, ErrCredentialOwnerOnly) {
		t.Fatalf("visitor session error = %v, want ErrCredentialOwnerOnly", err)
	}

	// Empty actor = legacy single-user mode (no tenant separation): the
	// owner lookup still succeeds, preserving pre-gate behavior.
	in.ActorUserID = ""
	if _, err := provider.AccessToken(ctx, in); err != nil {
		t.Fatalf("legacy empty-actor session refused: %v", err)
	}

	// A visitor with no stored credential gets the owner-only error, not a
	// store "not found" — fail closed, no leak of credential presence.
	tokens2 := newFakeTokens()
	provider2 := &TokenProvider{Tokens: tokens2}
	in2 := RefreshInput{
		UserID: "owner-2", AgentID: "agent-2",
		ServerName: "quandora", ServerURL: "https://mcp.example.test/quant",
		ActorUserID: "visitor-2",
	}
	if _, err := provider2.AccessToken(ctx, in2); !errors.Is(err, ErrCredentialOwnerOnly) {
		t.Fatalf("visitor (no token) error = %v, want ErrCredentialOwnerOnly", err)
	}

	// Sanity: same store lookup from the owner surfaces the real not-found
	// error (the gate only masks it for foreign actors).
	in3 := RefreshInput{
		UserID: "owner-2", AgentID: "agent-2",
		ServerName: "quandora", ServerURL: "https://mcp.example.test/quant",
		ActorUserID: "owner-2",
	}
	if _, err := provider2.AccessToken(ctx, in3); !errors.Is(err, port.ErrNotFound) {
		t.Fatalf("owner missing-token error = %v, want port.ErrNotFound", err)
	}
}

// TestTokenProviderOwnerGateBlocksRefresh: even when the stored token is
// expired, a foreign actor must be refused BEFORE any refresh is attempted —
// otherwise a visitor session could burn an owner rotation (or observe that
// a credential exists at all). Owner sessions still refresh normally.
func TestTokenProviderOwnerGateBlocksRefresh(t *testing.T) {
	ctx := context.Background()
	start, complete, _, provider, _, _, tokens, _, exch, _ := newFlow()

	// Authorize owner-1/agent-1 (the fake exchanger issues an already-
	// expired access token, so the next AccessToken must refresh).
	out, err := start.Execute(ctx, StartAuthInput{
		UserID: "owner-1", AgentID: "agent-1",
		ServerName: "quandora", ServerURL: "https://mcp.example.test/quant",
		CallbackURL: "https://app.example.com/oauth/mcp/cb",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := complete.Execute(ctx, CompleteAuthInput{
		Callback: domain.CallbackParams{Code: "c1", State: out.State},
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	in := RefreshInput{
		UserID: "owner-1", AgentID: "agent-1",
		ServerName: "quandora", ServerURL: "https://mcp.example.test/quant",
		ActorUserID: "visitor-1",
	}
	if _, err := provider.AccessToken(ctx, in); !errors.Is(err, ErrCredentialOwnerOnly) {
		t.Fatalf("visitor on expired token error = %v, want ErrCredentialOwnerOnly", err)
	}
	if exch.refreshCount != 0 {
		t.Fatalf("refresh calls = %d, want 0 (gate must precede refresh)", exch.refreshCount)
	}

	// The owner's own session goes through: expired token triggers exactly
	// one refresh and returns the rotated access token.
	in.ActorUserID = "owner-1"
	tok, err := provider.AccessToken(ctx, in)
	if err != nil {
		t.Fatalf("owner refresh: %v", err)
	}
	if tok != "at-new" {
		t.Fatalf("owner token after refresh = %q, want at-new", tok)
	}
	if exch.refreshCount != 1 {
		t.Fatalf("refresh calls = %d, want 1", exch.refreshCount)
	}
	_ = tokens // store used indirectly by the flow
}
