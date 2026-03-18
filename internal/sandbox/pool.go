package sandbox

import (
	"sync"
)

// SandboxPool manages sandbox containers per agent, reusing them across exec calls.
type SandboxPool struct {
	sandboxes map[string]*DockerSandbox // agentID -> sandbox
	mu        sync.Mutex
}

// NewPool creates a new sandbox pool.
func NewPool() *SandboxPool {
	return &SandboxPool{
		sandboxes: make(map[string]*DockerSandbox),
	}
}

// Get returns (or lazily creates) a sandbox for the given agent.
func (p *SandboxPool) Get(agentID, image, workspace string, policy *Policy) *DockerSandbox {
	p.mu.Lock()
	defer p.mu.Unlock()

	if sb, ok := p.sandboxes[agentID]; ok {
		return sb
	}

	sb := NewDockerSandbox(image, workspace, policy)
	p.sandboxes[agentID] = sb
	return sb
}

// Close shuts down and removes all sandbox containers.
func (p *SandboxPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for id, sb := range p.sandboxes {
		sb.Close()
		delete(p.sandboxes, id)
	}
}

// List returns info about all active sandboxes.
func (p *SandboxPool) List() []SandboxInfo {
	p.mu.Lock()
	defer p.mu.Unlock()

	var infos []SandboxInfo
	for agentID, sb := range p.sandboxes {
		infos = append(infos, SandboxInfo{
			AgentID:     agentID,
			ContainerID: sb.ContainerID(),
			Image:       sb.image,
			Workspace:   sb.workspace,
		})
	}
	return infos
}

// Remove destroys a specific sandbox by agent ID.
func (p *SandboxPool) Remove(agentID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	sb, ok := p.sandboxes[agentID]
	if !ok {
		return nil
	}
	err := sb.Close()
	delete(p.sandboxes, agentID)
	return err
}

// SandboxInfo holds display info for a sandbox.
type SandboxInfo struct {
	AgentID     string
	ContainerID string
	Image       string
	Workspace   string
}
