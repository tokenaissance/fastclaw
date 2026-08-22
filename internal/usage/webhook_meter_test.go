package usage

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/config"
)

func TestCalculateTokenCost(t *testing.T) {
	tests := []struct {
		name   string
		tokens Tokens
		cost   config.ModelCost
		want   int
	}{
		{
			name: "basic input/output tokens",
			tokens: Tokens{
				Input:  1000,
				Output: 500,
			},
			cost: config.ModelCost{
				Input:      15,
				Output:     75,
				CacheRead:  1.5,
				CacheWrite: 18.75,
			},
			// (1000/1M * 15) + (500/1M * 75) = 0.015 + 0.0375 = 0.0525 → ceiled = 1
			want: 1,
		},
		{
			name: "with cache tokens",
			tokens: Tokens{
				Input:         10000,
				Output:        5000,
				CacheRead:     20000,
				CacheCreation: 15000,
			},
			cost: config.ModelCost{
				Input:      15,
				Output:     75,
				CacheRead:  1.5,
				CacheWrite: 18.75,
			},
			// (10000/1M*15) + (5000/1M*75) + (20000/1M*1.5) + (15000/1M*18.75)
			// = 0.15 + 0.375 + 0.03 + 0.28125 = 0.83625 → ceiled = 1
			want: 1,
		},
		{
			name: "large token counts",
			tokens: Tokens{
				Input:  1_000_000,
				Output: 500_000,
			},
			cost: config.ModelCost{
				Input:  15,
				Output: 75,
			},
			// (1M/1M * 15) + (500K/1M * 75) = 15 + 37.5 = 52.5 → ceiled = 53
			want: 53,
		},
		{
			name: "zero tokens",
			tokens: Tokens{
				Input:  0,
				Output: 0,
			},
			cost: config.ModelCost{
				Input:  15,
				Output: 75,
			},
			want: 0,
		},
		{
			name: "fractional cents should ceil",
			tokens: Tokens{
				Input:  100,
				Output: 50,
			},
			cost: config.ModelCost{
				Input:  15,
				Output: 75,
			},
			// (100/1M * 15) + (50/1M * 75) = 0.0015 + 0.00375 = 0.00525 → ceiled = 1
			want: 1,
		},
		{
			name: "per-call billing: flat fee regardless of tokens",
			tokens: Tokens{
				Input:  1_000_000,
				Output: 500_000,
			},
			cost: config.ModelCost{
				PerCall: 5,
			},
			want: 5,
		},
		{
			name: "per-call billing: fractional fee is ceiled",
			tokens: Tokens{
				Input:  100,
				Output: 50,
			},
			cost: config.ModelCost{
				PerCall: 2.3,
			},
			// ceil(2.3) = 3
			want: 3,
		},
		{
			name: "per-call billing: ignores non-zero token prices",
			tokens: Tokens{
				Input:  1_000_000,
				Output: 1_000_000,
			},
			cost: config.ModelCost{
				Input:   15,
				Output:  75,
				PerCall: 10,
			},
			// PerCall > 0 takes priority; token prices are not applied
			want: 10,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CalculateTokenCost(tt.tokens, tt.cost)
			if got != tt.want {
				t.Errorf("CalculateTokenCost() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWebhookMeter_RecordTokens(t *testing.T) {
	tests := []struct {
		name              string
		webhookToken      string
		expectedAuthToken string
		tokens            Tokens
		cost              *config.ModelCost
		wantErr           bool
		validatePayload   func(t *testing.T, report UsageReport)
	}{
		{
			name:              "successful webhook with auth token",
			webhookToken:      "test-token-123",
			expectedAuthToken: "Bearer test-token-123",
			tokens: Tokens{
				Input:  10000,
				Output: 5000,
			},
			cost: &config.ModelCost{
				Input:      15,
				Output:     75,
				CacheRead:  1.5,
				CacheWrite: 18.75,
			},
			validatePayload: func(t *testing.T, report UsageReport) {
				if report.UserID != "test-user" {
					t.Errorf("UserID = %v, want test-user", report.UserID)
				}
				if report.AgentID != "test-agent" {
					t.Errorf("AgentID = %v, want test-agent", report.AgentID)
				}
				if report.ModelID != "gpt-4o" {
					t.Errorf("ModelID = %v, want gpt-4o", report.ModelID)
				}
				if report.Usage.InputTokens != 10000 {
					t.Errorf("InputTokens = %v, want 10000", report.Usage.InputTokens)
				}
				if report.Usage.OutputTokens != 5000 {
					t.Errorf("OutputTokens = %v, want 5000", report.Usage.OutputTokens)
				}
				// (10000/1M * 15) + (5000/1M * 75) = 0.15 + 0.375 = 0.525 → ceiled = 1
				if report.RawCostCents != 1 {
					t.Errorf("RawCostCents = %v, want 1", report.RawCostCents)
				}
			},
		},
		{
			name:         "per-call billing: flat fee in webhook payload",
			webhookToken: "test-token-percall",
			tokens: Tokens{
				Input:  500_000,
				Output: 200_000,
			},
			cost: &config.ModelCost{
				PerCall: 8,
			},
			validatePayload: func(t *testing.T, report UsageReport) {
				if report.RawCostCents != 8 {
					t.Errorf("RawCostCents = %v, want 8 (per-call flat fee)", report.RawCostCents)
				}
				// Token counts still reported for observability
				if report.Usage.InputTokens != 500_000 {
					t.Errorf("InputTokens = %v, want 500000", report.Usage.InputTokens)
				}
			},
		},
		{
			name:         "webhook with cache tokens",
			webhookToken: "test-token-456",
			tokens: Tokens{
				Input:         1000,
				Output:        500,
				CacheRead:     2000,
				CacheCreation: 1500,
			},
			cost: &config.ModelCost{
				Input:      15,
				Output:     75,
				CacheRead:  1.5,
				CacheWrite: 18.75,
			},
			validatePayload: func(t *testing.T, report UsageReport) {
				if report.Usage.CacheReadTokens != 2000 {
					t.Errorf("CacheReadTokens = %v, want 2000", report.Usage.CacheReadTokens)
				}
				if report.Usage.CacheCreationTokens != 1500 {
					t.Errorf("CacheCreationTokens = %v, want 1500", report.Usage.CacheCreationTokens)
				}
			},
		},
		{
			name:         "per-call billing: webhook ignores token prices when PerCall set",
			webhookToken: "test-token-percall-override",
			tokens: Tokens{
				Input:  1_000_000,
				Output: 1_000_000,
			},
			cost: &config.ModelCost{
				Input:   15,
				Output:  75,
				PerCall: 10,
			},
			validatePayload: func(t *testing.T, report UsageReport) {
				// token-based would be 90 cents; per-call must win
				if report.RawCostCents != 10 {
					t.Errorf("RawCostCents = %v, want 10 (per-call must override token pricing)", report.RawCostCents)
				}
			},
		},
		{
			name:         "webhook without cost data (should skip)",
			webhookToken: "test-token-789",
			tokens: Tokens{
				Input:  1000,
				Output: 500,
			},
			cost: nil,
			validatePayload: func(t *testing.T, report UsageReport) {
				t.Error("Should not receive webhook when cost is nil")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var receivedReport UsageReport
			var mu sync.Mutex
			webhookCalled := false

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()

				if r.Method != "POST" {
					t.Errorf("Expected POST, got %s", r.Method)
				}
				if r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("Expected application/json, got %s", r.Header.Get("Content-Type"))
				}
				if tt.expectedAuthToken != "" {
					if got := r.Header.Get("Authorization"); got != tt.expectedAuthToken {
						t.Errorf("Authorization = %v, want %v", got, tt.expectedAuthToken)
					}
				}

				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("Failed to read body: %v", err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				if err := json.Unmarshal(body, &receivedReport); err != nil {
					t.Errorf("Failed to unmarshal body: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}

				webhookCalled = true
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			localMeter := NewMemMeter()
			getCostFunc := func(provider, model string) *config.ModelCost { return tt.cost }
			webhookMeter := NewWebhookMeter(localMeter, server.URL, tt.webhookToken, getCostFunc)

			ctx := context.Background()
			err := webhookMeter.RecordTokens(ctx, "test-user", "test-agent", "test-session", "openai", "gpt-4o", tt.tokens)
			if (err != nil) != tt.wantErr {
				t.Errorf("RecordTokens() error = %v, wantErr %v", err, tt.wantErr)
			}

			if tt.cost != nil {
				time.Sleep(100 * time.Millisecond)
				mu.Lock()
				called := webhookCalled
				mu.Unlock()
				if !called {
					t.Error("Webhook was not called")
				} else {
					tt.validatePayload(t, receivedReport)
				}
			}
		})
	}
}

func TestWebhookMeter_Integration(t *testing.T) {
	var webhookCalls []UsageReport
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var report UsageReport
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &report); err != nil {
			t.Errorf("Failed to unmarshal: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		webhookCalls = append(webhookCalls, report)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	localMeter := NewMemMeter()
	getCostFunc := func(provider, model string) *config.ModelCost {
		return &config.ModelCost{Input: 15, Output: 75, CacheRead: 1.5, CacheWrite: 18.75}
	}
	meter := NewWebhookMeter(localMeter, server.URL, "integration-test-token", getCostFunc)

	ctx := context.Background()
	testCases := []struct {
		userID    string
		agentID   string
		sessionID string
		tokens    Tokens
	}{
		{userID: "user1", agentID: "agent1", sessionID: "session1", tokens: Tokens{Input: 1000, Output: 500}},
		{userID: "user1", agentID: "agent1", sessionID: "session1", tokens: Tokens{Input: 2000, Output: 1000}},
		{userID: "user2", agentID: "agent2", sessionID: "session2", tokens: Tokens{Input: 5000, Output: 2500}},
	}

	for _, tc := range testCases {
		if err := meter.RecordTokens(ctx, tc.userID, tc.agentID, tc.sessionID, "anthropic", "claude-3-sonnet", tc.tokens); err != nil {
			t.Errorf("RecordTokens failed: %v", err)
		}
	}

	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(webhookCalls) != len(testCases) {
		t.Errorf("Expected %d webhook calls, got %d", len(testCases), len(webhookCalls))
	}

	type expectedCall struct {
		userID string
		tokens Tokens
	}
	remaining := make([]expectedCall, len(testCases))
	for i, tc := range testCases {
		remaining[i] = expectedCall{tc.userID, tc.tokens}
	}
	for _, call := range webhookCalls {
		found := false
		for i, exp := range remaining {
			if exp.userID == call.UserID && exp.tokens.Input == call.Usage.InputTokens {
				remaining = append(remaining[:i], remaining[i+1:]...)
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Unexpected webhook call: UserID=%v InputTokens=%v", call.UserID, call.Usage.InputTokens)
		}
	}
	for _, r := range remaining {
		t.Errorf("Missing webhook call: UserID=%v InputTokens=%v", r.userID, r.tokens.Input)
	}

	totals, err := meter.Totals(ctx, Range{})
	if err != nil {
		t.Errorf("Failed to get totals: %v", err)
	}
	if totals.Input != int64(1000+2000+5000) {
		t.Errorf("Local meter Input = %v, want %v", totals.Input, 1000+2000+5000)
	}
	if totals.Output != int64(500+1000+2500) {
		t.Errorf("Local meter Output = %v, want %v", totals.Output, 500+1000+2500)
	}
}

// The readback + log methods added for the expanded Meter interface
// (TotalsForUser / DailyForUser / RecordTokenLog) must delegate straight to
// the local meter. They are pure reads / a separate append-only log — the
// webhook path only fires from RecordTokens — so a WebhookMeter with a live
// webhook URL must return identical results to calling the local meter
// directly, and must never contact the webhook.
func TestWebhookMeter_DelegatingMethods(t *testing.T) {
	webhookHits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webhookHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	localMeter := NewMemMeter()
	getCostFunc := func(provider, model string) *config.ModelCost {
		return &config.ModelCost{Input: 15, Output: 75}
	}
	meter := NewWebhookMeter(localMeter, server.URL, "tok", getCostFunc)

	ctx := context.Background()
	if err := localMeter.RecordTokens(ctx, "alice", "agentA", "sess1", "anthropic-messages", "sonnet-4-6",
		Tokens{Input: 100, Output: 50, CacheRead: 25}); err != nil {
		t.Fatalf("record: %v", err)
	}
	r := LastN(1)

	// TotalsForUser delegates: equals the local meter's own answer.
	viaWebhook, err := meter.TotalsForUser(ctx, "alice", r)
	if err != nil {
		t.Fatalf("TotalsForUser: %v", err)
	}
	direct, err := localMeter.TotalsForUser(ctx, "alice", r)
	if err != nil {
		t.Fatalf("local TotalsForUser: %v", err)
	}
	if viaWebhook != direct {
		t.Errorf("TotalsForUser = %+v, want %+v", viaWebhook, direct)
	}
	if viaWebhook.Input != 100 || viaWebhook.Output != 50 || viaWebhook.CacheRead != 25 || viaWebhook.Requests != 1 {
		t.Errorf("TotalsForUser = %+v, want in=100 out=50 cache=25 req=1", viaWebhook)
	}

	// DailyForUser delegates and returns the day+agent+model breakdown.
	daily, err := meter.DailyForUser(ctx, "alice", r)
	if err != nil {
		t.Fatalf("DailyForUser: %v", err)
	}
	if len(daily) != 1 {
		t.Fatalf("DailyForUser rows = %d, want 1", len(daily))
	}
	d := daily[0]
	if d.AgentID != "agentA" || d.Model != "sonnet-4-6" || d.InputTokens != 100 || d.Requests != 1 {
		t.Errorf("DailyForUser row = %+v, want agentA/sonnet-4-6 in=100 req=1", d)
	}

	// RecordTokenLog delegates without error (MemMeter persists nothing).
	if err := meter.RecordTokenLog(ctx, "alice", "agentA", "sess1", "anthropic-messages", "sonnet-4-6",
		Tokens{Input: 1, Output: 2}, 1234); err != nil {
		t.Errorf("RecordTokenLog: %v", err)
	}

	// None of the delegating methods may have triggered the webhook.
	if webhookHits != 0 {
		t.Errorf("webhook hit %d times from delegating methods, want 0", webhookHits)
	}
}

func TestWebhookMeter_ErrorHandling(t *testing.T) {
	tests := []struct {
		name           string
		serverResponse int
		serverDelay    time.Duration
	}{
		{name: "webhook returns 500", serverResponse: http.StatusInternalServerError},
		{name: "webhook returns 401", serverResponse: http.StatusUnauthorized},
		{name: "webhook timeout", serverResponse: http.StatusOK, serverDelay: 15 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.serverDelay > 0 {
					time.Sleep(tt.serverDelay)
				}
				w.WriteHeader(tt.serverResponse)
			}))
			defer server.Close()

			localMeter := NewMemMeter()
			getCostFunc := func(provider, model string) *config.ModelCost {
				return &config.ModelCost{Input: 15, Output: 75}
			}
			meter := NewWebhookMeter(localMeter, server.URL, "test-token", getCostFunc)

			ctx := context.Background()
			err := meter.RecordTokens(ctx, "test-user", "test-agent", "test-session", "openai", "gpt-4o", Tokens{Input: 1000, Output: 500})
			if err != nil {
				t.Errorf("RecordTokens should not fail when webhook fails: %v", err)
			}

			time.Sleep(200 * time.Millisecond)

			totals, _ := meter.Totals(ctx, Range{})
			if totals.Input != 1000 {
				t.Errorf("Local meter should still record: Input = %v, want 1000", totals.Input)
			}
		})
	}
}
