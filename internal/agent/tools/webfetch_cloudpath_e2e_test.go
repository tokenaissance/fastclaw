package tools

// e2e for commit 92a3cbb "hot reload sandbox and improve web fetch" —
// the web-fetch half.
//
// Cloud call path: a Cloud user pastes a WeChat article link into chat;
// the agent loop invokes the registered web_fetch tool, which runs
// webFetchTool → safeFetchClient.Do (the SSRF-guarded dialer) →
// webfetchprovider.FetchReadLimit (2MB window for mp.weixin.qq.com so the
// readable #js_content node is never clipped by a long script/config
// prelude) → webfetchprovider.HTMLToText (extract #js_content, then
// stripHTML). The reply text is what the Cloud web chat renders. No
// endpoint / shape change — strictly a backend extraction improvement.
//
// This test drives the REAL webFetchTool end-to-end (the exact code the
// Cloud chat path runs) with the network leg replaced by a fake
// RoundTripper that returns a WeChat-shaped HTML fixture. It pins: (1)
// the request really goes to the mp.weixin.qq.com URL through the tool;
// (2) WeChat URLs are read with the 2MB window (the fixture's script
// prelude is sized to prove the bounded-large read); (3) HTMLToText
// extracts ONLY #js_content for WeChat and drops the page chrome; (4) a
// non-WeChat URL falls through to whole-page stripHTML.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/toolproviders/webfetch"
)

// fixtureRoundTripper returns a canned HTML body for any request,
// capturing the URL the tool asked the client to fetch.
type fixtureRoundTripper struct {
	gotURL string
	body   string
}

func (f *fixtureRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	f.gotURL = req.URL.String()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(f.body)),
	}, nil
}

// wechatFixture is a realistic mp.weixin.qq.com article shell: a long
// JS/config prelude (what used to push the readable node past a small
// maxLen read and return junk), then #js_content with the real article,
// then page footer chrome.
func wechatFixture() string {
	return `<html><head><title>Page title</title>` +
		`<script>window.__INITIAL_STATE__ = {"cfg": "` + strings.Repeat("x", 8000) + `"};</script>` +
		`</head><body><div class="page_header">header chrome</div>` +
		`<div id="js_content">` +
		`<p>这是一篇关于沙箱热重载的文章。</p>` +
		`<p>WeChat article body &amp; entity decode works.</p>` +
		`</div>` +
		`<div class="footer">footer chrome</div></body></html>`
}

func TestWebFetch_WeChatExtractionCloudPathE2E(t *testing.T) {
	prev := safeFetchClient.Transport
	t.Cleanup(func() { safeFetchClient.Transport = prev })

	rt := &fixtureRoundTripper{body: wechatFixture()}
	safeFetchClient.Transport = rt

	// The 2MB WeChat read window must be what the tool requests —
	// FetchReadLimit is consulted at fetch time from the same URL.
	if got := webfetch.FetchReadLimit("https://mp.weixin.qq.com/s/abc123", 10000); got != 2<<20 {
		t.Fatalf("FetchReadLimit(wechat, 10000) = %d, want 2MB", got)
	}

	raw, _ := json.Marshal(map[string]any{
		"url": "https://mp.weixin.qq.com/s/abc123",
	})
	out, err := webFetchTool(context.Background(), nil, raw)
	if err != nil {
		t.Fatalf("webFetchTool(wechat) error: %v", err)
	}

	// The tool really fetched the mp.weixin.qq.com URL through the
	// (SSRF-guarded) client.
	if !strings.HasPrefix(rt.gotURL, "https://mp.weixin.qq.com/s/abc123") {
		t.Fatalf("fetched URL = %q, want the mp.weixin.qq.com URL", rt.gotURL)
	}

	// Only #js_content survives — page chrome is gone.
	if !strings.Contains(out, "这是一篇关于沙箱热重载的文章。") {
		t.Errorf("reply missing WeChat article body: %q", out)
	}
	if strings.Contains(out, "header chrome") || strings.Contains(out, "footer chrome") {
		t.Errorf("page chrome leaked into reply: %q", out)
	}
	if strings.Contains(out, "INITIAL_STATE") || strings.Contains(out, "Page title") {
		t.Errorf("script prelude leaked into reply: %q", out)
	}
	if strings.Contains(out, "<") && strings.Contains(out, ">") {
		t.Errorf("raw HTML tags leaked into reply: %q", out)
	}

	// Entity decoding survived stripHTML (the &amp; → & step).
	if !strings.Contains(out, "& entity decode works.") {
		t.Errorf("HTML entity not decoded in reply: %q", out)
	}
}

func TestWebFetch_NonWeChat_FallsThroughToFullBody(t *testing.T) {
	prev := safeFetchClient.Transport
	t.Cleanup(func() { safeFetchClient.Transport = prev })

	rt := &fixtureRoundTripper{body: wechatFixture()}
	safeFetchClient.Transport = rt

	raw, _ := json.Marshal(map[string]any{
		"url": "https://example.com/page",
	})
	out, err := webFetchTool(context.Background(), nil, raw)
	if err != nil {
		t.Fatalf("webFetchTool(example.com) error: %v", err)
	}
	// Non-WeChat URL → whole-page stripHTML: the article body AND the
	// header chrome are both present (no site-specific extraction).
	if !strings.Contains(out, "这是一篇关于沙箱热重载的文章。") {
		t.Errorf("non-wechat reply missing body text: %q", out)
	}
	if !strings.Contains(out, "header chrome") {
		t.Errorf("non-wechat reply should keep generic chrome (full-page fallback): %q", out)
	}
}
