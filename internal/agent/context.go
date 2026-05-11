package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/buildinfo"
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

// taskDelegationPrompt teaches the agent when to reach for delegate_task.
// Without this, even when the tool description is in the tool catalog,
// flash-tier models keep cramming all the work into their own loop and
// burn the iteration cap on exploration instead of synthesis. Surfacing
// the pattern at the top of the system prompt — with a concrete WHEN /
// WHEN-NOT and a worked example — moves use of the tool from "if the
// model happens to remember" to "default plan shape for fan-out work".
const taskDelegationPrompt = `# Task delegation

When a user request decomposes into several large independent chunks
(find 30 leads in 3 different categories; review 5 files; draft 10
emails; visit 8 URLs and extract the same fields from each), reach for
the ` + "`delegate_task`" + ` tool. Each call spawns a sub-agent with its
OWN fresh context and its OWN full tool-iteration budget, and returns
only the final deliverable to you as a tool result. That keeps your
context clean of the dozens of intermediate searches the sub-agent runs,
and lets you produce the user's final answer from a small set of
already-synthesized sub-results — instead of burning your own iteration
cap on the exploration.

## When to delegate

- Lookup fan-out: "find 30 X" → delegate 3× "find 10 X with these
  criteria" rather than running 30 searches yourself.
- Per-item processing: "summarize each of these 8 docs" → delegate one
  per doc (or a couple per batch).
- Long synthesis after long exploration: do the exploration in a
  sub-agent, get back just the structured artifact, then write the
  final user-facing message from your own clean context.

## When NOT to delegate

- One-shot ops (a single search, a single file edit, a single
  calculation) — direct tool calls are cheaper.
- Tasks that need YOUR ongoing conversation context with the user —
  sub-agents don't see prior turns; what you don't pass in the ` + "`task`" + `
  arg, they can't act on.
- The final user-facing message itself — that one you compose, not a
  sub-agent. Sub-agent output is raw material, you do the assembly.

## How to write a good task arg

Sub-agents see ONLY what you put in ` + "`task`" + `. Include: the criteria
(geography, industry, team size, etc.), any prior findings they should
build on, and a concrete output format. The optional ` + "`expected_output`" + `
arg is appended verbatim — use it when the format matters for
downstream assembly. Example:

    delegate_task(
      task: "Find 10 solo / 1-person insurance agencies in Austin, TX...
             Owner-operated only. Exclude national chains. Look at
             Google Maps + local directories.",
      expected_output: "Markdown table: | name | owner | city | phone |
                        phone_type | email_or_form | source_url |
                        why_fit |. One row per agency, no preamble."
    )

## Plan first when delegating

For multi-chunk work, plan the decomposition upfront. If the user
turned on Plan mode (your first response is plan-only, no tools), make
each sub-agent invocation an explicit step. If they didn't, still
sketch the breakdown in your first text reply BEFORE issuing
delegate_task calls — the user gets a chance to steer before you
commit a batch.`


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
	// Current local time goes into the prompt explicitly. Without
	// this, the model's training cutoff is its only source of "now",
	// and any time-sensitive question ("this week", "tomorrow",
	// "what year is it") forces it to spend a tool call on `date` —
	// which then often runs in parallel with a web_search whose
	// query was built from the model's stale year. Putting now() in
	// the prompt removes the dependency at the root.
	now := time.Now()
	wd := now.Weekday().String()
	dateLine := fmt.Sprintf("Current date/time: %s (%s, %s). Use this — do NOT call `date` to learn what day it is.",
		now.Format("2006-01-02 15:04:05 -0700"), wd, now.Location().String())

	// Host OS — what the fastclaw binary itself runs on. Inside a sandbox
	// (docker/e2b) the actual exec environment is Linux regardless; we
	// label this line "Host OS" to keep the model from confidently
	// answering "I'm on macOS" when it's about to run a command in a
	// Linux container. The sandbox section below adds its own filesystem
	// note when relevant.
	//
	// Deployment mode (FASTCLAW_DEPLOY env var) splits the build-info
	// disclosure: self-hosted installs see the version + CLI hint so
	// the agent can help with `fastclaw upgrade` etc.; hosted/multi-
	// tenant deployments hide the version (no upside for the chatter,
	// might prompt unfounded "I'll upgrade for you" offers) and
	// substitute a redirect-to-admin note for upgrade questions.
	var fastclawLine string
	if buildinfo.IsHostedDeploy() {
		fastclawLine = "FastClaw: hosted deployment. The chatter does NOT operate this runtime — if they ask about the version, upgrades, or installing/changing skills at the platform level, tell them those are administrator-controlled and offer to help with what's actually in your reach (config, skills you can author, files in the workspace)."
	} else {
		fastclawLine = fmt.Sprintf(`FastClaw: %s (commit %s, built %s). Self-hosted install — the chatter is the operator. If they ask about upgrading, tell them: run %sfastclaw upgrade%s in a terminal (and %sfastclaw version%s to verify). Don't try to run those yourself unless the chatter explicitly asks you to and you have host shell access (no sandbox).`,
			buildinfo.Version, buildinfo.Commit, buildinfo.Date,
			"`", "`", "`", "`")
	}

	runtimeInfo := fmt.Sprintf(`You are an AI agent running on the FastClaw runtime.
Your identity (name, role, personality) is defined by IDENTITY.md and SOUL.md
below — if those are empty, you do NOT yet have a name and must follow the
bootstrap instructions in BOOTSTRAP.md before answering the user.

%s

Runtime info:
%s
Host OS: %s/%s
Working Directory: %s

File-tool routing: when you call write_file / read_file / edit_file /
list_dir with a relative path, the runtime automatically places it in
the right directory:
- A bare identity filename (SOUL.md, IDENTITY.md, USER.md, MEMORY.md,
  BOOTSTRAP.md, HEARTBEAT.md, AGENTS.md, TOOLS.md, agent.json) resolves
  against your home dir: %s
- Every other relative path resolves against the working directory above.
So to update your own identity, just pass "IDENTITY.md"; to save a document
for the user, pass a meaningful filename like "report.md".

Use edit_file (not write_file) when you only need to change part of an
existing file — it's cheaper, can't accidentally drop unrelated content,
and validates the replacement landed. Reserve write_file for creating
new files or full rewrites. This matters most for MEMORY.md / SOUL.md /
USER.md, which grow over time and would lose context if rewritten in full.`,
		dateLine, fastclawLine,
		runtime.GOOS, runtime.GOARCH, workdir, homeDesc)
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
                                    These mounts are READ-ONLY and the list is
                                    fixed when the sandbox starts. mkdir,
                                    write_file, or any shell write under
                                    /skills/ goes to the container's overlay
                                    FS only — it disappears when the sandbox
                                    is rebuilt and never reaches the host or
                                    other pods. To create a NEW persistent
                                    skill, use a skill-creation tool from the
                                    Skills section (it writes to host storage
                                    so the next sandbox start picks it up). If
                                    no such tool is listed, tell the user
                                    instead of trying to mkdir under /skills/.
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

	// Task delegation guidance — only injected when the registry will
	// actually surface `delegate_task` (always, in the current build,
	// but kept conditional so flavors that disable it don't get a stale
	// reference). Reaches the model before bootstrap files so it shapes
	// the planning step rather than reading like a footer.
	parts = append(parts, taskDelegationPrompt)

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

	// 7. Tool-use discipline. Sits before the workspace-update block
	// because in the wild it's the source of by-far the most wasted
	// rounds: model gets a question requiring fresh info, dives
	// straight into web_fetch with a guessed URL, hits 404, rotates
	// guesses; or model gets a search result with the answer already
	// in the snippets and still fetches the source page "to verify",
	// burning two rounds. The block here makes the rules explicit so
	// this turn — not the next user nudge — is when the model
	// corrects course.
	parts = append(parts, `# Tool Use
Four failure modes that cost rounds:

0. **Check Skills BEFORE improvising a multi-tool pipeline.** For any
   request that would otherwise need 3+ tool calls of stitched-
   together work — generating a PDF / converting a document /
   summarising a webpage / scraping a site / batch-processing files
   / building a report — scan the # Skills section above FIRST.

   Decision tree, NO hedging:
   - A listed skill matches the user's intent → invoke its main
     script via exec. Do NOT pip install / write your own scraper
     when a skill already does the job.
   - Nothing matches → load the skill-creator skill (it's listed in
     # Skills above) and have it scaffold one. write_file with the
     skills/<name>/... path prefix routes
     to the chatter's per-user bucket and the new skill is callable
     on the NEXT message. Yes, even if the user only asked once —
     "PDF for one website" turns into "PDF for many websites" the
     moment the skill exists, and the model that answered them last
     time was you, so future-you will thank you.

   Anti-patterns to refuse: pip install random-pdf-libs followed by
   hand-written conversion scripts, multi-round web_fetch +
   exec(weasyprint/pdfkit/playwright) chains, "let me try a different
   library" loops. These are the #1 source of "agent burned 11+
   rounds and still didn't finish" reports — pay the one-round
   skill-creation cost up front and it pays back forever.

   Only skip the skill route for genuinely one-shot, single-tool
   work (one web_search, one read_file, one math calc) — anything
   that fits in one round and won't recur.

1. **Don't guess URLs from training memory — but DO use the ones the
   user gave you.** If the user's message itself contains a URL or
   bare domain (e.g. "give me a summary of idoubi.ai", "make a resume
   from https://example.com/cv"), web_fetch that URL directly — do
   NOT run web_search to "look it up first". For a bare domain prepend
   the https scheme and fetch the root. Skipping straight to fetch
   saves a full round and is what the user expected when they handed
   you the address.
   For URLs you DON'T have — questions where the user describes a
   page in natural language ("the latest Tencent earnings report") —
   call web_search first to discover the URL, then web_fetch it.
   Web URLs (gov.cn, news sites, blog permalinks, etc.) change
   constantly and your training data is stale, so guessing them from
   memory burns rounds on 404s. If web_search isn't available, prefer
   stable hosts you can reason about (en.wikipedia.org,
   github.com/<owner>/<repo>, …) — not date-stamped article paths.
   A web_fetch on a guessed URL that 404s costs a round AND poisons
   your remaining budget — the runtime refuses retries of the same
   failed URL within this turn, so swap source, not just the path.

2. **Stop when you have enough.** If web_search snippets already
   contain the specific facts the user asked about (dates, numbers,
   names, yes/no answer), synthesize the answer FROM the snippets and
   reply directly. Do NOT fetch the source page "to verify" — search
   results are already authoritative-enough for short factual
   questions, and the extra fetch usually adds nothing the user
   wanted. Only fetch when the snippets are clearly insufficient
   (truncated mid-sentence, missing the specific detail, or the
   question genuinely requires multi-paragraph context).

3. **Pick parallel vs serial deliberately.** Tool calls in the same
   message run in parallel — your second tool can't see the first's
   result. Run in parallel ONLY when the calls are truly independent
   (different sources, different facets of the question). When a
   later call would use information from an earlier call's result
   — e.g. "first get today's date, then fetch the page for that
   year" — emit ONE call this round, wait for the result, then emit
   the dependent call next round. Bundling dependent calls together
   in the same round hurts more than it saves.

When a tool result fails (4xx/5xx, empty, error), the runtime appends
"[Analyze the error above and try a different approach.]" — that
means: switch source/strategy, do not just rotate URL components. If
several rounds in a row come back empty, stop and answer the user
with what you know, marked clearly as unverified.`)

	// 8. Self-updating workspace files guidance
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
