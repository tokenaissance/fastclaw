package usecase

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/port"
)

// StartAuthorization orchestrates discovery -> client registration ->
// pending auth -> authorization URL.
type StartAuthorization struct {
	Meta              port.MetadataFetcher
	Registrar         port.ClientRegistrar
	Regs              port.ClientRegistrationStore
	Pending           port.PendingAuthStore
	MaxPendingPerUser int // 0 = unlimited
}

// StartAuthInput describes who is authorizing which server via which
// callback.
type StartAuthInput struct {
	UserID      string
	AgentID     string
	ServerName  string
	ServerURL   string
	CallbackURL string
	// CallbackBase is the web console's public callback base
	// (e.g. https://app.tokenaissance.com/oauth/mcp). When CallbackURL is
	// empty, fastagent appends /{callbackID}/callback so the cloud never
	// has to reimplement the hash.
	CallbackBase string
	Scopes       []string
}

// StartAuthOutput is what the UI/CLI needs to send the user to the
// provider's consent page.
type StartAuthOutput struct {
	AuthURL     string
	State       string
	CallbackURL string
}

// Execute runs the authorization start flow.
func (uc *StartAuthorization) Execute(ctx context.Context, in StartAuthInput) (StartAuthOutput, error) {
	if in.ServerURL == "" {
		return StartAuthOutput{}, errors.New("oauth: serverURL is required")
	}
	callbackURL := in.CallbackURL
	if callbackURL == "" && in.CallbackBase != "" {
		id, err := domain.CallbackID(in.ServerURL)
		if err != nil {
			return StartAuthOutput{}, err
		}
		callbackURL = strings.TrimRight(in.CallbackBase, "/") + "/" + id + "/callback"
	}
	if callbackURL == "" {
		return StartAuthOutput{}, errors.New("oauth: callbackURL (or callbackBase) is required")
	}
	in.CallbackURL = callbackURL
	md, err := uc.Meta.Fetch(ctx, in.ServerURL)
	if err != nil {
		return StartAuthOutput{}, fmt.Errorf("oauth: discovery: %w", err)
	}
	if md.RegistrationEndpoint == "" {
		return StartAuthOutput{}, errors.New("oauth: provider does not support dynamic client registration")
	}

	// Reuse an existing registration for this exact callback; otherwise
	// register a new client bound to this callback URL.
	reg, err := uc.Regs.Get(ctx, in.ServerName, in.CallbackURL)
	if errors.Is(err, port.ErrNotFound) {
		reg, err = uc.Registrar.Register(ctx, md.RegistrationEndpoint, []string{in.CallbackURL})
		if err != nil {
			return StartAuthOutput{}, fmt.Errorf("oauth: register client: %w", err)
		}
		if err := uc.Regs.Save(ctx, in.ServerName, in.CallbackURL, reg); err != nil {
			return StartAuthOutput{}, err
		}
	} else if err != nil {
		return StartAuthOutput{}, err
	}

	mode := domain.CallbackSpecific
	if md.IssParamSupported {
		mode = domain.IssuerBound
	}
	if uc.MaxPendingPerUser > 0 {
		n, err := uc.Pending.CountActive(ctx, in.UserID)
		if err != nil {
			return StartAuthOutput{}, fmt.Errorf("oauth: count pending: %w", err)
		}
		if n >= uc.MaxPendingPerUser {
			return StartAuthOutput{}, fmt.Errorf("oauth: too many pending authorizations for user (max %d); complete or wait for an existing one to expire", uc.MaxPendingPerUser)
		}
	}
	scopes := in.Scopes
	if len(scopes) == 0 {
		scopes = md.ScopesSupported
	}
	p, err := domain.NewPendingAuth(in.UserID, in.AgentID, in.ServerName, in.ServerURL, in.CallbackURL, scopes, mode)
	if err != nil {
		return StartAuthOutput{}, err
	}
	if err := uc.Pending.Save(ctx, p); err != nil {
		return StartAuthOutput{}, err
	}

	authURL, err := domain.BuildAuthorizationURL(md, reg, p)
	if err != nil {
		return StartAuthOutput{}, err
	}
	return StartAuthOutput{AuthURL: authURL, State: p.State, CallbackURL: callbackURL}, nil
}
