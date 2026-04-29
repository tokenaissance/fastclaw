package session

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/provider"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// StoreAdapter adapts store.Store to the SessionStore interface.
type StoreAdapter struct {
	st store.Store
}

func NewStoreAdapter(st store.Store) *StoreAdapter {
	return &StoreAdapter{st: st}
}

func (a *StoreAdapter) GetSession(ctx context.Context, agentID, sessionKey string) ([]provider.Message, error) {
	rec, err := a.st.GetSession(ctx, agentID, sessionKey)
	if err != nil || rec == nil {
		return nil, err
	}
	msgs := make([]provider.Message, len(rec.Messages))
	for i, m := range rec.Messages {
		msgs[i] = provider.Message{
			Role:         m.Role,
			Content:      m.Content,
			ToolCallID:   m.ToolCallID,
			Name:         m.Name,
			Metadata:     m.Metadata,
			Thinking:     m.Thinking,
			RawAssistant: m.RawAssistant,
		}
		// ToolCalls / ContentParts are stored as interface{} so a
		// JSON round-trip leaves them as []interface{} / map nests.
		// Re-marshal + unmarshal to recover the typed slice — without
		// this, a refreshed history loses tool-group bubbles AND the
		// next provider call sends a multimodal user turn with no
		// content (ContentParts dropped → Content "" → API rejects).
		if m.ToolCalls != nil {
			if raw, err := json.Marshal(m.ToolCalls); err == nil {
				var tcs []provider.ToolCall
				if json.Unmarshal(raw, &tcs) == nil {
					msgs[i].ToolCalls = tcs
				}
			}
		}
		if m.ContentParts != nil {
			if raw, err := json.Marshal(m.ContentParts); err == nil {
				var parts []provider.ContentPart
				if json.Unmarshal(raw, &parts) == nil {
					msgs[i].ContentParts = parts
				}
			}
		}
	}
	return msgs, nil
}

func (a *StoreAdapter) SaveSession(ctx context.Context, agentID, sessionKey string, messages []provider.Message) error {
	rec := &store.SessionRecord{
		Messages:  make([]store.SessionMessage, len(messages)),
		UpdatedAt: time.Now(),
	}
	for i, m := range messages {
		rec.Messages[i] = store.SessionMessage{
			Role:         m.Role,
			Content:      m.Content,
			ToolCallID:   m.ToolCallID,
			Name:         m.Name,
			Metadata:     m.Metadata,
			Timestamp:    time.Now(),
			Thinking:     m.Thinking,
			RawAssistant: m.RawAssistant,
		}
		if len(m.ToolCalls) > 0 {
			rec.Messages[i].ToolCalls = m.ToolCalls
		}
		if len(m.ContentParts) > 0 {
			rec.Messages[i].ContentParts = m.ContentParts
		}
	}
	return a.st.SaveSession(ctx, agentID, sessionKey, rec)
}

func (a *StoreAdapter) ListWebSessions(ctx context.Context, agentID string) ([]WebSession, error) {
	metas, err := a.st.ListSessions(ctx, agentID)
	if err != nil {
		return nil, err
	}
	var sessions []WebSession
	for _, m := range metas {
		if !strings.HasPrefix(m.Key, "web_") {
			continue
		}
		sessionId := strings.TrimPrefix(m.Key, "web_")
		preview := ""
		thumb := ""
		rec, err := a.st.GetSession(ctx, agentID, m.Key)
		if err == nil && rec != nil {
			for _, msg := range rec.Messages {
				if msg.Role != "user" {
					continue
				}
				// Multimodal user turns (text + image attachment) live
				// in ContentParts with Content="". Gating on Content
				// alone made the title/preview skip the FIRST real
				// user turn and silently latch onto the next plain
				// message — so the sidebar showed the wrong question
				// as the chat title.
				text := userText(msg)
				img := userImage(msg)
				if text == "" && img == "" {
					continue
				}
				preview = text
				if preview == "" {
					preview = "[image]"
				}
				if len(preview) > 100 {
					preview = preview[:100] + "..."
				}
				thumb = img
				break
			}
		}
		if preview == "" {
			continue
		}
		// Custom title (set via rename) takes precedence over the
		// auto-derived preview; fall back to preview so every session has
		// a sensible display label.
		title := m.Title
		if title == "" {
			title = preview
		}
		sessions = append(sessions, WebSession{
			ID:           sessionId,
			Title:        title,
			Preview:      preview,
			ThumbnailURL: thumb,
			CreatedAt:    m.UpdatedAt.UnixMilli(),
			UpdatedAt:    m.UpdatedAt.UnixMilli(),
		})
	}
	return sessions, nil
}

// userText pulls the user-visible text from a stored user turn. Falls
// back to ContentParts' "text" parts when Content is empty (the shape
// produced by HandleMessageStream when the turn carried image
// attachments). Without this, callers gating on Content silently treat
// multimodal turns as empty.
func userText(m store.SessionMessage) string {
	if m.Content != "" {
		return provider.StripAttachedPrefix(m.Content)
	}
	if m.ContentParts == nil {
		return ""
	}
	raw, err := json.Marshal(m.ContentParts)
	if err != nil {
		return ""
	}
	var parts []provider.ContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var out []string
	for _, p := range parts {
		if p.Type == "text" && p.Text != "" {
			out = append(out, p.Text)
		}
	}
	return provider.StripAttachedPrefix(strings.Join(out, "\n"))
}

// userImage returns the first image_url URL from a stored user turn's
// ContentParts, or "" if none. Powers the sidebar thumbnail next to
// the chat title.
func userImage(m store.SessionMessage) string {
	if m.ContentParts == nil {
		return ""
	}
	raw, err := json.Marshal(m.ContentParts)
	if err != nil {
		return ""
	}
	var parts []provider.ContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	for _, p := range parts {
		if p.Type == "image_url" && p.ImageURL != nil && p.ImageURL.URL != "" {
			return p.ImageURL.URL
		}
	}
	return ""
}

func (a *StoreAdapter) DeleteSession(ctx context.Context, agentID, sessionKey string) error {
	return a.st.DeleteSession(ctx, agentID, sessionKey)
}

func (a *StoreAdapter) RenameSession(ctx context.Context, agentID, sessionKey, title string) error {
	return a.st.RenameSession(ctx, agentID, sessionKey, title)
}
