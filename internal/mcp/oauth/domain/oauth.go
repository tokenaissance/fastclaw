// Package domain holds pure OAuth client logic with zero I/O and no
// external dependencies beyond the standard library. Every rule here is a
// pure function / model so it can be table-tested in isolation.
package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"time"
)

// OAuthTokens is the stored credential set for one (user, agent, server).
// It never leaves the MCP client layer and never enters agent context.
type OAuthTokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"` // access token expiry (UTC)
	Scopes       []string  `json:"scopes,omitempty"`
	Issuer       string    `json:"issuer,omitempty"`
}

// CallbackMode distinguishes the two RFC 9700 mix-up mitigations.
type CallbackMode int

const (
	// CallbackSpecific binds the callback path to hash(serverURL) — the
	// default for providers that don't declare iss support (Quandora).
	CallbackSpecific CallbackMode = iota
	// IssuerBound validates the iss parameter against metadata.issuer.
	IssuerBound
)

// PendingAuth is a one-shot in-flight authorization request. The state is
// cryptographically random and single-use; the code verifier never leaves
// the server.
type PendingAuth struct {
	State        string
	CodeVerifier string
	Scopes       []string
	UserID       string
	AgentID      string
	ServerName   string
	ServerURL    string
	CallbackURL  string
	Mode         CallbackMode
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

// Expired reports whether the pending authorization is past its TTL.
func (p *PendingAuth) Expired(now time.Time) bool { return now.After(p.ExpiresAt) }

// ClientRegistration is a dynamic-client registration (RFC 7591). Quandora
// is a public client: token_endpoint_auth_method is always "none".
type ClientRegistration struct {
	ClientID                string
	RedirectURIs            []string
	TokenEndpointAuthMethod string // 恒 "none"
	RegisteredAt            time.Time
}

// DiscoveryMetadata is the OAuth Authorization Server Metadata document
// (RFC 8414) subset the client needs.
type DiscoveryMetadata struct {
	Issuer                string
	AuthorizationEndpoint string
	TokenEndpoint         string
	RegistrationEndpoint  string
	RevocationEndpoint    string
	ScopesSupported       []string
	CodeChallengeMethods  []string
	IssParamSupported     bool // authorization_response_iss_parameter_supported
}

// CallbackParams carries the authorization response parameters back from
// the provider (via the public callback route, loopback receiver, or a
// pasted URL in headless mode).
type CallbackParams struct {
	Code       string
	State      string
	Issuer     string
	OAuthError string
}

// Domain errors — all fail closed.
var (
	ErrInvalidState   = errors.New("oauth: state mismatch or missing")
	ErrDenied         = errors.New("oauth: authorization denied")
	ErrMissingCode    = errors.New("oauth: missing authorization code")
	ErrIssuerMismatch = errors.New("oauth: issuer mismatch")
	ErrInvalidURL     = errors.New("oauth: invalid server url")
)

const (
	pendingAuthTTL = 10 * time.Minute
	// RefreshBuffer is how long before expiry the provider treats an
	// access token as stale and refreshes preemptively.
	RefreshBuffer = 60 * time.Second
)

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// NewPKCE generates a code_verifier (43 chars, RFC 7636).
func NewPKCE() (verifier string, err error) {
	return randomToken(32) // 32 bytes -> base64url 43 chars
}

// S256Challenge computes the PKCE S256 code_challenge for a verifier.
func S256Challenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// NewPendingAuth creates a fresh pending authorization with a random
// single-use state and PKCE verifier.
func NewPendingAuth(userID, agentID, serverName, serverURL, callbackURL string, scopes []string, mode CallbackMode) (*PendingAuth, error) {
	state, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	verifier, err := NewPKCE()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	return &PendingAuth{
		State:        state,
		CodeVerifier: verifier,
		Scopes:       scopes,
		UserID:       userID,
		AgentID:      agentID,
		ServerName:   serverName,
		ServerURL:    serverURL,
		CallbackURL:  callbackURL,
		Mode:         mode,
		CreatedAt:    now,
		ExpiresAt:    now.Add(pendingAuthTTL),
	}, nil
}

// BuildAuthorizationURL assembles the authorization endpoint URL with
// PKCE S256 + state + scopes.
func BuildAuthorizationURL(md *DiscoveryMetadata, reg *ClientRegistration, p *PendingAuth) (string, error) {
	if md == nil || md.AuthorizationEndpoint == "" {
		return "", errors.New("oauth: missing authorization endpoint")
	}
	if reg == nil || reg.ClientID == "" {
		return "", errors.New("oauth: missing client id")
	}
	if p.CallbackURL == "" {
		return "", errors.New("oauth: missing callback url")
	}
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", reg.ClientID)
	q.Set("redirect_uri", p.CallbackURL)
	q.Set("code_challenge", S256Challenge(p.CodeVerifier))
	q.Set("code_challenge_method", "S256")
	q.Set("state", p.State)
	if len(p.Scopes) > 0 {
		q.Set("scope", strings.Join(p.Scopes, " "))
	}
	// Resource indicator (RFC 8707): Quandora's authorization server
	// requires `resource` to identify the protected MCP resource (its
	// 422 without it is "query.resource: Field required"). Providers that
	// don't understand the parameter ignore it per OAuth's unknown-param
	// convention, so sending the MCP server URL is safe.
	if p.ServerURL != "" {
		q.Set("resource", p.ServerURL)
	}
	// RFC 9207: when the AS declares iss support, echo the issuer back in
	// the authorization request so the response's iss can be trusted.
	if p.Mode == IssuerBound && md.Issuer != "" {
		q.Set("iss", md.Issuer)
	}
	return md.AuthorizationEndpoint + "?" + q.Encode(), nil
}

// ParseCallbackURL extracts authorization response parameters from a full
// redirect URL — the headless ("paste the redirected URL") flow.
func ParseCallbackURL(raw string) (CallbackParams, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return CallbackParams{}, err
	}
	q := u.Query()
	return CallbackParams{
		Code:       q.Get("code"),
		State:      q.Get("state"),
		Issuer:     q.Get("iss"),
		OAuthError: q.Get("error"),
	}, nil
}

// ValidateCallback validates the authorization response against the
// expected state. Provider error descriptions are never echoed back to
// the user (they are attacker-controlled).
func ValidateCallback(p CallbackParams, expectedState string) error {
	if p.OAuthError != "" {
		return ErrDenied
	}
	if p.State == "" || p.State != expectedState {
		return ErrInvalidState
	}
	if p.Code == "" {
		return ErrMissingCode
	}
	return nil
}

// ValidateIssuer checks iss only when the provider declares support for
// the parameter; otherwise the callback-specific path binding is the
// mix-up mitigation.
func ValidateIssuer(md *DiscoveryMetadata, iss string) error {
	if md == nil || !md.IssParamSupported {
		return nil
	}
	if iss == "" || iss != md.Issuer {
		return ErrIssuerMismatch
	}
	return nil
}

// TokenNeedsRefresh reports whether a token is missing, empty, or within
// buffer of its expiry.
func TokenNeedsRefresh(t *OAuthTokens, now time.Time, buffer time.Duration) bool {
	if t == nil || t.AccessToken == "" || t.ExpiresAt.IsZero() {
		return true
	}
	return now.Add(buffer).After(t.ExpiresAt)
}

// StoreKey derives the per-(user, agent, server) credential key. Escaping
// keeps arbitrary IDs from walking the file tree.
func StoreKey(userID, agentID, serverName string) string {
	return "oauth/" + url.PathEscape(userID) + "/" + url.PathEscape(agentID) + "/" + url.PathEscape(serverName) + ".json"
}

// CallbackID derives a stable per-server callback path segment from the
// OAuth resource URL (Codex-style callback-specific mix-up protection).
func CallbackID(serverURL string) (string, error) {
	u, err := url.Parse(serverURL)
	if err != nil || u.Host == "" {
		return "", ErrInvalidURL
	}
	u.Fragment = ""
	h := sha256.Sum256([]byte(u.String()))
	return base64.RawURLEncoding.EncodeToString(h[:9]), nil
}

// AppendCallbackID appends the callback id to a redirect URI path.
func AppendCallbackID(redirectURI, id string) string {
	if redirectURI == "" {
		return redirectURI
	}
	if strings.HasSuffix(redirectURI, "/") {
		return redirectURI + id
	}
	return redirectURI + "/" + id
}
