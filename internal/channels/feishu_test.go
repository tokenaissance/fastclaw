package channels

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

func TestFeishuWebhookPreservesMentions(t *testing.T) {
	mb := bus.New()
	ch, err := NewFeishu("cli_test", "secret", "verify-token", "", false, "cli_test", mb)
	if err != nil {
		t.Fatalf("NewFeishu: %v", err)
	}

	event := map[string]any{
		"schema": "2.0",
		"header": map[string]any{
			"event_id":   "ev_1",
			"event_type": "im.message.receive_v1",
			"token":      "verify-token",
			"app_id":     "cli_test",
		},
		"event": map[string]any{
			"sender": map[string]any{
				"sender_id":   map[string]any{"open_id": "ou_sender"},
				"sender_type": "user",
			},
			"message": map[string]any{
				"message_id":   "om_1",
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
		if len(got.Mentions) != 1 || got.Mentions[0] != "机器人" {
			t.Fatalf("mentions = %#v, want [机器人]", got.Mentions)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for inbound message")
	}
}

func TestBuildFeishuMarkdownCardJSON(t *testing.T) {
	in := "**hello**\n\n| A | B |\n|---|---|\n| 1 | 2 |"
	got, err := buildFeishuMarkdownCardJSON(in)
	if err != nil {
		t.Fatalf("buildFeishuMarkdownCardJSON: %v", err)
	}

	var card map[string]any
	if err := json.Unmarshal(got, &card); err != nil {
		t.Fatalf("unmarshal card json: %v", err)
	}
	if card["schema"] != "2.0" {
		t.Fatalf("schema = %v, want 2.0", card["schema"])
	}

	body, ok := card["body"].(map[string]any)
	if !ok {
		t.Fatalf("body = %#v, want object", card["body"])
	}
	elements, ok := body["elements"].([]any)
	if !ok {
		t.Fatalf("body.elements = %#v, want array", body["elements"])
	}
	if len(elements) != 1 {
		t.Fatalf("len(body.elements) = %d, want 1", len(elements))
	}

	element, ok := elements[0].(map[string]any)
	if !ok {
		t.Fatalf("element = %#v, want object", elements[0])
	}
	if element["tag"] != "markdown" {
		t.Fatalf("element.tag = %v, want markdown", element["tag"])
	}
	if element["content"] != in {
		t.Fatalf("element.content = %q, want %q", element["content"], in)
	}
}
