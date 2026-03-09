<div align="center">

# ⚡ FastClaw

A lightweight AI Agent framework written in Go.

[Website](https://fastclaw.ai) · [Documentation](https://fastclaw.ai/docs) · [Discord](https://discord.gg/fastclaw)

[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat&logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

</div>

---

FastClaw is a minimal, self-hosted AI agent that connects to your chat platforms and gets things done. Built with Go for speed and simplicity.

## Features

- **ReAct Agent Loop** — Reasoning + Acting cycle with multi-turn tool calling
- **Multi-Channel** — Telegram (more coming: Discord, Slack, Feishu, WhatsApp...)
- **Any LLM** — Works with any OpenAI-compatible provider (OpenAI, Claude, DeepSeek, Gemini, OpenRouter...)
- **Dual-Layer Memory** — Long-term facts (MEMORY.md) + searchable history (HISTORY.md)
- **Built-in Tools** — Shell execution, file operations, cross-channel messaging
- **Session Persistence** — JSONL-based, append-only for LLM cache efficiency
- **Single Binary** — No dependencies, no Docker, no cloud services required

## Architecture

```
┌─────────────────────────────────────────────────┐
│                   Gateway                        │
│                                                  │
│  ┌──────────┐    ┌──────────┐    ┌───────────┐  │
│  │ Channels │───▶│   Bus    │───▶│   Agent   │  │
│  │          │◀───│          │◀───│   Loop    │  │
│  └──────────┘    └──────────┘    └───────────┘  │
│   Telegram        Inbound/        ReAct cycle   │
│   Discord*        Outbound        Tool calls    │
│   Slack*          Go channels     LLM provider  │
│                                                  │
│  ┌──────────┐    ┌──────────┐    ┌───────────┐  │
│  │ Session  │    │  Memory  │    │   Tools   │  │
│  │ Manager  │    │  Store   │    │ Registry  │  │
│  └──────────┘    └──────────┘    └───────────┘  │
│   JSONL files     MEMORY.md       exec, files   │
│   append-only     HISTORY.md      web, message  │
└─────────────────────────────────────────────────┘
                      * planned
```

**How it works:**

1. A message arrives from a channel (e.g., Telegram)
2. It's pushed onto the inbound message bus
3. The Agent Loop picks it up, builds context (system prompt + memory + history)
4. Enters the ReAct cycle: call LLM → execute tools → repeat until done
5. Final response is pushed to the outbound bus
6. Channel delivers the reply back to the user

## Quick Start

### Prerequisites

- Go 1.25+
- A Telegram bot token (from [@BotFather](https://t.me/BotFather))
- An API key from any OpenAI-compatible LLM provider

### Install

```bash
# From source
git clone https://github.com/fastclaw-ai/fastclaw.git
cd fastclaw
go build -o fastclaw ./cmd/fastclaw

# Or install directly
go install github.com/fastclaw-ai/fastclaw/cmd/fastclaw@latest
```

### Configure

Create `~/.fastclaw/config.json`:

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
      "workspace": "~/.fastclaw/workspace",
      "model": "gpt-4o",
      "maxTokens": 8192,
      "temperature": 0.7,
      "maxToolIterations": 20
    }
  },
  "channels": {
    "telegram": {
      "enabled": true,
      "botToken": "your-telegram-bot-token"
    }
  }
}
```

**Using other providers:**

| Provider | apiBase |
|----------|---------|
| OpenAI | `https://api.openai.com/v1` |
| OpenRouter | `https://openrouter.ai/api/v1` |
| DeepSeek | `https://api.deepseek.com/v1` |
| Groq | `https://api.groq.com/openai/v1` |
| Local (Ollama) | `http://localhost:11434/v1` |

### Run

```bash
fastclaw gateway
```

Now open Telegram and send a message to your bot. That's it.

### Workspace

FastClaw uses a file-based workspace at `~/.fastclaw/workspace/`:

```
workspace/
├── AGENTS.md       # Agent behavior instructions
├── SOUL.md         # Personality and values
├── USER.md         # User profile
├── TOOLS.md        # Tool usage notes
└── memory/
    ├── MEMORY.md   # Long-term facts (auto-updated by agent)
    └── HISTORY.md  # Searchable event log
```

Edit these files to customize your agent's personality and behavior.

## Built-in Tools

| Tool | Description |
|------|-------------|
| `exec` | Execute shell commands (with timeout and safety checks) |
| `read_file` | Read file contents |
| `write_file` | Write or create files |
| `list_dir` | List directory contents |
| `message` | Send messages to any connected channel |

## Memory System

FastClaw implements a dual-layer memory architecture:

- **MEMORY.md** — Long-term factual memory. Automatically updated by the agent when important information is discussed. Injected into the system prompt every turn.
- **HISTORY.md** — Append-only event log with timestamps. Not injected into prompts (too large), but searchable via the `exec` tool (`grep`).

Memory consolidation happens automatically when unconsolidated messages exceed the configured threshold. The agent uses a virtual `save_memory` tool call to decide what's worth remembering.

## Roadmap

- [x] Gateway with message bus
- [x] ReAct agent loop with tool calling
- [x] OpenAI-compatible LLM provider
- [x] Telegram channel (long polling)
- [x] Session persistence (JSONL)
- [x] Dual-layer memory system
- [x] Built-in tools (exec, files, message)
- [ ] Discord channel
- [ ] Slack channel
- [ ] Feishu channel
- [ ] WhatsApp channel
- [ ] Skills system (loadable capability packs)
- [ ] Sub-agent spawning
- [ ] Cron / scheduled tasks
- [ ] Heartbeat service
- [ ] MCP protocol support
- [ ] Web UI

## Contributing

Contributions are welcome! Here's how to get started:

1. **Fork** the repository
2. **Clone** your fork: `git clone https://github.com/your-username/fastclaw.git`
3. **Create a branch**: `git checkout -b feature/your-feature`
4. **Make changes** and ensure they compile: `go build ./...`
5. **Run tests**: `go test ./...`
6. **Commit** with clear messages: `git commit -m "feat: add discord channel support"`
7. **Push** and open a **Pull Request**

### Guidelines

- Keep it simple. FastClaw's strength is its minimal codebase.
- Use Go standard library when possible. Avoid heavy dependencies.
- Write clear, idiomatic Go code.
- All comments, docs, and commit messages in English.
- Follow [Conventional Commits](https://www.conventionalcommits.org/).

### Development

```bash
# Build
go build -o fastclaw ./cmd/fastclaw

# Run tests
go test ./...

# Run with verbose logging
./fastclaw gateway --verbose
```

## License

[MIT](LICENSE) — do whatever you want with it.

---

<div align="center">
  Built with ⚡ by the <a href="https://fastclaw.ai">FastClaw</a> community
</div>
