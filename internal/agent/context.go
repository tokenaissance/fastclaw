package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/config"
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
	home           string // agent's home: SOUL.md, IDENTITY.md, memory, sessions
	workspace      string // working dir where agent creates user-facing files
	memory         *Memory
	skillsSummary  string
	groupCtx       *GroupContext
	thinking       string // off, low, medium, high, adaptive
	sandboxEnabled bool
	sandboxBackend string
	store   MemoryStore
	userID  string
	agentID string
}

// ctx returns a context tagged with this builder's user, used when reading
// identity files (SOUL/IDENTITY/USER/...) from a store-backed setup so the
// SQL row scope matches per-(user, agent).
func (cb *ContextBuilder) ctx() context.Context {
	if cb.userID == "" {
		return context.Background()
	}
	return config.WithUserID(context.Background(), cb.userID)
}

// NewContextBuilder creates a new context builder.
func NewContextBuilder(home string, memory *Memory, skillsSummary string) *ContextBuilder {
	return &ContextBuilder{
		home:          home,
		memory:        memory,
		skillsSummary: skillsSummary,
	}
}

// SetWorkspace attaches the working directory for user-facing output. When
// set, the system prompt advertises it as "Working Directory" and keeps it
// distinct from the agent's home (identity) dir.
func (cb *ContextBuilder) SetWorkspace(p string) { cb.workspace = p }

// SetSkillsSummary updates the skills summary baked into the system prompt.
// Called from refreshSkillsFromStore so skills hydrated from the object
// store at turn start end up visible to the model without rebuilding the
// whole context builder.
func (cb *ContextBuilder) SetSkillsSummary(s string) { cb.skillsSummary = s }

// BuildSystemPrompt assembles the system prompt from identity, bootstrap files, memory, and skills.
func (cb *ContextBuilder) BuildSystemPrompt() string {
	var parts []string

	// 1. Runtime environment info. Deliberately NOT an identity claim —
	// the agent's name, role, and persona live in IDENTITY.md / SOUL.md.
	// A fresh agent has empty identity files and should follow BOOTSTRAP.md
	// to ask the user what identity to adopt, instead of introducing itself
	// as "FastClaw" (which is the runtime, not the agent).
	//
	// When the agent has a sandbox attached, every exec call runs INSIDE
	// the container — host paths don't exist there. Sandbox bind-mounts:
	//   <host workspace>  → /workspace
	//   <host skills/x>   → /skills/x  (read-only, one mount per skill)
	// We tell the LLM about the sandbox-side paths only, otherwise it
	// hallucinates `cd /Users/...` commands that fail with "No such file".
	var workdir, homeDesc string
	if cb.sandboxEnabled {
		workdir = "/workspace"
		homeDesc = "/workspace (identity files like SOUL.md / IDENTITY.md are managed by the runtime, not the sandbox FS — call write_file with a bare filename, never path it)"
	} else {
		workdir = cb.workspace
		if workdir == "" {
			workdir = cb.home
		}
		homeDesc = cb.home
	}
	runtimeInfo := fmt.Sprintf(`You are an AI agent running on the FastClaw runtime.
Your identity (name, role, personality) is defined by IDENTITY.md and SOUL.md
below — if those are empty, you do NOT yet have a name and must follow the
bootstrap instructions in BOOTSTRAP.md before answering the user.

Runtime info:
OS: %s/%s
Working Directory: %s

File-tool routing: when you call write_file / read_file / list_dir with a
relative path, the runtime automatically places it in the right directory:
- A bare identity filename (SOUL.md, IDENTITY.md, USER.md, MEMORY.md,
  BOOTSTRAP.md, HEARTBEAT.md, AGENTS.md, TOOLS.md, agent.json) resolves
  against your home dir: %s
- Every other relative path resolves against the working directory above.
So to update your own identity, just pass "IDENTITY.md"; to save a document
for the user, pass a meaningful filename like "report.md".`, runtime.GOOS, runtime.GOARCH, workdir, homeDesc)
	parts = append(parts, runtimeInfo)

	// 2. Sandbox capabilities (auto-injected when sandbox is enabled)
	if cb.sandboxEnabled {
		sandboxPrompt := `# Code Execution Environment
You have access to a sandbox environment for executing code. Key rules:
- When the user asks you to write a script, calculate something, or process data, **always execute it immediately** using the exec tool. Do NOT just show code.
- Python 3 is available. Use it for calculations, data processing, web scraping, etc.
- You can write files, read files, and list directories in the sandbox.
- Only show code without executing when the user explicitly asks to "just show" or "just write" the code.
- Always show the execution output/result to the user.

## Filesystem layout INSIDE the sandbox
- /workspace                      ← your working dir (cd here, save outputs here)
- /skills/<skill-name>/           ← every skill listed below is mounted here read-only.
                                    Invoke with: python /skills/<name>/main.py
- Host paths (anything starting with /Users/, /home/, /var/, etc.) DO NOT EXIST in the sandbox. Never reference them.

## Shell quirks
The exec tool runs commands through /bin/sh, NOT bash. Specifically:
- ` + "`" + `<<<` + "`" + ` (here-string) is NOT supported. Use a pipe instead:
    echo '{"prompt":"..."}' | python /skills/generate-image/main.py
- ` + "`" + `[[ ... ]]` + "`" + ` is NOT supported. Use ` + "`" + `[ ... ]` + "`" + ` (POSIX test).
- Process substitution ` + "`" + `<(...)` + "`" + ` is NOT supported. Use a temp file.

## Delivering Files to the User
When the user asks you to create a file (document, script, data, etc.):
- For **text files** (md, txt, csv, json, py, etc.): output the full content directly in your reply using a code block. The user can copy it.
- For **binary files written to /workspace/** (images, pdf, zip, etc.):
  reference them by path with markdown — **never** inline base64. The
  runtime resolves /workspace/<file> paths into actual uploads for
  whatever channel the user is on (Telegram, web UI, etc.). Examples:
    ![generated logo](/workspace/logo.png)
    [download report.pdf](/workspace/report.pdf)
- NEVER fabricate or hand-construct data:image/...;base64,... URLs.
  You don't have access to the actual bytes from inside your reply,
  and made-up base64 (with placeholders, ellipses, or partial data)
  shows up as garbage in the chat. Always reference the real file
  path that the tool returned in its "file" field.
- NEVER just say "file saved" without showing content or referencing
  the workspace path.

## Important: Multi-line Scripts
For multi-line code, ALWAYS use write_file first, then exec:
  1. write_file(path="/tmp/script.py", content="...your code...")
  2. exec(command="python3 /tmp/script.py")
NEVER put multi-line Python in a single exec command — it will fail.

## Package Installation
The sandbox may not have all packages. Install before use:
  exec(command="pip install -q pillow matplotlib requests")

## Visual/Graphics Tasks
The sandbox is a **headless** environment (no display). For visual tasks:
- **Drawing/charts/plots**: Use matplotlib with Agg backend.
- **Image generation/manipulation**: Use PIL/Pillow. Install first: pip install -q pillow
- **NEVER use turtle, tkinter, pygame or any GUI library** — they will fail.
- Save the image to **/workspace/** (NOT /tmp/) and reference it by
  path — the runtime takes care of delivering the file to whatever
  channel the user is on. Do NOT base64-inline the bytes into your
  reply.

Example (write to file then exec):
  write_file(path="/tmp/draw.py", content="""
import subprocess
subprocess.check_call(["pip", "install", "-q", "pillow"])
from PIL import Image, ImageDraw
img = Image.new('RGB', (400, 300), 'white')
draw = ImageDraw.Draw(img)
draw.ellipse([100, 50, 300, 250], fill='pink', outline='black')
img.save('/workspace/output.png')
print('done')
""")
  exec(command="python3 /tmp/draw.py")
Then in your final reply, write: ![](/workspace/output.png)`
		if cb.sandboxBackend == "e2b" {
			sandboxPrompt += "\n- The sandbox is a cloud-hosted E2B environment with network access."
		} else {
			sandboxPrompt += "\n- The sandbox is a Docker container."
		}
		parts = append(parts, sandboxPrompt)
	}

	// 3. Bootstrap files
	for _, name := range bootstrapFiles {
		content := cb.loadFile(name)
		if content != "" {
			parts = append(parts, fmt.Sprintf("# %s\n%s", name, content))
		}
	}

	// 4. Skills
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

	// 6. Thinking/Reasoning mode
	if cb.thinking != "" && cb.thinking != "off" {
		thinkingPrompt := cb.buildThinkingPrompt()
		if thinkingPrompt != "" {
			parts = append(parts, thinkingPrompt)
		}
	}

	// 7. Self-updating workspace files guidance
	parts = append(parts, `# Workspace Self-Update
You have the ability to update workspace files to maintain knowledge over time:
- MEMORY.md: Update when you learn important facts, user preferences, or key decisions. This file is loaded into your context every conversation.
- USER.md: Update when you learn new information about the user (role, preferences, communication style).
- HEARTBEAT.md: Conditional self-checks reviewed at every heartbeat tick (e.g. "if MEMORY.md exceeds 500 lines, compress it"). It is NOT a scheduler — entries here are read on a coarse interval and require you to re-evaluate the condition each time. Do not put time-bound reminders here.
- TOOLS.md: Update if you discover new tool usage patterns worth documenting.
Use the write_file tool to update these files when appropriate. Keep entries concise and useful.

# Scheduling Time-Bound Tasks
When the user asks you to do something at a specific moment, after a delay, or on a recurring schedule (e.g. "5 分钟后提醒我", "每天 9 点", "every Monday morning"), call the create_cron_job tool. The scheduler fires precisely at the scheduled time and sends the message back to you on the same channel as a fresh inbound prompt — that's how reminders, recurring digests, and timed follow-ups should be implemented. NEVER write timed reminders into HEARTBEAT.md: that file is reviewed only on a coarse heartbeat tick and is wrong for any short-fuse or precise-timing request.`)

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

// SetThinking configures the thinking/reasoning level.
func (cb *ContextBuilder) SetThinking(level string) {
	cb.thinking = level
}

func (cb *ContextBuilder) buildThinkingPrompt() string {
	var depth string
	switch cb.thinking {
	case "low":
		depth = "briefly reason through"
	case "medium":
		depth = "think step-by-step through"
	case "high":
		depth = "deeply and thoroughly reason through"
	case "adaptive":
		depth = "adaptively reason through (brief for simple tasks, deep for complex ones)"
	default:
		return ""
	}

	return fmt.Sprintf(`# Thinking Mode
Before responding to each message, %s your approach internally. Consider:
- What is the user really asking for?
- What are the key constraints and edge cases?
- What is the best approach and why?
- Are there any risks or trade-offs to consider?
Structure your reasoning before acting. Think before you respond.`, depth)
}

func (cb *ContextBuilder) loadFile(name string) string {
	// Per-agent only — store row first, FS as legacy fallback for
	// installs that predate the store-primary refactor.
	if cb.store != nil {
		data, err := cb.store.GetWorkspaceFile(cb.ctx(), cb.agentID, cb.userID, name)
		if err == nil && len(data) > 0 {
			return strings.TrimSpace(string(data))
		}
	}
	if cb.home != "" {
		if data, err := os.ReadFile(filepath.Join(cb.home, name)); err == nil && len(data) > 0 {
			return strings.TrimSpace(string(data))
		}
	}
	return ""
}
