package agent

import (
	"log/slog"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
)

// Manager loads and manages all agent instances.
type Manager struct {
	agents       map[string]*Agent
	defaultAgent *Agent
}

// NewManager creates agents from resolved configs.
func NewManager(resolved []config.ResolvedAgent, prov provider.Provider, mb *bus.MessageBus) (*Manager, error) {
	return NewManagerWithMem0(resolved, prov, mb, config.Mem0Cfg{})
}

// NewManagerWithMem0 creates agents with optional mem0 memory integration.
func NewManagerWithMem0(resolved []config.ResolvedAgent, prov provider.Provider, mb *bus.MessageBus, mem0Cfg config.Mem0Cfg) (*Manager, error) {
	m := &Manager{
		agents: make(map[string]*Agent),
	}

	homeDir, err := config.HomeDir()
	if err != nil {
		return nil, err
	}

	for _, rc := range resolved {
		ag := NewAgent(rc, prov, mb, homeDir)

		// Register mem0 hooks if enabled
		if mem0Cfg.Enabled {
			ag.RegisterMem0Hook(mem0Cfg)
		}

		m.agents[rc.ID] = ag

		slog.Info("loaded agent",
			"id", rc.ID,
			"model", rc.Model,
			"workspace", rc.Workspace,
		)
	}

	// If only one agent, make it the default
	if len(m.agents) == 1 {
		for _, ag := range m.agents {
			m.defaultAgent = ag
		}
	}

	return m, nil
}

// AgentByID returns an agent by its ID.
func (m *Manager) AgentByID(id string) *Agent {
	return m.agents[id]
}

// DefaultAgent returns the default agent (set when only one agent exists).
func (m *Manager) DefaultAgent() *Agent {
	return m.defaultAgent
}

// All returns all loaded agents.
func (m *Manager) All() []*Agent {
	result := make([]*Agent, 0, len(m.agents))
	for _, ag := range m.agents {
		result = append(result, ag)
	}
	return result
}

// Names returns all agent IDs.
func (m *Manager) Names() []string {
	names := make([]string, 0, len(m.agents))
	for name := range m.agents {
		names = append(names, name)
	}
	return names
}

// UpdateProvider replaces the LLM provider for all agents (hot-reload).
func (m *Manager) UpdateProvider(prov provider.Provider) {
	for _, ag := range m.agents {
		ag.provider = prov
	}
}
