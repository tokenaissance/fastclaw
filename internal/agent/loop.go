package agent

import (
	"context"
	"log/slog"

	"github.com/fastclaw-ai/fastclaw/internal/agent/tools"
	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
	"github.com/fastclaw-ai/fastclaw/internal/session"
)

// Agent is the ReAct agent loop.
type Agent struct {
	provider   provider.Provider
	registry   *tools.Registry
	sessions   *session.Manager
	memory     *Memory
	ctxBuilder *ContextBuilder
	config     config.AgentDefaults
}

// NewAgent creates a new Agent.
func NewAgent(p provider.Provider, cfg config.AgentDefaults, mb *bus.MessageBus) *Agent {
	memory := NewMemory(cfg.Workspace)
	registry := tools.NewRegistry(cfg.Workspace)
	tools.RegisterMessage(registry, mb)

	return &Agent{
		provider:   p,
		registry:   registry,
		sessions:   session.NewManager(cfg.Workspace + "/sessions"),
		memory:     memory,
		ctxBuilder: NewContextBuilder(cfg.Workspace, memory),
		config:     cfg,
	}
}

// HandleMessage processes an inbound message through the ReAct loop.
func (a *Agent) HandleMessage(ctx context.Context, msg bus.InboundMessage) string {
	sess := a.sessions.Get(msg.Channel, msg.ChatID)

	// Build system prompt
	systemPrompt := a.ctxBuilder.BuildSystemPrompt()

	// Build runtime context
	runtimeCtx := a.ctxBuilder.BuildRuntimeContext(msg.Channel, msg.ChatID)

	// Prepend runtime context to user message
	userContent := runtimeCtx + "\n\n" + msg.Text

	// Append user message to session
	sess.Append(provider.Message{Role: "user", Content: userContent})

	// Build messages for LLM
	messages := make([]provider.Message, 0, len(sess.GetMessages())+1)
	messages = append(messages, provider.Message{Role: "system", Content: systemPrompt})
	messages = append(messages, sess.GetMessages()...)

	toolDefs := a.registry.Definitions()

	// ReAct loop
	for i := 0; i < a.config.MaxToolIterations; i++ {
		slog.Info("agent loop iteration", "iteration", i+1, "channel", msg.Channel, "chat_id", msg.ChatID)

		resp, err := a.provider.Chat(ctx, messages, toolDefs, a.config.Model, a.config.MaxTokens, a.config.Temperature)
		if err != nil {
			slog.Error("LLM chat failed", "error", err)
			return "Sorry, I encountered an error processing your request."
		}

		if !resp.HasToolCalls() {
			// Final response
			sess.Append(provider.Message{Role: "assistant", Content: resp.Content})
			return resp.Content
		}

		// Append assistant message with tool calls
		assistantMsg := provider.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		}
		sess.Append(assistantMsg)
		messages = append(messages, assistantMsg)

		// Execute each tool call
		for _, tc := range resp.ToolCalls {
			slog.Info("executing tool", "name", tc.Function.Name, "id", tc.ID)

			result, err := a.registry.Execute(ctx, tc.Function.Name, tc.Function.Arguments)
			if err != nil {
				slog.Warn("tool execution error", "name", tc.Function.Name, "error", err)
			}

			toolMsg := provider.Message{
				Role:       "tool",
				Content:    result,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
			}
			sess.Append(toolMsg)
			messages = append(messages, toolMsg)
		}
	}

	slog.Warn("max tool iterations reached", "max", a.config.MaxToolIterations)
	return "I've reached the maximum number of tool iterations. Here's what I have so far."
}
