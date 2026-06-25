package webfetch

import (
	"strings"
	"testing"
)

func TestHTMLToTextExtractsWeChatArticleBody(t *testing.T) {
	html := `<html><head><script>window.cgiData = { noisy: "` + strings.Repeat("x", 1024) + `" }</script></head>
<body>
<div id="js_content" class="rich_media_content">
  <section><p>第一段&nbsp;内容</p><div><p>第二段 &amp; 更多</p></div></section>
</div>
<script>window.after = "ignore me"</script>
</body></html>`

	got := HTMLToText("https://mp.weixin.qq.com/s/example", html)
	if !strings.Contains(got, "第一段 内容") || !strings.Contains(got, "第二段 & 更多") {
		t.Fatalf("expected article text, got %q", got)
	}
	if strings.Contains(got, "window.cgiData") || strings.Contains(got, "ignore me") {
		t.Fatalf("expected scripts to be ignored, got %q", got)
	}
}

func TestFetchReadLimitUsesLargerWindowForWeChat(t *testing.T) {
	if got := FetchReadLimit("https://mp.weixin.qq.com/s/example", 10000); got < 1<<20 {
		t.Fatalf("FetchReadLimit for WeChat = %d, want a large bounded window", got)
	}
	if got := FetchReadLimit("https://example.com/post", 10000); got != 30000 {
		t.Fatalf("FetchReadLimit for regular URL = %d, want 30000", got)
	}
}
