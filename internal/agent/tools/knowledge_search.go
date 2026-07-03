package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// KnowledgeSearcher searches the owner-uploaded knowledge corpus. The
// store resolves the agent-owner fallback internally, so any of the
// registry's user ids finds the corpus on shared agents.
type KnowledgeSearcher interface {
	SearchKnowledgeChunks(ctx context.Context, agentID, userID, query string, limit int) ([]store.KnowledgeChunkRecord, error)
}

type knowledgeSearchArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"` // default 6
}

// RegisterKnowledgeSearch registers the knowledge_search tool. The system
// prompt's <agent_knowledge_base mode="index"> section tells the model to
// call it when the corpus is too large to inject in full; for small
// corpora the whole knowledge base is already in the prompt and the tool
// simply isn't needed.
func RegisterKnowledgeSearch(r *Registry, searcher KnowledgeSearcher) {
	r.Register("knowledge_search",
		"Search the agent's knowledge base (reference files uploaded by the agent owner) by keyword. "+
			"Use focused keywords from the question; if a search misses, retry once or twice with different or broader terms.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "Keywords to search for in the knowledge files",
				},
				"limit": map[string]interface{}{
					"type":        "integer",
					"description": "Maximum number of snippets to return (default 6)",
				},
			},
			"required": []string{"query"},
		}, makeKnowledgeSearch(r, searcher))
}

func makeKnowledgeSearch(r *Registry, searcher KnowledgeSearcher) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args knowledgeSearchArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		query := strings.TrimSpace(args.Query)
		if query == "" {
			return "", fmt.Errorf("query is required")
		}
		limit := args.Limit
		if limit <= 0 || limit > 20 {
			limit = 6
		}
		// Knowledge rows live under the agent owner's user id; the same
		// resolution write-side file tools use (owner for owner-scoped
		// system files, chatter otherwise) picks a valid id here and the
		// store's owner fallback covers the rest.
		userID := r.systemFileUserID("KNOWLEDGE.md")
		if userID == "" {
			return "The knowledge base is not available in this deployment.", nil
		}
		chunks, err := searcher.SearchKnowledgeChunks(ctx, r.agentID, userID, query, limit)
		if err != nil {
			return "", fmt.Errorf("knowledge search: %w", err)
		}
		if len(chunks) == 0 {
			return fmt.Sprintf("No knowledge base content matched %q. Try different or broader keywords, or answer from conversation context if the knowledge base doesn't cover this.", query), nil
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "Found %d knowledge base snippets for %q:\n\n", len(chunks), query)
		for _, chunk := range chunks {
			fmt.Fprintf(&sb, "--- %s (section %d) ---\n%s\n\n",
				knowledgeChunkDisplayName(chunk.Path), chunk.ChunkIndex+1, strings.TrimSpace(chunk.Content))
		}
		sb.WriteString("When you use facts from these snippets, mention the source file name in your answer. If they don't actually answer the question, say so instead of stretching them.")
		return sb.String(), nil
	}
}

// knowledgeChunkDisplayName strips the knowledge/ prefix and the
// 12-hex-hash dedup prefix so tool output shows the name the owner
// uploaded. Mirrors setup.knowledgeDisplayName.
func knowledgeChunkDisplayName(path string) string {
	name := strings.TrimPrefix(path, "knowledge/")
	if len(name) > 13 && name[12] == '-' {
		allHex := true
		for _, r := range name[:12] {
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
				allHex = false
				break
			}
		}
		if allHex {
			return name[13:]
		}
	}
	return name
}
