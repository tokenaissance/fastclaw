package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"math"
	"net/http"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/config"
)

// tokensForJSON is the JSON representation of Tokens for webhook payload
type tokensForJSON struct {
	InputTokens         int `json:"inputTokens"`
	OutputTokens        int `json:"outputTokens"`
	CacheReadTokens     int `json:"cacheReadTokens,omitempty"`
	CacheCreationTokens int `json:"cacheCreationTokens,omitempty"`
}

// UsageReport is the payload sent to webhook endpoint
type UsageReport struct {
	UserID       string           `json:"userId"`
	AgentID      string           `json:"agentId"`
	SessionID    string           `json:"sessionId,omitempty"`
	ProviderID   string           `json:"providerId"`
	ModelID      string           `json:"modelId"`
	Usage        tokensForJSON    `json:"usage"`
	Cost         config.ModelCost `json:"cost"`
	RawCostCents int              `json:"rawCostCents"`
	Timestamp    string           `json:"timestamp"`
}

// CalculateTokenCost computes cost in cents from token usage and model pricing.
//
// Billing modes are mutually exclusive:
//   - PerCall > 0: flat fee per API call, token counts are ignored.
//   - Otherwise: (tokens / 1M) × price per category, result ceiled to integer cents.
func CalculateTokenCost(tokens Tokens, cost config.ModelCost) int {
	if cost.PerCall > 0 {
		return int(math.Ceil(cost.PerCall))
	}
	inputCost := (float64(tokens.Input) / 1_000_000) * cost.Input
	outputCost := (float64(tokens.Output) / 1_000_000) * cost.Output
	cacheReadCost := (float64(tokens.CacheRead) / 1_000_000) * cost.CacheRead
	cacheCreationCost := (float64(tokens.CacheCreation) / 1_000_000) * cost.CacheWrite
	return int(math.Ceil(inputCost + outputCost + cacheReadCost + cacheCreationCost))
}

// WebhookMeter wraps a local Meter and sends usage reports to a webhook endpoint.
// This is a generic implementation that doesn't depend on any specific backend.
type WebhookMeter struct {
	localMeter   Meter
	webhookURL   string
	webhookToken string
	httpClient   *http.Client
	getCostFunc  func(provider, model string) *config.ModelCost
}

// NewWebhookMeter creates a meter that records locally and reports to a webhook.
func NewWebhookMeter(localMeter Meter, webhookURL, webhookToken string, getCostFunc func(string, string) *config.ModelCost) *WebhookMeter {
	return &WebhookMeter{
		localMeter:   localMeter,
		webhookURL:   webhookURL,
		webhookToken: webhookToken,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		getCostFunc:  getCostFunc,
	}
}

func (m *WebhookMeter) RecordTokens(ctx context.Context, userID, agentID, sessionKey, provider, model string, t Tokens) error {
	// 1. Record locally (existing behavior)
	err := m.localMeter.RecordTokens(ctx, userID, agentID, sessionKey, provider, model, t)
	if err != nil {
		return err
	}

	// 2. Send webhook asynchronously (new behavior)
	if m.webhookURL != "" && m.getCostFunc != nil {
		go m.sendWebhook(userID, agentID, sessionKey, provider, model, t)
	}

	return nil
}

func (m *WebhookMeter) sendWebhook(userID, agentID, sessionKey, provider, model string, t Tokens) {
	cost := m.getCostFunc(provider, model)
	if cost == nil {
		log.Printf("[WebhookMeter] No cost data for provider=%s model=%s, skipping webhook", provider, model)
		return
	}

	rawCostCents := CalculateTokenCost(t, *cost)

	report := UsageReport{
		UserID:    userID,
		AgentID:   agentID,
		SessionID: sessionKey,
		ProviderID: provider,
		ModelID:   model,
		Usage: tokensForJSON{
			InputTokens:         t.Input,
			OutputTokens:        t.Output,
			CacheReadTokens:     t.CacheRead,
			CacheCreationTokens: t.CacheCreation,
		},
		Cost:         *cost,
		RawCostCents: rawCostCents,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	}

	body, err := json.Marshal(report)
	if err != nil {
		log.Printf("[WebhookMeter] Failed to marshal webhook payload: %v", err)
		return
	}

	req, err := http.NewRequest("POST", m.webhookURL, bytes.NewBuffer(body))
	if err != nil {
		log.Printf("[WebhookMeter] Failed to create webhook request: %v", err)
		return
	}

	req.Header.Set("Content-Type", "application/json")
	if m.webhookToken != "" {
		req.Header.Set("Authorization", "Bearer "+m.webhookToken)
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		log.Printf("[WebhookMeter] Webhook request failed: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Printf("[WebhookMeter] Webhook returned status %d", resp.StatusCode)
		return
	}

	log.Printf("[WebhookMeter] Usage reported: user=%s agent=%s model=%s cost=%d cents",
		userID, agentID, model, rawCostCents)
}

// Proxy all other methods to localMeter
func (m *WebhookMeter) Totals(ctx context.Context, r Range) (Totals, error) {
	return m.localMeter.Totals(ctx, r)
}

func (m *WebhookMeter) TopAgents(ctx context.Context, r Range, limit int) ([]Rank, error) {
	return m.localMeter.TopAgents(ctx, r, limit)
}

func (m *WebhookMeter) TopUsers(ctx context.Context, r Range, limit int) ([]Rank, error) {
	return m.localMeter.TopUsers(ctx, r, limit)
}

func (m *WebhookMeter) SessionsForAgent(ctx context.Context, agentID, userID string, r Range, limit int) ([]Rank, error) {
	return m.localMeter.SessionsForAgent(ctx, agentID, userID, r, limit)
}

// TotalsForUser, DailyForUser, RecordTokenLog delegate to the local
// meter. They exist so WebhookMeter satisfies the expanded Meter
// interface (added with upstream billing); the webhook path only fires
// per-call from RecordTokens, so per-user readbacks and the append-only
// log stay on the local backend.
func (m *WebhookMeter) TotalsForUser(ctx context.Context, userID string, r Range) (Totals, error) {
	return m.localMeter.TotalsForUser(ctx, userID, r)
}

func (m *WebhookMeter) DailyForUser(ctx context.Context, userID string, r Range) ([]DailyUsage, error) {
	return m.localMeter.DailyForUser(ctx, userID, r)
}

func (m *WebhookMeter) RecordTokenLog(ctx context.Context, userID, agentID, sessionKey, provider, model string, t Tokens, durationMs int64) error {
	return m.localMeter.RecordTokenLog(ctx, userID, agentID, sessionKey, provider, model, t, durationMs)
}

func (m *WebhookMeter) Close() error {
	return m.localMeter.Close()
}
