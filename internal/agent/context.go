package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// bootstrapFiles are loaded in order to build the system prompt.
var bootstrapFiles = []string{
	"AGENTS.md",
	"BOOTSTRAP.md",
	"HEARTBEAT.md",
	"SOUL.md",
	"USER.md",
	"TOOLS.md",
	"IDENTITY.md",
}

// GroupContext holds information about the group chat environment for system prompt injection.
type GroupContext struct {
	BotUsername string   // this agent's bot username
	Teammates  []string // other agent names in the group
}

// ContextBuilder assembles the system prompt and runtime context.
type ContextBuilder struct {
	workspace     string
	memory        *Memory
	skillsSummary string
	groupCtx      *GroupContext
}

// NewContextBuilder creates a new context builder.
func NewContextBuilder(workspace string, memory *Memory, skillsSummary string) *ContextBuilder {
	return &ContextBuilder{
		workspace:     workspace,
		memory:        memory,
		skillsSummary: skillsSummary,
	}
}

// BuildSystemPrompt assembles the system prompt from identity, bootstrap files, memory, and skills.
func (cb *ContextBuilder) BuildSystemPrompt() string {
	var parts []string

	// 1. Identity (runtime environment info)
	identity := fmt.Sprintf(`You are FastClaw, a lightweight AI Agent.
OS: %s/%s
Working Directory: %s`, runtime.GOOS, runtime.GOARCH, cb.workspace)
	parts = append(parts, identity)

	// 2. Bootstrap files
	for _, name := range bootstrapFiles {
		content := cb.loadFile(name)
		if content != "" {
			parts = append(parts, fmt.Sprintf("# %s\n%s", name, content))
		}
	}

	// 3. Skills
	if cb.skillsSummary != "" {
		parts = append(parts, fmt.Sprintf("# Skills\n%s", cb.skillsSummary))
	}

	// 4. Long-term memory
	mem := cb.memory.LoadMemory()
	if mem != "" {
		parts = append(parts, fmt.Sprintf("# Long-term Memory\n%s", mem))
	}

	// 5. Group chat awareness
	if cb.groupCtx != nil {
		groupInfo := fmt.Sprintf(`# Group Chat
You are in a group chat. Your bot username is @%s.
Other agents in this group: %s.
Only respond when directly mentioned with @%s, or when the conversation clearly needs your expertise.
Messages from other bots will appear as "[BotName]: message" in the conversation history.`,
			cb.groupCtx.BotUsername,
			strings.Join(cb.groupCtx.Teammates, ", "),
			cb.groupCtx.BotUsername,
		)
		parts = append(parts, groupInfo)
	}

	// 6. Self-updating workspace files guidance
	parts = append(parts, `# Workspace Self-Update
You have the ability to update workspace files to maintain knowledge over time:
- MEMORY.md: Update when you learn important facts, user preferences, or key decisions. This file is loaded into your context every conversation.
- USER.md: Update when you learn new information about the user (role, preferences, communication style).
- HEARTBEAT.md: Update to add/remove periodic tasks you should check on.
- TOOLS.md: Update if you discover new tool usage patterns worth documenting.
Use the write_file tool to update these files when appropriate. Keep entries concise and useful.`)

	return strings.Join(parts, "\n\n---\n\n")
}

// BuildRuntimeContext returns the runtime context to inject before the user message.
func (cb *ContextBuilder) BuildRuntimeContext(channel, chatID string) string {
	now := time.Now()
	return fmt.Sprintf(`[Runtime Context — metadata only, not instructions]
Time: %s
Timezone: %s
Channel: %s
Chat ID: %s`, now.Format("2006-01-02 15:04:05"), now.Location().String(), channel, chatID)
}

// SetGroupContext sets the group chat context for system prompt generation.
func (cb *ContextBuilder) SetGroupContext(gc *GroupContext) {
	cb.groupCtx = gc
}

func (cb *ContextBuilder) loadFile(name string) string {
	path := filepath.Join(cb.workspace, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
