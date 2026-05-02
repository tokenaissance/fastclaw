<div align="center">

# FastClaw

A lightweight AI Agent runtime written in Go.

[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat&logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**Single binary - Any LLM - Multi-agent - Sandbox - Cloud-ready**

[Install](#install) - [Quick Start](#quick-start) - [Architecture](#architecture) - [Features](#features)

</div>

---

## What is FastClaw?

FastClaw is an **Agent Factory** — it creates, manages, and runs AI agents. Each agent has its own personality (SOUL.md), memory, skills, and tools. FastClaw handles the LLM communication, tool execution, sandbox isolation, and session management.

```bash
# Install and run
curl -fsSL https://raw.githubusercontent.com/fastclaw-ai/fastclaw/main/install.sh | bash
```

## Quick Start

### 1. First Run

```bash
./fastclaw
# Opens setup wizard → configure LLM provider → creates default agent
# Admin token is generated — save it for dashboard login
```

### 2. Dashboard

Open `http://localhost:18953` and login with your admin token.

- **Agents** — Create and manage agents, each with its own personality and model
- **Skills** — Install shared skills from ClawHub or GitHub
- **Models** — Configure LLM providers (OpenAI, Anthropic, Ollama, OpenRouter, etc.)
- **Settings** — Storage, sandbox, gateway configuration

### 3. Agent Management

Click an agent to enter its management panel:

- **Chat** — Talk to the agent (debug/test)
- **Files** — Edit SOUL.md, IDENTITY.md, MEMORY.md, etc.
- **Skills** — Agent-private skills
- **Models** — Agent-specific provider + model overrides (shadow system entries by name; agent-scope `agents.defaults.model` overrides the system default)
- **Channels** — Connect IM bots (Telegram, Discord, Slack) so end-users can chat with the agent on their platform of choice
- **Sessions** — Conversation history

## Architecture

```
~/.fastclaw/
  fastclaw.json              # Global config (gateway, storage, providers, defaults)
  apikeys.json               # API keys for external access
  skills/                    # Shared skills (bundled + installed)
  agents/
    default/agent/           # Agent workspace
      agent.json             # Agent config (model override)
      SOUL.md                # Personality
      MEMORY.md              # Long-term memory
      sessions/              # Conversation history
      skills/                # Agent-private skills
    my-coder/agent/
      ...
```

### Storage

| Data | `storage: "file"` | `storage: "postgres"` |
|------|-------------------|-----------------------|
| Global config | File (always) | File (bootstrap) |
| Sessions | JSONL files | DB |
| Memory / SOUL.md | Files | DB |
| Agent config | Files | DB |
| Skills | Files (always) | Files (always) |

### What FastClaw Stores

| Data | Belongs to | Storage |
|------|-----------|---------|
| SOUL.md, IDENTITY.md | Agent | FastClaw |
| MEMORY.md | Agent | FastClaw |
| Skills | Agent / Global | FastClaw |
| Sessions | Agent | FastClaw |
| User accounts, billing | Application | Your app (ChatClaw, etc.) |
| Output files | Application | Your app / S3 |

## Features

### LLM Providers
- OpenAI, Anthropic, Ollama, OpenRouter, Groq, DeepSeek, Mistral, and any OpenAI-compatible API
- Per-agent provider + model override (agent-scope shadows system by name)
- Prompt cache support (RawAssistant preservation)

### Channels
- Per-agent Telegram / Discord / Slack bot bindings — end-users chat with the agent on their platform
- Tokens validated before save (Telegram `getMe`, Discord `/users/@me`, Slack `auth.test`)
- Sessions are isolated per channel + chatID, so a user's Telegram thread and Discord thread stay separate

### Tools & Sandbox
- Built-in: exec, read_file, write_file, list_dir, web_fetch, web_search, memory_search
- E2B cloud sandbox or Docker sandbox — automatic skill + workspace hydrate, post-exec sync (sandbox-side files mirrored back to the durable store after every tool call)
- MCP server support
- Plugin system (JSON-RPC subprocess)

### Skills
- Bundled skills: code-runner, image-gen, data-analysis, translation, web-search, skill-creator
- Install from [ClawHub](https://clawhub.ai) or [skills.sh](https://skills.sh)
- Agent-private or globally shared

### Memory
- MEMORY.md — long-term facts, auto-updated by heartbeat
- Session-based context with full history preservation
- Thinking/reasoning content preserved for memory extraction

### API
- OpenAI-compatible `/v1/chat/completions` (streaming)
- Web chat `/api/chat/stream` (SSE)
- Session management `/api/chat/sessions`
- Agent CRUD `/api/agents`
- Provider management `/api/config`
- Skill install `/api/skills/install` (ClawHub + GitHub)
- API key management `/v1/admin/apikeys`

## Configuration

### fastclaw.json

```json
{
  "gateway": {
    "port": 18953,
    "auth": { "token": "your-admin-token" }
  },
  "storage": {
    "type": "postgres",
    "dsn": "postgres://user:pass@localhost:5432/fastclaw?sslmode=disable",
    "autoMigrate": true
  },
  "sandbox": {
    "enabled": true,
    "backend": "e2b",
    "e2bKey": "e2b_..."
  },
  "providers": {
    "openrouter": {
      "apiKey": "sk-or-...",
      "apiBase": "https://openrouter.ai/api/v1",
      "apiType": "openai"
    }
  },
  "agents": {
    "defaults": {
      "model": "openrouter/openai/gpt-4o",
      "maxTokens": 8192,
      "temperature": 0.7
    }
  }
}
```

## Deployment

### Local
```bash
./fastclaw gateway
```

### Local agent instances

For temporary local agents, run isolated gateway instances by name. Each
instance has its own sqlite database, config, users, agents, workspaces, and
logs.

```bash
# Create a local instance and seed its provider/model config.
fastclaw agents init scratch \
  --provider openai \
  --model openai/gpt-4.1 \
  --api-key-env OPENAI_API_KEY

# Configure runtime settings directly from the CLI.
fastclaw agents config scratch set sandbox.enabled true
fastclaw agents config scratch set temperature 0.2

# Customize identity files.
fastclaw agents files put scratch SOUL.md ./SOUL.md
fastclaw agents files put scratch IDENTITY.md ./IDENTITY.md

# Run and inspect the instance.
fastclaw agents start scratch
fastclaw agents ls
fastclaw agents status scratch
fastclaw agents log scratch
fastclaw agents log scratch -f -n 200
fastclaw agents restart scratch
fastclaw agents stop scratch

# Tear it down. Default keeps the home dir + logs so a later `init` recovers.
fastclaw agents rm scratch
fastclaw agents rm scratch --purge   # also wipe ~/.fastclaw/local-agents/scratch and the log file
fastclaw agents rm scratch --force   # stop the agent first if it is still running
```

`agents init` writes the local sqlite store directly, so provider/model
defaults and system files can be configured without opening the dashboard.
When the local DB has no users, it creates a super admin account. If
`--password` is omitted, a password is generated and printed once. When the
DB already has users, passing `--username` requires that account to exist —
the command refuses to silently bind the agent to a different user.

Re-running `agents init` against the same name is non-destructive: the agent
record's `Config` map, the agent system files, and existing model entry
metadata (context window, max tokens, cost) are preserved. Provider fields
that are not explicitly overridden also keep their previous values.

Provider presets are available for `openai`, `openrouter`, `anthropic`,
`ollama`, `groq`, `deepseek`, and `mistral`. Use `--api-key-env` instead of
placing API keys directly on the command line:

```bash
fastclaw agents init scratch --provider openai --model openai/gpt-4.1 --api-key-env OPENAI_API_KEY
fastclaw agents init local-llama --provider ollama --model llama3.1
```

Configuration can be read or updated by key. Common keys include `model`,
`temperature`, `maxTokens`, `thinking`, `policy`, `sandbox.enabled`,
`sandbox.backend`, and provider fields such as `provider.openai.apiBase`,
`provider.openai.apiType`, and `provider.openai.apiKeyEnv`.

```bash
fastclaw agents config scratch get
fastclaw agents config scratch get model
fastclaw agents config scratch get sandbox
fastclaw agents config scratch set model openai/gpt-4.1-mini
fastclaw agents config scratch set provider.openai.apiKeyEnv OPENAI_API_KEY
fastclaw agents config scratch set sandbox '{"enabled":true,"backend":"docker"}'
```

System files are stored in the local instance database under the configured
agent/user scope. Supported filenames are `SOUL.md`, `IDENTITY.md`, `USER.md`,
`BOOTSTRAP.md`, `MEMORY.md`, `HEARTBEAT.md`, `AGENTS.md`, `TOOLS.md`, and
`agent.json`:

```bash
fastclaw agents files ls scratch
fastclaw agents files get scratch SOUL.md
fastclaw agents files get scratch SOUL.md ./SOUL.md
fastclaw agents files put scratch SOUL.md ./SOUL.md
```

Each instance gets its own `FASTCLAW_HOME` under `~/.fastclaw/local-agents/<name>`,
with process metadata in `~/.fastclaw/agent-runs/` and logs in
`~/.fastclaw/logs/agents/`. Use `--port` or `--home` on `agents start` when you
need an explicit port or home directory. CLI config writes do not hot-reload a
running gateway; use `agents restart <name>` after `agents init`, `agents
config set`, or `agents files put` if the instance is already running.

A few sharp edges worth knowing:

- `agents start <name>` does not require `agents init` first. A bare `start`
  boots a gateway with an empty database; the web setup wizard at the printed
  URL will walk you through provider/admin configuration.
- `agents rm` keeps `~/.fastclaw/local-agents/<name>/` and the log file by
  default so a later `agents init <name>` can recover prior data. Pass
  `--purge` to wipe them too. `--force` only stops a running agent before
  removal — it does not imply `--purge`.
- On Unix, `agents stop` SIGTERMs the whole gateway process group (sandbox
  runners, plugin subprocesses, etc.) and escalates to SIGKILL after 5
  seconds. On Windows, the gateway is detached with `CREATE_NEW_PROCESS_GROUP`
  and `agents stop` sends `CTRL_BREAK_EVENT`, falling back to a hard kill if
  the gateway does not handle it.
- `agents init` reuse rules: re-running against the same `<name>` preserves
  the agent record's `Config` map, system files, existing model entry
  metadata, and provider fields not explicitly overridden. The agent is
  bound to the existing owner — passing `--username` for a different
  account refuses rather than silently rebinding.

| Subcommand | Purpose |
|---|---|
| `agents init <name>` | Create or update an instance's sqlite config (provider, model, sandbox, admin user) |
| `agents start <name>` | Launch the gateway as a detached background process |
| `agents stop <name>` | SIGTERM (then SIGKILL after 5s) the running instance |
| `agents restart <name>` | Stop (if running) and start, optionally with new `--port` / `--home` |
| `agents ls` | List all known instances with status/PID/port/uptime |
| `agents status <name>` | Show one instance's status, URL, log path, uptime |
| `agents rm <name>` | Remove instance metadata; pass `--purge` to wipe sqlite + logs, `--force` to stop first |
| `agents log <name> [-f] [-n N]` | Show / follow the instance's log file (no `tail` binary required) |
| `agents config <name> get\|set [key] [value]` | Read or update saved provider/setting values |
| `agents files ls\|put\|get <name>` | Read / write the agent's system files (SOUL.md, IDENTITY.md, …) |

### Docker
```bash
cd deploy/docker && ./start.sh
```

### Kubernetes
```yaml
volumeMounts:
  - name: config
    mountPath: /root/.fastclaw/fastclaw.json
    subPath: fastclaw.json
env:
  - name: FASTCLAW_STORAGE_DSN
    valueFrom:
      secretKeyRef:
        name: fastclaw-db
        key: dsn
```

## Building

```bash
# Build frontend
cd web && pnpm install && pnpm build
cp -r out ../internal/setup/web

# Build binary
go build -o fastclaw ./cmd/fastclaw
```

## License

MIT
