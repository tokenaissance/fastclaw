package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// MemoryStoreAdapter exposes the agent's identity + memory files via the
// underlying store. Reads pass userID through so the per-user override
// row wins when present (USER.md / MEMORY.md the agent autopersisted
// for that chatter); writes also carry userID so chat-time updates land
// in the chatter's row, never the shared template.
type MemoryStoreAdapter struct {
	st store.Store
}

func NewMemoryStoreAdapter(st store.Store) *MemoryStoreAdapter {
	return &MemoryStoreAdapter{st: st}
}

const memoryFilename = "MEMORY.md"

// GetMemory uses the *Exact* (no owner-fallback) variant deliberately.
// MEMORY.md is per-chatter — a public-link visitor must not inherit the
// agent owner's accumulated memories of past conversations.
func (a *MemoryStoreAdapter) GetMemory(ctx context.Context, agentID, userID string) (string, error) {
	data, err := a.st.GetAgentFileExact(ctx, agentID, userID, memoryFilename)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (a *MemoryStoreAdapter) SaveMemory(ctx context.Context, agentID, userID, content string) error {
	return a.st.SaveAgentFile(ctx, agentID, userID, memoryFilename, []byte(content))
}

// GetWorkspaceFile keeps the owner-fallback overlay because the
// ContextBuilder uses this method for shared identity files
// (SOUL/IDENTITY/AGENTS/BOOTSTRAP/HEARTBEAT/TOOLS). Chatters inheriting
// the owner's identity is the desired behavior there.
func (a *MemoryStoreAdapter) GetWorkspaceFile(ctx context.Context, agentID, userID, filename string) ([]byte, error) {
	return a.st.GetAgentFile(ctx, agentID, userID, filename)
}

// GetWorkspaceFileExact bypasses the owner-fallback overlay. Used for
// per-chatter files (USER.md) so a fresh visitor sees an empty profile
// instead of the owner's.
func (a *MemoryStoreAdapter) GetWorkspaceFileExact(ctx context.Context, agentID, userID, filename string) ([]byte, error) {
	return a.st.GetAgentFileExact(ctx, agentID, userID, filename)
}

func (a *MemoryStoreAdapter) SaveWorkspaceFile(ctx context.Context, agentID, userID, filename string, data []byte) error {
	return a.st.SaveAgentFile(ctx, agentID, userID, filename, data)
}

// SaveWorkspaceFileIfUnchanged is the belt the identity files were missing: the
// store compares `expected` (the bytes this caller read) inside the write, and a
// row that moved in between comes back as a conflict.
//
// Two readers check it and they live in two packages, so the error carries BOTH
// sentinels (multi-%w): the distiller asks errors.Is(err, ErrMemoryConflict) and
// must not have to import the store, while the file tools ask
// errors.Is(err, store.ErrAgentFileConflict) and are in a package this one
// imports (tools ← agent, so tools cannot see ErrMemoryConflict without a
// cycle). One write, one conflict, two vocabularies — rather than two methods
// that could drift apart.
// (docs/fs-formal-proof/11-change-register.md §13.2, row 83)
func (a *MemoryStoreAdapter) SaveWorkspaceFileIfUnchanged(ctx context.Context, agentID, userID, filename string, data []byte, expected string) error {
	err := a.st.SaveAgentFileIfVersion(ctx, agentID, userID, filename, data,
		store.AgentFileVersion{Content: []byte(expected)})
	if errors.Is(err, store.ErrAgentFileConflict) {
		return fmt.Errorf("%w (%w)", ErrMemoryConflict, store.ErrAgentFileConflict)
	}
	return err
}

// ListKnowledgeDocs / SearchKnowledgeChunks expose the owner-uploaded
// knowledge corpus. Both resolve the agent-owner fallback inside the
// store, so a chatter's userID finds the owner's corpus on shared agents.
func (a *MemoryStoreAdapter) ListKnowledgeDocs(ctx context.Context, agentID, userID string) ([]store.KnowledgeDoc, error) {
	return a.st.ListAgentKnowledgeDocs(ctx, agentID, userID)
}

func (a *MemoryStoreAdapter) SearchKnowledgeChunks(ctx context.Context, agentID, userID, query string, limit int) ([]store.KnowledgeChunkRecord, error) {
	return a.st.SearchAgentKnowledgeChunks(ctx, agentID, userID, query, limit)
}
