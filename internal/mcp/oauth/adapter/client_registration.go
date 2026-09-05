package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
)

// HTTPClientRegistrar performs RFC 7591 dynamic client registration.
type HTTPClientRegistrar struct {
	Client *http.Client
}

// Register POSTs a public-client registration for the given redirect URIs.
func (r *HTTPClientRegistrar) Register(ctx context.Context, endpoint string, redirectURIs []string) (*domain.ClientRegistration, error) {
	body, err := json.Marshal(map[string]any{
		"client_name":                "fastagent",
		"redirect_uris":              redirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"application_type":           "native",
		"token_endpoint_auth_method": "none",
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oauth: registration http %d", resp.StatusCode)
	}
	var raw struct {
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	if raw.ClientID == "" {
		return nil, fmt.Errorf("oauth: empty client_id")
	}
	return &domain.ClientRegistration{
		ClientID:                raw.ClientID,
		RedirectURIs:            redirectURIs,
		TokenEndpointAuthMethod: "none",
		RegisteredAt:            time.Now().UTC(),
	}, nil
}
