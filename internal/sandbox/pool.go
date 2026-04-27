package sandbox

import (
	"os"
	"path/filepath"
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
//
// On creation we wire BOTH skill dirs into the sandbox so the LLM's
// `python /skills/<name>/main.py` resolves whether the skill lives in
// the global $FASTCLAW_HOME/skills/ tree or this agent's private
// $FASTCLAW_HOME/agents/<agentID>/agent/skills/. Without the per-agent
// mount, skills the operator dropped into agents/<id>/agent/skills/
// (e.g. via SkillsLoader's per-agent layer) silently fail to load
// inside the container.
func (p *SandboxPool) Get(agentID, image, workspace string, policy *Policy) *DockerSandbox {
	p.mu.Lock()
	defer p.mu.Unlock()

	if sb, ok := p.sandboxes[agentID]; ok {
		return sb
	}

	sb := NewDockerSandbox(image, workspace, policy)
	if dirs := skillDirsForAgent(agentID); len(dirs) > 0 {
		sb.SetSkillDirs(dirs)
	}
	p.sandboxes[agentID] = sb
	return sb
}

// skillDirsForAgent returns the host paths whose `<dir>/<skill-name>/`
// children should be mounted at /skills/<skill-name>/ inside the
// sandbox. Order matters only for conflict resolution at mount time
// (later dirs win since Docker rejects duplicate mount paths) — keep
// the per-agent dir first so it overrides any global skill of the
// same name, matching SkillsLoader's per-agent-wins precedence.
func skillDirsForAgent(agentID string) []string {
	home := os.Getenv("FASTCLAW_HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".fastclaw")
		}
	}
	if home == "" {
		return nil
	}
	dirs := []string{filepath.Join(home, "agents", agentID, "agent", "skills")}
	dirs = append(dirs, filepath.Join(home, "skills"))
	return dirs
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
