package usecase

import (
	"context"
	"errors"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/port"
)

// ErrCredentialOwnerOnly is returned when a session whose acting user is
// not the credential owner tries to use an OAuth-protected MCP server.
// Scheme A (owner authorizes once, usage gated to the owner's own
// sessions) refuses the lookup before any store read so a foreign
// session can never pull the owner's token.
var ErrCredentialOwnerOnly = errors.New("oauth: credential is authorized for the agent owner only")

// TokenProvider hands the MCP HTTP client a usable access token,
// refreshing preemptively when the stored one is near expiry.
type TokenProvider struct {
	Tokens  port.TokenStore
	Refresh *RefreshToken
}

// AccessToken returns a non-expired access token for the identity.
//
// Owner gate: when in.ActorUserID is non-empty and differs from
// in.UserID (the credential owner), access is refused. Multi-tenant
// deployments always stamp both; empty ActorUserID means legacy
// single-user mode where there is no separation to enforce.
func (p *TokenProvider) AccessToken(ctx context.Context, in RefreshInput) (string, error) {
	if in.ActorUserID != "" && in.UserID != "" && in.ActorUserID != in.UserID {
		return "", ErrCredentialOwnerOnly
	}
	key := domain.StoreKey(in.UserID, in.AgentID, in.ServerName)
	tokens, err := p.Tokens.Load(ctx, key)
	if err != nil {
		return "", err
	}
	if domain.TokenNeedsRefresh(tokens, time.Now().UTC(), domain.RefreshBuffer) {
		tokens, err = p.Refresh.Execute(ctx, in)
		if err != nil {
			return "", err
		}
	}
	return tokens.AccessToken, nil
}
