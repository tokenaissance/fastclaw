package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
)

// HTTPMetadataFetcher fetches RFC 8414 discovery documents.
type HTTPMetadataFetcher struct {
	Client *http.Client
}

// Fetch GETs {origin}/.well-known/oauth-authorization-server.
func (f *HTTPMetadataFetcher) Fetch(ctx context.Context, serverURL string) (*domain.DiscoveryMetadata, error) {
	md, err := f.fetchWellKnown(ctx, serverURL)
	if err == nil {
		return md, nil
	}
	// RFC 8414 resource metadata fallback: when {origin}/.well-known/
	// oauth-authorization-server is missing, the resource itself may
	// advertise the issuer via WWW-Authenticate: Bearer auth-issuer=….
	if issuer, werr := f.issuerFromResource(ctx, serverURL); werr == nil && issuer != "" {
		if md, derr := f.fetchWellKnown(ctx, issuer); derr == nil {
			return md, nil
		}
	}
	return nil, err
}

var authIssuerRe = regexp.MustCompile(`auth-issuer\s*=\s*"?([^",\s]+)"?`)

func (f *HTTPMetadataFetcher) issuerFromResource(ctx context.Context, serverURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serverURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	for _, h := range resp.Header.Values("WWW-Authenticate") {
		if m := authIssuerRe.FindStringSubmatch(h); m != nil {
			return m[1], nil
		}
	}
	return "", fmt.Errorf("oauth: no auth-issuer in WWW-Authenticate")
}

func (f *HTTPMetadataFetcher) fetchWellKnown(ctx context.Context, serverURL string) (*domain.DiscoveryMetadata, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, err
	}
	u.Path = "/.well-known/oauth-authorization-server"
	u.RawQuery, u.Fragment = "", ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oauth: discovery http %d", resp.StatusCode)
	}
	var raw struct {
		Issuer                string   `json:"issuer"`
		AuthorizationEndpoint string   `json:"authorization_endpoint"`
		TokenEndpoint         string   `json:"token_endpoint"`
		RegistrationEndpoint  string   `json:"registration_endpoint"`
		RevocationEndpoint    string   `json:"revocation_endpoint"`
		ScopesSupported       []string `json:"scopes_supported"`
		CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
		IssParamSupported     bool     `json:"authorization_response_iss_parameter_supported"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	if raw.AuthorizationEndpoint == "" || raw.TokenEndpoint == "" {
		return nil, fmt.Errorf("oauth: discovery document missing authorization/token endpoint")
	}
	return &domain.DiscoveryMetadata{
		Issuer:                raw.Issuer,
		AuthorizationEndpoint: raw.AuthorizationEndpoint,
		TokenEndpoint:         raw.TokenEndpoint,
		RegistrationEndpoint:  raw.RegistrationEndpoint,
		RevocationEndpoint:    raw.RevocationEndpoint,
		ScopesSupported:       raw.ScopesSupported,
		CodeChallengeMethods:  raw.CodeChallengeMethods,
		IssParamSupported:     raw.IssParamSupported,
	}, nil
}

// CachingMetadataFetcher wraps a fetcher with a per-URL TTL cache so high
// fan-out (every MCP request) doesn't re-fetch discovery each time.
type CachingMetadataFetcher struct {
	inner MetadataFetchFunc
	ttl   time.Duration
	mu    sync.Mutex
	cache map[string]cacheEntry
}

// MetadataFetchFunc matches port.MetadataFetcher.Fetch.
type MetadataFetchFunc func(ctx context.Context, serverURL string) (*domain.DiscoveryMetadata, error)

type cacheEntry struct {
	md    *domain.DiscoveryMetadata
	until time.Time
}

// NewCachingMetadataFetcher builds a cached fetcher over an HTTP fetcher.
func NewCachingMetadataFetcher(cli *http.Client, ttl time.Duration) *CachingMetadataFetcher {
	return &CachingMetadataFetcher{
		inner: (&HTTPMetadataFetcher{Client: cli}).Fetch,
		ttl:   ttl,
		cache: make(map[string]cacheEntry),
	}
}

// Fetch returns a cached document when fresh, else fetches and caches.
func (c *CachingMetadataFetcher) Fetch(ctx context.Context, serverURL string) (*domain.DiscoveryMetadata, error) {
	now := time.Now()
	c.mu.Lock()
	if e, ok := c.cache[serverURL]; ok && now.Before(e.until) {
		md := e.md
		c.mu.Unlock()
		return md, nil
	}
	c.mu.Unlock()

	md, err := c.inner(ctx, serverURL)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.cache[serverURL] = cacheEntry{md: md, until: now.Add(c.ttl)}
	c.mu.Unlock()
	return md, nil
}
