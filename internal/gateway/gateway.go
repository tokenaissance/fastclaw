package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/channels"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/cron"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
)

const dedupTTL = 60 * time.Second

// dedupEntry tracks when a message was first seen.
type dedupEntry struct {
	seenAt time.Time
}

// Gateway is the main orchestrator that starts all services.
type Gateway struct {
	config       *config.Config
	bus          *bus.MessageBus
	agents       *agent.Manager
	chanMgr      *channels.Manager
	bindings     []config.Binding
	botUsernames map[string]string          // agentID -> bot username
	teams        map[string]config.TeamEntry // team name -> team config
	dedup        sync.Map                    // dedup key -> dedupEntry
	heartbeats   []*agent.Heartbeat
	scheduler    *cron.Scheduler
}

// New creates a new Gateway with multi-agent support.
func New(cfg *config.Config) (*Gateway, error) {
	mb := bus.New()

	// Create LLM provider
	providerCfg := cfg.Providers["openai"]
	llm := provider.NewOpenAI(providerCfg.APIKey, providerCfg.APIBase)

	// Resolve agent configs
	resolved := config.ResolveAgents(cfg)

	// Create agent manager
	agentMgr, err := agent.NewManager(resolved, llm, mb)
	if err != nil {
		return nil, err
	}

	slog.Info("agents loaded", "count", len(resolved), "names", agentMgr.Names())

	// Create channel manager and register channel instances
	chanMgr := channels.NewManager(mb)

	if err := registerChannels(cfg, mb, chanMgr); err != nil {
		return nil, err
	}

	// Build agentID -> botUsername mapping from bindings + channel manager
	botUsernames := buildBotUsernames(cfg.Bindings, chanMgr)
	if len(botUsernames) > 0 {
		slog.Info("bot username mappings", "map", botUsernames)
	}

	teams := cfg.Teams
	if teams == nil {
		teams = make(map[string]config.TeamEntry)
	}

	// Set up group context for agents in teams
	for _, team := range teams {
		for _, agentID := range team.Agents {
			ag := agentMgr.AgentByID(agentID)
			if ag == nil {
				continue
			}
			var teammates []string
			for _, otherID := range team.Agents {
				if otherID != agentID {
					if uname, ok := botUsernames[otherID]; ok {
						teammates = append(teammates, "@"+uname)
					} else {
						teammates = append(teammates, otherID)
					}
				}
			}
			if botUname, ok := botUsernames[agentID]; ok {
				ag.SetGroupContext(&agent.GroupContext{
					BotUsername: botUname,
					Teammates:  teammates,
				})
			}
		}
	}

	// Set up heartbeats for each agent
	heartbeatInterval := time.Duration(cfg.Heartbeat.IntervalMinutes) * time.Minute
	if heartbeatInterval <= 0 {
		heartbeatInterval = agent.DefaultHeartbeatInterval
	}
	var heartbeats []*agent.Heartbeat
	for _, ag := range agentMgr.All() {
		hb := agent.NewHeartbeat(ag, mb, heartbeatInterval)
		heartbeats = append(heartbeats, hb)
	}

	// Set up cron scheduler
	var cronJobs []cron.Job
	for _, cj := range cfg.CronJobs {
		cronJobs = append(cronJobs, cron.Job{
			Name:     cj.Name,
			Type:     cron.JobType(cj.Type),
			Schedule: cj.Schedule,
			AgentID:  cj.AgentID,
			Channel:  cj.Channel,
			ChatID:   cj.ChatID,
			Message:  cj.Message,
		})
	}
	scheduler := cron.NewScheduler(cronJobs, mb)

	return &Gateway{
		config:       cfg,
		bus:          mb,
		agents:       agentMgr,
		chanMgr:      chanMgr,
		bindings:     cfg.Bindings,
		botUsernames: botUsernames,
		teams:        teams,
		heartbeats:   heartbeats,
		scheduler:    scheduler,
	}, nil
}

// buildBotUsernames creates agentID -> botUsername mapping by looking at bindings
// and resolving the bot username from the channel manager.
func buildBotUsernames(bindings []config.Binding, chanMgr *channels.Manager) map[string]string {
	m := make(map[string]string)
	for _, b := range bindings {
		if b.Match.Channel == "" {
			continue
		}
		username := chanMgr.BotUsername(b.Match.Channel, b.Match.AccountID)
		if username != "" {
			m[b.AgentID] = username
		}
	}
	return m
}

// registerChannels creates channel instances from config, one per account.
func registerChannels(cfg *config.Config, mb *bus.MessageBus, chanMgr *channels.Manager) error {
	for name, chCfg := range cfg.Channels {
		if !chCfg.Enabled {
			continue
		}

		switch name {
		case "telegram":
			if err := registerTelegramChannels(chCfg, mb, chanMgr); err != nil {
				return err
			}
		}
	}
	return nil
}

func registerTelegramChannels(chCfg config.ChannelConfig, mb *bus.MessageBus, chanMgr *channels.Manager) error {
	if len(chCfg.Accounts) == 0 {
		// No accounts defined — use the channel-level botToken as the default account
		tg, err := channels.NewTelegram(chCfg.BotToken, "", mb)
		if err != nil {
			return err
		}
		chanMgr.Register(tg)
		return nil
	}

	// One instance per account
	for accountID, acct := range chCfg.Accounts {
		token := acct.BotToken
		if token == "" {
			token = chCfg.BotToken // fall back to parent
		}
		tg, err := channels.NewTelegram(token, accountID, mb)
		if err != nil {
			return err
		}
		chanMgr.Register(tg)
	}
	return nil
}

// Run starts the gateway and blocks until shutdown signal.
func (g *Gateway) Run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		slog.Info("received signal, shutting down", "signal", sig)
		cancel()
	}()

	var wg sync.WaitGroup

	// Start dedup cleanup goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		g.cleanupDedup(ctx)
	}()

	// Start inbound message processor
	wg.Add(1)
	go func() {
		defer wg.Done()
		g.processInbound(ctx)
	}()

	// Start channel manager
	wg.Add(1)
	go func() {
		defer wg.Done()
		g.chanMgr.Start(ctx)
	}()

	// Start heartbeats for each agent
	for _, hb := range g.heartbeats {
		wg.Add(1)
		go func(h *agent.Heartbeat) {
			defer wg.Done()
			h.Start(ctx)
		}(hb)
	}

	// Start cron scheduler
	wg.Add(1)
	go func() {
		defer wg.Done()
		g.scheduler.Start(ctx)
	}()

	slog.Info("gateway started")

	wg.Wait()
	slog.Info("gateway stopped")
	return nil
}

// cleanupDedup periodically removes expired entries from the dedup cache.
func (g *Gateway) cleanupDedup(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			g.dedup.Range(func(key, value any) bool {
				entry := value.(dedupEntry)
				if now.Sub(entry.seenAt) > dedupTTL {
					g.dedup.Delete(key)
				}
				return true
			})
		}
	}
}

// isDuplicate returns true if this group message has already been seen.
// Uses channel:chatID:messageID as the dedup key.
func (g *Gateway) isDuplicate(msg bus.InboundMessage) bool {
	// In Telegram supergroups, each bot gets a different message_id for the same message.
	// So we deduplicate using chatID + userID + text hash instead.
	if msg.PeerKind != "group" {
		return false
	}
	key := fmt.Sprintf("%s:%s:%s:%x", msg.Channel, msg.ChatID, msg.UserID, hashString(msg.Text))
	_, loaded := g.dedup.LoadOrStore(key, dedupEntry{seenAt: time.Now()})
	return loaded
}

func hashString(s string) uint32 {
	var h uint32
	for _, c := range s {
		h = h*31 + uint32(c)
	}
	return h
}

func (g *Gateway) processInbound(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-g.bus.Inbound:
			// For DMs, use existing binding-based routing
			if msg.PeerKind != "group" {
				g.routeDM(ctx, msg)
				continue
			}

			// Deduplicate group messages (multiple bots receive the same message)
			if g.isDuplicate(msg) {
				slog.Info("dropping duplicate group message",
					"channel", msg.Channel,
					"chat_id", msg.ChatID,
					"message_id", msg.MessageID,
				)
				continue
			}

			// Group message handling
			slog.Info("group message accepted", "message_id", msg.MessageID, "account", msg.AccountID, "chat_id", msg.ChatID, "is_bot", msg.IsBotMessage)
			g.routeGroup(ctx, msg)
		}
	}
}

// routeDM handles direct message routing (existing behavior).
func (g *Gateway) routeDM(ctx context.Context, msg bus.InboundMessage) {
	ag := g.matchAgent(msg)
	if ag == nil {
		slog.Warn("no agent matched for DM, dropping",
			"channel", msg.Channel,
			"account", msg.AccountID,
			"chat_id", msg.ChatID,
		)
		return
	}

	slog.Info("routing DM",
		"channel", msg.Channel,
		"account", msg.AccountID,
		"chat_id", msg.ChatID,
		"agent", ag.Name(),
	)

	go func(m bus.InboundMessage, a *agent.Agent) {
		reply := a.HandleMessage(ctx, m)
		g.bus.Outbound <- bus.OutboundMessage{
			Channel:   m.Channel,
			AccountID: m.AccountID,
			ChatID:    m.ChatID,
			Text:      reply,
		}
	}(msg, ag)
}

// routeGroup handles group message routing with mention-based and team-aware logic.
func (g *Gateway) routeGroup(ctx context.Context, msg bus.InboundMessage) {
	// Find all agents bound to this group chat
	boundAgents := g.agentsBoundToMessage(msg)

	if len(boundAgents) == 0 {
		slog.Warn("no agents bound for group message, dropping",
			"channel", msg.Channel,
			"chat_id", msg.ChatID,
		)
		return
	}

	// If message is from a bot, inject into all agents for awareness,
	// and also trigger any @mentioned agent to respond.
	if msg.IsBotMessage {
		slog.Info("processing bot message in group",
			"sender", msg.SenderName,
			"chat_id", msg.ChatID,
			"mentions", msg.Mentions,
			"agents_count", len(boundAgents),
		)

		// Inject into all agents for awareness
		for _, ag := range boundAgents {
			ag.InjectGroupMessage(ctx, msg)
		}

		// If this bot message @mentions another agent, trigger that agent to respond
		if len(msg.Mentions) > 0 {
			target := g.agentByMention(msg.Mentions, boundAgents)
			if target != nil {
				slog.Info("bot message triggers mentioned agent",
					"sender", msg.SenderName,
					"target", target.Name(),
					"chat_id", msg.ChatID,
				)

				// Build a trigger message with the sender bot's name as context
				triggerMsg := msg
				triggerMsg.Text = fmt.Sprintf("[%s]: %s", msg.SenderName, msg.Text)
				triggerMsg.IsBotMessage = false // treat as actionable for HandleMessage

				go func(m bus.InboundMessage, a *agent.Agent) {
					reply := a.HandleMessage(ctx, m)
					g.bus.Outbound <- bus.OutboundMessage{
						Channel:   m.Channel,
						AccountID: g.accountIDForAgent(a.Name(), m.Channel),
						ChatID:    m.ChatID,
						Text:      reply,
					}
				}(triggerMsg, target)
			}
		}
		return
	}

	// If message has @mentions, only route to the mentioned agent
	if len(msg.Mentions) > 0 {
		target := g.agentByMention(msg.Mentions, boundAgents)
		if target != nil {
			slog.Info("routing group message by @mention",
				"chat_id", msg.ChatID,
				"agent", target.Name(),
				"mentions", msg.Mentions,
			)

			// Inject into other agents for awareness (without triggering reply)
			for _, ag := range boundAgents {
				if ag.Name() != target.Name() {
					ag.InjectGroupMessage(ctx, msg)
				}
			}

			go func(m bus.InboundMessage, a *agent.Agent) {
				reply := a.HandleMessage(ctx, m)
				g.bus.Outbound <- bus.OutboundMessage{
					Channel:   m.Channel,
					AccountID: g.accountIDForAgent(a.Name(), m.Channel),
					ChatID:    m.ChatID,
					Text:      reply,
				}
			}(msg, target)
			return
		}
		// Mentioned username doesn't match any agent — fall through to default behavior
	}

	// No @mention: use team groupBehavior
	behavior, defaultAgentID := g.groupBehaviorFor(boundAgents)

	switch behavior {
	case "default-agent":
		target := g.agents.AgentByID(defaultAgentID)
		if target == nil {
			// Fallback: use first bound agent
			target = boundAgents[0]
		}

		slog.Info("routing group message to default agent",
			"chat_id", msg.ChatID,
			"agent", target.Name(),
		)

		// Inject into other agents for awareness
		for _, ag := range boundAgents {
			if ag.Name() != target.Name() {
				ag.InjectGroupMessage(ctx, msg)
			}
		}

		go func(m bus.InboundMessage, a *agent.Agent) {
			reply := a.HandleMessage(ctx, m)
			g.bus.Outbound <- bus.OutboundMessage{
				Channel:   m.Channel,
				AccountID: g.accountIDForAgent(a.Name(), m.Channel),
				ChatID:    m.ChatID,
				Text:      reply,
			}
		}(msg, target)

	default: // "mention-only"
		// No @mention and behavior is mention-only: inject into all agents for awareness, but no reply
		slog.Info("group message without mention (mention-only mode), injecting for awareness",
			"chat_id", msg.ChatID,
			"agents_count", len(boundAgents),
		)
		for _, ag := range boundAgents {
			ag.InjectGroupMessage(ctx, msg)
		}
	}
}

// agentsBoundToMessage returns all agents whose bindings match this message.
func (g *Gateway) agentsBoundToMessage(msg bus.InboundMessage) []*agent.Agent {
	if len(g.bindings) == 0 {
		if def := g.agents.DefaultAgent(); def != nil {
			return []*agent.Agent{def}
		}
		return nil
	}

	seen := make(map[string]bool)
	var result []*agent.Agent
	for _, b := range g.bindings {
		if !matchBinding(b.Match, msg) {
			continue
		}
		if seen[b.AgentID] {
			continue
		}
		ag := g.agents.AgentByID(b.AgentID)
		if ag != nil {
			seen[b.AgentID] = true
			result = append(result, ag)
		}
	}
	return result
}

// agentByMention finds the agent whose bot username matches one of the @mentions.
func (g *Gateway) agentByMention(mentions []string, candidates []*agent.Agent) *agent.Agent {
	for _, mention := range mentions {
		for _, ag := range candidates {
			botUsername, ok := g.botUsernames[ag.Name()]
			if ok && botUsername == mention {
				return ag
			}
		}
	}
	return nil
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

// matchAgent evaluates bindings top-to-bottom and returns the first matching agent.
// Falls back to the default agent if no bindings are defined.
func (g *Gateway) matchAgent(msg bus.InboundMessage) *agent.Agent {
	if len(g.bindings) == 0 {
		return g.agents.DefaultAgent()
	}

	for _, b := range g.bindings {
		if !matchBinding(b.Match, msg) {
			continue
		}
		ag := g.agents.AgentByID(b.AgentID)
		if ag != nil {
			return ag
		}
		slog.Warn("binding references unknown agent", "agentId", b.AgentID)
	}

	// No binding matched — fall back to default
	return g.agents.DefaultAgent()
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
