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
)

type webFetchArgs struct {
	URL     string `json:"url"`
	MaxLen  int    `json:"max_length,omitempty"` // default 10000
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

// RegisterWebFetch registers the web_fetch tool.
func RegisterWebFetch(r *Registry) {
	r.Register("web_fetch", "Fetch a web page and return its plain text content (HTML tags stripped)", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"url": map[string]interface{}{
				"type":        "string",
				"description": "The URL to fetch",
			},
			"max_length": map[string]interface{}{
				"type":        "integer",
				"description": "Maximum characters to return (default 10000)",
			},
		},
		"required": []string{"url"},
	}, webFetchTool)
}

func webFetchTool(ctx context.Context, rawArgs json.RawMessage) (string, error) {
	var args webFetchArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	if args.URL == "" {
		return "", fmt.Errorf("url is required")
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
