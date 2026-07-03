package setup

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestProviderTest_CloudPathMaxCompletionTokensRetryE2E drives the
// max_completion_tokens fallback through the real handler (the Cloud
// models-panel "Test connection" path: POST /api/test-provider →
// handleTestProvider → runProviderTest). A fake OpenAI-compat endpoint
// rejects the first max_tokens probe with a 4xx naming both parameters
// (the newer OpenAI error), then accepts the retried max_completion_tokens
// probe and returns a real chat completion. The handler must report
// ok:true — proving the 3627bff retry negotiation works end to end.
func TestProviderTest_CloudPathMaxCompletionTokensRetryE2E(t *testing.T) {
	s := setupTestServer(t)

	var mu sync.Mutex
	requests := []string{} // captured request bodies
	completions := 0
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model               string `json:"model"`
			MaxTokens           *int   `json:"max_tokens"`
			MaxCompletionTokens *int   `json:"max_completion_tokens"`
		}
		raw := make([]byte, 4096)
		n, _ := r.Body.Read(raw)
		_ = json.Unmarshal(raw[:n], &body)

		mu.Lock()
		requests = append(requests, string(raw[:n]))
		mu.Unlock()

		if body.MaxCompletionTokens != nil {
			// Retried probe accepted → real completion.
			completions++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-e2e","object":"chat.completion",` +
				`"created":0,"model":"` + body.Model + `","choices":[{"index":0,` +
				`"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
			return
		}
		// First probe rejected exactly like newer OpenAI endpoints do.
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"'max_tokens' is not supported for this model,` +
			` use 'max_completion_tokens' instead.","type":"invalid_request_error"}}`))
	}))
	defer llm.Close()

	body := `{"apiBase":"` + llm.URL + `","apiKey":"sk-e2e","model":"gpt-4o-mini",` +
		`"apiType":"openai","authType":"bearer"}`
	req := httptest.NewRequest(http.MethodPost, "/api/test-provider", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleTestProvider(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("handleTestProvider status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.OK {
		t.Fatalf("provider test reported not-ok: error=%q", resp.Error)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("llm hit %d times; want 2 (max_tokens reject + max_completion_tokens retry)", len(requests))
	}
	if !strings.Contains(requests[0], `"max_tokens"`) || strings.Contains(requests[0], `max_completion_tokens`) {
		t.Errorf("first probe body = %s; want max_tokens, no max_completion_tokens", requests[0])
	}
	if !strings.Contains(requests[1], `"max_completion_tokens"`) {
		t.Errorf("retry probe body = %s; want max_completion_tokens", requests[1])
	}
	if completions != 1 {
		t.Errorf("completions served = %d; want 1", completions)
	}
}
