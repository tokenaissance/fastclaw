package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
)

// HTTPCodeExchanger implements the token endpoint: code exchange, refresh,
// and revocation.
type HTTPCodeExchanger struct {
	Client *http.Client
}

// Exchange trades the authorization code for tokens (PKCE).
func (e *HTTPCodeExchanger) Exchange(ctx context.Context, tokenEndpoint, code, codeVerifier, redirectURI, resource string, reg *domain.ClientRegistration) (*domain.OAuthTokens, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("code_verifier", codeVerifier)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", reg.ClientID)
	if resource != "" {
		// RFC 8707 resource indicator — some ASs (Quandora) reject the
		// exchange without it.
		form.Set("resource", resource)
	}
	return e.postToken(ctx, tokenEndpoint, form)
}

// Refresh rotates an access token with a refresh token.
func (e *HTTPCodeExchanger) Refresh(ctx context.Context, tokenEndpoint string, tokens *domain.OAuthTokens, resource string, reg *domain.ClientRegistration) (*domain.OAuthTokens, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", tokens.RefreshToken)
	form.Set("client_id", reg.ClientID)
	if resource != "" {
		form.Set("resource", resource)
	}
	return e.postToken(ctx, tokenEndpoint, form)
}

// Revoke revokes a token at the provider. A missing revocation endpoint
// is treated as success (local deletion still happens).
func (e *HTTPCodeExchanger) Revoke(ctx context.Context, revocationEndpoint, token, clientID, resource string) error {
	if revocationEndpoint == "" {
		return nil
	}
	form := url.Values{}
	form.Set("token", token)
	form.Set("client_id", clientID)
	if resource != "" {
		form.Set("resource", resource)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, revocationEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := e.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("oauth: revocation http %d", resp.StatusCode)
	}
	return nil
}

func (e *HTTPCodeExchanger) postToken(ctx context.Context, endpoint string, form url.Values) (*domain.OAuthTokens, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := e.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Surface the provider's error body (truncated) so token-endpoint
		// rejections are diagnosable end-to-end (invalid_grant,
		// redirect_uri_mismatch, invalid_client, …).
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(raw))
		if msg == "" {
			return nil, fmt.Errorf("oauth: token endpoint http %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("oauth: token endpoint http %d: %s", resp.StatusCode, msg)
	}
	var raw struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	if raw.AccessToken == "" {
		return nil, fmt.Errorf("oauth: empty access token")
	}
	tokens := &domain.OAuthTokens{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
	}
	if raw.ExpiresIn > 0 {
		tokens.ExpiresAt = time.Now().UTC().Add(time.Duration(raw.ExpiresIn) * time.Second)
	}
	if raw.Scope != "" {
		tokens.Scopes = strings.Fields(raw.Scope)
	}
	return tokens, nil
}
