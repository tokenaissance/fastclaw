package gateway

import (
	"github.com/fastclaw-ai/fastclaw/internal/agent"
)

// groupBehaviorFor determines the group behavior and default agent for a set of agents.
// Looks up teams config to find matching team settings.
func (g *Gateway) groupBehaviorFor(agents []*agent.Agent) (behavior string, defaultAgent string) {
	agentIDs := make(map[string]bool, len(agents))
	for _, ag := range agents {
		agentIDs[ag.Name()] = true
	}

	for _, team := range g.teams {
		// Check if this team contains any of the bound agents
		match := 0
		for _, tid := range team.Agents {
			if agentIDs[tid] {
				match++
			}
		}
		if match > 0 {
			b := team.GroupBehavior
			if b == "" {
				b = "mention-only"
			}
			return b, team.DefaultAgent
		}
	}

	return "mention-only", ""
}

// accountIDForAgent returns the accountID associated with an agent for a given channel.
// Looks up bindings to find the account.
func (g *Gateway) accountIDForAgent(agentID, channel string) string {
	for _, b := range g.bindings {
		if b.AgentID == agentID && b.Match.Channel == channel {
			return b.Match.AccountID
		}
	}
	return ""
}
