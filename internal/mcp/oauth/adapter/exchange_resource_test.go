package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
)

// TestHTTPCodeExchangerCarriesResourceIndicator pins RFC 8707 behavior:
// the resource indicator is sent on token exchange, refresh, and revoke —
// ASs like Quandora reject the exchange with "missing token request field"
// when it is absent.
func TestHTTPCodeExchangerCarriesResourceIndicator(t *testing.T) {
	const resource = "https://mcp.quandora.ai/quant"
	seen := map[string]url.Values{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		seen[r.URL.Path] = r.PostForm
		switch r.URL.Path {
		case "/token":
			json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "at",
				"refresh_token": "rt",
				"expires_in":    3600,
				"scope":         "quant",
			})
		case "/revoke":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	e := &HTTPCodeExchanger{Client: srv.Client()}
	reg := &domain.ClientRegistration{ClientID: "cid-1"}

	if _, err := e.Exchange(context.Background(), srv.URL+"/token", "code-1", "verifier-1", "https://cb.example/oauth", resource, reg); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if _, err := e.Refresh(context.Background(), srv.URL+"/token", &domain.OAuthTokens{RefreshToken: "rt"}, resource, reg); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if err := e.Revoke(context.Background(), srv.URL+"/revoke", "rt", "cid-1", resource); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	for _, path := range []string{"/token", "/revoke"} {
		if got := seen[path].Get("resource"); got != resource {
			t.Fatalf("%s resource = %q, want %q (form: %v)", path, got, resource, seen[path])
		}
	}
	// Exchange carries the full PKCE + redirect set; refresh carries the
	// rotated grant; revoke identifies the token.
	if got := seen["/token"].Get("grant_type"); got == "" {
		t.Fatal("token request missing grant_type")
	}
	if got := seen["/token"].Get("client_id"); got != "cid-1" {
		t.Fatalf("client_id = %q, want cid-1", got)
	}
}

// TestHTTPCodeExchangerOmitsEmptyResource keeps the legacy behavior for
// flows without a known resource (CLI logout without oauthResource).
func TestHTTPCodeExchangerOmitsEmptyResource(t *testing.T) {
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at"})
	}))
	defer srv.Close()

	e := &HTTPCodeExchanger{Client: srv.Client()}
	if _, err := e.Exchange(context.Background(), srv.URL, "code-1", "v", "https://cb", "", &domain.ClientRegistration{ClientID: "cid"}); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if form.Get("resource") != "" {
		t.Fatalf("resource should be omitted when empty, got %q", form.Get("resource"))
	}
	if got := form.Get("code_verifier"); got != "v" {
		t.Fatalf("code_verifier = %q, want v", got)
	}
}
