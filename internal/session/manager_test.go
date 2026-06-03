package session

import (
	"context"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/provider"
)

type noopSessionStore struct{}

func (noopSessionStore) GetSession(context.Context, string, string) ([]provider.Message, error) {
	return nil, nil
}
func (noopSessionStore) SaveSession(context.Context, string, string, string, string, string, string, []provider.Message) error {
	return nil
}
func (noopSessionStore) AppendMessage(context.Context, string, string, provider.Message) error {
	return nil
}
func (noopSessionStore) ListMessages(context.Context, string, string) ([]provider.Message, error) {
	return nil, nil
}
func (noopSessionStore) ListWebSessions(context.Context, string) ([]WebSession, error) {
	return nil, nil
}
func (noopSessionStore) DeleteSession(context.Context, string, string) error {
	return nil
}
func (noopSessionStore) RenameSession(context.Context, string, string, string) error {
	return nil
}
func (noopSessionStore) MoveSession(context.Context, string, string, string) error {
	return nil
}
func (noopSessionStore) ResolveActiveSessionKey(context.Context, string, string, string, string) (string, error) {
	return "", nil
}
func (noopSessionStore) LookupSessionTriple(context.Context, string, string) (string, string, string, error) {
	return "", "", "", nil
}
func (noopSessionStore) LookupSessionProject(context.Context, string, string) (string, error) {
	return "", nil
}

func TestNewManagerWithStoreForUserEmptyUserIDDoesNotPanic(t *testing.T) {
	mgr := NewManagerWithStoreForUser(t.TempDir(), noopSessionStore{}, "", "agent-1")
	if mgr == nil {
		t.Fatal("expected manager")
	}
	s := mgr.Get("web", "", "chat-1", "")
	if s == nil {
		t.Fatal("expected session")
	}
	s.Append(provider.Message{Role: "user", Content: "hello"})
	if got := len(s.GetMessages()); got != 1 {
		t.Fatalf("message count = %d, want 1", got)
	}
}
