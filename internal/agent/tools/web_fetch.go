package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/toolproviders"
)

type webFetchArgs struct {
	URL    string `json:"url"`
	MaxLen int    `json:"max_length,omitempty"` // default 10000
}

const (
	defaultMaxLen  = 10000
	fetchTimeout   = 30 * time.Second
	fetchUserAgent = "FastClaw/1.0 (AI Agent Web Fetcher)"
)

var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

func init() {
	// Register will be called from registerWebFetch
}

const webFetchDescription = "Fetch a single known URL and return its plain text. " +
	"If the user's message itself contains a URL or bare domain " +
	"(e.g. 'idoubi.ai', 'https://example.com/cv'), fetch THAT URL " +
	"directly — prepend https:// for bare domains — instead of " +
	"running web_search first. DO NOT guess URLs from memory: " +
	"your training data has stale paths and you will burn rounds " +
	"on 404s. When the user described a page in natural language " +
	"with no URL, run web_search first to discover the URL, then " +
	"web_fetch that exact URL. If web_search isn't available, " +
	"prefer well-known stable hosts (en.wikipedia.org, github.com), " +
	"not date-stamped article URLs. A URL that returned 4xx/5xx " +
	"earlier in this turn will be refused if you retry it."

var webFetchSchema = map[string]interface{}{
	"type": "object",
	"properties": map[string]interface{}{
		"url": map[string]interface{}{
			"type":        "string",
			"description": "The exact URL to fetch (full https://… form). Don't paste search-result snippets or guessed paths.",
		},
		"max_length": map[string]interface{}{
			"type":        "integer",
			"description": "Maximum characters to return (default 10000)",
		},
	},
	"required": []string{"url"},
}

// RegisterWebFetch registers the web_fetch tool with the built-in
// http.DefaultClient backend. This is the legacy zero-config path; callers
// that want provider routing should use RegisterWebFetchChain instead and
// skip this.
//
// The description is deliberately blunt about the "search-before-fetch"
// rule and the URL-guessing failure mode. Models that have only
// web_fetch tend to fall back on training-memory URLs — which are
// often stale / hallucinated — and burn a dozen rounds 404'ing.
// Calling that out here, where the tool catalog gets serialized into
// the model's prompt, is far more effective than burying the
// guidance in SOUL.md.
func RegisterWebFetch(r *Registry) {
	r.Register("web_fetch", webFetchDescription, webFetchSchema, webFetchToolWith(r))
}

// RegisterWebFetchChain registers web_fetch backed by a toolproviders.Chain.
// When the chain is nil or unavailable, the registration falls back to the
// legacy direct fetcher so the tool stays available — chain-first, built-in
// fallback. Tool name + schema are unchanged so the model's perspective is
// identical regardless of which backend services the call.
func RegisterWebFetchChain(r *Registry, chain *toolproviders.Chain) {
	if chain == nil || !chain.Available() {
		RegisterWebFetch(r)
		return
	}
	r.Register("web_fetch", webFetchDescription, webFetchSchema, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args webFetchArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.URL == "" {
			return "", fmt.Errorf("url is required")
		}
		// Re-use the same per-turn duplicate-URL guard the direct
		// fetcher uses so the model can't burn rounds rotating through
		// guessed URLs that all 404. Applies regardless of which
		// provider is actually backing the call.
		if r != nil {
			if prev := r.PriorFailure("web_fetch", string(rawArgs)); prev != "" {
				return "", fmt.Errorf(
					"already tried %s earlier in this turn (%s). DO NOT retry the same URL — pick a different source, or use web_search to find a verified URL",
					args.URL, prev,
				)
			}
		}
		raw := map[string]any{"url": args.URL}
		if args.MaxLen > 0 {
			raw["max_length"] = args.MaxLen
		}
		resp, err := chain.Execute(ctx, raw)
		if err != nil {
			return "", err
		}
		return resp.Text, nil
	})
}

// webFetchToolWith binds the registry into the tool closure so the
// implementation can consult turn-state failure history. Wrapping
// instead of having the tool reach into a global keeps per-agent
// registries isolated when the same process serves multiple agents.
func webFetchToolWith(r *Registry) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		return webFetchTool(ctx, r, rawArgs)
	}
}

func webFetchTool(ctx context.Context, r *Registry, rawArgs json.RawMessage) (string, error) {
	var args webFetchArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	if args.URL == "" {
		return "", fmt.Errorf("url is required")
	}

	// Refuse a retry of a URL that already failed in this turn. The
	// agent loop's loop-detector catches "exact same call 3 times in
	// a row" but not the more common pattern: the model rotates
	// through five guessed URLs that all 404, then comes back to the
	// first guess. Each attempt looks "different" to the loop
	// detector but the user is paying for round-trips that we know
	// up-front will fail.
	if r != nil {
		if prev := r.PriorFailure("web_fetch", string(rawArgs)); prev != "" {
			return "", fmt.Errorf(
				"already tried %s earlier in this turn (%s). DO NOT retry the same URL — pick a different source, or use web_search to find a verified URL",
				args.URL, prev,
			)
		}
	}

	maxLen := args.MaxLen
	if maxLen <= 0 {
		maxLen = defaultMaxLen
	}

	fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, args.URL, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", fetchUserAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch url: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	// Read body with a limit to prevent memory issues
	limitReader := io.LimitReader(resp.Body, int64(maxLen*3)) // read more than needed since HTML is verbose
	body, err := io.ReadAll(limitReader)
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}

	// Strip HTML tags
	text := stripHTML(string(body))

	// Truncate to max length
	if len(text) > maxLen {
		text = text[:maxLen] + "\n[...truncated]"
	}

	return text, nil
}

// stripHTML removes HTML tags and cleans up whitespace.
func stripHTML(html string) string {
	// Remove script and style elements entirely
	scriptRe := regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	html = scriptRe.ReplaceAllString(html, "")
	styleRe := regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	html = styleRe.ReplaceAllString(html, "")

	// Remove HTML tags
	text := htmlTagRe.ReplaceAllString(html, " ")

	// Decode common HTML entities
	text = strings.ReplaceAll(text, "&amp;", "&")
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	text = strings.ReplaceAll(text, "&quot;", "\"")
	text = strings.ReplaceAll(text, "&#39;", "'")
	text = strings.ReplaceAll(text, "&nbsp;", " ")

	// Collapse whitespace
	spaceRe := regexp.MustCompile(`[ \t]+`)
	text = spaceRe.ReplaceAllString(text, " ")

	// Collapse multiple newlines
	nlRe := regexp.MustCompile(`\n{3,}`)
	text = nlRe.ReplaceAllString(text, "\n\n")

	return strings.TrimSpace(text)
}
