<div align="center">

# ⚡ FastClaw

A lightweight, self-hosted AI Agent framework written in Go.

[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat&logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Release](https://img.shields.io/github/v/release/fastclaw-ai/fastclaw?include_prereleases)](https://github.com/fastclaw-ai/fastclaw/releases)

**Single binary · Any LLM · Multi-user · Multi-agent · Cloud-ready**

[Install](#-install) · [Quick Start](#-quick-start) · [Cloud Deploy](#-cloud-deployment) · [Features](#-features) · [Documentation](#-documentation)

</div>

---

## What is FastClaw?

FastClaw is an AI agent runtime — the "Agent OS" that runs your agents. It connects LLMs to tools, manages memory, and handles multiple users. Run it locally as a single-user agent like OpenClaw, or deploy to the cloud as a multi-tenant platform.

```bash
# Local (single user)
curl -fsSL https://raw.githubusercontent.com/fastclaw-ai/fastclaw/main/install.sh | bash
fastclaw    # Opens setup wizard in browser

# Cloud (multi-user, one-click)
cd deploy/docker && ./start.sh
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

## ☁️ Cloud Deployment

FastClaw supports multi-user cloud deployment where each user gets isolated agents, sessions, memory, and credentials.

### Docker Compose (single machine)

```bash
cd deploy/docker
./start.sh                        # Interactive — prompts for port + API key
./start.sh --port 19000           # Specify port
LLM_API_KEY=sk-xxx ./start.sh     # Non-interactive
```

The script builds the image, starts FastClaw + Postgres, creates a demo user, and prints access tokens.

### Kubernetes (Helm)

```bash
helm install fastclaw deploy/helm/fastclaw \
  --namespace fastclaw --create-namespace \
  --set gateway.adminToken=$(openssl rand -hex 32) \
  --set postgres.password=$(openssl rand -hex 16) \
  --set ingress.enabled=true \
  --set ingress.host=fastclaw.yourdomain.com
```

### User Management

```bash
# CLI
fastclaw user add alice --name Alice    # → prints access token
fastclaw user list
fastclaw user token alice               # issue additional token

# Admin API
curl -X POST http://localhost:18953/v1/admin/users \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -d '{"id":"alice","name":"Alice"}'

# Web UI
# Admin login → sidebar "Users" page → Add User
```

Each user logs into the same web UI with their token and sees only their own data.

### Local vs Cloud

| | Local Mode | Cloud Mode |
|---|---|---|
| Config | `gateway.mode: "local"` (default) | `gateway.mode: "cloud"` |
| Users | Single implicit "local" user | Multiple users, token auth |
| Data | `~/.fastclaw/users/local/` | `~/.fastclaw/users/{id}/` per user |
| Web UI | No login required | Token login screen |
| Upgrade path | Zero config change | Same binary, just set mode |

## ✨ Features

### Core

| Feature | Description |
|---------|-------------|
| **ReAct Agent Loop** | Multi-turn reasoning + tool calling |
| **Any LLM** | OpenAI-compatible API (OpenAI, Claude, DeepSeek, Gemini, Groq, Ollama, OpenRouter) |
| **Multi-Agent** | Multiple agents with independent personalities, memory, and skills |
| **Multi-User** | Per-user isolated agents, sessions, memory, and credentials |
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
| `exec` | Shell commands (with optional Docker/E2B sandbox) |
| `read_file` / `write_file` / `list_dir` | File operations (sandboxed in cloud mode) |
| `web_fetch` | Fetch web pages → markdown |
| `web_search` | Brave Search API |
| `memory_search` | Search conversation history |
| `message` | Send messages to any channel |
| `spawn_subagent` | Delegate tasks to other agents |
| `create_cron_job` / `list_cron_jobs` / `delete_cron_job` | Manage scheduled tasks |
| `load_skill` | Load skill instructions on demand |
| MCP tools | Connect external tools via Model Context Protocol |

### Security

| Feature | Description |
|---------|-------------|
| **Sandbox Exec** | Docker or E2B sandboxed execution |
| **Path Sandbox** | Cloud mode restricts file access to user workspace |
| **Per-User KEK** | Each user's credentials encrypted with independent key |
| **Policy Engine** | YAML policies for filesystem, network, tools, resources |
| **Credential Manager** | AES-256-GCM encrypted key storage, env auto-discovery |
| **Rate Limiting** | Per-user RPM limiting on API endpoints |

### Platform

| Feature | Description |
|---------|-------------|
| **Web Dashboard** | Full management UI with token-based login |
| **Admin Panel** | User management page for admins |
| **Plugin System** | JSON-RPC subprocess plugins (any language) |
| **Pluggable Storage** | File (default), PostgreSQL, SQLite |
| **OpenAI-Compatible API** | `POST /v1/chat/completions` with SSE streaming |
| **Admin API** | `POST/GET/DELETE /v1/admin/users` |
| **WebSocket Gateway** | Real-time chat protocol |
| **Docker / Helm** | One-click cloud deployment |

## 🏗 Architecture

```
                ┌────────────────────────────────────────────────────┐
                │                 FastClaw Gateway                    │
                │                                                    │
  Web UI ─────▶│  ┌──────────────────────────────────────────────┐  │
  API ────────▶│  │          Auth Middleware                      │  │
  WebSocket ──▶│  │   token → userID → UserSpace (lazy loaded)   │  │
  Webhook ────▶│  └──────────────────┬───────────────────────────┘  │
                │                     │                              │
                │  ┌──────────────────▼───────────────────────────┐ │
                │  │          UserSpace Registry                   │ │
                │  │                                               │ │
                │  │  user:local  → Config + AgentManager + Creds  │ │
                │  │  user:alice  → Config + AgentManager + Creds  │ │
                │  │  user:bob    → Config + AgentManager + Creds  │ │
                │  │  (idle users evicted after 30 min)            │ │
                │  └──────────────────┬───────────────────────────┘ │
                │                     │                              │
                │        ┌────────────┼────────────┐                │
                │        ▼            ▼            ▼                │
                │  ┌──────────┐ ┌──────────┐ ┌──────────┐          │
                │  │  Tools   │ │  Memory  │ │ Sessions │          │
                │  │ exec     │ │MEMORY.md │ │ per-chat │          │
                │  │ files    │ │ search   │ │ history  │          │
                │  │ sandbox  │ │ per-user │ │ per-user │          │
                │  └──────────┘ └──────────┘ └──────────┘          │
                └────────────────────────────────────────────────────┘
```

## 🔌 Plugin System

Extend FastClaw with plugins in any language. Plugins communicate via JSON-RPC 2.0 over stdin/stdout, running as isolated subprocesses.

**Plugin types:** `channel` · `tool` · `provider` · `hook`

```bash
# Install from FastClaw Hub
fastclaw plugins install telegram

# Install from GitHub
fastclaw plugins install github.com/user/fastclaw-plugin
```

Official plugins are in the [`plugins/`](plugins/) directory. Community plugins are indexed at [FastClaw Hub](https://github.com/fastclaw-ai/fastclaw-hub).

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
| Users | (Admin only) Create and manage cloud users |

In cloud mode, users log in with a bearer token. Each user sees only their own agents, conversations, and settings.

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

# Admin: list users (admin token only)
curl http://localhost:18953/v1/admin/users -H "Authorization: Bearer $ADMIN_TOKEN"
```

## 🛠 CLI Reference

```bash
# Core
fastclaw                    # Start (setup wizard or gateway)
fastclaw gateway            # Start gateway explicitly
fastclaw version            # Version info
fastclaw doctor             # Check config health
fastclaw upgrade            # Update to latest

# Users (cloud mode)
fastclaw user add alice --name Alice   # Create user, get token
fastclaw user list                     # List all users
fastclaw user token alice              # Issue new token
fastclaw user remove alice             # Remove user

# Agents
fastclaw agent create mike  # Create new agent
fastclaw agent list          # List agents

# Plugins
fastclaw plugins install NAME   # Install from Hub / GitHub
fastclaw plugins list           # List installed plugins
fastclaw plugins remove ID      # Remove a plugin

# Skills
fastclaw skill list          # List installed skills
fastclaw skill remove NAME   # Remove a skill

# Sessions
fastclaw session list        # List all sessions
fastclaw session clear KEY   # Clear specific session
fastclaw session clear-all   # Clear all sessions
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

- **Core framework & official plugins** — contribute to this repo
- **Community plugins** — create your own repo, submit to [FastClaw Hub](https://github.com/fastclaw-ai/fastclaw-hub) index

## License

[MIT](LICENSE)

---

<div align="center">
  <sub>Built with ⚡ by the FastClaw community</sub>
</div>
