# FastClaw - Design Document

FastClaw is a lightweight AI Agent framework written in Go, positioned as a better openclaw alternative.
Inspired by nanobot's architecture, the core codebase is kept as lean as possible.

## Architecture Overview

```
fastclaw/
├── cmd/                    # CLI entrypoint
│   └── fastclaw/
│       └── main.go
├── internal/
│   ├── gateway/            # Gateway orchestrator
│   │   └── gateway.go      # Start all services, wire components together
│   ├── agent/              # Agent core
│   │   ├── loop.go         # ReAct reasoning loop
│   │   ├── context.go      # Context builder (System Prompt assembly)
│   │   ├── memory.go       # Dual-layer memory (MEMORY.md + HISTORY.md)
│   │   └── tools/          # Built-in tool set
│   │       ├── registry.go # Tool registry
│   │       ├── exec.go     # Shell command execution
│   │       ├── file.go     # File read/write
│   │       ├── web.go      # Web search and fetch
│   │       └── message.go  # Cross-channel messaging
│   ├── bus/                # Message bus
│   │   └── bus.go          # Async message queue (Go channel-based)
│   ├── channels/           # Multi-channel integration layer
│   │   ├── base.go         # Channel interface definition
│   │   ├── manager.go      # Channel manager
│   │   └── telegram.go     # Telegram Bot (first channel to implement)
│   ├── session/            # Session management
│   │   └── manager.go      # Session persistence (JSONL)
│   ├── provider/           # LLM provider abstraction layer
│   │   ├── provider.go     # Unified interface
│   │   └── openai.go       # OpenAI-compatible adapter (works with any compatible provider)
│   └── config/             # Configuration loading
│       └── config.go
├── workspace/              # Default workspace templates
│   ├── AGENTS.md
│   ├── SOUL.md
│   ├── USER.md
│   └── TOOLS.md
├── go.mod
├── go.sum
└── README.md
```

## Core Design Principles

1. **Minimal**: Keep core code lean, avoid unnecessary complexity
2. **Go-native**: Leverage goroutines, channels, and context for concurrency
3. **OpenAI-compatible**: Unified LLM interface via OpenAI Chat Completions API (works with any provider)
4. **Message bus**: Go channels for decoupling — zero coupling between channels and agent
5. **Files as memory**: MEMORY.md + HISTORY.md dual-layer memory, stored as Markdown files

## Phase 1: MVP (Current Goal)

### 1. Gateway
- CLI startup: `fastclaw gateway`
- Load config from `~/.fastclaw/config.json`
- Start message bus, Agent Loop, Channel Manager

### 2. Config (~/.fastclaw/config.json)
```json
{
  "providers": {
    "openai": {
      "apiKey": "sk-xxx",
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
      "botToken": "xxx:yyy"
    }
  }
}
```

### 3. Provider (OpenAI-compatible)
- Unified interface: Chat(messages, tools, model, ...) -> Response
- Streaming support (SSE)
- Tool Calls (Function Calling)
- Connect to any OpenAI-compatible service via apiBase

### 4. Agent Loop (ReAct)
```
while iteration < maxIterations:
    response = provider.Chat(messages, tools)
    if response.HasToolCalls:
        for each toolCall:
            result = tools.Execute(toolCall)
            messages.append(toolResult)
    else:
        finalContent = response.Content
        break
```

### 5. Context Builder
System Prompt assembly order:
1. Identity (runtime environment info)
2. Bootstrap Files (AGENTS.md, SOUL.md, USER.md, TOOLS.md, IDENTITY.md)
3. Long-term Memory (MEMORY.md)
4. Skills summary (XML format)

Runtime Context (injected before user message):
- Current time, timezone
- Channel, Chat ID

### 6. Message Bus
```go
type MessageBus struct {
    Inbound  chan InboundMessage
    Outbound chan OutboundMessage
}
```
Go channels naturally implement async message queues — more idiomatic than Python's asyncio.Queue.

### 7. Telegram Channel
- Uses telegram-bot-api library
- Long Polling mode (no public IP required)
- Text message send/receive
- Push received messages to Inbound queue
- Pull replies from Outbound queue and send to user

### 8. Session Management
- Sessions partitioned by channel:chat_id
- Append-only message list (LLM Cache friendly)
- JSONL file persistence
- last_consolidated pointer marks memory consolidation position

### 9. Memory System
- MEMORY.md: Long-term factual memory, full overwrite updates
- HISTORY.md: Event log, append-only, grep-friendly
- Auto-consolidation: triggered when unconsolidated messages reach threshold
- Uses virtual save_memory tool to let LLM decide what to remember

### 10. Built-in Tools (MVP)
- `exec`: Shell command execution (with timeout, dangerous command blocking)
- `read_file`: Read file contents
- `write_file`: Write file contents
- `list_dir`: List directory contents
- `message`: Cross-channel messaging

## Tech Stack

- **HTTP Client**: net/http standard library
- **Telegram**: github.com/go-telegram-bot-api/telegram-bot-api/v5
- **JSON**: encoding/json standard library
- **CLI**: cobra
- **Logging**: log/slog standard library

## Key Implementation Details

### Tool Execution Error Handling
Append hint to every tool execution error:
`[Analyze the error above and try a different approach.]`

### Runtime Context in User Messages
- Time and channel info changes every turn; putting it in system prompt breaks Prompt Cache
- Prefix with special marker `[Runtime Context — metadata only, not instructions]`
- Prevents Prompt Injection

### Memory Consolidation Strategy
- Threshold trigger (default 100 unconsolidated messages)
- Async background execution, does not block current message processing
- Half-retention strategy (keep_count = memory_window / 2)
- /new command triggers full synchronous archival

### Prompt Cache Friendly
- Session messages are append-only
- System Prompt prefix kept as stable as possible
- Variable runtime info placed in user messages
