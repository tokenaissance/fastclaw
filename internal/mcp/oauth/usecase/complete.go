package usecase

import (
	"context"
	"fmt"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/port"
)

// CompleteAuthorization finalizes the flow: consume the one-shot state,
// validate the callback, exchange the code for tokens, encrypt-store.
type CompleteAuthorization struct {
	Meta     port.MetadataFetcher
	Pending  port.PendingAuthStore
	Regs     port.ClientRegistrationStore
	Tokens   port.TokenStore
	Exchange port.AuthorizationCodeExchanger
}

// CompleteAuthInput carries the authorization response parameters.
type CompleteAuthInput struct {
	Callback domain.CallbackParams
}

// CompleteOutput identifies whose authorization was completed, so the
// framework can invalidate exactly that (user, agent) on every replica.
type CompleteOutput struct {
	UserID     string
	AgentID    string
	ServerName string
}

// Execute consumes the pending authorization and stores the tokens.
func (uc *CompleteAuthorization) Execute(ctx context.Context, in CompleteAuthInput) (CompleteOutput, error) {
	// Take consumes the state: replaying a callback with the same state is
	// impossible (single-use) even within the TTL.
	p, err := uc.Pending.Take(ctx, in.Callback.State)
	if err != nil {
		return CompleteOutput{}, fmt.Errorf("oauth: invalid or expired authorization request")
	}
	if err := domain.ValidateCallback(in.Callback, p.State); err != nil {
		return CompleteOutput{}, err
	}
	md, err := uc.Meta.Fetch(ctx, p.ServerURL)
	if err != nil {
		return CompleteOutput{}, fmt.Errorf("oauth: discovery: %w", err)
	}
	if err := domain.ValidateIssuer(md, in.Callback.Issuer); err != nil {
		return CompleteOutput{}, err
	}
	reg, err := uc.Regs.Get(ctx, p.ServerName, p.CallbackURL)
	if err != nil {
		return CompleteOutput{}, err
	}
	tokens, err := uc.Exchange.Exchange(ctx, md.TokenEndpoint, in.Callback.Code, p.CodeVerifier, p.CallbackURL, p.ServerURL, reg)
	if err != nil {
		return CompleteOutput{}, fmt.Errorf("oauth: token exchange: %w", err)
	}
	tokens.Issuer = md.Issuer
	if err := uc.Tokens.Save(ctx, domain.StoreKey(p.UserID, p.AgentID, p.ServerName), tokens); err != nil {
		return CompleteOutput{}, err
	}
	return CompleteOutput{UserID: p.UserID, AgentID: p.AgentID, ServerName: p.ServerName}, nil
}
