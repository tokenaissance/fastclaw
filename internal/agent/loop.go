package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/fastclaw-ai/fastclaw/internal/agent/tools"
	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/mcp"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
	"github.com/fastclaw-ai/fastclaw/internal/session"
)

// Agent is the ReAct agent loop.
type Agent struct {
	name              string
	provider          provider.Provider
	registry          *tools.Registry
	sessions          *session.Manager
	memory            *Memory
	ctxBuilder        *ContextBuilder
	mcpMgr            *mcp.Manager
	hooks             *HookRegistry
	model             string
	maxTokens         int
	temperature       float64
	maxToolIterations int
	workspacePath     string
}

// NewAgent creates a new Agent from a resolved config.
func NewAgent(rc config.ResolvedAgent, prov provider.Provider, mb *bus.MessageBus, homeDir string) *Agent {
	memory := NewMemory(rc.Workspace)
	registry := tools.NewRegistry(rc.Workspace)
	tools.RegisterMessage(registry, mb)
	tools.RegisterMemorySearch(registry, rc.Workspace)
	tools.RegisterWebFetch(registry)
	tools.RegisterLoadSkill(registry, homeDir, rc.Workspace, "")

	// Load skills
	loader := NewSkillsLoader(homeDir, rc.Workspace, "", rc.Skills)
	skills := loader.LoadSkills()
	skillsSummary := loader.BuildSkillsSummary(skills)

	if len(skills) > 0 {
		slog.Info("loaded skills", "agent", rc.ID, "count", len(skills))
	}

	// Set up hooks with logging
	hooks := NewHookRegistry()
	hooks.Register(BeforeModelCall, LoggingHook())
	hooks.Register(AfterModelCall, LoggingHook())
	hooks.Register(BeforeToolCall, LoggingHook())
	hooks.Register(AfterToolCall, LoggingHook())

	ag := &Agent{
		name:              rc.ID,
		provider:          prov,
		registry:          registry,
		sessions:          session.NewManager(rc.Workspace + "/sessions"),
		memory:            memory,
		ctxBuilder:        NewContextBuilder(rc.Workspace, memory, skillsSummary),
		hooks:             hooks,
		model:             rc.Model,
		maxTokens:         rc.MaxTokens,
		temperature:       rc.Temperature,
		maxToolIterations: rc.MaxToolIterations,
		workspacePath:     rc.Workspace,
	}

	// Connect MCP servers and register their tools
	if len(rc.MCPServers) > 0 {
		mcpMgr := mcp.NewManager(rc.MCPServers)
		ag.mcpMgr = mcpMgr

		for _, td := range mcpMgr.ToolDefs() {
			toolName := td.Name
			ag.registry.Register(toolName, td.Description, td.InputSchema,
				func(ctx context.Context, args json.RawMessage) (string, error) {
					return mcpMgr.CallTool(ctx, toolName, args)
				},
			)
		}

		if mcpMgr.HasTools() {
			slog.Info("registered MCP tools", "agent", rc.ID)
		}
	}

	return ag
}

// Name returns the agent's name.
func (a *Agent) Name() string {
	return a.name
}

// workspace returns the agent's workspace path.
func (a *Agent) workspace() string {
	return a.workspacePath
}

// SetGroupContext configures group chat awareness for this agent's system prompt.
func (a *Agent) SetGroupContext(gc *GroupContext) {
	a.ctxBuilder.SetGroupContext(gc)
}

// InjectGroupMessage appends a message from another bot into the session history
// without triggering an LLM call. This gives the agent awareness of what other
// bots said in the group chat.
func (a *Agent) InjectGroupMessage(ctx context.Context, msg bus.InboundMessage) {
	sess := a.sessions.Get(msg.Channel, msg.ChatID)
	label := msg.SenderName
	if label == "" {
		label = "Bot"
	}
	content := fmt.Sprintf("[%s]: %s", label, msg.Text)
	sess.Append(provider.Message{Role: "user", Content: content})
}

// HandleMessage processes an inbound message through the ReAct loop.
func (a *Agent) HandleMessage(ctx context.Context, msg bus.InboundMessage) string {
	sess := a.sessions.Get(msg.Channel, msg.ChatID)

	// Hook: BeforeSystemPrompt
	a.hooks.Run(ctx, &HookContext{AgentName: a.name, Point: BeforeSystemPrompt})

	systemPrompt := a.ctxBuilder.BuildSystemPrompt()

	// Hook: AfterSystemPrompt
	a.hooks.Run(ctx, &HookContext{AgentName: a.name, Point: AfterSystemPrompt})

	runtimeCtx := a.ctxBuilder.BuildRuntimeContext(msg.Channel, msg.ChatID)
	userContent := runtimeCtx + "\n\n" + msg.Text

	sess.Append(provider.Message{Role: "user", Content: userContent})

	// Context compaction: check if session messages are too large
	sessionMsgs := sess.GetMessages()
	compactResult, err := CompactMessages(sessionMsgs, a.workspacePath, a.provider, a.model)
	if err != nil {
		slog.Warn("compaction error", "agent", a.name, "error", err)
	}
	if compactResult != nil && compactResult.Pruned {
		// Replace session messages with compacted version
		sess.ReplaceMessages(compactResult.Messages)
		sessionMsgs = compactResult.Messages
		slog.Info("context compacted", "agent", a.name, "log_file", compactResult.LogFile)
	}

	messages := make([]provider.Message, 0, len(sessionMsgs)+1)
	messages = append(messages, provider.Message{Role: "system", Content: systemPrompt})
	messages = append(messages, sessionMsgs...)

	toolDefs := a.registry.Definitions()

	// ReAct loop
	for i := 0; i < a.maxToolIterations; i++ {
		slog.Info("agent loop iteration",
			"agent", a.name,
			"iteration", i+1,
			"channel", msg.Channel,
			"chat_id", msg.ChatID,
		)

		// Hook: BeforeModelCall
		hcBefore := &HookContext{AgentName: a.name, Point: BeforeModelCall, Messages: messages}
		a.hooks.Run(ctx, hcBefore)

		resp, err := a.provider.Chat(ctx, messages, toolDefs, a.model, a.maxTokens, a.temperature)

		// Hook: AfterModelCall
		hcAfter := &HookContext{AgentName: a.name, Point: AfterModelCall, Messages: messages, Response: resp, Error: err, StartTime: hcBefore.StartTime}
		a.hooks.Run(ctx, hcAfter)

		if err != nil {
			slog.Error("LLM chat failed", "agent", a.name, "error", err)
			return "Sorry, I encountered an error processing your request."
		}

		if !resp.HasToolCalls() {
			sess.Append(provider.Message{Role: "assistant", Content: resp.Content})
			return resp.Content
		}

		assistantMsg := provider.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		}
		sess.Append(assistantMsg)
		messages = append(messages, assistantMsg)

		for _, tc := range resp.ToolCalls {
			// Hook: BeforeToolCall
			hcToolBefore := &HookContext{
				AgentName: a.name,
				Point:     BeforeToolCall,
				ToolName:  tc.Function.Name,
				ToolArgs:  tc.Function.Arguments,
			}
			a.hooks.Run(ctx, hcToolBefore)

			slog.Info("executing tool",
				"agent", a.name,
				"name", tc.Function.Name,
				"id", tc.ID,
			)

			result, err := a.registry.Execute(ctx, tc.Function.Name, tc.Function.Arguments)

			// Hook: AfterToolCall
			hcToolAfter := &HookContext{
				AgentName:  a.name,
				Point:      AfterToolCall,
				ToolName:   tc.Function.Name,
				ToolResult: result,
				Error:      err,
				StartTime:  hcToolBefore.StartTime,
			}
			a.hooks.Run(ctx, hcToolAfter)

			if err != nil {
				slog.Warn("tool execution error",
					"agent", a.name,
					"name", tc.Function.Name,
					"error", err,
				)
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

	slog.Warn("max tool iterations reached", "agent", a.name, "max", a.maxToolIterations)
	return "I've reached the maximum number of tool iterations. Here's what I have so far."
}
