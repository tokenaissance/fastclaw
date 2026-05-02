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
- **Scheduler** — Inspect and manage cron jobs the agent created via `create_cron_job` ("每天 9 点提醒我", "5 分钟后叫我"); pause / delete from the UI
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
- Live agent push via `/api/chat/subscribe` (SSE) — surfaces cron-fired and other async replies into the open chat panel without a refresh
- Session management `/api/chat/sessions`
- Agent CRUD `/api/agents`
- Per-agent scheduler `/api/agents/{id}/cron` (list / toggle / delete)
- Provider management `/api/config`
- Skill install `/api/skills/install` (ClawHub + GitHub)
- API key management `/v1/admin/apikeys`
- App-user provisioning `POST /v1/users` — third-party apps mint a stable fastclaw user_id per end-user, idempotent on `(api_key, external_id)`. Or pass `user` on `/v1/chat/completions` (or `X-Fastclaw-End-User` header) for lazy mint on first call

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

### Manage agents from the CLI (`fastclaw agents …`)

The `fastclaw agents` subcommand is a thin convenience wrapper around the
same store the dashboard uses. Agents you create here show up in the web
UI and vice-versa — there's only ever one fastclaw deployment per
`FASTCLAW_HOME`.

```bash
# Zero to a chattable agent in one command. On a fresh install this
# creates an `admin` user (random password printed once) and starts
# the gateway daemon if it isn't already running.
fastclaw agents init alpha \
  --provider openai \
  --model openai/gpt-4o-mini \
  --api-key-env OPENAI_API_KEY

# Set per-agent overrides (model, temperature, sandbox, …).
fastclaw agents config alpha set temperature 0.7
fastclaw agents config alpha set sandbox.enabled true

# Upload the agent's identity files.
fastclaw agents files put alpha SOUL.md ./SOUL.md
fastclaw agents files put alpha IDENTITY.md ./IDENTITY.md

# Inspect.
fastclaw agents ls
fastclaw agents config alpha get
fastclaw agents files ls alpha

# Tear down.
fastclaw agents rm alpha
```

The CLI opens the operator's store directly (sqlite at
`~/.fastclaw/fastclaw.db`, or whatever `FASTCLAW_STORAGE_DSN` points at)
and writes through the same code paths the gateway uses. It does not
require the gateway to be running — but `agents init` will spin one up
in the background so a fresh agent is immediately reachable at
`http://localhost:18953`. Subsequent CLI writes (`config set`,
`files put`, `rm`, `init` re-runs) send `SIGHUP` to the running gateway
so it hot-reloads without restart. Windows lacks `SIGHUP` delivery, so
the CLI falls back to a hint asking you to run `fastclaw daemon restart`.

The default owner is the `admin` user. On an empty database
`agents init` creates that account with a generated password (printed
once); on a populated database it expects `admin` to exist or
`--username` to point at an existing user.

#### Resolving agents

CLI commands accept either a display name or an `agt_…` id:

- `fastclaw agents config alpha get` — by display name (must be unique)
- `fastclaw agents config agt_d3c4a5… get` — by id

If the same text matches one agent's id and a different agent's display
name, the CLI reports an ambiguity instead of guessing.

When you create an agent via `agents init <name>`, the name is the
display name and the id is auto-generated. To update an agent that was
created via the dashboard, pass its id explicitly:

```bash
fastclaw agents init "Cool Agent" --id agt_d3c4a5...
```

#### Configuration keys

Per-agent (saved at `scope=agent` under the agent's id):

- `model`, `temperature`, `maxTokens`, `thinking`, `policy`
- `sandbox`, `sandbox.enabled`, `sandbox.backend`, `sandbox.image`, `sandbox.network`

System-wide (saved at `scope=system`):

- `plugins`, `plugins.<name>`
- `skills.install`, `skills.entries`, `skillsLearner`
- `tools.providers`, `tools.categories`
- `objectstore`, `taskqueue`, `heartbeat`, `memory`, `privacy`, `hooks`, `teams`

Provider configs live in `scope=system` and are addressed as
`provider.<name>.<field>`:

```bash
fastclaw agents config alpha set provider.openai.apiKeyEnv OPENAI_API_KEY
fastclaw agents config alpha set provider.openrouter.apiBase https://openrouter.ai/api/v1
fastclaw agents config alpha set provider.openai.model gpt-4o      # adds; idempotent
fastclaw agents config alpha set provider.openai.models '[]'        # explicit clear
```

Provider presets ship for `openai`, `openrouter`, `anthropic`, `ollama`,
`groq`, `deepseek`, `mistral` — `--api-key-env` populates `apiKey` from
the named environment variable, the rest comes from the preset.

#### Agent system files

The CLI reads and writes the same `agent_files` table the dashboard's
file editor uses. Allowlisted filenames: `SOUL.md`, `IDENTITY.md`,
`USER.md`, `BOOTSTRAP.md`, `MEMORY.md`, `HEARTBEAT.md`, `AGENTS.md`,
`TOOLS.md`, `agent.json`.

| Subcommand | Purpose |
|---|---|
| `agents init <name>` | Create or update an agent (provider/model/sandbox/files) |
| `agents ls` | List all agents in the store |
| `agents config <name> get\|set [key] [value]` | Read or update a config value |
| `agents files ls\|put\|get <name>` | Read / write the agent's system files |
| `agents rm <name>` | Delete the agent record and its system files |

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
