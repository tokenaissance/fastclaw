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
