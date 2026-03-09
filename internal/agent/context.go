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
	"SOUL.md",
	"USER.md",
	"TOOLS.md",
	"IDENTITY.md",
}

// ContextBuilder assembles the system prompt and runtime context.
type ContextBuilder struct {
	workspace string
	memory    *Memory
}

// NewContextBuilder creates a new context builder.
func NewContextBuilder(workspace string, memory *Memory) *ContextBuilder {
	return &ContextBuilder{
		workspace: workspace,
		memory:    memory,
	}
}

// BuildSystemPrompt assembles the system prompt from identity, bootstrap files, and memory.
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

	// 3. Long-term memory
	mem := cb.memory.LoadMemory()
	if mem != "" {
		parts = append(parts, fmt.Sprintf("# Long-term Memory\n%s", mem))
	}

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

func (cb *ContextBuilder) loadFile(name string) string {
	path := filepath.Join(cb.workspace, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
