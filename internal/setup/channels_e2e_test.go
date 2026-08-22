package setup

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeTelegramTransport stubs the Bot API getMe call so the connect
// handler's external validation (telegramGetMe → http.Get) returns a
// canned bot username instead of hitting the network. Safe to swap
// http.DefaultTransport here: the setup package has zero t.Parallel()
// tests, so tests run serially and each package's test binary owns its
// own transport.
type fakeTelegramTransport struct {
	hits *int
}

func (f *fakeTelegramTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if f.hits != nil {
		*f.hits++
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{"username":"e2e_bot"}}`)),
	}, nil
}

func installFakeTelegram(t *testing.T, hits *int) {
	t.Helper()
	orig := http.DefaultTransport
	http.DefaultTransport = &fakeTelegramTransport{hits: hits}
	t.Cleanup(func() { http.DefaultTransport = orig })
}

// TestChannels_CloudPathE2E drives the full channels-table HTTP lifecycle
// through the real handlers — connect (telegram getMe + dual-write to
// configs AND channels), list (channels-table-first read), disconnect
// (dual-delete) — and asserts both tables stay in sync the way the Cloud
// proxy path (POST/GET/DELETE /api/fastagent/agents/{id}/channels) does.
func TestChannels_CloudPathE2E(t *testing.T) {
	var getMeHits int
	installFakeTelegram(t, &getMeHits)

	s, uid, aid := setupFileUploadTest(t)
	ctx := context.Background()

	// 1. Connect a Telegram bot → 200 + botUsername from getMe.
	connect := httptest.NewRequest(http.MethodPost,
		"/api/agents/"+aid+"/channels/telegram/connect",
		strings.NewReader(`{"botToken":"123456:FAKE"}`))
	connect.Header.Set("Content-Type", "application/json")
	connect.SetPathValue("id", aid)
	connect = stampAuthAndUserID(connect, uid)
	rec := httptest.NewRecorder()
	s.handleConnectAgentTelegram(rec, connect)
	if rec.Code != http.StatusOK {
		t.Fatalf("connect status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var connResp struct {
		OK          bool   `json:"ok"`
		BotUsername string `json:"botUsername"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &connResp); err != nil {
		t.Fatalf("decode connect response: %v", err)
	}
	if !connResp.OK || connResp.BotUsername != "e2e_bot" {
		t.Fatalf("connect = %+v; want ok + botUsername e2e_bot", connResp)
	}
	if getMeHits != 1 {
		t.Errorf("telegram getMe hits = %d; want 1", getMeHits)
	}

	// 2. Dual-write: the configs row (legacy authority) AND the channels
	//    row both exist with the same ownership.
	cfgRec, err := s.dataStore.LookupChannelByCredential(ctx, "telegram", "e2e_bot")
	if err != nil || cfgRec == nil {
		t.Fatalf("configs row after connect: rec=%+v err=%v", cfgRec, err)
	}
	if cfgRec.UserID != uid || cfgRec.AgentID != aid {
		t.Errorf("configs row ownership = (user=%q agent=%q); want (%q %q)",
			cfgRec.UserID, cfgRec.AgentID, uid, aid)
	}
	chRec, err := s.dataStore.LookupChannel(ctx, "telegram", "e2e_bot")
	if err != nil || chRec == nil {
		t.Fatalf("channels row after connect: rec=%+v err=%v", chRec, err)
	}
	if chRec.UserID != uid || chRec.AgentID != aid || !chRec.Enabled {
		t.Errorf("channels row = %+v; want user=%q agent=%q enabled=true", chRec, uid, aid)
	}

	// 3. List → channels-table-first read surfaces the bot.
	list := httptest.NewRequest(http.MethodGet, "/api/agents/"+aid+"/channels", nil)
	list.SetPathValue("id", aid)
	list = stampAuthAndUserID(list, uid)
	rec = httptest.NewRecorder()
	s.handleListAgentChannels(rec, list)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var listResp struct {
		Channels []channelOut `json:"channels"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(listResp.Channels) != 1 {
		t.Fatalf("list channels = %d; want 1, got %+v", len(listResp.Channels), listResp.Channels)
	}
	if ch := listResp.Channels[0]; ch.Type != "telegram" || ch.AccountID != "e2e_bot" || !ch.Enabled {
		t.Errorf("listed channel = %+v; want telegram/e2e_bot enabled", listResp.Channels[0])
	} else if ch.BotToken == "123456:FAKE" {
		t.Errorf("botToken not masked: %q", ch.BotToken)
	}

	// 4. Disconnect → both tables drained.
	disc := httptest.NewRequest(http.MethodDelete,
		"/api/agents/"+aid+"/channels/telegram/e2e_bot", nil)
	disc.SetPathValue("id", aid)
	disc.SetPathValue("type", "telegram")
	disc.SetPathValue("accountId", "e2e_bot")
	disc = stampAuthAndUserID(disc, uid)
	rec = httptest.NewRecorder()
	s.handleDisconnectAgentChannel(rec, disc)
	if rec.Code != http.StatusOK {
		t.Fatalf("disconnect status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if cfgAfter, err := s.dataStore.LookupChannelByCredential(ctx, "telegram", "e2e_bot"); err == nil || cfgAfter != nil {
		t.Errorf("configs row still present after disconnect: rec=%+v err=%v", cfgAfter, err)
	}
	if chAfter, err := s.dataStore.LookupChannel(ctx, "telegram", "e2e_bot"); err == nil || chAfter != nil {
		t.Errorf("channels row still present after disconnect: rec=%+v err=%v", chAfter, err)
	}
}

// TestChannels_OwnershipGate verifies a non-owner caller cannot connect
// a channel to someone else's private agent (the multi-tenant leak the
// fork's ownership gate closes). The Cloud proxy forwards the caller's
// own identity, so an attacker would need to own the target agent to
// reach the dual-write path.
func TestChannels_OwnershipGate(t *testing.T) {
	installFakeTelegram(t, nil)

	s, _, aid := setupFileUploadTest(t)

	// Attacker user with no relationship to the agent.
	intruder := "user_intruder"
	connect := httptest.NewRequest(http.MethodPost,
		"/api/agents/"+aid+"/channels/telegram/connect",
		strings.NewReader(`{"botToken":"123456:FAKE"}`))
	connect.Header.Set("Content-Type", "application/json")
	connect.SetPathValue("id", aid)
	connect = stampAuthAndUserID(connect, intruder)
	rec := httptest.NewRecorder()
	s.handleConnectAgentTelegram(rec, connect)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("intruder connect status = %d, body=%s; want 403", rec.Code, rec.Body.String())
	}
	if _, err := s.dataStore.LookupChannel(context.Background(), "telegram", "e2e_bot"); err == nil {
		t.Fatal("channels row written by intruder despite 403")
	}
}
