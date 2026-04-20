package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fastclaw-ai/fastclaw/internal/toolproviders"
)

// RegisterWebSearchChain exposes the web_search tool backed by a
// toolproviders.Chain. When the chain has no configured provider, nothing is
// registered at all — the LLM doesn't see a tool it can't use. One tool, many
// providers, fallback chosen at runtime.
func RegisterWebSearchChain(r *Registry, chain *toolproviders.Chain) {
	if chain == nil || !chain.Available() {
		return
	}
	r.Register("web_search", "Search the web and return results with titles, URLs, and snippets. Backed by a configurable provider chain (e.g. exa, brave, searxng) with automatic fallback.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"query": map[string]interface{}{
				"type":        "string",
				"description": "The search query",
			},
			"count": map[string]interface{}{
				"type":        "integer",
				"description": "Number of results to return (default 5, max 20)",
			},
		},
		"required": []string{"query"},
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
