package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/agent/tools"
	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

func chatKey(channel, chatID string) string {
	return channel + ":" + chatID
}

// processInbound consumes the message bus and routes each message to the
// correct user's agent. Identity resolution order:
//   1. msg.OwnerUserID set explicitly (cron, webhook with user_id)
//   2. lookup the receiving channel's row in the channels table — its
//      (scope, scope_id) tells us which user owns this conversation
// If neither yields a user_id the message is dropped, never silently
// routed to a default identity.
func (g *Gateway) processInbound(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-g.bus.Inbound:
			ownerID := msg.OwnerUserID
			if ownerID == "" {
				ownerID = g.resolveChannelOwner(ctx, msg)
			}
			if ownerID == "" {
				slog.Warn("dropping inbound: cannot resolve owner",
					"channel", msg.Channel, "chat_id", msg.ChatID, "account", msg.AccountID)
				continue
			}
			msg.OwnerUserID = ownerID

			if msg.PeerKind != "group" {
				g.routeDM(ctx, msg)
				continue
			}
			if g.isDuplicate(msg) {
				slog.Info("dropping duplicate group message",
					"channel", msg.Channel, "chat_id", msg.ChatID, "message_id", msg.MessageID)
				continue
			}
			slog.Info("group message accepted",
				"message_id", msg.MessageID, "account", msg.AccountID,
				"chat_id", msg.ChatID, "is_bot", msg.IsBotMessage, "owner", ownerID)
			g.routeGroup(ctx, msg)
		}
	}
}

// resolveChannelOwner looks up the channels table for the inbound's
// receiving channel and returns the owning user_id, or "" if not found
// or scope==system (system channels have no individual owner).
func (g *Gateway) resolveChannelOwner(ctx context.Context, msg bus.InboundMessage) string {
	if g.store == nil {
		return ""
	}
	rec, err := g.store.LookupChannelByCredential(ctx, msg.Channel, msg.AccountID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Warn("channel lookup failed", "channel", msg.Channel, "error", err)
		}
		return ""
	}
	switch rec.Scope {
	case "user":
		return rec.ScopeID
	case "agent":
		// agent-scoped channel — find the agent's owner
		if rec.ScopeID == "" {
			return ""
		}
		// We don't know the user yet; ListAllAgents + filter is
		// fine since channel-scope lookups are infrequent.
		all, err := g.store.ListAllAgents(ctx)
		if err != nil {
			return ""
		}
		for _, ar := range all {
			if ar.ID == rec.ScopeID {
				return ar.UserID
			}
		}
	}
	return ""
}

func (g *Gateway) routeDM(ctx context.Context, msg bus.InboundMessage) {
	space, err := g.users.getOrLoad(ctx, msg.OwnerUserID)
	if err != nil {
		slog.Warn("user space load failed", "user", msg.OwnerUserID, "error", err)
		return
	}
	ag := g.matchAgent(space, msg)
	if ag == nil {
		slog.Warn("no agent matched for DM, dropping",
			"user", msg.OwnerUserID, "channel", msg.Channel,
			"account", msg.AccountID, "chat_id", msg.ChatID)
		return
	}
	slog.Info("routing DM",
		"user", msg.OwnerUserID, "channel", msg.Channel,
		"chat_id", msg.ChatID, "agent", ag.Name())
	g.taskQueue.Submit(ag.Name(), chatKey(msg.Channel, msg.ChatID), msg, msg.AccountID)
}

func (g *Gateway) routeGroup(ctx context.Context, msg bus.InboundMessage) {
	space, err := g.users.getOrLoad(ctx, msg.OwnerUserID)
	if err != nil {
		slog.Warn("user space load failed", "user", msg.OwnerUserID, "error", err)
		return
	}
	boundAgents := g.agentsBoundToMessage(space, msg)
	if len(boundAgents) == 0 {
		slog.Warn("no agents bound for group message, dropping",
			"user", msg.OwnerUserID, "chat_id", msg.ChatID)
		return
	}
	if msg.IsBotMessage {
		for _, ag := range boundAgents {
			ag.InjectGroupMessage(ctx, msg)
		}
		if len(msg.Mentions) > 0 {
			if target := g.agentByMention(space, msg.Mentions, boundAgents); target != nil {
				triggerMsg := msg
				triggerMsg.Text = fmt.Sprintf("[%s]: %s", msg.SenderName, msg.Text)
				triggerMsg.IsBotMessage = false
				g.taskQueue.Submit(target.Name(), chatKey(triggerMsg.Channel, triggerMsg.ChatID), triggerMsg, g.accountIDForAgent(space, target.Name(), triggerMsg.Channel))
			}
		}
		return
	}
	if len(msg.Mentions) > 0 {
		if target := g.agentByMention(space, msg.Mentions, boundAgents); target != nil {
			for _, ag := range boundAgents {
				if ag.Name() != target.Name() {
					ag.InjectGroupMessage(ctx, msg)
				}
			}
			g.taskQueue.Submit(target.Name(), chatKey(msg.Channel, msg.ChatID), msg, g.accountIDForAgent(space, target.Name(), msg.Channel))
			return
		}
	}
	behavior, defaultAgentID := groupBehaviorFor(space, boundAgents)
	switch behavior {
	case "default-agent":
		target := space.Agents.AgentByID(defaultAgentID)
		if target == nil {
			target = boundAgents[0]
		}
		for _, ag := range boundAgents {
			if ag.Name() != target.Name() {
				ag.InjectGroupMessage(ctx, msg)
			}
		}
		g.taskQueue.Submit(target.Name(), chatKey(msg.Channel, msg.ChatID), msg, g.accountIDForAgent(space, target.Name(), msg.Channel))
	default:
		for _, ag := range boundAgents {
			ag.InjectGroupMessage(ctx, msg)
		}
	}
}

func (g *Gateway) matchAgent(space *UserSpace, msg bus.InboundMessage) *agent.Agent {
	if space == nil {
		return nil
	}
	// Explicit agent target wins. Cron jobs, web chat, and sub-agent
	// spawns all know the agent at the source — without this, multi-
	// agent users with no web/cron binding fell back to DefaultAgent()
	// which returns nil whenever the manager holds more than one
	// agent, and the message got dropped with "no agent matched for
	// DM, dropping" even though the cron row had AgentID right there.
	if msg.AgentID != "" {
		if ag := space.Agents.AgentByID(msg.AgentID); ag != nil {
			return ag
		}
	}
	bindings := space.Config.Bindings
	if len(bindings) == 0 {
		return space.Agents.DefaultAgent()
	}
	for _, b := range bindings {
		if !matchBinding(b.Match, msg) {
			continue
		}
		if ag := space.Agents.AgentByID(b.AgentID); ag != nil {
			return ag
		}
	}
	return space.Agents.DefaultAgent()
}

func matchBinding(m config.Match, msg bus.InboundMessage) bool {
	if m.Channel != "" && m.Channel != msg.Channel {
		return false
	}
	if m.AccountID != "" && m.AccountID != msg.AccountID {
		return false
	}
	if m.Peer != nil {
		if m.Peer.Kind != "" && m.Peer.Kind != msg.PeerKind {
			return false
		}
		if m.Peer.ID != "" && m.Peer.ID != msg.ChatID {
			return false
		}
	}
	return true
}

func (g *Gateway) agentsBoundToMessage(space *UserSpace, msg bus.InboundMessage) []*agent.Agent {
	if space == nil {
		return nil
	}
	bindings := space.Config.Bindings
	if len(bindings) == 0 {
		if def := space.Agents.DefaultAgent(); def != nil {
			return []*agent.Agent{def}
		}
		return nil
	}
	seen := make(map[string]bool)
	var out []*agent.Agent
	for _, b := range bindings {
		if !matchBinding(b.Match, msg) || seen[b.AgentID] {
			continue
		}
		if ag := space.Agents.AgentByID(b.AgentID); ag != nil {
			seen[b.AgentID] = true
			out = append(out, ag)
		}
	}
	return out
}

func (g *Gateway) agentByMention(space *UserSpace, mentions []string, candidates []*agent.Agent) *agent.Agent {
	usernames := buildBotUsernames(space.Config.Bindings, g.chanMgr)
	for _, mention := range mentions {
		for _, ag := range candidates {
			if u, ok := usernames[ag.Name()]; ok && u == mention {
				return ag
			}
		}
	}
	return nil
}

// groupBehaviorFor returns the team's groupBehavior + defaultAgent for the
// given candidate agents, or ("mention-only", "") when there's no team.
func groupBehaviorFor(space *UserSpace, agents []*agent.Agent) (string, string) {
	if space == nil {
		return "mention-only", ""
	}
	for _, team := range space.Config.Teams {
		matching := 0
		for _, ag := range agents {
			for _, member := range team.Agents {
				if member == ag.Name() {
					matching++
					break
				}
			}
		}
		if matching == len(agents) && matching > 0 {
			behavior := team.GroupBehavior
			if behavior == "" {
				behavior = "mention-only"
			}
			return behavior, team.DefaultAgent
		}
	}
	return "mention-only", ""
}

func (g *Gateway) accountIDForAgent(space *UserSpace, agentID, channel string) string {
	for _, b := range space.Config.Bindings {
		if b.AgentID == agentID && b.Match.Channel == channel && b.Match.AccountID != "" {
			return b.Match.AccountID
		}
	}
	return ""
}

// gatewaySubAgentSpawner implements tools.SubAgentSpawner. Sub-agents
// always run inside the *same* user's agent manager — there's no cross-
// tenant agent invocation.
type gatewaySubAgentSpawner struct {
	gateway *Gateway
	userID  string
}

func (s *gatewaySubAgentSpawner) SpawnSubAgent(ctx context.Context, agentID string, msg bus.InboundMessage) string {
	space, err := s.gateway.users.getOrLoad(ctx, s.userID)
	if err != nil {
		return fmt.Sprintf("Error: load user space: %v", err)
	}
	ag := space.Agents.AgentByID(agentID)
	if ag == nil {
		return fmt.Sprintf("Error: agent %q not found", agentID)
	}
	return ag.HandleMessage(ctx, msg)
}

var _ tools.SubAgentSpawner = (*gatewaySubAgentSpawner)(nil)

// webhookAgentHandler routes a webhook payload to the named agent within
// the resolved user's space.
type webhookAgentHandler struct {
	gateway *Gateway
}

func (h *webhookAgentHandler) HandleMessage(ctx context.Context, agentID string, msg bus.InboundMessage) (string, error) {
	if msg.OwnerUserID == "" {
		return "", fmt.Errorf("webhook: owner user_id required")
	}
	space, err := h.gateway.users.getOrLoad(ctx, msg.OwnerUserID)
	if err != nil {
		return "", err
	}
	ag := space.Agents.AgentByID(agentID)
	if ag == nil {
		return "", fmt.Errorf("agent %q not found for user %q", agentID, msg.OwnerUserID)
	}
	return ag.HandleMessage(ctx, msg), nil
}
