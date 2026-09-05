package usecase

import (
	"context"
	"fmt"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/port"
)

// RevokeToken revokes credentials at the provider and removes them
// locally.
type RevokeToken struct {
	Meta     port.MetadataFetcher
	Regs     port.ClientRegistrationStore
	Tokens   port.TokenStore
	Exchange port.AuthorizationCodeExchanger
}

// Execute revokes (best-effort at the provider) and always deletes the
// local credential.
func (uc *RevokeToken) Execute(ctx context.Context, in RefreshInput) error {
	key := domain.StoreKey(in.UserID, in.AgentID, in.ServerName)
	tokens, err := uc.Tokens.Load(ctx, key)
	if err != nil {
		return err
	}
	// Without a server URL (e.g. CLI logout) skip the provider round-trip
	// and only remove the local credential.
	if in.ServerURL != "" {
		md, err := uc.Meta.Fetch(ctx, in.ServerURL)
		if err != nil {
			return fmt.Errorf("oauth: discovery: %w", err)
		}
		if reg, err := uc.Regs.GetAny(ctx, in.ServerName); err == nil {
			_ = uc.Exchange.Revoke(ctx, md.RevocationEndpoint, tokens.RefreshToken, reg.ClientID, in.ServerURL)
		}
	}
	return uc.Tokens.Delete(ctx, key)
}
