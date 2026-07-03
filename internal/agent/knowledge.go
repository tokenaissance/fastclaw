package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// The knowledge base is owner-curated reference material: the pinned
// KNOWLEDGE.md (edited as a system file) plus files uploaded under the
// knowledge/ prefix. This module renders it into a prompt section that
// depends only on the corpus itself — never on the current message — so
// the system prompt stays stable across turns (prompt-cache friendly)
// and follow-up questions keep the same context.
//
// Two shapes, picked by corpus size:
//
//	full  — the whole corpus fits the budget: every file is injected
//	        verbatim with a per-file source id ([K1], [K2], …) the model
//	        cites inline; the web UI turns those ids into clickable
//	        source badges (see knowledge_citations.go).
//	index — corpus too large to inline: the pinned KNOWLEDGE.md (capped)
//	        plus a file index is injected, and the model is pointed at
//	        the knowledge_search tool for on-demand retrieval.
const (
	// knowledgeFullInjectMaxChars is the total corpus size (runes) at or
	// below which every knowledge file is injected verbatim. Small corpora
	// skip retrieval entirely — 100% recall, no missed-lookup failure mode.
	knowledgeFullInjectMaxChars = 24000
	// knowledgePinnedMaxChars caps the always-injected KNOWLEDGE.md in
	// index mode so a runaway pinned note can't crowd out the prompt.
	knowledgePinnedMaxChars = 8000
)

// knowledgeDocLister is the optional MemoryStore capability exposing the
// uploaded corpus. MemoryStoreAdapter implements it; the legacy filesystem
// setup doesn't, in which case only the pinned KNOWLEDGE.md applies.
type knowledgeDocLister interface {
	ListKnowledgeDocs(ctx context.Context, agentID, userID string) ([]store.KnowledgeDoc, error)
}

func modKnowledge(p *promptCtx) string {
	return p.cb.buildKnowledgeSection()
}

func (cb *ContextBuilder) buildKnowledgeSection() string {
	pinned := cb.loadFileForUser("KNOWLEDGE.md", cb.userID)
	var docs []store.KnowledgeDoc
	if lister, ok := cb.store.(knowledgeDocLister); ok {
		ctx := context.Background()
		if cb.userID != "" {
			ctx = config.WithUserID(ctx, cb.userID)
		}
		if got, err := lister.ListKnowledgeDocs(ctx, cb.agentID, cb.userID); err == nil {
			docs = got
		}
	}
	if pinned == "" && len(docs) == 0 {
		return ""
	}
	total := len([]rune(pinned))
	for _, doc := range docs {
		total += len([]rune(doc.Content))
	}
	if total <= knowledgeFullInjectMaxChars {
		return knowledgeFullSection(pinned, docs)
	}
	return knowledgeIndexSection(pinned, docs)
}

// knowledgeFullSection injects the entire corpus, one source block per
// file. The source_id/file/path lines are parsed back out by
// extractKnowledgeCitationSources so [K#] citations in the reply can be
// resolved to their file.
func knowledgeFullSection(pinned string, docs []store.KnowledgeDoc) string {
	var blocks []string
	id := 0
	addBlock := func(file, path, content string) {
		id++
		blocks = append(blocks, fmt.Sprintf("## [K%d] %s\nsource_id: K%d\nfile: %s\npath: %s\n\n%s",
			id, file, id, file, path, strings.TrimSpace(content)))
	}
	if pinned != "" {
		addBlock("KNOWLEDGE.md", "KNOWLEDGE.md", pinned)
	}
	for _, doc := range docs {
		if strings.TrimSpace(doc.Content) == "" {
			continue
		}
		addBlock(knowledgeDocDisplayName(doc.Path), doc.Path, doc.Content)
	}
	if len(blocks) == 0 {
		return ""
	}
	return "<agent_knowledge_base mode=\"full\">\n" +
		"Reference files curated by the agent owner. Treat them as factual, current, and authoritative source material. " +
		"Every file has a source_id like K1. When you use a fact from a file, cite it inline with the source_id, e.g. [K1]; " +
		"if several files support a point, cite all of them, e.g. [K1][K3]. " +
		"If the chatter's question is not covered by these files or the conversation, say what is unknown instead of inventing details.\n\n" +
		strings.Join(blocks, "\n\n---\n\n") +
		"\n</agent_knowledge_base>"
}

// knowledgeIndexSection lists the corpus without inlining it and points
// the model at the knowledge_search tool.
func knowledgeIndexSection(pinned string, docs []store.KnowledgeDoc) string {
	var sb strings.Builder
	sb.WriteString("<agent_knowledge_base mode=\"index\">\n")
	sb.WriteString(fmt.Sprintf(
		"The agent owner uploaded %d knowledge files — too large to include in full, so only an index is shown. "+
			"Whenever the chatter asks something these files may cover, call the knowledge_search tool with focused keywords "+
			"(retry with different terms if the first search misses) and answer from the returned snippets, mentioning the "+
			"source file name. If the files don't cover the question, say what is unknown instead of inventing details.\n",
		len(docs)))
	if pinned != "" {
		if runes := []rune(pinned); len(runes) > knowledgePinnedMaxChars {
			pinned = string(runes[:knowledgePinnedMaxChars]) + "\n[KNOWLEDGE.md truncated — use knowledge_search for the rest]"
		}
		sb.WriteString("\n## Pinned notes (KNOWLEDGE.md)\n")
		sb.WriteString(pinned)
		sb.WriteString("\n")
	}
	sb.WriteString("\n## Files\n")
	for _, doc := range docs {
		sb.WriteString(fmt.Sprintf("- %s (%.1f KB)\n", knowledgeDocDisplayName(doc.Path), float64(len(doc.Content))/1024))
	}
	sb.WriteString("</agent_knowledge_base>")
	return sb.String()
}

// knowledgeDocDisplayName strips the knowledge/ prefix and the
// 12-hex-hash filename prefix the upload handler adds for dedup, giving
// back the name the owner uploaded. Mirrors setup.knowledgeDisplayName.
func knowledgeDocDisplayName(path string) string {
	name := strings.TrimPrefix(path, "knowledge/")
	if len(name) > 13 && name[12] == '-' && isHexString(name[:12]) {
		return name[13:]
	}
	return name
}

func isHexString(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
