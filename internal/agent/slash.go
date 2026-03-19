package agent

import (
	"fmt"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

// slashResult holds the result of a slash command.
type slashResult struct {
	handled bool
	reply   string
}

// handleSlashCommand checks if the message is a slash command and handles it.
// Returns (handled, reply). If handled is false, the message should be processed normally.
func (a *Agent) handleSlashCommand(msg bus.InboundMessage) slashResult {
	text := strings.TrimSpace(msg.Text)
	if !strings.HasPrefix(text, "/") {
		return slashResult{}
	}

	parts := strings.Fields(text)
	cmd := strings.ToLower(parts[0])
	// Strip @botname suffix from Telegram commands: /status@mybot → /status
	if idx := strings.Index(cmd, "@"); idx > 0 {
		cmd = cmd[:idx]
	}

	switch cmd {
	case "/start":
		return slashResult{
			handled: true,
			reply:   fmt.Sprintf("👋 Hi! I'm %s, your AI assistant.\n\nJust send me a message to chat. Use /help to see available commands.", a.name),
		}
	case "/new", "/reset":
		sess := a.sessions.Get(msg.Channel, msg.ChatID)
		sess.Clear()
		return slashResult{handled: true, reply: "Session cleared. Starting fresh."}

	case "/compact":
		return a.slashCompact(msg)

	case "/status":
		return a.slashStatus(msg)

	case "/help":
		return slashResult{handled: true, reply: a.slashHelp()}

	case "/version":
		return slashResult{handled: true, reply: fmt.Sprintf("FastClaw agent: %s\nModel: %s", a.name, a.model)}

	default:
		return slashResult{}
	}
}

func (a *Agent) slashCompact(msg bus.InboundMessage) slashResult {
	sess := a.sessions.Get(msg.Channel, msg.ChatID)
	sessionMsgs := sess.GetMessages()

	if len(sessionMsgs) == 0 {
		return slashResult{handled: true, reply: "No messages to compact."}
	}

	result, err := CompactMessages(sessionMsgs, a.workspacePath, a.provider, a.model)
	if err != nil {
		return slashResult{handled: true, reply: fmt.Sprintf("Compaction error: %v", err)}
	}
	if result != nil && result.Pruned {
		sess.ReplaceMessages(result.Messages)
		return slashResult{handled: true, reply: fmt.Sprintf("Compacted: %d messages -> %d messages.", len(sessionMsgs), len(result.Messages))}
	}

	return slashResult{handled: true, reply: "Session is within limits, no compaction needed."}
}

func (a *Agent) slashStatus(msg bus.InboundMessage) slashResult {
	sess := a.sessions.Get(msg.Channel, msg.ChatID)
	sessionMsgs := sess.GetMessages()

	memContent := a.memory.LoadMemory()
	memLines := 0
	if memContent != "" {
		memLines = strings.Count(memContent, "\n") + 1
	}

	status := fmt.Sprintf(`Agent: %s
Model: %s
Max Tokens: %d
Temperature: %.1f
Max Tool Iterations: %d
Session Messages: %d
Memory Lines: %d
Workspace: %s`,
		a.name,
		a.model,
		a.maxTokens,
		a.temperature,
		a.maxToolIterations,
		len(sessionMsgs),
		memLines,
		a.workspacePath,
	)

	return slashResult{handled: true, reply: status}
}

func (a *Agent) slashHelp() string {
	return `Available commands:
/new, /reset  — Clear session history
/compact      — Trigger manual context compaction
/status       — Show agent status (model, session info, memory)
/help         — Show this help message
/version      — Show FastClaw version`
}
