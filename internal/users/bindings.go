package users

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/fastclaw-ai/fastclaw/internal/config"
)

// AgentBindings maps each agent to at most one API key. Agents without a
// binding are "admin-owned by default" — only the admin token can touch
// them. Storage is a flat file next to apikeys.json:
//
//	~/.fastclaw/agent-bindings.json  →  { "<agent-id>": "<api-key-id>", ... }
//
// Bindings are control-plane only; the data plane (sessions, memory,
// workspace files) still keys purely on agent_id.
type AgentBindings struct {
	path string
	mu   sync.RWMutex
	// byAgent: agent id → owning api key id
	byAgent map[string]string
}

// DefaultBindingsPath returns ~/.fastclaw/agent-bindings.json.
func DefaultBindingsPath() (string, error) {
	home, err := config.HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "agent-bindings.json"), nil
}

// LoadBindings reads bindings from the default path, returning an empty
// registry if the file doesn't exist yet.
func LoadBindings() (*AgentBindings, error) {
	path, err := DefaultBindingsPath()
	if err != nil {
		return nil, err
	}
	return LoadBindingsFrom(path)
}

func LoadBindingsFrom(path string) (*AgentBindings, error) {
	b := &AgentBindings{path: path, byAgent: make(map[string]string)}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return b, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read agent-bindings: %w", err)
	}
	if err := json.Unmarshal(data, &b.byAgent); err != nil {
		return nil, fmt.Errorf("parse agent-bindings: %w", err)
	}
	return b, nil
}

// Save persists the current state. Callers should invoke this after every
// mutation — bindings are small (1 line per agent) so rewriting the file
// wholesale is cheap.
func (b *AgentBindings) Save() error {
	b.mu.RLock()
	data, _ := json.MarshalIndent(b.byAgent, "", "  ")
	b.mu.RUnlock()
	if err := os.MkdirAll(filepath.Dir(b.path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(b.path, data, 0o600)
}

// Bind associates agentID with apiKeyID, replacing any prior binding. An
// empty apiKeyID is treated as Unbind(agentID).
func (b *AgentBindings) Bind(agentID, apiKeyID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if apiKeyID == "" {
		delete(b.byAgent, agentID)
		return
	}
	b.byAgent[agentID] = apiKeyID
}

// Unbind removes any binding for agentID. Safe to call even if none exists.
func (b *AgentBindings) Unbind(agentID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.byAgent, agentID)
}

// OwnerOf returns the api key id that owns agentID, or "" if unbound.
func (b *AgentBindings) OwnerOf(agentID string) string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.byAgent[agentID]
}

// AgentsOf returns every agent id bound to apiKeyID, in no particular order.
func (b *AgentBindings) AgentsOf(apiKeyID string) []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var out []string
	for agentID, ownerID := range b.byAgent {
		if ownerID == apiKeyID {
			out = append(out, agentID)
		}
	}
	return out
}

// All returns the full binding map (copy, safe to mutate).
func (b *AgentBindings) All() map[string]string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make(map[string]string, len(b.byAgent))
	for k, v := range b.byAgent {
		out[k] = v
	}
	return out
}
