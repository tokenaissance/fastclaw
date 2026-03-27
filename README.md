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
curl -fsSL https://raw.githubusercontent.com/fastclaw-ai/fastclaw/main/install.sh | bash
fastclaw    # Opens setup wizard in browser
```

## 📦 Install

**One-liner (macOS / Linux):**

```bash
curl -fsSL https://raw.githubusercontent.com/fastclaw-ai/fastclaw/main/install.sh | bash
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
2. Pick your LLM provider (OpenRouter, Ollama, or custom)
3. Click Launch — start chatting in the browser

That's it. Your agent is live. Connect messaging channels (Telegram, Discord, etc.) later via plugins.

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
| Web Chat | ✅ Built-in at /chat |
| Telegram | ✅ Via plugin |
| Discord | ✅ Via plugin |
| Slack | ✅ Via plugin |
| Any platform | ✅ Add via plugin |

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

## 🏗 Architecture

```
                    ┌─────────────────────────────────────────────┐
                    │              FastClaw Gateway                │
                    │                                             │
  Web UI ────────▶ │  ┌──────────┐    ┌──────────────────────┐  │
  Plugins ───────▶ │  │ Message  │    │    Agent Manager     │  │
  Webhooks ──────▶ │  │   Bus    │───▶│                      │  │
  API ───────────▶ │  │          │◀───│  Agent 1 (Mike)      │  │
                    │  │          │    │  Agent 2 (Mary)      │  │
                    │  └──────────┘    │  Agent N ...         │  │
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

## 🔌 Plugin System

Extend FastClaw with plugins in any language. Plugins communicate via JSON-RPC 2.0 over stdin/stdout, running as isolated subprocesses.

**Plugin types:** `channel` · `tool` · `provider` · `hook`

```bash
# Install from FastClaw Hub
fastclaw plugins install telegram

# Install from GitHub
fastclaw plugins install github.com/user/fastclaw-plugin

# Bridge an OpenClaw tool plugin
fastclaw plugins install @ollama/openclaw-web-search
```

Browse available plugins at [FastClaw Hub](https://github.com/fastclaw-ai/fastclaw-hub).

## 🖥 Web Dashboard

Full management UI at `http://localhost:18953`:

| Page | What you can do |
|------|----------------|
| Overview | Gateway status, stats, quick actions |
| Chat | Talk to your agents in the browser |
| Agents | Create, edit, delete agents; edit SOUL.md |
| Models | Manage LLM providers and default model |
| Skills | View and manage installed skills |
| Plugins | Enable/disable plugins, edit config |
| Channels | Channel status and configuration |
| Cron Jobs | Create and manage scheduled tasks |
| Settings | Storage, webhook config |

## 🔗 API

FastClaw exposes an OpenAI-compatible API for programmatic access:

```bash
# Chat with an agent (SSE streaming)
curl -X POST http://localhost:18953/v1/chat/completions \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"auto","messages":[{"role":"user","content":"hello"}],"stream":true}'

# List agents
curl http://localhost:18953/v1/agents -H "Authorization: Bearer $TOKEN"
```

## 🛠 CLI Reference

```bash
# Core
fastclaw                    # Start (setup wizard or gateway)
fastclaw gateway            # Start gateway explicitly
fastclaw version            # Version info
fastclaw doctor             # Check config health
fastclaw upgrade            # Update to latest

# Plugins
fastclaw plugins install NAME   # Install from Hub / GitHub / npm
fastclaw plugins list           # List installed plugins
fastclaw plugins remove ID      # Remove a plugin

# Agents
fastclaw agent create mike  # Create new agent
fastclaw agent list          # List agents

# Skills
fastclaw skill list          # List installed skills
fastclaw skill remove NAME   # Remove a skill
```

## 🛠 Development

```bash
git clone https://github.com/fastclaw-ai/fastclaw.git
cd fastclaw

make build          # Build binary
make build-web      # Build web UI
make dev            # Dev mode with hot reload
make release-local  # Build all platforms
make test           # Run tests
```

## Contributing

Contributions welcome. FastClaw's strength is simplicity — keep it that way.

- **Core framework** — contribute to this repo
- **Plugins & skills** — contribute to [fastclaw-hub](https://github.com/fastclaw-ai/fastclaw-hub)

## License

[MIT](LICENSE)

---

<div align="center">
  <sub>Built with ⚡ by the FastClaw community</sub>
</div>
