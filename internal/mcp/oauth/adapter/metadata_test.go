package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPMetadataFetcherWellKnown(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/oauth-authorization-server" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 srv.URL,
			"authorization_endpoint": srv.URL + "/authorize",
			"token_endpoint":         srv.URL + "/token",
		})
	}))
	defer srv.Close()

	md, err := (&HTTPMetadataFetcher{Client: srv.Client()}).Fetch(context.Background(), srv.URL+"/quant")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if md.AuthorizationEndpoint == "" || md.TokenEndpoint == "" {
		t.Fatalf("incomplete metadata: %+v", md)
	}
}

func TestHTTPMetadataFetcherWWWAuthenticateFallback(t *testing.T) {
	// Issuer server carries the real discovery document.
	var issuer *httptest.Server
	issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/oauth-authorization-server" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 issuer.URL,
			"authorization_endpoint": issuer.URL + "/authorize",
			"token_endpoint":         issuer.URL + "/token",
		})
	}))
	defer issuer.Close()

	// Resource server has no well-known doc but advertises the issuer.
	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-authorization-server" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer auth-issuer="`+issuer.URL+`"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer resource.Close()

	md, err := (&HTTPMetadataFetcher{Client: resource.Client()}).Fetch(context.Background(), resource.URL+"/quant")
	if err != nil {
		t.Fatalf("fetch with fallback: %v", err)
	}
	if md.Issuer != issuer.URL || md.AuthorizationEndpoint == "" {
		t.Fatalf("fallback metadata wrong: %+v", md)
	}
}
