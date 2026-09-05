package usecase

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/port"
)

// fakeMeta serves a fixed discovery document.
type fakeMeta struct{ md *domain.DiscoveryMetadata }

func (f *fakeMeta) Fetch(_ context.Context, _ string) (*domain.DiscoveryMetadata, error) {
	return f.md, nil
}

// fakeRegistrar hands out sequential client ids.
type fakeRegistrar struct {
	mu   sync.Mutex
	next int
}

func (f *fakeRegistrar) Register(_ context.Context, _ string, redirectURIs []string) (*domain.ClientRegistration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	return &domain.ClientRegistration{ClientID: "cid-" + string(rune('a'+f.next-1)), RedirectURIs: redirectURIs, TokenEndpointAuthMethod: "none"}, nil
}

// memRegs is a trivial in-memory registration store.
type memRegs struct {
	mu sync.Mutex
	m  map[string]*domain.ClientRegistration
}

func newMemRegs() *memRegs { return &memRegs{m: map[string]*domain.ClientRegistration{}} }

func (m *memRegs) Get(_ context.Context, serverName, callbackURL string) (*domain.ClientRegistration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.m[serverName+"|"+callbackURL]; ok {
		return r, nil
	}
	return nil, port.ErrNotFound
}

func (m *memRegs) GetAny(_ context.Context, serverName string) (*domain.ClientRegistration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, r := range m.m {
		if len(k) > len(serverName) && k[:len(serverName)] == serverName && k[len(serverName)] == '|' {
			return r, nil
		}
	}
	return nil, port.ErrNotFound
}

func (m *memRegs) Save(_ context.Context, serverName, callbackURL string, r *domain.ClientRegistration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m[serverName+"|"+callbackURL] = r
	return nil
}

// fakeTokens is an in-memory token store.
type fakeTokens struct {
	mu sync.Mutex
	m  map[string]*domain.OAuthTokens
}

func newFakeTokens() *fakeTokens { return &fakeTokens{m: map[string]*domain.OAuthTokens{}} }

func (f *fakeTokens) Save(_ context.Context, key string, t *domain.OAuthTokens) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[key] = t
	return nil
}
func (f *fakeTokens) Load(_ context.Context, key string) (*domain.OAuthTokens, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.m[key]; ok {
		return t, nil
	}
	return nil, port.ErrNotFound
}
func (f *fakeTokens) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, key)
	return nil
}

// fakeExchanger issues tokens and rotates refresh tokens on refresh.
type fakeExchanger struct {
	refreshCount int
	lastResource string
}

func (f *fakeExchanger) Exchange(_ context.Context, _, code, _, _, resource string, _ *domain.ClientRegistration) (*domain.OAuthTokens, error) {
	if code == "" {
		return nil, errors.New("empty code")
	}
	f.lastResource = resource
	return &domain.OAuthTokens{AccessToken: "at-" + code, RefreshToken: "rt-1", ExpiresAt: time.Now().UTC().Add(-time.Minute)}, nil
}

func (f *fakeExchanger) Refresh(_ context.Context, _ string, tokens *domain.OAuthTokens, resource string, _ *domain.ClientRegistration) (*domain.OAuthTokens, error) {
	f.refreshCount++
	f.lastResource = resource
	return &domain.OAuthTokens{AccessToken: "at-new", RefreshToken: "rt-" + string(rune('2'+f.refreshCount-1)), ExpiresAt: time.Now().UTC().Add(time.Hour)}, nil
}

func (f *fakeExchanger) Revoke(_ context.Context, _, _, _, resource string) error {
	f.lastResource = resource
	return nil
}

// noRotateExchanger mimics providers that only rotate the access token.
type noRotateExchanger struct{}

func (n *noRotateExchanger) Exchange(_ context.Context, _, code, _, _, _ string, _ *domain.ClientRegistration) (*domain.OAuthTokens, error) {
	return &domain.OAuthTokens{AccessToken: "at-" + code, RefreshToken: "rt-1"}, nil
}
func (n *noRotateExchanger) Refresh(_ context.Context, _ string, _ *domain.OAuthTokens, _ string, _ *domain.ClientRegistration) (*domain.OAuthTokens, error) {
	return &domain.OAuthTokens{AccessToken: "at-new", ExpiresAt: time.Now().UTC().Add(time.Hour)}, nil
}
func (n *noRotateExchanger) Revoke(_ context.Context, _, _, _, _ string) error { return nil }

func newFlow() (*StartAuthorization, *CompleteAuthorization, *RefreshToken, *TokenProvider, *RevokeToken, *Status, *fakeTokens, *memRegs, *fakeExchanger, *fakeMeta) {
	md := &domain.DiscoveryMetadata{
		Issuer:                "https://mcp.quandora.ai",
		AuthorizationEndpoint: "https://mcp.quandora.ai/oauth/authorize",
		TokenEndpoint:         "https://mcp.quandora.ai/oauth/token",
		RegistrationEndpoint:  "https://mcp.quandora.ai/oauth/register",
		RevocationEndpoint:    "https://mcp.quandora.ai/oauth/revoke",
		ScopesSupported:       []string{"quant"},
	}
	meta := &fakeMeta{md: md}
	registrar := &fakeRegistrar{}
	regs := newMemRegs()
	tokens := newFakeTokens()
	exch := &fakeExchanger{}
	locks := NewRefreshLocks()
	start := &StartAuthorization{Meta: meta, Registrar: registrar, Regs: regs, Pending: newPendingStore()}
	complete := &CompleteAuthorization{Meta: meta, Pending: start.Pending.(port.PendingAuthStore), Regs: regs, Tokens: tokens, Exchange: exch}
	refresh := &RefreshToken{Meta: meta, Regs: regs, Tokens: tokens, Exchange: exch, Locks: locks}
	provider := &TokenProvider{Tokens: tokens, Refresh: refresh}
	revoke := &RevokeToken{Meta: meta, Regs: regs, Tokens: tokens, Exchange: exch}
	status := &Status{Tokens: tokens}
	return start, complete, refresh, provider, revoke, status, tokens, regs, exch, meta
}

// newPendingStore is declared in the same package for test reuse.
type pendingStore struct {
	mu sync.Mutex
	m  map[string]*domain.PendingAuth
}

func newPendingStore() *pendingStore { return &pendingStore{m: map[string]*domain.PendingAuth{}} }

func (p *pendingStore) Save(_ context.Context, pa *domain.PendingAuth) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m[pa.State] = pa
	return nil
}
func (p *pendingStore) Take(_ context.Context, state string) (*domain.PendingAuth, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pa, ok := p.m[state]
	if !ok {
		return nil, port.ErrNotFound
	}
	delete(p.m, state)
	return pa, nil
}

func (p *pendingStore) CountActive(_ context.Context, userID string) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, pa := range p.m {
		if pa.UserID == userID {
			n++
		}
	}
	return n, nil
}

func TestFullAuthorizationFlow(t *testing.T) {
	ctx := context.Background()
	start, complete, _, provider, revoke, status, _, _, _, _ := newFlow()

	out, err := start.Execute(ctx, StartAuthInput{
		UserID: "u1", AgentID: "a1", ServerName: "quandora",
		ServerURL: "https://mcp.quandora.ai/quant", CallbackURL: "https://app.example.com/oauth/mcp/cb",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if out.State == "" || out.AuthURL == "" {
		t.Fatal("start returned empty state/authURL")
	}

	if _, err := complete.Execute(ctx, CompleteAuthInput{Callback: domain.CallbackParams{Code: "c1", State: out.State}}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	st, err := status.Execute(ctx, RefreshInput{UserID: "u1", AgentID: "a1", ServerName: "quandora", ServerURL: "https://mcp.quandora.ai/quant"})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Status != StatusExpired {
		t.Fatalf("status = %s, want expired (fake token already expired)", st.Status)
	}

	tok, err := provider.AccessToken(ctx, RefreshInput{UserID: "u1", AgentID: "a1", ServerName: "quandora", ServerURL: "https://mcp.quandora.ai/quant"})
	if err != nil {
		t.Fatalf("access token: %v", err)
	}
	if tok != "at-new" {
		t.Fatalf("access token = %q, want at-new", tok)
	}
	// Refresh rotated the refresh token; a second access must not fail.
	if _, err := provider.AccessToken(ctx, RefreshInput{UserID: "u1", AgentID: "a1", ServerName: "quandora", ServerURL: "https://mcp.quandora.ai/quant"}); err != nil {
		t.Fatalf("second access token: %v", err)
	}

	if err := revoke.Execute(ctx, RefreshInput{UserID: "u1", AgentID: "a1", ServerName: "quandora", ServerURL: "https://mcp.quandora.ai/quant"}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	st, _ = status.Execute(ctx, RefreshInput{UserID: "u1", AgentID: "a1", ServerName: "quandora", ServerURL: "https://mcp.quandora.ai/quant"})
	if st.Status != StatusNone {
		t.Fatalf("status after revoke = %s, want none", st.Status)
	}
}

func TestStateIsSingleUse(t *testing.T) {
	ctx := context.Background()
	start, complete, _, _, _, _, _, _, _, _ := newFlow()
	out, err := start.Execute(ctx, StartAuthInput{
		UserID: "u1", AgentID: "a1", ServerName: "quandora",
		ServerURL: "https://mcp.quandora.ai/quant", CallbackURL: "https://app.example.com/oauth/mcp/cb",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	in := CompleteAuthInput{Callback: domain.CallbackParams{Code: "c1", State: out.State}}
	if _, err := complete.Execute(ctx, in); err != nil {
		t.Fatalf("first complete: %v", err)
	}
	if _, err := complete.Execute(ctx, in); err == nil {
		t.Fatal("replaying the same state must fail (single-use)")
	}
}

func TestRefreshKeepsOldRefreshTokenWhenNotRotated(t *testing.T) {
	ctx := context.Background()
	start, complete, refresh, provider, _, _, tokens, _, _, _ := newFlow()
	out, err := start.Execute(ctx, StartAuthInput{
		UserID: "u1", AgentID: "a1", ServerName: "quandora",
		ServerURL: "https://mcp.quandora.ai/quant", CallbackURL: "https://app.example.com/oauth/mcp/cb",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := complete.Execute(ctx, CompleteAuthInput{Callback: domain.CallbackParams{Code: "c1", State: out.State}}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	// Force a provider that doesn't rotate the refresh token.
	tokens.Save(ctx, domain.StoreKey("u1", "a1", "quandora"), &domain.OAuthTokens{
		AccessToken: "stale", RefreshToken: "rt-original", ExpiresAt: time.Now().UTC().Add(-time.Minute),
	})
	exch := &noRotateExchanger{}
	refresh.Exchange = exch
	provider.Refresh = refresh
	if _, err := provider.AccessToken(ctx, RefreshInput{UserID: "u1", AgentID: "a1", ServerName: "quandora", ServerURL: "https://mcp.quandora.ai/quant"}); err != nil {
		t.Fatalf("access token: %v", err)
	}
	stored, err := tokens.Load(ctx, domain.StoreKey("u1", "a1", "quandora"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if stored.RefreshToken != "rt-original" {
		t.Fatalf("refresh token = %q, want original retained when provider doesn't rotate", stored.RefreshToken)
	}
}

func TestPendingCapacityLimit(t *testing.T) {
	ctx := context.Background()
	md := &domain.DiscoveryMetadata{
		Issuer:                "https://mcp.quandora.ai",
		AuthorizationEndpoint: "https://mcp.quandora.ai/oauth/authorize",
		TokenEndpoint:         "https://mcp.quandora.ai/oauth/token",
		RegistrationEndpoint:  "https://mcp.quandora.ai/oauth/register",
		ScopesSupported:       []string{"quant"},
	}
	meta := &fakeMeta{md: md}
	registrar := &fakeRegistrar{}
	regs := newMemRegs()
	pending := newPendingStore()
	start := &StartAuthorization{
		Meta: meta, Registrar: registrar, Regs: regs, Pending: pending,
		MaxPendingPerUser: 2,
	}
	in := StartAuthInput{
		UserID: "u1", AgentID: "a1", ServerName: "quandora",
		ServerURL: "https://mcp.quandora.ai/quant", CallbackBase: "https://app.example.com/oauth/mcp",
	}
	for i := 0; i < 2; i++ {
		if _, err := start.Execute(ctx, in); err != nil {
			t.Fatalf("start %d within cap: %v", i, err)
		}
	}
	if _, err := start.Execute(ctx, in); err == nil {
		t.Fatal("third pending must be rejected by the per-user cap")
	}
}
