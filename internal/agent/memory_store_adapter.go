package agent

import (
	"context"

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

func (a *MemoryStoreAdapter) GetMemory(ctx context.Context, agentID, userID string) (string, error) {
	data, err := a.st.GetAgentFile(ctx, agentID, userID, memoryFilename)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (a *MemoryStoreAdapter) SaveMemory(ctx context.Context, agentID, userID, content string) error {
	return a.st.SaveAgentFile(ctx, agentID, userID, memoryFilename, []byte(content))
}

func (a *MemoryStoreAdapter) GetWorkspaceFile(ctx context.Context, agentID, userID, filename string) ([]byte, error) {
	return a.st.GetAgentFile(ctx, agentID, userID, filename)
}

func (a *MemoryStoreAdapter) SaveWorkspaceFile(ctx context.Context, agentID, userID, filename string, data []byte) error {
	return a.st.SaveAgentFile(ctx, agentID, userID, filename, data)
}
