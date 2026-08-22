package channels

// E2E coverage for the Feishu adapter, exercising the seams that don't
// require a real Feishu network connection:
//
//   - json 2.0 card outbound rendering (upstream c90fc25 "feishu replies
//     as json 2.0 cards") through the adapter layer with a fake transport.
//   - mention preservation on the inbound path — both the webhook seam
//     (HandleWebhook → bus.InboundMessage) and the long-connection seam
//     (sdkEventToInternal, a pure translation, no network).
//
// No real network: every HTTP interaction goes through a fake
// RoundTripper; the long-conn path is exercised only via its pure
// sdkEventToInternal function (starting the real WS client is out of
// scope for a unit test).

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// fakeFeishuTransport is a RoundTripper that answers Feishu's token +
// send endpoints without touching the network. It captures every
// /im/v1/messages request body so tests can assert what the adapter
// actually POSTs. failSendFirstN makes the first N send attempts fail
// with HTTP 500 (drives the card→plain-text fallback).
type fakeFeishuTransport struct {
	mu             sync.Mutex
	sendAttempts   int
	failSendFirstN int
	captures       []map[string]string
}

func (f *fakeFeishuTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.Contains(req.URL.Path, "tenant_access_token/internal"):
		return fakeJSONResponse(http.StatusOK, `{"code":0,"tenant_access_token":"tok_test","expire":7200}`), nil
	case strings.Contains(req.URL.Path, "/im/v1/messages"):
		f.sendAttempts++
		body, _ := io.ReadAll(req.Body)
		var payload map[string]string
		_ = json.Unmarshal(body, &payload)
		f.captures = append(f.captures, payload)
		if f.sendAttempts <= f.failSendFirstN {
			return fakeJSONResponse(http.StatusInternalServerError, `{"code":0}`), nil
		}
		return fakeJSONResponse(http.StatusOK, `{"code":0,"msg":"success"}`), nil
	default:
		return fakeJSONResponse(http.StatusNotFound, `{}`), nil
	}
}

func (f *fakeFeishuTransport) sends() []map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]string, len(f.captures))
	copy(out, f.captures)
	return out
}

func fakeJSONResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

// newTestFeishu builds a Feishu adapter wired to the fake transport.
func newTestFeishu(t *testing.T, ft *fakeFeishuTransport) (*Feishu, *bus.MessageBus) {
	t.Helper()
	mb := bus.New()
	ch, err := NewFeishu("cli_e2e", "secret", "verify-token", "", false, "cli_e2e", mb)
	if err != nil {
		t.Fatalf("NewFeishu: %v", err)
	}
	ch.httpClient = &http.Client{Transport: ft}
	return ch, mb
}

// TestFeishuSendMessageRendersJSON20Card covers upstream c90fc25
// (feishu replies as json 2.0 cards): SendMessage must POST an
// `interactive` message whose content is a JSON 2.0 card with a single
// markdown element carrying the reply text verbatim (so GFM tables and
// bold render natively instead of arriving as literal pipes).
func TestFeishuSendMessageRendersJSON20Card(t *testing.T) {
	ft := &fakeFeishuTransport{}
	ch, _ := newTestFeishu(t, ft)

	text := "**hello**\n\n| A | B |\n|---|---|\n| 1 | 2 |"
	if err := ch.SendMessage(bus.OutboundMessage{ChatID: "oc_chat", Text: text}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	sends := ft.sends()
	if len(sends) != 1 {
		t.Fatalf("send calls = %d; want 1", len(sends))
	}
	payload := sends[0]
	if payload["msg_type"] != "interactive" {
		t.Errorf("msg_type = %q; want interactive (json 2.0 card)", payload["msg_type"])
	}
	if payload["receive_id"] != "oc_chat" {
		t.Errorf("receive_id = %q; want oc_chat", payload["receive_id"])
	}

	var card struct {
		Schema string `json:"schema"`
		Body   struct {
			Elements []struct {
				Tag     string `json:"tag"`
				Content string `json:"content"`
			} `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(payload["content"]), &card); err != nil {
		t.Fatalf("card content is not valid JSON: %v", err)
	}
	if card.Schema != "2.0" {
		t.Errorf("card schema = %q; want 2.0", card.Schema)
	}
	if len(card.Body.Elements) != 1 {
		t.Fatalf("card elements = %d; want 1", len(card.Body.Elements))
	}
	el := card.Body.Elements[0]
	if el.Tag != "markdown" {
		t.Errorf("element.tag = %q; want markdown", el.Tag)
	}
	if el.Content != text {
		t.Errorf("element.content = %q; want original text %q", el.Content, text)
	}
}

// TestFeishuSendMessageFallsBackToPlainTextOnCardFailure covers the
// same upstream commit's resilience: when the interactive-card send
// fails, SendMessage retries as a plain `text` message rather than
// dropping the reply. The fake transport fails the first send and
// succeeds the second, so we observe both attempts.
func TestFeishuSendMessageFallsBackToPlainTextOnCardFailure(t *testing.T) {
	ft := &fakeFeishuTransport{failSendFirstN: 1}
	ch, _ := newTestFeishu(t, ft)

	text := "fallback me"
	if err := ch.SendMessage(bus.OutboundMessage{ChatID: "oc_chat", Text: text}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	sends := ft.sends()
	if len(sends) != 2 {
		t.Fatalf("send calls = %d; want 2 (card attempt + plain fallback)", len(sends))
	}
	if sends[0]["msg_type"] != "interactive" {
		t.Errorf("first attempt msg_type = %q; want interactive", sends[0]["msg_type"])
	}
	last := sends[1]
	if last["msg_type"] != "text" {
		t.Errorf("fallback msg_type = %q; want text", last["msg_type"])
	}
	var content struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(last["content"]), &content); err != nil {
		t.Fatalf("fallback content not JSON: %v", err)
	}
	if content.Text != text {
		t.Errorf("fallback text = %q; want %q", content.Text, text)
	}
}

// TestFeishuMentionNames covers the pure mention-normalization helper
// from upstream 91434c0: name preferred over key, key used as fallback,
// empty entries dropped, duplicates collapsed. This is what group
// routing downstream consumes (msg.Mentions).
func TestFeishuMentionNames(t *testing.T) {
	mention := func(key, name string) struct {
		Key  string `json:"key,omitempty"`
		Name string `json:"name,omitempty"`
	} {
		return struct {
			Key  string `json:"key,omitempty"`
			Name string `json:"name,omitempty"`
		}{Key: key, Name: name}
	}

	if got := feishuMentionNames(feishuMessageEvent{}); got != nil {
		t.Fatalf("empty event mentions = %#v; want nil", got)
	}

	ev := feishuMessageEvent{}
	ev.Message.Mentions = []struct {
		Key  string `json:"key,omitempty"`
		Name string `json:"name,omitempty"`
	}{
		mention("@_u1", "机器人"),
		mention("@_u2", ""), // name empty → key fallback
		mention("", ""),     // both empty → dropped
		mention("@_u1", "机器人"), // duplicate → collapsed
	}
	got := feishuMentionNames(ev)
	want := []string{"机器人", "@_u2"}
	if len(got) != len(want) {
		t.Fatalf("mentions = %#v; want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mentions[%d] = %q; want %q (full: %#v)", i, got[i], want[i], got)
		}
	}
}

// TestFeishuSDKEventToInternalPreservesMentions covers the
// long-connection seam from upstream 91434c0: the SDK's typed event →
// internal feishuMessageEvent translation must carry @mentions through
// (key + name) so the ws path feeds group routing the same way the
// webhook path does. Pure function — no network, no SDK client started.
func TestFeishuSDKEventToInternalPreservesMentions(t *testing.T) {
	senderType, openID := "user", "ou_sender"
	msgID, chatID, chatType, msgType := "om_1", "oc_group", "group", "text"
	content := `{"text":"@机器人 你好"}`
	mentionKey, mentionName := "@_user_1", "机器人"

	ev := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderType: &senderType,
				SenderId:   &larkim.UserId{OpenId: &openID},
			},
			Message: &larkim.EventMessage{
				MessageId:   &msgID,
				ChatId:      &chatID,
				ChatType:    &chatType,
				MessageType: &msgType,
				Content:     &content,
				Mentions: []*larkim.MentionEvent{
					{Key: &mentionKey, Name: &mentionName},
				},
			},
		},
	}

	out := sdkEventToInternal(ev)
	if out.Sender.SenderType != "user" {
		t.Errorf("sender_type = %q; want user", out.Sender.SenderType)
	}
	if out.Sender.SenderID.OpenID != "ou_sender" {
		t.Errorf("sender open_id = %q; want ou_sender", out.Sender.SenderID.OpenID)
	}
	if out.Message.ChatType != "group" || out.Message.MessageType != "text" {
		t.Errorf("chat/message type = %q/%q; want group/text", out.Message.ChatType, out.Message.MessageType)
	}
	if len(out.Message.Mentions) != 1 {
		t.Fatalf("mentions = %d; want 1 preserved", len(out.Message.Mentions))
	}
	if out.Message.Mentions[0].Key != mentionKey || out.Message.Mentions[0].Name != mentionName {
		t.Errorf("mention = key %q name %q; want key %q name %q",
			out.Message.Mentions[0].Key, out.Message.Mentions[0].Name, mentionKey, mentionName)
	}

	// nil event → zero value, no panic.
	if zero := sdkEventToInternal(nil); zero.Sender.SenderType != "" {
		t.Errorf("nil event should produce zero value, got %+v", zero)
	}
}

// TestFeishuGroupWebhookRoutesMentionsToBus covers upstream 91434c0
// at the webhook seam: a group message that @-mentions the bot must
// arrive on the bus as a group message carrying both the text and the
// @mentions (msg.Mentions is what the gateway's routeGroup uses to
// pick the addressed agent). Asserts the full InboundMessage
// translation, not just Mentions.
func TestFeishuGroupWebhookRoutesMentionsToBus(t *testing.T) {
	mb := bus.New()
	ch, err := NewFeishu("cli_e2e", "secret", "verify-token", "", false, "cli_e2e", mb)
	if err != nil {
		t.Fatalf("NewFeishu: %v", err)
	}

	event := map[string]any{
		"schema": "2.0",
		"header": map[string]any{
			"event_id":   "ev_g1",
			"event_type": "im.message.receive_v1",
			"token":      "verify-token",
			"app_id":     "cli_e2e",
		},
		"event": map[string]any{
			"sender": map[string]any{
				"sender_id":   map[string]any{"open_id": "ou_sender"},
				"sender_type": "user",
			},
			"message": map[string]any{
				"message_id":   "om_g1",
				"chat_id":      "oc_group",
				"chat_type":    "group",
				"message_type": "text",
				"content":      `{"text":"@机器人 你好"}`,
				"mentions": []map[string]any{
					{"key": "@_user_1", "name": "机器人"},
				},
			},
		},
	}
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	if _, status, err := ch.HandleWebhook(body); err != nil || status != 200 {
		t.Fatalf("HandleWebhook status=%d err=%v", status, err)
	}

	select {
	case got := <-mb.Inbound:
		if got.Channel != "feishu" {
			t.Errorf("channel = %q; want feishu", got.Channel)
		}
		if got.AccountID != "cli_e2e" {
			t.Errorf("account = %q; want cli_e2e", got.AccountID)
		}
		if got.PeerKind != "group" {
			t.Errorf("peer_kind = %q; want group (drives routeGroup)", got.PeerKind)
		}
		if got.UserID != "ou_sender" {
			t.Errorf("user_id = %q; want ou_sender", got.UserID)
		}
		if got.MessageID != "om_g1" {
			t.Errorf("message_id = %q; want om_g1", got.MessageID)
		}
		if got.Text != "@机器人 你好" {
			t.Errorf("text = %q; want @机器人 你好", got.Text)
		}
		if len(got.Mentions) != 1 || got.Mentions[0] != "机器人" {
			t.Errorf("mentions = %#v; want [机器人] (preserved for group routing)", got.Mentions)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for inbound message")
	}

	// A p2p (dm) message must set peer_kind=dm.
	p2p := map[string]any{
		"schema": "2.0",
		"header": map[string]any{
			"event_id": "ev_d1", "event_type": "im.message.receive_v1",
			"token": "verify-token", "app_id": "cli_e2e",
		},
		"event": map[string]any{
			"sender": map[string]any{
				"sender_id": map[string]any{"open_id": "ou_dm"}, "sender_type": "user",
			},
			"message": map[string]any{
				"message_id": "om_d1", "chat_id": "oc_dm", "chat_type": "p2p",
				"message_type": "text", "content": `{"text":"hi"}`,
			},
		},
	}
	p2pBody, _ := json.Marshal(p2p)
	if _, status, err := ch.HandleWebhook(p2pBody); err != nil || status != 200 {
		t.Fatalf("p2p HandleWebhook status=%d err=%v", status, err)
	}
	select {
	case got := <-mb.Inbound:
		if got.PeerKind != "dm" {
			t.Errorf("p2p peer_kind = %q; want dm", got.PeerKind)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for p2p inbound")
	}

	// Self/bot-sent messages (sender_type != user) are dropped entirely.
	self := map[string]any{
		"schema": "2.0",
		"header": map[string]any{
			"event_id": "ev_s1", "event_type": "im.message.receive_v1",
			"token": "verify-token", "app_id": "cli_e2e",
		},
		"event": map[string]any{
			"sender": map[string]any{
				"sender_id": map[string]any{"open_id": "ou_bot"}, "sender_type": "app",
			},
			"message": map[string]any{
				"message_id": "om_s1", "chat_id": "oc_g", "chat_type": "group",
				"message_type": "text", "content": `{"text":"loop"}`,
			},
		},
	}
	selfBody, _ := json.Marshal(self)
	if _, status, err := ch.HandleWebhook(selfBody); err != nil || status != 200 {
		t.Fatalf("self-message HandleWebhook status=%d err=%v", status, err)
	}
	select {
	case got := <-mb.Inbound:
		t.Fatalf("self/bot message should be dropped, got %+v", got)
	case <-time.After(200 * time.Millisecond):
		// expected — nothing should arrive
	}
}
