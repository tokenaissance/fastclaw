package domain

import (
	"strings"
	"testing"
	"time"
)

func TestNewPKCE(t *testing.T) {
	v1, err := NewPKCE()
	if err != nil {
		t.Fatalf("NewPKCE: %v", err)
	}
	v2, err := NewPKCE()
	if err != nil {
		t.Fatalf("NewPKCE: %v", err)
	}
	if v1 == v2 {
		t.Fatal("two verifiers should differ")
	}
	if len(v1) != 43 {
		t.Fatalf("verifier length = %d, want 43", len(v1))
	}
	if S256Challenge(v1) == "" || S256Challenge(v1) == v1 {
		t.Fatal("challenge must be non-empty and different from verifier")
	}
}

func TestBuildAuthorizationURL(t *testing.T) {
	p, err := NewPendingAuth("u1", "a1", "quandora", "https://mcp.quandora.ai/quant", "http://127.0.0.1:9999/callback/x", []string{"quant"}, CallbackSpecific)
	if err != nil {
		t.Fatalf("NewPendingAuth: %v", err)
	}
	md := &DiscoveryMetadata{AuthorizationEndpoint: "https://mcp.quandora.ai/oauth/authorize"}
	reg := &ClientRegistration{ClientID: "cid", RedirectURIs: []string{p.CallbackURL}}
	u, err := BuildAuthorizationURL(md, reg, p)
	if err != nil {
		t.Fatalf("BuildAuthorizationURL: %v", err)
	}
	for _, want := range []string{
		"response_type=code",
		"client_id=cid",
		"redirect_uri=http%3A%2F%2F127.0.0.1%3A9999%2Fcallback%2Fx",
		"code_challenge=" + S256Challenge(p.CodeVerifier),
		"code_challenge_method=S256",
		"state=" + p.State,
		"scope=quant",
		"resource=https%3A%2F%2Fmcp.quandora.ai%2Fquant",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("auth URL missing %q: %s", want, u)
		}
	}
}

func TestValidateCallback(t *testing.T) {
	valid := CallbackParams{Code: "code", State: "s1"}
	cases := []struct {
		name    string
		params  CallbackParams
		state   string
		wantErr error
	}{
		{"ok", valid, "s1", nil},
		{"missing state", CallbackParams{Code: "code"}, "s1", ErrInvalidState},
		{"state mismatch", CallbackParams{Code: "code", State: "s2"}, "s1", ErrInvalidState},
		{"missing code", CallbackParams{State: "s1"}, "s1", ErrMissingCode},
		{"denied", CallbackParams{State: "s1", OAuthError: "access_denied"}, "s1", ErrDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateCallback(tc.params, tc.state); err != tc.wantErr {
				t.Fatalf("got %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateIssuer(t *testing.T) {
	md := &DiscoveryMetadata{Issuer: "https://mcp.quandora.ai", IssParamSupported: true}
	if err := ValidateIssuer(md, "https://mcp.quandora.ai"); err != nil {
		t.Fatalf("matching issuer rejected: %v", err)
	}
	if err := ValidateIssuer(md, "https://evil.example"); err != ErrIssuerMismatch {
		t.Fatalf("mismatched issuer: got %v", err)
	}
	noIss := &DiscoveryMetadata{Issuer: "https://mcp.quandora.ai"}
	if err := ValidateIssuer(noIss, "anything"); err != nil {
		t.Fatalf("issuer must be skipped when unsupported: %v", err)
	}
}

func TestTokenNeedsRefresh(t *testing.T) {
	now := time.Now().UTC()
	valid := &OAuthTokens{AccessToken: "tok", ExpiresAt: now.Add(10 * time.Minute)}
	if TokenNeedsRefresh(valid, now, 60*time.Second) {
		t.Fatal("valid token flagged for refresh")
	}
	expiring := &OAuthTokens{AccessToken: "tok", ExpiresAt: now.Add(30 * time.Second)}
	if !TokenNeedsRefresh(expiring, now, 60*time.Second) {
		t.Fatal("token inside buffer should refresh")
	}
	if !TokenNeedsRefresh(&OAuthTokens{}, now, 60*time.Second) {
		t.Fatal("empty token should refresh")
	}
	if !TokenNeedsRefresh(nil, now, 60*time.Second) {
		t.Fatal("nil token should refresh")
	}
}

func TestCallbackIDStableAndBound(t *testing.T) {
	id1, err := CallbackID("https://mcp.quandora.ai/quant")
	if err != nil {
		t.Fatalf("CallbackID: %v", err)
	}
	id2, _ := CallbackID("https://mcp.quandora.ai/quant")
	if id1 != id2 {
		t.Fatal("callback id must be stable")
	}
	other, _ := CallbackID("https://other.example/quant")
	if id1 == other {
		t.Fatal("different servers must have different callback ids")
	}
	if _, err := CallbackID("not a url"); err == nil {
		t.Fatal("invalid url must error")
	}
}

func TestParseCallbackURLAndStoreKey(t *testing.T) {
	params, err := ParseCallbackURL("http://127.0.0.1:9999/callback/x?code=abc&state=xyz&iss=issuer")
	if err != nil {
		t.Fatalf("ParseCallbackURL: %v", err)
	}
	if params.Code != "abc" || params.State != "xyz" || params.Issuer != "issuer" {
		t.Fatalf("unexpected params: %+v", params)
	}
	key := StoreKey("user@x", "agent/1", "quandora")
	if !strings.HasPrefix(key, "oauth/") || !strings.HasSuffix(key, ".json") {
		t.Fatalf("unexpected store key: %s", key)
	}
}
