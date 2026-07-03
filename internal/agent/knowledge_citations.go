package agent

import (
	"regexp"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/provider"
)

// knowledgeCitationSource is one [K#]-citable knowledge file, parsed back
// out of the full-mode <agent_knowledge_base> section of the system
// prompt (see knowledgeFullSection). Attached to assistant messages as
// metadata so the web UI can render [K#] citations as clickable badges
// that open the source file.
type knowledgeCitationSource struct {
	ID   string `json:"id"`
	File string `json:"file"`
	Path string `json:"path"`
}

var knowledgeSourceBlockRe = regexp.MustCompile(`source_id:\s*(K\d+)\s*\nfile:\s*([^\n]+)\s*\npath:\s*([^\n]+)`)

func extractKnowledgeCitationSources(systemPrompt string) []knowledgeCitationSource {
	matches := knowledgeSourceBlockRe.FindAllStringSubmatch(systemPrompt, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]knowledgeCitationSource, 0, len(matches))
	for _, m := range matches {
		out = append(out, knowledgeCitationSource{
			ID:   strings.TrimSpace(m[1]),
			File: strings.TrimSpace(m[2]),
			Path: strings.TrimSpace(m[3]),
		})
	}
	return out
}

func knowledgeMetadata(sources []knowledgeCitationSource) map[string]any {
	if len(sources) == 0 {
		return nil
	}
	return map[string]any{"knowledgeSources": sources}
}

func mergeMetadata(base map[string]any, extra map[string]any) map[string]any {
	if len(base) == 0 && len(extra) == 0 {
		return nil
	}
	out := make(map[string]any, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func firstSystemContent(messages []provider.Message) string {
	for _, msg := range messages {
		if msg.Role == "system" {
			return msg.Content
		}
	}
	return ""
}
