// Package webfetch bundles built-in web_fetch providers. Every provider takes
// the same {url, max_length} arg shape and returns plain text the LLM can
// read directly. Per-call credentials/endpoint come from the
// toolproviders.Request.Config so providers stay stateless.
package webfetch

import (
	"fmt"
	stdhtml "html"
	"net/url"
	"regexp"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/toolproviders"
)

// Category is the tool category these providers plug into.
const Category = "web_fetch"

// DefaultMaxLen is the cap when the caller doesn't pass max_length. Mirrors
// the value that the legacy direct-only web_fetch tool used so swapping in
// a chain doesn't change reply truncation behaviour.
const DefaultMaxLen = 10000

// RegisterAll installs every built-in web_fetch provider in r.
func RegisterAll(r *toolproviders.Registry) {
	r.Register(&Direct{})
	r.Register(&Jina{})
	r.Register(&Firecrawl{})
}

type args struct {
	URL    string
	MaxLen int
}

func parseArgs(raw map[string]any) (args, error) {
	var a args
	if s, ok := raw["url"].(string); ok {
		a.URL = strings.TrimSpace(s)
	}
	if a.URL == "" {
		return a, fmt.Errorf("url is required")
	}
	switch v := raw["max_length"].(type) {
	case float64:
		a.MaxLen = int(v)
	case int:
		a.MaxLen = v
	}
	if a.MaxLen <= 0 {
		a.MaxLen = DefaultMaxLen
	}
	return a, nil
}

// truncate caps text at maxLen with a visible marker so the LLM knows the
// page was longer than what it received and can ask for a higher cap (or
// pick a more specific URL) instead of treating the cut as authoritative.
func truncate(text string, maxLen int) string {
	if maxLen <= 0 || len(text) <= maxLen {
		return text
	}
	return text[:maxLen] + "\n[...truncated]"
}

func isWeChatArticle(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "mp.weixin.qq.com" || strings.HasSuffix(host, ".mp.weixin.qq.com")
}

func FetchReadLimit(rawURL string, maxLen int) int64 {
	if isWeChatArticle(rawURL) {
		return 2 << 20
	}
	return int64(maxLen * 3)
}

func HTMLToText(rawURL, htmlBody string) string {
	if isWeChatArticle(rawURL) {
		if article := extractElementByID(htmlBody, "js_content"); article != "" {
			return stripHTML(article)
		}
	}
	return stripHTML(htmlBody)
}

func extractElementByID(htmlBody, id string) string {
	re := regexp.MustCompile(`(?is)<([a-z0-9]+)\b[^>]*\bid\s*=\s*['"]` + regexp.QuoteMeta(id) + `['"][^>]*>`)
	loc := re.FindStringSubmatchIndex(htmlBody)
	if loc == nil {
		return ""
	}
	tagName := strings.ToLower(htmlBody[loc[2]:loc[3]])
	start := loc[0]
	searchFrom := loc[1]
	tagRe := regexp.MustCompile(`(?is)<\s*(/?)\s*` + regexp.QuoteMeta(tagName) + `\b[^>]*>`)
	depth := 1
	for _, m := range tagRe.FindAllStringSubmatchIndex(htmlBody[searchFrom:], -1) {
		tagStart := searchFrom + m[0]
		tagEnd := searchFrom + m[1]
		closing := m[2] >= 0 && htmlBody[searchFrom+m[2]:searchFrom+m[3]] == "/"
		selfClosing := strings.HasSuffix(strings.TrimSpace(htmlBody[tagStart:tagEnd]), "/>")
		if closing {
			depth--
			if depth == 0 {
				return htmlBody[start:tagEnd]
			}
			continue
		}
		if !selfClosing {
			depth++
		}
	}
	return ""
}

var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

// stripHTML removes script/style blocks, drops remaining HTML tags, and
// collapses whitespace. Mirrors the helper that lived in the agent's
// web_fetch tool so the Direct provider produces identical output.
func stripHTML(html string) string {
	scriptRe := regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	html = scriptRe.ReplaceAllString(html, "")
	styleRe := regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	html = styleRe.ReplaceAllString(html, "")

	text := htmlTagRe.ReplaceAllString(html, " ")

	text = strings.ReplaceAll(text, "&amp;", "&")
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	text = strings.ReplaceAll(text, "&quot;", "\"")
	text = strings.ReplaceAll(text, "&#39;", "'")
	text = strings.ReplaceAll(text, "&nbsp;", " ")
	text = stdhtml.UnescapeString(text)

	spaceRe := regexp.MustCompile(`[ \t]+`)
	text = spaceRe.ReplaceAllString(text, " ")
	nlRe := regexp.MustCompile(`\n{3,}`)
	text = nlRe.ReplaceAllString(text, "\n\n")

	return strings.TrimSpace(text)
}
