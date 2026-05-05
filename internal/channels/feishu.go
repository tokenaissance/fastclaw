package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

// Feishu (飞书) bot adapter. Webhook-driven: inbound messages arrive via
// HTTPS POSTs from Feishu's open platform to the fastclaw webhook route
// (set up in internal/setup/server.go); outbound replies go through
// /open-apis/im/v1/messages with a tenant_access_token we mint on
// demand and cache.
//
// Long-connection (WebSocket) is also offered by Feishu but uses
// Protobuf framing — too much surface area to hand-roll without the
// official SDK. Webhook is JSON-only and integrates with the existing
// fastclaw HTTP server. Trade-off: needs a publicly reachable URL.
//
// AppID is the credential_key + accountID. AppSecret is held in
// AccountConfig.BotToken (semantic match: "the secret credential the
// bot uses"). Verification token is held in AccountConfig.UserID
// (matches the "extra account-scoped identifier" comment on that
// field).

const (
	feishuBaseURL          = "https://open.feishu.cn"
	feishuTokenURL         = feishuBaseURL + "/open-apis/auth/v3/tenant_access_token/internal"
	feishuSendURL          = feishuBaseURL + "/open-apis/im/v1/messages"
	feishuBotInfoURL       = feishuBaseURL + "/open-apis/bot/v3/info"
	feishuSendTimeout      = 15 * time.Second
	feishuTokenTimeout     = 10 * time.Second
	feishuTokenRefreshSkew = 60 * time.Second
)

// Feishu implements the Channel interface for Feishu / Feishu custom apps.
type Feishu struct {
	bus               *bus.MessageBus
	accountID         string // == app_id
	appID             string
	appSecret         string
	verificationToken string

	httpClient *http.Client

	mu          sync.Mutex
	accessTok   string
	accessTokExp time.Time
	botName     string // populated on Start via /bot/v3/info; best-effort
	botOpenID   string
}

// NewFeishu creates a Feishu adapter. verificationToken matches the value
// configured under "Event Subscriptions → Verification Token" in the
// Feishu Developer Console; we use it to validate inbound webhook
// payloads.
func NewFeishu(appID, appSecret, verificationToken, accountID string, mb *bus.MessageBus) (*Feishu, error) {
	if appID == "" || appSecret == "" {
		return nil, errors.New("feishu: appID and appSecret required")
	}
	if accountID == "" {
		accountID = appID
	}
	return &Feishu{
		bus:               mb,
		accountID:         accountID,
		appID:             appID,
		appSecret:         appSecret,
		verificationToken: verificationToken,
		httpClient:        &http.Client{Timeout: feishuSendTimeout},
	}, nil
}

func (l *Feishu) Name() string        { return "feishu" }
func (l *Feishu) AccountID() string   { return l.accountID }
func (l *Feishu) BotUsername() string { return l.botName }

// Start is mostly a no-op — Feishu pushes events to the webhook route
// rather than us polling. We do one /bot/v3/info call up front to
// surface the bot's display name, then block until ctx is done.
// Errors fetching bot info don't fail the channel: outbound still
// works, the username is just empty (which the cron-binding fallback
// already tolerates).
func (l *Feishu) Start(ctx context.Context) error {
	if name, openID, err := l.fetchBotInfo(ctx); err != nil {
		slog.Warn("feishu bot info fetch failed", "account", l.accountID, "error", err)
	} else {
		l.mu.Lock()
		l.botName = name
		l.botOpenID = openID
		l.mu.Unlock()
		slog.Info("feishu bot connected", "account", l.accountID, "name", name)
	}
	<-ctx.Done()
	return nil
}

// Send posts plain text. Used by tools / test paths that don't carry
// any rich payload.
func (l *Feishu) Send(chatID, text string) error {
	return l.SendMessage(bus.OutboundMessage{ChatID: chatID, Text: text})
}

// SendMessage delivers Text + (optionally) MediaItems. Feishu's text
// shape is `{"text":"..."}` JSON-stringified inside the `content`
// field. MediaItems are deferred — sending images requires uploading
// to Feishu's CDN first via /im/v1/images, which is a separate dance
// we don't need until users complain.
func (l *Feishu) SendMessage(msg bus.OutboundMessage) error {
	if msg.Text == "" && len(msg.MediaItems) == 0 {
		return nil
	}
	if msg.Text == "" {
		// MediaItems-only without an upload path — skip rather than
		// posting an empty bubble. Logged so it's debuggable if it
		// ever happens in practice.
		slog.Debug("feishu send: media-only message dropped (image upload not implemented)",
			"account", l.accountID, "chat", msg.ChatID)
		return nil
	}
	tok, err := l.tenantAccessToken(context.Background())
	if err != nil {
		return fmt.Errorf("feishu token: %w", err)
	}
	contentJSON, err := json.Marshal(map[string]string{"text": msg.Text})
	if err != nil {
		return fmt.Errorf("feishu marshal content: %w", err)
	}
	payload := map[string]string{
		"receive_id": msg.ChatID,
		"content":    string(contentJSON),
		"msg_type":   "text",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("feishu marshal: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost,
		feishuSendURL+"?receive_id_type=chat_id",
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("feishu send: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("feishu send HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	var apiResp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return fmt.Errorf("feishu send parse: %w", err)
	}
	if apiResp.Code != 0 {
		return fmt.Errorf("feishu send: code=%d msg=%s", apiResp.Code, apiResp.Msg)
	}
	return nil
}

// SendTyping is a no-op. Feishu's open platform doesn't expose a typing
// indicator API for custom-app bots — only first-party apps get it.
// The gateway's typing relay still fires every 5s but degenerates to
// a cheap no-op call.
func (l *Feishu) SendTyping(_ string) error { return nil }

// --- Inbound (webhook handler entry point) ---

// FeishuEventEnvelope is the v2 schema Feishu uses for event subscriptions.
// We match on header.event_type == "im.message.receive_v1".
type FeishuEventEnvelope struct {
	Schema string         `json:"schema"`
	Header FeishuEventHeader `json:"header"`
	Event  json.RawMessage `json:"event"`

	// v1 url_verification challenge fields (also surfaced here for the
	// initial subscribe-time handshake; Feishu's v2 events use
	// header.event_type == "url_verification" too on newer apps but
	// older flows still send the legacy top-level shape).
	Type      string `json:"type,omitempty"`
	Challenge string `json:"challenge,omitempty"`
	Token     string `json:"token,omitempty"`
}

type FeishuEventHeader struct {
	EventID    string `json:"event_id"`
	EventType  string `json:"event_type"`
	CreateTime string `json:"create_time"`
	Token      string `json:"token"`
	AppID      string `json:"app_id"`
	TenantKey  string `json:"tenant_key,omitempty"`
}

type feishuMessageEvent struct {
	Sender struct {
		SenderID struct {
			OpenID  string `json:"open_id"`
			UserID  string `json:"user_id,omitempty"`
			UnionID string `json:"union_id,omitempty"`
		} `json:"sender_id"`
		SenderType string `json:"sender_type"`
	} `json:"sender"`
	Message struct {
		MessageID    string `json:"message_id"`
		RootID       string `json:"root_id,omitempty"`
		ParentID     string `json:"parent_id,omitempty"`
		CreateTime   string `json:"create_time"`
		ChatID       string `json:"chat_id"`
		ChatType     string `json:"chat_type"` // "p2p" | "group"
		MessageType  string `json:"message_type"`
		Content      string `json:"content"`
	} `json:"message"`
}

// HandleWebhook is invoked by the HTTP route receiving POSTs from
// Feishu. It validates `header.token` against the configured
// verification token, handles the one-time URL-verification challenge,
// and dispatches im.message.receive_v1 events onto the bus.
//
// Returns the JSON body the handler should write back, plus an HTTP
// status code. The handler is intentionally small/synchronous so a
// single goroutine drives one webhook through to bus enqueue — Feishu
// retries on non-200, so we'd rather block briefly than ack early and
// drop on a panic.
func (l *Feishu) HandleWebhook(body []byte) (responseBody []byte, status int, err error) {
	var env FeishuEventEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("parse: %w", err)
	}

	// URL verification — Feishu sends this once at subscribe-config time.
	// Echo the challenge back so it considers the URL valid. Two
	// shapes coexist in the wild: legacy top-level {type, challenge,
	// token} and v2 {schema, header.event_type=url_verification,
	// event.{challenge}}. Handle both.
	if env.Type == "url_verification" || env.Header.EventType == "url_verification" {
		token := env.Token
		if token == "" {
			token = env.Header.Token
		}
		if l.verificationToken != "" && token != l.verificationToken {
			return nil, http.StatusUnauthorized, errors.New("verification token mismatch")
		}
		challenge := env.Challenge
		if challenge == "" {
			// v2 shape: challenge nests inside event
			var inner struct {
				Challenge string `json:"challenge"`
			}
			_ = json.Unmarshal(env.Event, &inner)
			challenge = inner.Challenge
		}
		out, _ := json.Marshal(map[string]string{"challenge": challenge})
		return out, http.StatusOK, nil
	}

	// Real event. Validate token, then dispatch.
	if l.verificationToken != "" && env.Header.Token != l.verificationToken {
		return nil, http.StatusUnauthorized, errors.New("verification token mismatch")
	}

	switch env.Header.EventType {
	case "im.message.receive_v1":
		var ev feishuMessageEvent
		if err := json.Unmarshal(env.Event, &ev); err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("parse event: %w", err)
		}
		l.dispatchInbound(ev)
	default:
		// Unknown event_type — ack with 200 so Feishu doesn't retry, but
		// log so misconfigured subscriptions are visible.
		slog.Debug("feishu unhandled event", "event_type", env.Header.EventType, "event_id", env.Header.EventID)
	}
	return []byte(`{"ok":true}`), http.StatusOK, nil
}

// dispatchInbound translates a Feishu message event into a
// bus.InboundMessage. Drops self-sent messages (sender_type != "user")
// and non-text messages. Feishu's `content` is a JSON-encoded string
// inside the event JSON — `{"text":"hello"}` — which we have to
// re-decode separately.
func (l *Feishu) dispatchInbound(ev feishuMessageEvent) {
	if ev.Sender.SenderType != "user" {
		return
	}
	if ev.Message.MessageType != "text" {
		// We support only text in V1. Feishu's "post" / "image" / "file"
		// types each have their own content shape; defer until users ask.
		slog.Debug("feishu non-text message skipped",
			"account", l.accountID, "type", ev.Message.MessageType)
		return
	}
	var content struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(ev.Message.Content), &content); err != nil {
		slog.Debug("feishu content parse failed", "error", err)
		return
	}
	if content.Text == "" {
		return
	}

	peerKind := "dm"
	if ev.Message.ChatType == "group" {
		peerKind = "group"
	}

	// Use a stable per-message ID so dedup at the gateway can squash
	// retries (Feishu resends events on non-2xx replies).
	msgID := ev.Message.MessageID
	if msgID == "" {
		msgID = strconv.FormatInt(time.Now().UnixNano(), 10)
	}

	slog.Info("feishu message received",
		"account", l.accountID,
		"from", ev.Sender.SenderID.OpenID,
		"chat", ev.Message.ChatID,
		"len", len(content.Text))

	l.bus.Inbound <- bus.InboundMessage{
		Channel:   "feishu",
		AccountID: l.accountID,
		ChatID:    ev.Message.ChatID,
		UserID:    ev.Sender.SenderID.OpenID,
		MessageID: msgID,
		Text:      content.Text,
		PeerKind:  peerKind,
	}
}

// --- HTTP plumbing ---

// tenantAccessToken returns a cached Feishu tenant token, refreshing
// when expired (or about to expire — RefreshSkew). One in-flight
// refresh at a time via the struct mutex; concurrent callers wait.
func (l *Feishu) tenantAccessToken(ctx context.Context) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.accessTok != "" && time.Now().Before(l.accessTokExp.Add(-feishuTokenRefreshSkew)) {
		return l.accessTok, nil
	}
	tok, ttl, err := l.fetchTenantAccessToken(ctx)
	if err != nil {
		return "", err
	}
	l.accessTok = tok
	l.accessTokExp = time.Now().Add(time.Duration(ttl) * time.Second)
	return tok, nil
}

func (l *Feishu) fetchTenantAccessToken(ctx context.Context) (string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, feishuTokenTimeout)
	defer cancel()
	body, _ := json.Marshal(map[string]string{
		"app_id":     l.appID,
		"app_secret": l.appSecret,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, feishuTokenURL, bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.httpClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("contact feishu: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("feishu token HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	var out struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", 0, fmt.Errorf("feishu token parse: %w", err)
	}
	if out.Code != 0 {
		return "", 0, fmt.Errorf("feishu token: code=%d msg=%s", out.Code, out.Msg)
	}
	if out.TenantAccessToken == "" {
		return "", 0, errors.New("feishu token: empty tenant_access_token")
	}
	if out.Expire == 0 {
		out.Expire = 7200 // documented default
	}
	return out.TenantAccessToken, out.Expire, nil
}

// fetchBotInfo calls /bot/v3/info to get the bot's display name +
// open_id. Best-effort; failures don't break the channel.
func (l *Feishu) fetchBotInfo(ctx context.Context) (name, openID string, err error) {
	tok, err := l.tenantAccessToken(ctx)
	if err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(ctx, feishuTokenTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feishuBotInfoURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := l.httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Bot  struct {
			ActivateStatus int    `json:"activate_status"`
			AppName        string `json:"app_name"`
			OpenID         string `json:"open_id"`
		} `json:"bot"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", "", err
	}
	if out.Code != 0 {
		return "", "", fmt.Errorf("code=%d msg=%s", out.Code, out.Msg)
	}
	return out.Bot.AppName, out.Bot.OpenID, nil
}

// FeishuValidateCredentials is the connect-handler validation step:
// mints a tenant_access_token to confirm app_id/app_secret are good,
// then fetches /bot/v3/info to capture the bot's display name. No
// adapter state created — caller persists and hot-registers.
func FeishuValidateCredentials(ctx context.Context, appID, appSecret string) (botName, botOpenID string, err error) {
	stub := &Feishu{
		appID:      appID,
		appSecret:  appSecret,
		httpClient: &http.Client{Timeout: feishuSendTimeout},
	}
	if _, err := stub.tenantAccessToken(ctx); err != nil {
		return "", "", err
	}
	return stub.fetchBotInfo(ctx)
}
