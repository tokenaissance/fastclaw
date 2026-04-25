package session

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
)

// Session holds the message history for a channel_chat_id pair.
type Session struct {
	mu                sync.Mutex
	Messages          []provider.Message
	LastConsolidated  int // index of last consolidated message
	filePath          string
	snapshot          []provider.Message // undo snapshot
	store             SessionStore
	userID            string
	agentID           string
	sessionKey        string
}

// ctx returns a context tagged with this Session's user so the store layer
// can scope SQL by user_id. Falls back to context.Background() when no
// user is set; the store will then default to config.DefaultUserID.
func (s *Session) ctx() context.Context {
	if s.userID == "" {
		return context.Background()
	}
	return config.WithUserID(context.Background(), s.userID)
}

// Manager manages sessions, keyed by "channel_chat_id".
// SessionStore is an optional interface for database-backed session persistence.
type SessionStore interface {
	GetSession(ctx context.Context, agentID, sessionKey string) ([]provider.Message, error)
	SaveSession(ctx context.Context, agentID, sessionKey string, messages []provider.Message) error
	ListWebSessions(ctx context.Context, agentID string) ([]WebSession, error)
	DeleteSession(ctx context.Context, agentID, sessionKey string) error
	RenameSession(ctx context.Context, agentID, sessionKey, title string) error
}

type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	dataDir  string
	store    SessionStore
	userID   string
	agentID  string
}

func NewManager(dataDir string) *Manager {
	return &Manager{
		sessions: make(map[string]*Session),
		dataDir:  dataDir,
	}
}

func NewManagerWithStore(dataDir string, st SessionStore, agentID string) *Manager {
	return NewManagerWithStoreForUser(dataDir, st, config.DefaultUserID, agentID)
}

// NewManagerWithStoreForUser is the user-scoped constructor: store reads
// and writes carry user_id so per-(user, agent) sessions don't collide
// when multiple users hit the same agentID. CLI / heartbeat callers that
// don't have a user can use NewManagerWithStore which defaults to local.
func NewManagerWithStoreForUser(dataDir string, st SessionStore, userID, agentID string) *Manager {
	if userID == "" {
		userID = config.DefaultUserID
	}
	return &Manager{
		sessions: make(map[string]*Session),
		dataDir:  dataDir,
		store:    st,
		userID:   userID,
		agentID:  agentID,
	}
}

// ctx returns a context tagged with this Manager's user for store calls.
func (m *Manager) ctx() context.Context {
	if m.userID == "" {
		return context.Background()
	}
	return config.WithUserID(context.Background(), m.userID)
}

// sessionKey returns the canonical, storage-agnostic key for a session.
// Uses "_" rather than ":" so filesystem backends don't need to re-encode
// the key into a filename-safe form on every call and DB-backed backends
// end up with the same literal string in their session_key column.
// Everything downstream (DB rows, JSONL filenames, ListWebSessions' prefix
// filter) can now share one encoding instead of each picking its own.
func sessionKey(channel, chatID string) string {
	return channel + "_" + chatID
}

// Get returns or creates a session for the given channel and chat ID.
//
// In multi-replica deployments (store-backed mode), every Get() reloads
// Messages from the store so a request served by pod B sees writes made
// by pod A. Without this, each pod's in-memory cache drifts away from
// Postgres: the first refresh after a cross-pod write returns whichever
// pod-local snapshot happened to be warm. We deliberately overwrite
// Messages on the cached Session rather than re-creating the struct so
// transient fields (snapshot, LastConsolidated) survive.
//
// File-backed mode stays cache-first since there's only one process.
func (m *Manager) Get(channel, chatID string) *Session {
	key := sessionKey(channel, chatID)

	m.mu.Lock()
	defer m.mu.Unlock()

	if s, ok := m.sessions[key]; ok {
		if m.store != nil {
			if msgs, err := m.store.GetSession(m.ctx(), m.agentID, key); err == nil {
				s.mu.Lock()
				s.Messages = msgs
				s.mu.Unlock()
			}
		}
		return s
	}

	filePath := filepath.Join(m.dataDir, key+".jsonl")

	s := &Session{
		filePath:   filePath,
		store:      m.store,
		userID:     m.userID,
		agentID:    m.agentID,
		sessionKey: key,
	}

	// Load from store (DB) if available, otherwise from file
	if m.store != nil {
		msgs, err := m.store.GetSession(m.ctx(), m.agentID, key)
		if err == nil && len(msgs) > 0 {
			s.Messages = msgs
		}
	} else {
		s.load()
	}

	m.sessions[key] = s
	return s
}

// Append adds a message to the session and persists it.
func (s *Session) Append(msg provider.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Auto-set timestamp if not provided
	if msg.Timestamp == 0 {
		msg.Timestamp = time.Now().UnixMilli()
	}

	s.Messages = append(s.Messages, msg)

	if s.store != nil {
		s.store.SaveSession(s.ctx(), s.agentID, s.sessionKey, s.Messages)
	} else {
		s.appendToFile(msg)
	}
}

// GetMessages returns a copy of all messages.
func (s *Session) GetMessages() []provider.Message {
	s.mu.Lock()
	defer s.mu.Unlock()

	msgs := make([]provider.Message, len(s.Messages))
	copy(msgs, s.Messages)
	return msgs
}

// UnconsolidatedCount returns the number of messages since last consolidation.
func (s *Session) UnconsolidatedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Messages) - s.LastConsolidated
}

// MarkConsolidated updates the consolidation pointer.
func (s *Session) MarkConsolidated(index int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastConsolidated = index
}

// ReplaceMessages replaces all session messages with the given list.
// This is used after context compaction to trim the session.
func (s *Session) ReplaceMessages(msgs []provider.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.Messages = make([]provider.Message, len(msgs))
	copy(s.Messages, msgs)
	s.LastConsolidated = 0

	if s.store != nil {
		s.store.SaveSession(s.ctx(), s.agentID, s.sessionKey, s.Messages)
	} else {
		s.rewriteFile()
	}
}

// Clear resets the session messages.
func (s *Session) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Messages = nil
	s.LastConsolidated = 0
	if s.store != nil {
		s.store.DeleteSession(s.ctx(), s.agentID, s.sessionKey)
	} else {
		os.Remove(s.filePath)
	}
}

func (s *Session) load() {
	f, err := os.Open(s.filePath)
	if err != nil {
		return // file doesn't exist yet
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var msg provider.Message
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		s.Messages = append(s.Messages, msg)
	}
}

func (s *Session) rewriteFile() {
	dir := filepath.Dir(s.filePath)
	os.MkdirAll(dir, 0o755)

	f, err := os.Create(s.filePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "session rewrite error: %v\n", err)
		return
	}
	defer f.Close()

	for _, msg := range s.Messages {
		data, err := json.Marshal(msg)
		if err != nil {
			continue
		}
		f.Write(data)
		f.Write([]byte("\n"))
	}
}

func (s *Session) appendToFile(msg provider.Message) {
	dir := filepath.Dir(s.filePath)
	os.MkdirAll(dir, 0o755)

	f, err := os.OpenFile(s.filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "session persist error: %v\n", err)
		return
	}
	defer f.Close()

	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	f.Write(data)
	f.Write([]byte("\n"))
}

// WebSession holds metadata for a web chat session.
type WebSession struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Preview   string `json:"preview"`
	CreatedAt int64  `json:"createdAt"` // unix ms
	UpdatedAt int64  `json:"updatedAt"` // unix ms
}

// ListWebSessions scans session files for web chat sessions and returns
// a list with id, title, preview, and timestamps.
func (m *Manager) ListWebSessions() []WebSession {
	if m.store != nil {
		sessions, err := m.store.ListWebSessions(m.ctx(), m.agentID)
		if err == nil {
			return sessions
		}
	}
	pattern := filepath.Join(m.dataDir, "web_*.jsonl")
	files, err := filepath.Glob(pattern)
	if err != nil {
		return nil
	}

	var sessions []WebSession
	for _, f := range files {
		base := filepath.Base(f)
		// "web_<sessionId>.jsonl" -> "<sessionId>"
		sessionId := strings.TrimPrefix(base, "web_")
		sessionId = strings.TrimSuffix(sessionId, ".jsonl")

		info, err := os.Stat(f)
		if err != nil {
			continue
		}

		// Read first user message as preview
		preview := ""
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(fh)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			var msg struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			}
			if json.Unmarshal(scanner.Bytes(), &msg) == nil && msg.Role == "user" && msg.Content != "" {
				preview = msg.Content
				if len(preview) > 100 {
					preview = preview[:100] + "..."
				}
				break
			}
		}
		fh.Close()

		if preview == "" {
			continue // skip empty sessions
		}

		// Read title from metadata file, fallback to preview
		title := m.readSessionTitle(sessionId)
		if title == "" {
			title = preview
			if len(title) > 60 {
				title = title[:60] + "..."
			}
		}

		sessions = append(sessions, WebSession{
			ID:        sessionId,
			Title:     title,
			Preview:   preview,
			CreatedAt: info.ModTime().UnixMilli(),
			UpdatedAt: info.ModTime().UnixMilli(),
		})
	}

	// Sort by updatedAt descending (newest first)
	for i := 0; i < len(sessions); i++ {
		for j := i + 1; j < len(sessions); j++ {
			if sessions[j].UpdatedAt > sessions[i].UpdatedAt {
				sessions[i], sessions[j] = sessions[j], sessions[i]
			}
		}
	}

	return sessions
}

// DeleteWebSession removes a web chat session file and its metadata.
func (m *Manager) DeleteWebSession(sessionId string) error {
	key := sessionKey("web", sessionId)

	// Remove from in-memory cache
	m.mu.Lock()
	delete(m.sessions, key)
	m.mu.Unlock()

	if m.store != nil {
		return m.store.DeleteSession(m.ctx(), m.agentID, key)
	}

	safeId := strings.ReplaceAll(sessionId, "/", "_")
	safeId = strings.ReplaceAll(safeId, "..", "_")
	sessionFile := filepath.Join(m.dataDir, "web_"+safeId+".jsonl")
	metaFile := filepath.Join(m.dataDir, "web_"+safeId+".meta.json")
	os.Remove(metaFile)
	return os.Remove(sessionFile)
}

// RenameWebSession sets a custom title for a web chat session.
func (m *Manager) RenameWebSession(sessionId, title string) error {
	if m.store != nil {
		return m.store.RenameSession(m.ctx(), m.agentID, sessionKey("web", sessionId), title)
	}

	safeId := strings.ReplaceAll(sessionId, "/", "_")
	safeId = strings.ReplaceAll(safeId, "..", "_")
	metaFile := filepath.Join(m.dataDir, "web_"+safeId+".meta.json")
	data, _ := json.Marshal(map[string]string{"title": title})
	return os.WriteFile(metaFile, data, 0o644)
}

// readSessionTitle reads the title from a session metadata file.
func (m *Manager) readSessionTitle(sessionId string) string {
	safeId := strings.ReplaceAll(sessionId, "/", "_")
	safeId = strings.ReplaceAll(safeId, "..", "_")

	metaFile := filepath.Join(m.dataDir, "web_"+safeId+".meta.json")
	data, err := os.ReadFile(metaFile)
	if err != nil {
		return ""
	}
	var meta struct {
		Title string `json:"title"`
	}
	json.Unmarshal(data, &meta)
	return meta.Title
}

// Snapshot saves the current message list as a restore point (for undo).
func (s *Session) Snapshot() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot = make([]provider.Message, len(s.Messages))
	copy(s.snapshot, s.Messages)
}

// Undo restores the last snapshot. Returns false if no snapshot exists.
func (s *Session) Undo() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot == nil {
		return false
	}
	s.Messages = make([]provider.Message, len(s.snapshot))
	copy(s.Messages, s.snapshot)
	s.snapshot = nil
	s.LastConsolidated = 0
	s.rewriteFile()
	return true
}

// HasSnapshot returns true if an undo snapshot exists.
func (s *Session) HasSnapshot() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot != nil
}
