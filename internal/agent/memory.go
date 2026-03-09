package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Memory manages the dual-layer memory system (MEMORY.md + HISTORY.md).
type Memory struct {
	workspace string
}

// NewMemory creates a new memory manager.
func NewMemory(workspace string) *Memory {
	return &Memory{workspace: workspace}
}

// memoryPath returns the path to MEMORY.md.
func (m *Memory) memoryPath() string {
	return filepath.Join(m.workspace, "MEMORY.md")
}

// historyPath returns the path to HISTORY.md.
func (m *Memory) historyPath() string {
	return filepath.Join(m.workspace, "HISTORY.md")
}

// LoadMemory reads the long-term memory file.
func (m *Memory) LoadMemory() string {
	data, err := os.ReadFile(m.memoryPath())
	if err != nil {
		return ""
	}
	return string(data)
}

// SaveMemory overwrites the long-term memory file.
func (m *Memory) SaveMemory(content string) error {
	os.MkdirAll(m.workspace, 0o755)
	return os.WriteFile(m.memoryPath(), []byte(content), 0o644)
}

// AppendHistory adds an entry to the history log.
func (m *Memory) AppendHistory(entry string) error {
	os.MkdirAll(m.workspace, 0o755)
	f, err := os.OpenFile(m.historyPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	timestamp := time.Now().Format("2006-01-02 15:04:05")
	_, err = fmt.Fprintf(f, "- [%s] %s\n", timestamp, entry)
	return err
}

// LoadHistory reads the history log.
func (m *Memory) LoadHistory() string {
	data, err := os.ReadFile(m.historyPath())
	if err != nil {
		return ""
	}
	return string(data)
}
