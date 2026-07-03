package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestRunProviderTestRetriesMaxCompletionTokens(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := atomic.AddInt32(&calls, 1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if call == 1 {
			if _, ok := body["max_tokens"]; !ok {
				t.Fatalf("first request missing max_tokens: %#v", body)
			}
			http.Error(w, `{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead."}}`, http.StatusBadRequest)
			return
		}
		if _, ok := body["max_completion_tokens"]; !ok {
			t.Fatalf("retry missing max_completion_tokens: %#v", body)
		}
		if _, ok := body["max_tokens"]; ok {
			t.Fatalf("retry still sent max_tokens: %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-test","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer srv.Close()

	got := runProviderTest(context.Background(), testProviderRequest{
		APIBase: srv.URL,
		APIKey:  "test-key",
		Model:   "gpt-5.5",
		APIType: "openai-chat",
	})
	if ok, _ := got["ok"].(bool); !ok {
		t.Fatalf("runProviderTest ok = false, got %#v", got)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}
