package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/codeany-ai/open-agent-sdk-go/costtracker"

	"github.com/fastclaw-ai/fastclaw/internal/agent/tools"
	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/mcp"
	"github.com/fastclaw-ai/fastclaw/internal/privacy"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
	"github.com/fastclaw-ai/fastclaw/internal/session"
	"github.com/fastclaw-ai/fastclaw/internal/store"
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
	thinking          string
	workspacePath     string
	homeDir           string
	skillsCfg         config.SkillsConfig
	globalSkillsCfg   config.SkillsCfg
	messageBus        *bus.MessageBus
	subAgentSpawner   tools.SubAgentSpawner
	ftsStore          *store.FTSStore
	piiScrubEnabled   bool
	memoryCfg         config.MemoryCfg
	skillsLearner     *SkillsLearner
	turnCount         int
	engine            *sdkEngine
	costTracker       *costtracker.Tracker
}

// NewAgent creates a new Agent from a resolved config.
func NewAgent(rc config.ResolvedAgent, prov provider.Provider, mb *bus.MessageBus, homeDir string) *Agent {
	return NewAgentWithSkillsCfg(rc, prov, mb, homeDir, config.SkillsCfg{})
}

// NewAgentWithFullCfg creates a new Agent with full config support (memory, privacy, skills learner).
func NewAgentWithFullCfg(rc config.ResolvedAgent, prov provider.Provider, mb *bus.MessageBus, homeDir string, fullCfg *config.Config) *Agent {
	ag := NewAgentWithSkillsCfg(rc, prov, mb, homeDir, fullCfg.Skills)
	ag.memoryCfg = fullCfg.Memory
	ag.piiScrubEnabled = fullCfg.Privacy.PIIScrubbing.Enabled

	// Set up FTS store if configured
	if fullCfg.Memory.FTS.Enabled {
		dbPath := fullCfg.Memory.FTS.DBPath
		if dbPath == "" {
			dbPath = rc.Workspace + "/memory/fts.db"
		}
		if fts, err := store.NewFTSStore(dbPath); err == nil {
			if err := fts.Init(); err == nil {
				ag.ftsStore = fts
				slog.Info("FTS5 search enabled", "agent", rc.ID, "db", dbPath)
			} else {
				slog.Warn("FTS5 init failed, falling back to file scan", "error", err)
			}
		} else {
			slog.Warn("FTS5 store open failed, falling back to file scan", "error", err)
		}
	}

	// Set up skills learner if configured
	if fullCfg.SkillsLearner.Enabled {
		model := fullCfg.SkillsLearner.Model
		if model == "" {
			model = rc.Model
		}
		learnerLoader := NewSkillsLoaderWithGlobal(homeDir, rc.Workspace, "", rc.Skills, fullCfg.Skills)
		ag.skillsLearner = NewSkillsLearner(rc.Workspace, prov, model, learnerLoader.AllSkillDirs()...)
		if fullCfg.SkillsLearner.MinToolCalls > 0 {
			ag.skillsLearner.minToolCalls = fullCfg.SkillsLearner.MinToolCalls
		}
	}

	// Set memory auto-persist defaults
	if ag.memoryCfg.AutoPersist.EveryNTurns == 0 {
		ag.memoryCfg.AutoPersist.EveryNTurns = 5
	}

	return ag
}

// NewAgentWithSkillsCfg creates a new Agent with global skills config for env injection.
func NewAgentWithSkillsCfg(rc config.ResolvedAgent, prov provider.Provider, mb *bus.MessageBus, homeDir string, globalSkillsCfg config.SkillsCfg) *Agent {
	memory := NewMemory(rc.Workspace)
	registry := tools.NewRegistry(rc.Workspace)
	tools.RegisterMessage(registry, mb)
	tools.RegisterMemorySearch(registry, rc.Workspace)
	tools.RegisterWebFetch(registry)
	tools.RegisterLoadSkill(registry, homeDir, rc.Workspace, "")

	// Load skills with OpenClaw compatibility
	loader := NewSkillsLoaderWithGlobal(homeDir, rc.Workspace, "", rc.Skills, globalSkillsCfg)
	skills := loader.LoadSkills()
	skillsSummary := loader.BuildSkillsSummary(skills)

	// Set up skill env injection for exec tool
	skillDirs := loader.AllSkillDirs()
	tools.RegisterExecWithSkillEnv(registry, nil, loader.SkillEnvVars, skillDirs)

	if len(skills) > 0 {
		slog.Info("loaded skills", "agent", rc.ID, "count", len(skills))
	}

	// Set up hooks with logging
	hooks := NewHookRegistry()
	hooks.Register(BeforeModelCall, LoggingHook())
	hooks.Register(AfterModelCall, LoggingHook())
	hooks.Register(BeforeToolCall, LoggingHook())
	hooks.Register(AfterToolCall, LoggingHook())

	eng := newSDKEngine(rc.ID)

	ag := &Agent{
		name:              rc.ID,
		provider:          prov,
		registry:          registry,
		sessions:          session.NewManager(rc.Workspace + "/sessions"),
		memory:            memory,
		ctxBuilder:        newContextBuilderWithThinking(rc.Workspace, memory, skillsSummary, rc.Thinking),
		hooks:             hooks,
		model:             rc.Model,
		maxTokens:         rc.MaxTokens,
		temperature:       rc.Temperature,
		maxToolIterations: rc.MaxToolIterations,
		thinking:          rc.Thinking,
		workspacePath:     rc.Workspace,
		homeDir:           homeDir,
		skillsCfg:         rc.Skills,
		globalSkillsCfg:   globalSkillsCfg,
		messageBus:        mb,
		engine:            eng,
		costTracker:       eng.costTracker,
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

func newContextBuilderWithThinking(workspace string, memory *Memory, skillsSummary string, thinking string) *ContextBuilder {
	cb := NewContextBuilder(workspace, memory, skillsSummary)
	if thinking != "" {
		cb.SetThinking(thinking)
	}
	return cb
}

// Name returns the agent's name.
func (a *Agent) Name() string {
	return a.name
}

// HandleWebChat handles a chat message from the web UI with a session ID.
func (a *Agent) HandleWebChat(ctx context.Context, sessionId, text string) string {
	if sessionId == "" {
		sessionId = "web-ui"
	}
	msg := bus.InboundMessage{
		Channel:  "web",
		ChatID:   sessionId,
		UserID:   "web-user",
		Text:     text,
		PeerKind: "dm",
	}
	return a.HandleMessage(ctx, msg)
}

// HandleWebChatStream handles a web chat message with real-time event streaming.
func (a *Agent) HandleWebChatStream(ctx context.Context, sessionId, text string, events chan<- ChatEvent) string {
	if sessionId == "" {
		sessionId = "web-ui"
	}
	ctx = ContextWithChatEvents(ctx, events)
	msg := bus.InboundMessage{
		Channel:  "web",
		ChatID:   sessionId,
		UserID:   "web-user",
		Text:     text,
		PeerKind: "dm",
	}
	return a.HandleMessage(ctx, msg)
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

// SetSubAgentSpawner sets the sub-agent spawner for the spawn_subagent tool.
func (a *Agent) SetSubAgentSpawner(spawner tools.SubAgentSpawner) {
	a.subAgentSpawner = spawner
	tools.RegisterSubAgent(a.registry, spawner, a.name)
}

// ToolRegistry returns the agent's tool registry for external registration.
func (a *Agent) ToolRegistry() *tools.Registry {
	return a.registry
}

// HookRegistry returns the agent's hook registry for external hook registration.
func (a *Agent) HookRegistry() *HookRegistry {
	return a.hooks
}

// RegisterWebSearchTool registers the web_search tool with the given API key.
func (a *Agent) RegisterWebSearchTool(apiKey string) {
	tools.RegisterWebSearch(a.registry, apiKey)
}

// Sessions returns the session manager for this agent.
func (a *Agent) Sessions() *session.Manager {
	return a.sessions
}

// WebChatHistory returns chat history for a specific web session.
func (a *Agent) WebChatHistory(sessionId string) []map[string]any {
	if sessionId == "" {
		sessionId = "web-ui"
	}
	sess := a.sessions.Get("web", sessionId)
	msgs := sess.GetMessages()
	var history []map[string]any
	for _, m := range msgs {
		switch m.Role {
		case "user":
			if m.Content != "" {
				history = append(history, map[string]any{
					"role":    "user",
					"content": m.Content,
				})
			}
		case "assistant":
			entry := map[string]any{"role": "assistant"}
			if m.Content != "" {
				entry["content"] = m.Content
			}
			if len(m.ToolCalls) > 0 {
				var calls []map[string]string
				for _, tc := range m.ToolCalls {
					calls = append(calls, map[string]string{
						"id":        tc.ID,
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
					})
				}
				entry["toolCalls"] = calls
			}
			// Skip empty assistant messages (no content, no tool calls)
			if m.Content == "" && len(m.ToolCalls) == 0 {
				continue
			}
			history = append(history, entry)
		case "tool":
			history = append(history, map[string]any{
				"role":       "tool",
				"content":    m.Content,
				"name":       m.Name,
				"toolCallId": m.ToolCallID,
			})
		}
	}
	return history
}

// WebChatSessions returns a list of web chat sessions with their first user message as preview.
func (a *Agent) WebChatSessions() []map[string]string {
	return a.sessions.ListWebSessions()
}

// Model returns the agent's model name.
func (a *Agent) Model() string {
	return a.model
}

// CostTracker returns the agent's cost tracker for usage/billing queries.
func (a *Agent) CostTracker() *costtracker.Tracker {
	return a.costTracker
}

// HandleMessage processes an inbound message through the ReAct loop.
func (a *Agent) HandleMessage(ctx context.Context, msg bus.InboundMessage) string {
	// Check for slash commands first
	if result := a.handleSlashCommand(msg); result.handled {
		emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{"content": result.reply}})
		emitEvent(ctx, ChatEvent{Type: "done"})
		return result.reply
	}

	sess := a.sessions.Get(msg.Channel, msg.ChatID)

	// Hook: BeforeSystemPrompt
	a.hooks.Run(ctx, &HookContext{AgentName: a.name, Point: BeforeSystemPrompt})

	systemPrompt := a.ctxBuilder.BuildSystemPrompt()

	// Hook: AfterSystemPrompt
	a.hooks.Run(ctx, &HookContext{AgentName: a.name, Point: AfterSystemPrompt})

	// Store the raw user message
	userMsg := provider.Message{Role: "user", Content: msg.Text}
	if msg.PhotoURL != "" {
		userMsg.Content = ""
		userMsg.ContentParts = []provider.ContentPart{
			{Type: "text", Text: msg.Text},
			{Type: "image_url", ImageURL: &provider.ImageURL{URL: msg.PhotoURL, Detail: "auto"}},
		}
	}
	sess.Append(userMsg)

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

	// Loop detection: track consecutive identical tool calls
	type toolCallSig struct {
		name string
		hash [32]byte
	}
	var lastSig toolCallSig
	consecutiveCount := 0
	totalToolCalls := 0

	// ReAct loop
	for i := 0; i < a.maxToolIterations; i++ {
		slog.Info("agent loop iteration",
			"agent", a.name,
			"iteration", i+1,
			"channel", msg.Channel,
			"chat_id", msg.ChatID,
		)

		// Hook: BeforeModelCall
		hcBefore := &HookContext{AgentName: a.name, Point: BeforeModelCall, Messages: messages, ChatID: msg.ChatID}
		a.hooks.Run(ctx, hcBefore)

		// PII scrubbing: redact sensitive data before sending to LLM
		llmMessages := messages
		if a.piiScrubEnabled {
			llmMessages = privacy.ScrubMessages(messages)
		}

		resp, err := a.provider.Chat(ctx, llmMessages, toolDefs, a.model, a.maxTokens, a.temperature)

		// Hook: AfterModelCall
		hcAfter := &HookContext{AgentName: a.name, Point: AfterModelCall, Messages: messages, Response: resp, Error: err, StartTime: hcBefore.StartTime, ChatID: msg.ChatID}
		a.hooks.Run(ctx, hcAfter)

		if err != nil {
			slog.Error("LLM chat failed", "agent", a.name, "error", err)
			return "Sorry, I encountered an error processing your request."
		}

		if !resp.HasToolCalls() {
			sess.Append(provider.Message{Role: "assistant", Content: resp.Content})
			emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{"content": resp.Content}})
			emitEvent(ctx, ChatEvent{Type: "done"})
			a.runPostTurn(ctx, messages, totalToolCalls)
			return resp.Content
		}

		// Emit assistant content before tool calls if present
		if resp.Content != "" {
			emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{"content": resp.Content}})
		}

		// Emit tool_call events
		for _, tc := range resp.ToolCalls {
			emitEvent(ctx, ChatEvent{Type: "tool_call", Data: map[string]any{
				"id":        tc.ID,
				"name":      tc.Function.Name,
				"arguments": tc.Function.Arguments,
			}})
		}

		assistantMsg := provider.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		}
		sess.Append(assistantMsg)
		messages = append(messages, assistantMsg)

		// Loop detection: check before executing
		loopDetected := false
		for _, tc := range resp.ToolCalls {
			sig := toolCallSig{
				name: tc.Function.Name,
				hash: sha256.Sum256([]byte(tc.Function.Arguments)),
			}
			if sig.name == lastSig.name && sig.hash == lastSig.hash {
				consecutiveCount++
			} else {
				consecutiveCount = 1
				lastSig = sig
			}
			if consecutiveCount >= 3 {
				slog.Warn("tool loop detected", "agent", a.name, "tool", tc.Function.Name)
				warnMsg := provider.Message{
					Role:    "system",
					Content: "Loop detected: you called the same tool with the same arguments 3 times. Please try a different approach.",
				}
				sess.Append(warnMsg)
				messages = append(messages, warnMsg)
				loopDetected = true
				break
			}
		}
		if loopDetected {
			break
		}

		// Fire BeforeToolCall hooks
		for _, tc := range resp.ToolCalls {
			a.hooks.Run(ctx, &HookContext{
				AgentName: a.name,
				Point:     BeforeToolCall,
				ToolName:  tc.Function.Name,
				ToolArgs:  tc.Function.Arguments,
			})
		}

		// Execute tools concurrently via SDK engine
		slog.Info("executing tools concurrently",
			"agent", a.name,
			"count", len(resp.ToolCalls),
		)
		results := a.engine.executeToolsConcurrently(ctx, a.registry, resp.ToolCalls, a.workspacePath)

		// Process results
		for idx, r := range results {
			totalToolCalls++
			tc := resp.ToolCalls[idx]

			// Hook: AfterToolCall
			a.hooks.Run(ctx, &HookContext{
				AgentName:  a.name,
				Point:      AfterToolCall,
				ToolName:   r.toolName,
				ToolResult: r.result,
				Error:      r.err,
			})

			if r.err != nil {
				slog.Warn("tool execution error",
					"agent", a.name,
					"name", r.toolName,
					"error", r.err,
				)
			}

			// Index in FTS if available
			if a.ftsStore != nil {
				_ = a.ftsStore.Index(a.name, msg.ChatID, "tool:"+r.toolName, r.result, time.Now())
			}

			// Check for MEDIA: protocol in tool output
			if mediaPaths := extractMediaPaths(r.result); len(mediaPaths) > 0 {
				a.sendMediaFiles(msg, mediaPaths)
			}

			toolMsg := provider.Message{
				Role:       "tool",
				Content:    r.result,
				ToolCallID: tc.ID,
				Name:       r.toolName,
			}
			sess.Append(toolMsg)
			messages = append(messages, toolMsg)

			emitEvent(ctx, ChatEvent{Type: "tool_result", Data: map[string]any{
				"id":     tc.ID,
				"name":   r.toolName,
				"result": r.result,
			}})
		}
	}

	a.runPostTurn(ctx, messages, totalToolCalls)
	slog.Warn("max tool iterations reached", "agent", a.name, "max", a.maxToolIterations)
	return "I've reached the maximum number of tool iterations. Here's what I have so far."
}

// runPostTurn fires PostTurn hooks and handles auto-persist and skills learning.
func (a *Agent) runPostTurn(ctx context.Context, messages []provider.Message, toolCallCount int) {
	a.turnCount++

	// Index user/assistant messages in FTS
	if a.ftsStore != nil {
		for _, m := range messages {
			if m.Role == "user" || m.Role == "assistant" {
				_ = a.ftsStore.Index(a.name, "", m.Role, m.Content, time.Now())
			}
		}
	}

	// Fire PostTurn hooks
	a.hooks.Run(ctx, &HookContext{
		AgentName:     a.name,
		Point:         PostTurn,
		Messages:      messages,
		TurnCount:     a.turnCount,
		ToolCallCount: toolCallCount,
		Workspace:     a.workspacePath,
	})

	// Auto-persist memory every N turns
	if a.memoryCfg.AutoPersist.Enabled && a.turnCount%a.memoryCfg.AutoPersist.EveryNTurns == 0 {
		model := a.memoryCfg.AutoPersist.Model
		if model == "" {
			model = a.model
		}
		go AutoPersistMemory(ctx, a.memory, a.provider, model, messages)
	}

	// Skills learner
	if a.skillsLearner != nil {
		go func() {
			if err := a.skillsLearner.MaybeExtract(ctx, messages, toolCallCount); err != nil {
				slog.Debug("skills learner error", "error", err)
			}
		}()
	}
}

// HandleMessageStream processes a message through the ReAct loop and returns
// a StreamReader for the final response. Tool call iterations use non-streaming Chat;
// the final text response uses ChatStream for true SSE streaming.
func (a *Agent) HandleMessageStream(ctx context.Context, msg bus.InboundMessage) *provider.StreamReader {
	// Reuse setup logic from HandleMessage
	if result := a.handleSlashCommand(msg); result.handled {
		ch := make(chan provider.StreamChunk, 2)
		go func() {
			ch <- provider.StreamChunk{Content: result.reply, Done: true}
			close(ch)
		}()
		return provider.NewStreamReader(ch)
	}

	sess := a.sessions.Get(msg.Channel, msg.ChatID)
	a.hooks.Run(ctx, &HookContext{AgentName: a.name, Point: BeforeSystemPrompt})
	systemPrompt := a.ctxBuilder.BuildSystemPrompt()
	a.hooks.Run(ctx, &HookContext{AgentName: a.name, Point: AfterSystemPrompt})

	// Store raw user message
	userMsg := provider.Message{Role: "user", Content: msg.Text}
	if msg.PhotoURL != "" {
		userMsg.Content = ""
		userMsg.ContentParts = []provider.ContentPart{
			{Type: "text", Text: msg.Text},
			{Type: "image_url", ImageURL: &provider.ImageURL{URL: msg.PhotoURL, Detail: "auto"}},
		}
	}
	sess.Append(userMsg)

	sessionMsgs := sess.GetMessages()
	compactResult, err := CompactMessages(sessionMsgs, a.workspacePath, a.provider, a.model)
	if err != nil {
		slog.Warn("compaction error", "agent", a.name, "error", err)
	}
	if compactResult != nil && compactResult.Pruned {
		sess.ReplaceMessages(compactResult.Messages)
		sessionMsgs = compactResult.Messages
	}

	messages := make([]provider.Message, 0, len(sessionMsgs)+1)
	messages = append(messages, provider.Message{Role: "system", Content: systemPrompt})
	messages = append(messages, sessionMsgs...)

	toolDefs := a.registry.Definitions()

	type toolCallSig struct {
		name string
		hash [32]byte
	}
	var lastSig toolCallSig
	consecutiveCount := 0

	// ReAct loop - use Chat for tool iterations
	for i := 0; i < a.maxToolIterations; i++ {
		hcBefore := &HookContext{AgentName: a.name, Point: BeforeModelCall, Messages: messages, ChatID: msg.ChatID}
		a.hooks.Run(ctx, hcBefore)

		resp, err := a.provider.Chat(ctx, messages, toolDefs, a.model, a.maxTokens, a.temperature)

		hcAfter := &HookContext{AgentName: a.name, Point: AfterModelCall, Messages: messages, Response: resp, Error: err, StartTime: hcBefore.StartTime, ChatID: msg.ChatID}
		a.hooks.Run(ctx, hcAfter)

		if err != nil {
			slog.Error("LLM chat failed", "agent", a.name, "error", err)
			return a.stringStream("Sorry, I encountered an error processing your request.")
		}

		if !resp.HasToolCalls() {
			// Final response - use streaming
			sr, err := a.provider.ChatStream(ctx, messages, toolDefs, a.model, a.maxTokens, a.temperature)
			if err != nil {
				slog.Error("LLM stream failed, falling back", "agent", a.name, "error", err)
				sess.Append(provider.Message{Role: "assistant", Content: resp.Content})
				return a.stringStream(resp.Content)
			}

			// Collect content in background for session storage
			outCh := make(chan provider.StreamChunk, 64)
			outReader := provider.NewStreamReader(outCh)
			go func() {
				defer close(outCh)
				var full strings.Builder
				for {
					chunk, ok := sr.Next()
					if !ok {
						break
					}
					if chunk.Content != "" {
						full.WriteString(chunk.Content)
					}
					select {
					case outCh <- chunk:
					case <-ctx.Done():
						return
					}
				}
				sess.Append(provider.Message{Role: "assistant", Content: full.String()})
			}()
			return outReader
		}

		// Tool calls - process concurrently via SDK engine
		assistantMsg := provider.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		}
		sess.Append(assistantMsg)
		messages = append(messages, assistantMsg)

		// Loop detection
		loopDetected := false
		for _, tc := range resp.ToolCalls {
			sig := toolCallSig{
				name: tc.Function.Name,
				hash: sha256.Sum256([]byte(tc.Function.Arguments)),
			}
			if sig.name == lastSig.name && sig.hash == lastSig.hash {
				consecutiveCount++
			} else {
				consecutiveCount = 1
				lastSig = sig
			}
			if consecutiveCount >= 3 {
				slog.Warn("tool loop detected", "agent", a.name, "tool", tc.Function.Name)
				warnMsg := provider.Message{
					Role:    "system",
					Content: "Loop detected: you called the same tool with the same arguments 3 times. Please try a different approach.",
				}
				sess.Append(warnMsg)
				messages = append(messages, warnMsg)
				loopDetected = true
				break
			}
		}
		if loopDetected {
			break
		}

		// Fire BeforeToolCall hooks
		for _, tc := range resp.ToolCalls {
			a.hooks.Run(ctx, &HookContext{AgentName: a.name, Point: BeforeToolCall, ToolName: tc.Function.Name, ToolArgs: tc.Function.Arguments})
		}

		// Execute tools concurrently via SDK engine
		results := a.engine.executeToolsConcurrently(ctx, a.registry, resp.ToolCalls, a.workspacePath)

		for idx, r := range results {
			tc := resp.ToolCalls[idx]
			a.hooks.Run(ctx, &HookContext{AgentName: a.name, Point: AfterToolCall, ToolName: r.toolName, ToolResult: r.result, Error: r.err})

			if r.err != nil {
				slog.Warn("tool execution error", "agent", a.name, "name", r.toolName, "error", r.err)
			}

			if mediaPaths := extractMediaPaths(r.result); len(mediaPaths) > 0 {
				a.sendMediaFiles(msg, mediaPaths)
			}

			toolMsg := provider.Message{Role: "tool", Content: r.result, ToolCallID: tc.ID, Name: r.toolName}
			sess.Append(toolMsg)
			messages = append(messages, toolMsg)
		}
	}

	return a.stringStream("I've reached the maximum number of tool iterations. Here's what I have so far.")
}

// stringStream creates a StreamReader that yields a single string.
func (a *Agent) stringStream(text string) *provider.StreamReader {
	ch := make(chan provider.StreamChunk, 2)
	go func() {
		ch <- provider.StreamChunk{Content: text, Done: true}
		close(ch)
	}()
	return provider.NewStreamReader(ch)
}

// WorkspacePath returns the agent's workspace directory.
func (a *Agent) WorkspacePath() string {
	return a.workspacePath
}

// UpdateConfig updates the agent's runtime config (model, temperature, etc.)
func (a *Agent) UpdateConfig(rc config.ResolvedAgent) {
	a.model = rc.Model
	a.maxTokens = rc.MaxTokens
	a.temperature = rc.Temperature
	a.maxToolIterations = rc.MaxToolIterations
}

// ReloadWorkspaceFiles re-reads workspace .md files (SOUL.md, AGENTS.md, etc.)
// and rebuilds the context builder.
func (a *Agent) ReloadWorkspaceFiles() {
	a.memory = NewMemory(a.workspacePath)
	// Rebuild skills summary
	loader := NewSkillsLoaderWithGlobal(a.homeDir, a.workspacePath, "", a.skillsCfg, a.globalSkillsCfg)
	skills := loader.LoadSkills()
	skillsSummary := loader.BuildSkillsSummary(skills)
	a.ctxBuilder = NewContextBuilder(a.workspacePath, a.memory, skillsSummary)
}

// extractMediaPaths scans tool output for MEDIA: lines and returns file paths.
// The MEDIA: protocol is used by OpenClaw skills to attach files to chat messages.
func extractMediaPaths(output string) []string {
	var paths []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "MEDIA:") {
			path := strings.TrimSpace(strings.TrimPrefix(line, "MEDIA:"))
			if path != "" {
				if _, err := os.Stat(path); err == nil {
					paths = append(paths, path)
				}
			}
		}
	}
	return paths
}

// sendMediaFiles sends extracted MEDIA: files to the outbound bus.
func (a *Agent) sendMediaFiles(msg bus.InboundMessage, mediaPaths []string) {
	if len(mediaPaths) == 0 || a.messageBus == nil {
		return
	}
	outMsg := bus.OutboundMessage{
		Channel:    msg.Channel,
		AccountID:  msg.AccountID,
		ChatID:     msg.ChatID,
		MediaPaths: mediaPaths,
	}
	select {
	case a.messageBus.Outbound <- outMsg:
	default:
		slog.Warn("outbound channel full, dropping media message", "agent", a.name)
	}
}
