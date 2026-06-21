package agent

// e2e for commit b892d3f "fix(cron): include AccountID in fireJob
// InboundMessage" — second half: llmRetry transient-failure retry.
//
// Cloud zero-impact rationale: retry lives entirely inside the agent
// loop. Cloud (Next.js app) posts /chat messages through the
// /api/fastagent proxy and reads back the streamed reply; the retry is
// invisible to the client (same final response, just more robust when
// the upstream LLM provider glitches). No endpoint, response shape, or
// auth change.
//
// The behavior pinned here is the llmRetry policy itself: transient
// errors (network EOF, 5xx) are retried up to llmRetryAttempts times
// with exponential backoff (1s, 4s, 9s); context.Canceled and
// DeadlineExceeded are terminal — retrying after the caller has gone
// away or the deadline passed is pointless. Both LLM call sites the
// upstream wrapped are covered by construction: chatbot mode
// (streamChatToResponse in HandleMessage) and agent mode
// (provider.Chat in HandleMessageStream) both funnel through this one
// helper. The persistent-failure case exercises the real 1s+4s backoff
// so the wait cost is bounded (~5s).

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/provider"
)

func TestAgent_LLMRetry_CloudPathE2E(t *testing.T) {
	// Transient failure then success — retried, final response returned.
	t.Run("transient-then-success", func(t *testing.T) {
		var calls int
		resp, err := llmRetry(context.Background(), "t", func(ctx context.Context) (*provider.Response, error) {
			calls++
			if calls < 3 {
				return nil, io.EOF // network glitch
			}
			return &provider.Response{Content: "ok"}, nil
		})
		if err != nil {
			t.Fatalf("err=%v, want nil after retries", err)
		}
		if calls != 3 {
			t.Errorf("fn called %d times, want 3 (2 failures + success)", calls)
		}
		if resp.Content != "ok" {
			t.Errorf("content=%q, want ok", resp.Content)
		}
	})

	// Immediate success — exactly one call, no retry.
	t.Run("immediate-success", func(t *testing.T) {
		var calls int
		resp, err := llmRetry(context.Background(), "t", func(ctx context.Context) (*provider.Response, error) {
			calls++
			return &provider.Response{Content: "first"}, nil
		})
		if err != nil || calls != 1 {
			t.Errorf("err=%v calls=%d, want nil/1", err, calls)
		}
		if resp.Content != "first" {
			t.Errorf("content=%q, want first", resp.Content)
		}
	})

	// Persistent transient failure — exhausted after llmRetryAttempts,
	// returns the last error. Exercises the real 1s+4s backoff (~5s).
	t.Run("persistent-failure-exhausts-retries", func(t *testing.T) {
		var calls int
		boom := errors.New("transient boom")
		_, err := llmRetry(context.Background(), "t", func(ctx context.Context) (*provider.Response, error) {
			calls++
			return nil, boom
		})
		if calls != llmRetryAttempts {
			t.Errorf("fn called %d times, want %d", calls, llmRetryAttempts)
		}
		if !errors.Is(err, boom) {
			t.Errorf("err=%v, want boom", err)
		}
	})

	// context.Canceled is terminal — no retry.
	t.Run("canceled-terminal", func(t *testing.T) {
		var calls int
		_, err := llmRetry(context.Background(), "t", func(ctx context.Context) (*provider.Response, error) {
			calls++
			return nil, context.Canceled
		})
		if calls != 1 {
			t.Errorf("fn called %d times, want 1 (no retry on cancel)", calls)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err=%v, want context.Canceled", err)
		}
	})

	// DeadlineExceeded is terminal — no retry.
	t.Run("deadline-terminal", func(t *testing.T) {
		var calls int
		_, err := llmRetry(context.Background(), "t", func(ctx context.Context) (*provider.Response, error) {
			calls++
			return nil, context.DeadlineExceeded
		})
		if calls != 1 {
			t.Errorf("fn called %d times, want 1 (no retry on deadline)", calls)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err=%v, want context.DeadlineExceeded", err)
		}
	})

	// Backoff aborts when the caller cancels mid-wait; both errors join.
	t.Run("cancel-during-backoff", func(t *testing.T) {
		var calls int
		boom := errors.New("transient during backoff")
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // already canceled → the 1s backoff select aborts instantly
		_, err := llmRetry(ctx, "t", func(ctx context.Context) (*provider.Response, error) {
			calls++
			return nil, boom
		})
		if calls != 1 {
			t.Errorf("fn called %d times, want 1 (aborted on first backoff)", calls)
		}
		if !errors.Is(err, boom) || !errors.Is(err, context.Canceled) {
			t.Errorf("err=%v, want join(boom, Canceled)", err)
		}
	})
}
