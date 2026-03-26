package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
)

// mem0SearchResult is a single memory entry from the Mem0 search API.
type mem0SearchResult struct {
	ID     string  `json:"id"`
	Memory string  `json:"memory"`
	Score  float64 `json:"score"`
}

// mem0SearchResponse is the response from POST /search.
type mem0SearchResponse struct {
	Results []mem0SearchResult `json:"results"`
}

// mem0AddRequest is the request body for POST /memories.
type mem0AddRequest struct {
	Messages []mem0Message      `json:"messages"`
	UserID   string             `json:"user_id"`
}

type mem0Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Mem0HookState holds per-request state for the mem0 hook pair.
// BeforeModelCall stores the user message; AfterModelCall uses it for async storage.
type Mem0HookState struct {
	cfg    config.Mem0Cfg
	client *http.Client
}

// NewMem0HookState creates a new mem0 hook state.
func NewMem0HookState(cfg config.Mem0Cfg) *Mem0HookState {
	url := cfg.URL
	if url == "" {
		url = "http://127.0.0.1:8100"
	}
	cfg.URL = url
	if cfg.TopK <= 0 {
		cfg.TopK = 5
	}
	return &Mem0HookState{
		cfg:    cfg,
		client: &http.Client{Timeout: 3 * time.Second},
	}
}

// BeforeModelCallHook searches mem0 for relevant memories and injects them
// into the message list as a system message before the first model call.
func (m *Mem0HookState) BeforeModelCallHook() HookFunc {
	return func(ctx context.Context, hc *HookContext) {
		if hc.Messages == nil || len(hc.Messages) < 2 {
			return
		}

		// Only inject on the first model call (when there's exactly one user message
		// at the end, no assistant messages yet).
		hasAssistant := false
		for _, msg := range hc.Messages {
			if msg.Role == "assistant" {
				hasAssistant = true
				break
			}
		}
		if hasAssistant {
			return // not first turn, skip
		}

		// Extract user message (last message should be user)
		lastMsg := hc.Messages[len(hc.Messages)-1]
		if lastMsg.Role != "user" {
			return
		}
		userText := lastMsg.Content

		// Derive user_id from chat context in the runtime context line
		userID := extractUserID(hc.Messages)
		if userID == "" {
			return
		}

		// Search mem0
		memories, err := m.searchMemories(ctx, userID, userText)
		if err != nil {
			slog.Debug("mem0: search error", "error", err, "agent", hc.AgentName)
			return
		}
		if len(memories) == 0 {
			return
		}

		// Build memory injection
		var sb strings.Builder
		sb.WriteString("# User Memories (from long-term memory store)\n")
		sb.WriteString("The following facts were previously learned about this user:\n")
		for _, mem := range memories {
			sb.WriteString(fmt.Sprintf("- %s\n", mem.Memory))
		}
		sb.WriteString("\nUse these memories to personalize your response when relevant.")

		// Inject as a system message right before the last user message
		injected := make([]provider.Message, 0, len(hc.Messages)+1)
		injected = append(injected, hc.Messages[:len(hc.Messages)-1]...)
		injected = append(injected, provider.Message{
			Role:    "system",
			Content: sb.String(),
		})
		injected = append(injected, lastMsg)
		hc.Messages = injected

		slog.Info("mem0: injected memories",
			"agent", hc.AgentName,
			"user_id", userID,
			"count", len(memories),
		)
	}
}

// AfterModelCallHook asynchronously stores the conversation turn in mem0
// when the model produces a final response (no tool calls).
func (m *Mem0HookState) AfterModelCallHook() HookFunc {
	return func(ctx context.Context, hc *HookContext) {
		if hc.Response == nil || hc.Response.HasToolCalls() {
			return // only store on final text response
		}
		if hc.Error != nil {
			return
		}

		// Find last user message
		var userText string
		for i := len(hc.Messages) - 1; i >= 0; i-- {
			if hc.Messages[i].Role == "user" {
				userText = hc.Messages[i].Content
				break
			}
		}
		if userText == "" {
			return
		}

		userID := extractUserID(hc.Messages)
		if userID == "" {
			return
		}

		assistantText := hc.Response.Content

		// Store async — don't block the response
		go func() {
			storeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := m.storeMemory(storeCtx, userID, userText, assistantText); err != nil {
				slog.Debug("mem0: store error", "error", err, "user_id", userID)
			} else {
				slog.Info("mem0: stored memory", "user_id", userID)
			}
		}()
	}
}

// searchMemories calls POST /search on the mem0 server.
func (m *Mem0HookState) searchMemories(ctx context.Context, userID, query string) ([]mem0SearchResult, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"query":   query,
		"user_id": userID,
		"limit":   m.cfg.TopK,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", m.cfg.URL+"/search", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if m.cfg.APIKey != "" {
		req.Header.Set("X-API-Key", m.cfg.APIKey)
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("mem0 search HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result mem0SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Results, nil
}

// storeMemory calls POST /memories on the mem0 server.
func (m *Mem0HookState) storeMemory(ctx context.Context, userID, userText, assistantText string) error {
	body, _ := json.Marshal(mem0AddRequest{
		Messages: []mem0Message{
			{Role: "user", Content: userText},
			{Role: "assistant", Content: assistantText},
		},
		UserID: userID,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", m.cfg.URL+"/memories", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if m.cfg.APIKey != "" {
		req.Header.Set("X-API-Key", m.cfg.APIKey)
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("mem0 store HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// extractUserID derives a user identifier from the messages.
// It looks for the "Chat ID:" line in the runtime context message.
func extractUserID(messages []provider.Message) string {
	for _, msg := range messages {
		if msg.Role != "user" {
			continue
		}
		for _, line := range strings.Split(msg.Content, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "Chat ID:") {
				chatID := strings.TrimSpace(strings.TrimPrefix(line, "Chat ID:"))
				if chatID != "" && chatID != "web-ui" {
					return chatID
				}
			}
		}
	}
	return ""
}
