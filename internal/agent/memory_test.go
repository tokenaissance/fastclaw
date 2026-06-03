package agent

import (
	"context"
	"testing"
)

type memoryStoreSpy struct {
	getMemoryCalls            int
	saveMemoryCalls           int
	getWorkspaceFileExactCall int
	saveWorkspaceFileCalls    int
}

func (s *memoryStoreSpy) GetMemory(context.Context, string, string) (string, error) {
	s.getMemoryCalls++
	return "persisted", nil
}

func (s *memoryStoreSpy) SaveMemory(context.Context, string, string, string) error {
	s.saveMemoryCalls++
	return nil
}

func (s *memoryStoreSpy) GetWorkspaceFile(context.Context, string, string, string) ([]byte, error) {
	return nil, nil
}

func (s *memoryStoreSpy) GetWorkspaceFileExact(context.Context, string, string, string) ([]byte, error) {
	s.getWorkspaceFileExactCall++
	return []byte("user-profile"), nil
}

func (s *memoryStoreSpy) SaveWorkspaceFile(context.Context, string, string, string, []byte) error {
	s.saveWorkspaceFileCalls++
	return nil
}

func TestNewMemoryWithStoreForUserEmptyUserIDFailsClosed(t *testing.T) {
	store := &memoryStoreSpy{}
	mem := NewMemoryWithStoreForUser(t.TempDir(), store, "", "agent-1")
	if mem == nil {
		t.Fatal("expected memory")
	}
	if got := mem.LoadMemory(); got != "" {
		t.Fatalf("LoadMemory() = %q, want empty", got)
	}
	if store.getMemoryCalls != 0 {
		t.Fatalf("GetMemory called %d times, want 0", store.getMemoryCalls)
	}
	if err := mem.SaveMemory("x"); err == nil {
		t.Fatal("SaveMemory() error = nil, want error")
	}
	if store.saveMemoryCalls != 0 {
		t.Fatalf("SaveMemory store calls = %d, want 0", store.saveMemoryCalls)
	}
	if got := mem.LoadUserFile(); got != "" {
		t.Fatalf("LoadUserFile() = %q, want empty", got)
	}
	if store.getWorkspaceFileExactCall != 0 {
		t.Fatalf("GetWorkspaceFileExact called %d times, want 0", store.getWorkspaceFileExactCall)
	}
	if err := mem.SaveUserFile("x"); err == nil {
		t.Fatal("SaveUserFile() error = nil, want error")
	}
	if store.saveWorkspaceFileCalls != 0 {
		t.Fatalf("SaveWorkspaceFile calls = %d, want 0", store.saveWorkspaceFileCalls)
	}
}

func TestNewMemoryWithStoreForUserCanBeRebound(t *testing.T) {
	store := &memoryStoreSpy{}
	mem := NewMemoryWithStoreForUser(t.TempDir(), store, "", "agent-1").WithUserID("user-1")
	if got := mem.LoadMemory(); got != "persisted" {
		t.Fatalf("LoadMemory() = %q, want persisted", got)
	}
	if err := mem.SaveMemory("x"); err != nil {
		t.Fatalf("SaveMemory() error = %v", err)
	}
	if got := mem.LoadUserFile(); got != "user-profile" {
		t.Fatalf("LoadUserFile() = %q, want user-profile", got)
	}
	if err := mem.SaveUserFile("x"); err != nil {
		t.Fatalf("SaveUserFile() error = %v", err)
	}
}
