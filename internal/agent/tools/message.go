package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

type messageArgs struct {
	Channel string `json:"channel"`
	ChatID  string `json:"chat_id"`
	Text    string `json:"text"`
}

// RegisterMessage registers the message tool with the given message bus.
func RegisterMessage(r *Registry, mb *bus.MessageBus) {
	r.tools["message"] = registeredTool{
		def: r.tools["message"].def,
		fn:  makeMessageTool(mb),
	}
}

func registerMessage(r *Registry) {
	// Register with a placeholder; will be re-registered with actual bus later.
	r.Register("message", "Send a message to a channel", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"channel": map[string]interface{}{
				"type":        "string",
				"description": "Target channel (e.g. 'telegram')",
			},
			"chat_id": map[string]interface{}{
				"type":        "string",
				"description": "Target chat ID",
			},
			"text": map[string]interface{}{
				"type":        "string",
				"description": "Message text to send",
			},
		},
		"required": []string{"channel", "chat_id", "text"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		return "", fmt.Errorf("message bus not initialized")
	})
}

func makeMessageTool(mb *bus.MessageBus) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args messageArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}

		mb.Outbound <- bus.OutboundMessage{
			Channel: args.Channel,
			ChatID:  args.ChatID,
			Text:    args.Text,
		}

		return "Message sent", nil
	}
}
