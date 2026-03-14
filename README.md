<div align="center">

# ⚡ FastClaw

A lightweight, self-hosted AI Agent framework written in Go.

[Website](https://fastclaw.ai) · [Documentation](https://fastclaw.ai/docs) · [Discord](https://discord.gg/fastclaw)

[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat&logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![GitHub Stars](https://img.shields.io/github/stars/fastclaw-ai/fastclaw?style=flat)](https://github.com/fastclaw-ai/fastclaw)

</div>

---

FastClaw is a minimal, self-hosted AI agent that connects to your chat platforms and gets things done. It runs on your own machine, uses any LLM provider, and learns about you over time. Built with Go for speed and simplicity — single binary, zero dependencies.

## ✨ Features

- **ReAct Agent Loop** — Reasoning + Acting cycle with multi-turn tool calling
- **Multi-Channel** — Telegram with multi-bot support (more coming: Discord, Slack, WhatsApp...)
- **Any LLM** — Works with any OpenAI-compatible provider (OpenAI, Claude, DeepSeek, Gemini, OpenRouter...)
- **Context Engineering** — Auto-pruning & compression to handle long conversations without context overflow
- **Dual-Layer Memory** — Long-term facts (MEMORY.md) + searchable conversation history with recency weighting
- **Hook System** — Before/After hooks for system prompt, model calls, and tool calls
- **CronTab + Heartbeat** — Scheduled tasks and periodic wake-ups for proactive behavior
- **Skill System** — Progressive disclosure: 100+ skills without bloating context
- **Built-in Tools** — Shell, file ops, web fetch, memory search, cross-channel messaging
- **MCP Support** — Connect external tools via Model Context Protocol (HTTP + stdio)
- **Multi-Agent** — Run multiple agents with independent personalities and workspaces
- **Single Binary** — No Docker, no cloud services, no dependencies required

## 🏗 Architecture

```
┌──────────────────────────────────────────────────────────┐
│                       Gateway                             │
│                                                           │
│  ┌───────────┐   ┌──────────┐   ┌──────────────────────┐ │
│  │ Channels  │──▶│   Bus    │──▶│     Agent Loop       │ │
│  │           │◀──│          │◀──│                      │ │
│  │ Telegram  │   │ Inbound/ │   │  System Prompt Build │ │
│  │ Discord*  │   │ Outbound │   │  ReAct Cycle         │ │
│  │ Slack*    │   │          │   │  Tool Execution      │ │
│  └───────────┘   └──────────┘   │  Context Compaction  │ │
│                                  └──────────────────────┘ │
│                                                           │
│  ┌───────────┐   ┌──────────┐   ┌──────────────────────┐ │
│  │  Session  │   │  Memory  │   │       Tools          │ │
│  │  Manager  │   │          │   │                      │ │
│  │           │   │ MEMORY.md│   │ exec, files, web     │ │
│  │ JSONL     │   │ Logs/    │   │ memory_search        │ │
│  │ Compaction│   │ Search   │   │ load_skill, message  │ │
│  └───────────┘   └──────────┘   │ MCP tools            │ │
│                                  └──────────────────────┘ │
│                                                           │
│  ┌───────────┐   ┌──────────┐   ┌──────────────────────┐ │
│  │   Hooks   │   │   Cron   │   │     Heartbeat        │ │
│  │           │   │ Scheduler│   │   (every 30 min)     │ │
│  │ Pre/Post  │   │          │   │                      │ │
│  │ Logging   │   │ Exact    │   │ Check task list      │ │
│  │ Timing    │   │ Interval │   │ Update memory        │ │
│  └───────────┘   │ Cron Expr│   │ Proactive actions    │ │
│                   └──────────┘   └──────────────────────┘ │
└──────────────────────────────────────────────────────────┘
                        * planned
```

## 🚀 Quick Start

### Install (one-liner)

```bash
curl -fsSL https://raw.githubusercontent.com/fastclaw-ai/fastclaw/main/install.sh | bash
```

Or with Go:

```bash
go install github.com/fastclaw-ai/fastclaw/cmd/fastclaw@latest
```

Or download a pre-built binary from [Releases](https://github.com/fastclaw-ai/fastclaw/releases).

### Configure

Create `~/.fastclaw/fastclaw.json`:

```json
{
  "providers": {
    "openai": {
      "apiKey": "your-api-key",
      "apiBase": "https://api.openai.com/v1"
    }
  },
  "agents": {
    "defaults": {
      "model": "gpt-4o",
      "maxTokens": 8192,
      "temperature": 0.7,
      "maxToolIterations": 20
    },
    "list": [
      { "id": "main", "workspace": "~/.fastclaw/agents/main/agent" }
    ]
  },
  "channels": {
    "telegram": {
      "enabled": true,
      "accounts": {
        "default": {
          "botToken": "your-telegram-bot-token"
        }
      }
    }
  }
}
```

**Supported LLM providers:**

| Provider | apiBase | Example Model |
|----------|---------|---------------|
| OpenAI | `https://api.openai.com/v1` | `gpt-4o` |
| Anthropic (via OpenRouter) | `https://openrouter.ai/api/v1` | `anthropic/claude-sonnet-4` |
| DeepSeek | `https://api.deepseek.com/v1` | `deepseek-chat` |
| Groq | `https://api.groq.com/openai/v1` | `llama-3.3-70b` |
| Local (Ollama) | `http://localhost:11434/v1` | `qwen2.5:32b` |

### Run

```bash
fastclaw gateway
```

Open Telegram and message your bot. That's it. ⚡

## 📁 Workspace

Each agent has its own workspace with Markdown-based configuration:

```
workspace/
├── AGENTS.md       # Agent behavior instructions & SOP
├── SOUL.md         # Personality, values, communication style
├── IDENTITY.md     # Name, role, specialty
├── USER.md         # User profile (auto-updated as agent learns about you)
├── TOOLS.md        # Tool usage notes & environment specifics
├── MEMORY.md       # Long-term facts (auto-updated by heartbeat)
├── HEARTBEAT.md    # Periodic task checklist
├── memory/
│   └── logs/       # Compressed conversation history (searchable)
├── sessions/       # JSONL conversation files
└── skills/         # Agent-specific skills
```

Edit these files to customize your agent. The agent can also update `USER.md` and `MEMORY.md` on its own as it learns.

## 🔧 Built-in Tools

| Tool | Description |
|------|-------------|
| `exec` | Execute shell commands with timeout |
| `read_file` | Read file contents |
| `write_file` | Write or create files |
| `list_dir` | List directory contents |
| `web_fetch` | Fetch web pages, strip HTML, return text |
| `memory_search` | Search conversation history with keyword matching |
| `load_skill` | Load full skill instructions on demand |
| `message` | Send messages to any connected channel |

Plus any tools connected via **MCP** (Model Context Protocol).

## 🧠 Memory System

FastClaw implements a dual-layer memory architecture:

**Layer 1: MEMORY.md** — Core facts auto-extracted during heartbeat. Loaded into every system prompt. The agent knows your preferences, important dates, ongoing projects.

**Layer 2: Memory Search** — Full conversation history stored as logs. Searchable via `memory_search` tool with keyword matching and time-decay weighting. The agent can recall details from hundreds of conversations ago.

## ⏰ Proactive Behavior

FastClaw doesn't just wait for you — it comes to you.

**CronTab** — Schedule tasks in `cron.json`:
- Exact time: `"2026-03-15T08:00:00"`
- Interval: `"every 20m"`
- Cron expression: `"0 8 * * 1-5"` (weekdays at 8am)

**Heartbeat** — Wakes every 30 minutes to check `HEARTBEAT.md`. If something needs attention (a reminder, a birthday, a follow-up), it acts proactively.

## 🎯 Skills

Skills are plug-and-play capability packs. Install them in `~/.fastclaw/skills/`:

```
~/.fastclaw/skills/
├── skill-creator/
│   └── SKILL.md
├── weather/
│   └── SKILL.md
└── translator/
    └── SKILL.md
```

**Progressive disclosure**: Only skill names + one-line summaries go into the system prompt. Full instructions are loaded on-demand via `load_skill` — so 100+ skills won't blow up your context.

## 🤖 Multi-Agent

Run multiple agents with different personalities on the same gateway:

```json
{
  "agents": {
    "list": [
      { "id": "mike", "workspace": "~/.fastclaw/agents/mike/agent" },
      { "id": "mary", "workspace": "~/.fastclaw/agents/mary/agent" }
    ]
  },
  "channels": {
    "telegram": {
      "accounts": {
        "mike": { "botToken": "MIKE_BOT_TOKEN" },
        "mary": { "botToken": "MARY_BOT_TOKEN" }
      }
    }
  },
  "bindings": [
    { "agentId": "mike", "match": { "channel": "telegram", "accountId": "mike" } },
    { "agentId": "mary", "match": { "channel": "telegram", "accountId": "mary" } }
  ]
}
```

Each agent has its own workspace, personality, memory, and skills.

## 📋 Roadmap

- [x] Gateway with message bus
- [x] ReAct agent loop with tool calling
- [x] OpenAI-compatible LLM provider (streaming SSE)
- [x] Telegram channel (multi-bot, groups, DMs)
- [x] Session persistence (JSONL)
- [x] Dual-layer memory system
- [x] Context pruning & compression
- [x] Hook system (pre/post for prompts, model, tools)
- [x] CronTab scheduled tasks
- [x] Heartbeat proactive service
- [x] Skill system with progressive disclosure
- [x] MCP protocol support (HTTP + stdio)
- [x] Web fetch tool
- [x] Multi-agent routing with bindings
- [ ] Discord channel
- [ ] Slack channel
- [ ] WhatsApp channel
- [ ] Vector-based memory search (SQLite + embeddings)
- [ ] Web dashboard
- [ ] Plugin system

## 🛠 Development

```bash
# Clone
git clone https://github.com/fastclaw-ai/fastclaw.git
cd fastclaw

# Build
go build -o fastclaw ./cmd/fastclaw

# Run tests
go test ./...

# Run
./fastclaw gateway
```

## Contributing

Contributions welcome! Keep it simple — FastClaw's strength is its minimal codebase.

1. Fork → Clone → Branch → Code → Test → PR
2. Follow [Conventional Commits](https://www.conventionalcommits.org/)
3. Use Go standard library when possible

## License

[MIT](LICENSE)

---

<div align="center">
  Built with ⚡ by the <a href="https://fastclaw.ai">FastClaw</a> community
</div>
