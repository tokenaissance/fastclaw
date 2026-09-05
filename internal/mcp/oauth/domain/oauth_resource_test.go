package domain

import (
	"net/url"
	"testing"
)

// TestBuildAuthorizationURLIssuerBound pins RFC 9207: when the AS declares
// iss support the authorization request echoes iss back; the callback-
// specific (default) mode must not add it.
func TestBuildAuthorizationURLIssuerBound(t *testing.T) {
	md := &DiscoveryMetadata{
		Issuer:                "https://as.example",
		AuthorizationEndpoint: "https://as.example/oauth/authorize",
		IssParamSupported:     true,
	}
	reg := &ClientRegistration{ClientID: "cid"}

	base := func(mode CallbackMode) *PendingAuth {
		return &PendingAuth{
			State: "s", CodeVerifier: "v", CallbackURL: "https://cb.example/x",
			ServerURL: "https://mcp.example/quant", Mode: mode,
		}
	}

	issuerBound, err := BuildAuthorizationURL(md, reg, base(IssuerBound))
	if err != nil {
		t.Fatalf("issuer-bound: %v", err)
	}
	q, err := url.Parse(issuerBound)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := q.Query().Get("iss"); got != "https://as.example" {
		t.Fatalf("issuer-bound iss = %q, want https://as.example", got)
	}
	if got := q.Query().Get("resource"); got != "https://mcp.example/quant" {
		t.Fatalf("resource = %q, want https://mcp.example/quant", got)
	}

	callbackSpecific, err := BuildAuthorizationURL(md, reg, base(CallbackSpecific))
	if err != nil {
		t.Fatalf("callback-specific: %v", err)
	}
	q2, _ := url.Parse(callbackSpecific)
	if got := q2.Query().Get("iss"); got != "" {
		t.Fatalf("callback-specific must not send iss, got %q", got)
	}
}
