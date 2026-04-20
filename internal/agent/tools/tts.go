package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fastclaw-ai/fastclaw/internal/toolproviders"
)

// RegisterTTSChain registers the tts tool against a provider chain. Absent
// credentials ⇒ the tool isn't visible to the agent at all.
func RegisterTTSChain(r *Registry, chain *toolproviders.Chain) {
	if chain == nil || !chain.Available() {
		return
	}
	r.Register("tts", "Convert text to speech. Uses a configurable provider chain (OpenAI tts-1, MiniMax speech-02, …) with automatic fallback. The audio file is attached to the chat message automatically.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"text": map[string]interface{}{
				"type":        "string",
				"description": "Text to synthesize",
			},
			"voice": map[string]interface{}{
				"type":        "string",
				"description": "Voice id (provider-specific; default picked automatically)",
			},
		},
		"required": []string{"text"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args map[string]any
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		resp, err := chain.Execute(ctx, args)
		if err != nil {
			return "", err
		}
		return resp.Text, nil
	})
}
