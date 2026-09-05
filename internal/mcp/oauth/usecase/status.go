package usecase

import (
	"context"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/port"
)

// TokenStatus describes the local credential state for the UI.
type TokenStatus string

const (
	StatusNone    TokenStatus = "none"
	StatusValid   TokenStatus = "authorized"
	StatusExpired TokenStatus = "expired"
)

// Status queries whether a server is authorized for the identity.
type Status struct {
	Tokens port.TokenStore
}

// StatusOutput is the status response.
type StatusOutput struct {
	Status    TokenStatus
	Scopes    []string
	Issuer    string
	ExpiresAt time.Time
}

// Execute returns the stored credential state (never the tokens).
func (uc *Status) Execute(ctx context.Context, in RefreshInput) (StatusOutput, error) {
	key := domain.StoreKey(in.UserID, in.AgentID, in.ServerName)
	tokens, err := uc.Tokens.Load(ctx, key)
	if err != nil {
		if err == port.ErrNotFound {
			return StatusOutput{Status: StatusNone}, nil
		}
		return StatusOutput{}, err
	}
	out := StatusOutput{
		Status:    StatusValid,
		Scopes:    tokens.Scopes,
		Issuer:    tokens.Issuer,
		ExpiresAt: tokens.ExpiresAt,
	}
	if tokens.ExpiresAt.IsZero() || time.Now().UTC().Add(domain.RefreshBuffer).After(tokens.ExpiresAt) {
		out.Status = StatusExpired
	}
	return out, nil
}
