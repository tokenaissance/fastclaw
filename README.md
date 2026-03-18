<div align="center">

# ⚡ FastClaw

A lightweight, self-hosted AI Agent framework written in Go.

[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat&logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Release](https://img.shields.io/github/v/release/fastclaw-ai/fastclaw?include_prereleases)](https://github.com/fastclaw-ai/fastclaw/releases)

**Single binary · Any LLM · Multi-channel · Plugin system · Web dashboard**

[Install](#-install) · [Quick Start](#-quick-start) · [Features](#-features) · [Documentation](#-documentation)

</div>

---

## What is FastClaw?

FastClaw is a self-hosted AI agent runtime. It connects your LLM to chat platforms, executes tools, manages memory, and runs scheduled tasks — all from a single Go binary with zero dependencies.

```bash
curl -fsSL https://raw.githubusercontent.com/fastclaw-ai/fastclaw/dev/install.sh | bash
fastclaw    # Opens setup wizard in browser
```

## 📦 Install

**One-liner (macOS / Linux):**

```bash
curl -fsSL https://raw.githubusercontent.com/fastclaw-ai/fastclaw/dev/install.sh | bash
```

**Windows:** Download `.zip` from [Releases](https://github.com/fastclaw-ai/fastclaw/releases), extract, double-click `fastclaw.exe`.

**Already installed?**

```bash
fastclaw upgrade
```

**From source:**

```bash
git clone https://github.com/fastclaw-ai/fastclaw.git
cd fastclaw && make build
```

## 🚀 Quick Start

1. Run `fastclaw` — browser opens the setup wizard at `http://localhost:18953`
2. Pick your LLM provider (OpenAI, OpenRouter, DeepSeek, Groq, Ollama...)
3. Add a Telegram bot token (optional)
4. Click Launch ⚡

That's it. Your agent is live.

## ✨ Features

### Core

| Feature | Description |
|---------|-------------|
| **ReAct Agent Loop** | Multi-turn reasoning + tool calling |
| **Any LLM** | OpenAI-compatible API (OpenAI, Claude, DeepSeek, Gemini, Groq, Ollama, OpenRouter) |
| **Multi-Agent** | Multiple agents with independent personalities, memory, and skills |
| **Context Engineering** | Auto-pruning & LLM compression for long conversations |
| **Dual-Layer Memory** | MEMORY.md (facts) + searchable conversation logs |
| **Hook System** | Before/After hooks on prompts, model calls, tool calls |
| **Hot Reload** | Edit config or SOUL.md → takes effect immediately, no restart |

### Channels

| Channel | Status |
|---------|--------|
| Telegram | ✅ Multi-bot, groups, DMs |
| Discord | ✅ Bot API + Gateway |
| Slack | ✅ Socket Mode |
| Web Chat | ✅ Built-in at /chat |
| Plugin channels | ✅ Add any channel via plugin |

### Tools

| Tool | Description |
|------|-------------|
| `exec` | Shell commands (with optional Docker sandbox) |
| `read_file` / `write_file` / `list_dir` | File operations |
| `web_fetch` | Fetch web pages → markdown |
| `web_search` | Brave Search API |
| `memory_search` | Search conversation history |
| `message` | Send messages to any channel |
| `spawn_subagent` | Delegate tasks to other agents |
| `create_cron_job` / `list_cron_jobs` / `delete_cron_job` | Manage scheduled tasks |
| `load_skill` | Load skill instructions on demand |
| MCP tools | Connect external tools via Model Context Protocol |

### Automation

| Feature | Description |
|---------|-------------|
| **CronTab** | Schedule tasks: cron expressions, intervals, one-time |
| **Heartbeat** | Agent wakes every 30 min to check HEARTBEAT.md |
| **Webhooks** | POST /hooks to trigger agent actions from external systems |
| **Slash Commands** | `/new` `/compact` `/status` `/help` `/version` |

### Security (inspired by [NVIDIA OpenShell](https://github.com/NVIDIA/OpenShell))

| Feature | Description |
|---------|-------------|
| **Sandbox Exec** | Docker-based isolated command execution |
| **Policy Engine** | YAML policies for filesystem, network, tools, resources |
| **Credential Manager** | AES-256-GCM encrypted key storage, env auto-discovery |
| **Tool Loop Detection** | Breaks after 3 identical consecutive calls |

### Platform

| Feature | Description |
|---------|-------------|
| **Web Dashboard** | Full management UI at localhost:18953 |
| **Plugin System** | JSON-RPC subprocess plugins (any language) |
| **Pluggable Storage** | File (default), PostgreSQL, SQLite |
| **OpenAI-Compatible API** | `POST /v1/chat/completions` with SSE streaming |
| **WebSocket Gateway** | OpenClaw-compatible protocol |
| **ChatClaw Integration** | Works as ChatClaw backend runtime |

## 🏗 Architecture

```
                    ┌─────────────────────────────────────────────┐
                    │              FastClaw Gateway                │
                    │                                             │
  Telegram ────────▶│  ┌──────────┐    ┌──────────────────────┐  │
  Discord ─────────▶│  │ Message  │    │    Agent Manager     │  │
  Slack ───────────▶│  │   Bus    │───▶│                      │  │
  Web UI ──────────▶│  │          │◀───│  Agent 1 (Mike)      │  │
  Webhooks ────────▶│  │          │    │  Agent 2 (Mary)      │  │
  Plugins ─────────▶│  └──────────┘    │  Agent N ...         │  │
                    │                   └──────────────────────┘  │
                    │                            │                │
                    │        ┌───────────────────┼──────────┐    │
                    │        ▼                   ▼          ▼    │
                    │  ┌──────────┐  ┌──────────┐  ┌──────────┐ │
                    │  │  Tools   │  │  Memory  │  │ Sessions │ │
                    │  │          │  │          │  │          │ │
                    │  │ exec     │  │MEMORY.md │  │ JSONL    │ │
                    │  │ files    │  │ logs/    │  │ compress │ │
                    │  │ web      │  │ search   │  │ per-chat │ │
                    │  │ MCP      │  │          │  │          │ │
                    │  └──────────┘  └──────────┘  └──────────┘ │
                    │                                             │
                    │  ┌──────────┐  ┌──────────┐  ┌──────────┐ │
                    │  │  Cron    │  │ Plugins  │  │  Policy  │ │
                    │  │ Schedule │  │ JSON-RPC │  │  Engine  │ │
                    │  │ Heartbeat│  │ channels │  │  Sandbox │ │
                    │  │ Webhooks │  │ tools    │  │  Creds   │ │
                    │  └──────────┘  └──────────┘  └──────────┘ │
                    │                                             │
                    │  ┌──────────────────────────────────────┐  │
                    │  │     /v1/chat/completions (SSE)       │  │
                    │  │     /ws (WebSocket)                  │  │
                    │  │     /api/* (REST)                    │  │
                    │  │     Web Dashboard (:18953)           │  │
                    │  └──────────────────────────────────────┘  │
                    └─────────────────────────────────────────────┘
```

## 📁 Agent Workspace

Each agent has its own workspace:

```
~/.fastclaw/agents/mike/agent/
├── SOUL.md         # Personality & communication style
├── IDENTITY.md     # Name, role, specialty
├── AGENTS.md       # Behavior instructions
├── USER.md         # About the user (auto-learns)
├── TOOLS.md        # Tool usage notes
├── MEMORY.md       # Long-term facts (auto-updated)
├── HEARTBEAT.md    # Periodic task checklist
├── policy.yaml     # Security policy (optional)
├── agent.json      # Model & config overrides
├── memory/         # Searchable conversation logs
├── sessions/       # JSONL conversation files
└── skills/         # Agent-specific skills
```

## 🔌 Plugin System

Extend FastClaw with plugins in any language. Plugins communicate via JSON-RPC over stdin/stdout.

```
~/.fastclaw/plugins/feishu/
├── plugin.json     # Manifest: id, type, command
└── plugin.py       # Implementation (Python/Node/Go/...)
```

**Plugin types:** `channel` · `tool` · `provider` · `hook`

```json
{
  "plugins": {
    "enabled": true,
    "entries": {
      "feishu": { "enabled": true, "config": {"appId": "...", "appSecret": "..."} }
    }
  }
}
```

See [examples/plugins/echo/](examples/plugins/echo/) for a complete example.

## 🖥 Web Dashboard

Full management UI at `http://localhost:18953`:

| Page | What you can do |
|------|----------------|
| Overview | Gateway status, stats, quick actions |
| Chat | Talk to your agents in the browser |
| Agents | Create, edit, delete agents; edit SOUL.md |
| Skills | View and manage installed skills |
| Plugins | Enable/disable plugins, edit config |
| Channels | Channel status and configuration |
| Cron Jobs | Create and manage scheduled tasks |
| Settings | Provider, storage, webhook config |

## 🔗 API

FastClaw exposes an OpenAI-compatible API for programmatic access:

```bash
# Chat with an agent (SSE streaming)
curl -X POST http://localhost:18953/v1/chat/completions \
  -H "Authorization: Bearer $TOKEN" \
  -H "x-openclaw-agent-id: mike" \
  -H "Content-Type: application/json" \
  -d '{"model":"auto","messages":[{"role":"user","content":"hello"}],"stream":true}'

# List agents
curl http://localhost:18953/v1/agents -H "Authorization: Bearer $TOKEN"
```

**ChatClaw integration:** FastClaw works as a drop-in backend for [ChatClaw](https://github.com/user/chatclaw). Auto-detected via `~/.openclaw/openclaw.json`.

## 🔒 Security

**Sandbox execution** — Run agent commands in Docker containers:

```json
{"sandbox": {"enabled": true, "image": "fastclaw/sandbox:latest"}}
```

**Policy engine** — Declarative YAML policies:

```yaml
name: standard
filesystem:
  allowRead: ["/workspace/**"]
  denyWrite: ["/etc/**"]
network:
  mode: allowlist
  outbound:
    - host: api.openai.com
      ports: [443]
tools:
  deny: ["exec"]
```

**Credential manager** — Encrypted key storage:

```bash
fastclaw provider create openai --from-env
fastclaw provider list
```

## 🛠 CLI Reference

```bash
# Core
fastclaw                    # Start (setup wizard or gateway)
fastclaw gateway            # Start gateway explicitly
fastclaw version            # Version info
fastclaw doctor             # Check config health
fastclaw upgrade            # Update to latest

# Agents
fastclaw agent create mike  # Create new agent
fastclaw agent list          # List agents

# Sessions
fastclaw session list        # List sessions
fastclaw session clear KEY   # Clear a session
fastclaw session clear-all   # Clear all sessions

# Skills
fastclaw skill list          # List installed skills
fastclaw skill remove NAME   # Remove a skill

# Plugins
fastclaw plugin list         # List plugins
fastclaw plugin install PATH # Install plugin
fastclaw plugin remove ID    # Remove plugin

# Security
fastclaw provider list       # List credential providers
fastclaw provider create ... # Add credentials
fastclaw sandbox create      # Create Docker sandbox
fastclaw sandbox list        # List sandboxes
fastclaw policy list         # List policies

# Maintenance
fastclaw backup              # Backup ~/.fastclaw/
fastclaw reset               # Reset sessions & memory
```

## 🧩 Storage

| Backend | Use Case | Config |
|---------|----------|--------|
| **File** (default) | Single user, zero config | — |
| **SQLite** | Single user, structured queries | `{"storage": {"type": "sqlite", "dsn": "file:fastclaw.db"}}` |
| **PostgreSQL** | Multi-tenant cloud | `{"storage": {"type": "postgres", "dsn": "postgres://..."}}` |

## 🛠 Development

```bash
git clone https://github.com/fastclaw-ai/fastclaw.git
cd fastclaw

make build          # Build binary
make build-web      # Build web UI
make release-local  # Build all platforms
make test           # Run tests
```

## Contributing

Contributions welcome. FastClaw's strength is simplicity — keep it that way.

1. Fork → Branch → Code → PR
2. `go build ./...` must pass
3. Follow [Conventional Commits](https://www.conventionalcommits.org/)

## License

[MIT](LICENSE)

---

<div align="center">
  <sub>Built with ⚡ by the FastClaw community</sub>
</div>
