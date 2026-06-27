# FastAgent Upstream App Integration API

This is the integration contract for upstream applications that want to use
FastAgent as their Agent runtime.

Use this document when building a SaaS, bot, marketplace, or workflow product
that owns its own users but delegates agent execution, tools, memory, usage
metering, quota checks, and optional channels to FastAgent.

## Which Interface To Use

| Need | Interface | Notes |
|---|---|---|
| End-user chat with an agent | `/v1/chat/completions` | OpenAI-compatible, plus FastAgent extensions. This is the primary upstream app API. |
| List callable agents for an API key | `GET /v1/agents` | Respects API-key scope. |
| Provision one upstream end-user | `POST /v1/users` | Optional. Chat can lazy-provision via `user` or `X-Fastagent-End-User`. |
| Query usage / token spend | `GET /v1/usage` | For upstream billing dashboards. |
| Set paid-plan limits | `/v1/quota` | For subscription and entitlement enforcement. |
| Admin/dashboard automation | `/api/*` or `fastagent ...` CLI | Cookie/admin oriented, broader surface, not the minimal upstream app contract. |
| Coding-agent live preview runtime | `docs/coding-agent-runtime.md` | Project/runtime endpoints are documented separately. |

For a normal upstream product, start with `/v1/*`. Do not build against
dashboard internals unless the product is also administering FastAgent.

## Authentication

Use an API key:

```http
Authorization: Bearer fc_...
```

API key types:

| Type | Intended use |
|---|---|
| `admin` | Platform automation. Can operate broadly. Keep server-side only. |
| `user` | App backend acting as one FastAgent owner account. Can use that owner's agents. |
| `agent` | App backend scoped to explicit agent IDs. Recommended for single-agent integrations. |

Do not expose FastAgent API keys in browsers or mobile apps. The upstream app
backend should call FastAgent and map its own auth/session model to FastAgent
`user` or `X-Fastagent-End-User`.

## Identity Model

FastAgent has its own `user_id` because memory, sessions, usage, quotas, and
per-user preferences need a stable isolation key.

For upstream apps, there are two supported patterns:

1. Explicit provisioning:

   ```http
   POST /v1/users
   Authorization: Bearer fc_...
   Content-Type: application/json

   {
     "external_id": "upstream-user-123",
     "display_name": "Alice"
   }
   ```

   Response:

   ```json
   {
     "user_id": "u_...",
     "external_id": "upstream-user-123"
   }
   ```

2. Lazy provisioning on chat:

   Pass either:

   - body field: `"user": "upstream-user-123"`
   - header: `X-Fastagent-End-User: upstream-user-123`

   FastAgent will create or reuse the app-user row for that API key.

Use the upstream user's stable internal ID, not email or display name. That
keeps identity stable if the user changes their profile.

## Chat API

### `POST /v1/chat/completions`

OpenAI-compatible endpoint with FastAgent extensions.

Required:

- `messages`: array with at least one `user` message

Agent selection:

- Preferred: body field `agent_id`
- Alternative: `X-Fastagent-Agent-ID` header
- If omitted, FastAgent resolves the default accessible agent for the caller

Session selection:

- Header: `X-Fastagent-Session-Key`
- If omitted, FastAgent creates an API session key for that turn

Request:

```http
POST /v1/chat/completions
Authorization: Bearer fc_...
Content-Type: application/json
X-Fastagent-Session-Key: chat-upstream-user-123-default

{
  "agent_id": "agt_...",
  "model": "ignored-by-fastagent-agent-config",
  "stream": true,
  "user": "upstream-user-123",
  "messages": [
    { "role": "user", "content": "帮我总结今天的订单异常" }
  ],
  "params": {
    "tenant_id": "tenant_1",
    "locale": "zh-CN"
  }
}
```

FastAgent extensions:

| Field | Type | Purpose |
|---|---|---|
| `agent_id` | string | Agent to call. Body wins over header. |
| `user` | string | Upstream end-user ID. Body wins over `X-Fastagent-End-User`. |
| `params` | object | Per-turn structured context shown to the agent. Not persisted. |
| `images` | string[] | Image URLs/data URLs shown to vision models and materialized into workspace. |
| `imageUrls` | string[] | Alias for `images`. |
| `attachments` | array | General files materialized into workspace. Use for PDFs, docs, zips, etc. |

Attachment shape:

```json
{
  "attachments": [
    {
      "url": "https://example.com/report.pdf",
      "name": "report.pdf"
    }
  ]
}
```

Streaming responses follow OpenAI SSE shape:

```text
data: {"id":"chatcmpl-...","object":"chat.completion.chunk","choices":[{"delta":{"content":"..."},"index":0}]}

data: [DONE]
```

Non-streaming responses follow OpenAI response shape:

```json
{
  "id": "chatcmpl-...",
  "object": "chat.completion",
  "model": "agent",
  "choices": [
    {
      "index": 0,
      "message": { "role": "assistant", "content": "..." },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 0,
    "completion_tokens": 0,
    "total_tokens": 0
  }
}
```

### Session Key Guidance

Choose a deterministic session key from your product model:

```text
<app-name>:<upstream-user-id>:<conversation-id>
```

Do not reuse one session key for unrelated conversations. Session keys control
chat history, memory extraction context, and usage grouping.

## Agents

### `GET /v1/agents`

Lists only agents accessible to the API key.

```http
GET /v1/agents
Authorization: Bearer fc_...
```

Response:

```json
{
  "agents": [
    {
      "id": "agt_...",
      "name": "agt_...",
      "model": "openai/gpt-4.1-mini"
    }
  ]
}
```

For provisioning agents, cloning templates, installing skills, or configuring
providers, use dashboard `/api/*` endpoints or the `fastagent` CLI. Those are
operator/admin workflows, not the minimal end-user chat API.

## Usage And Quotas

### `GET /v1/usage`

Returns token usage for the authenticated FastAgent user, or for a specific
app-user when `user_id` is provided.

Query params:

| Param | Required | Notes |
|---|---|---|
| `days` | no | Default `30`, max `90`. |
| `user_id` | no | FastAgent user ID returned by `/v1/users`. |

```http
GET /v1/usage?user_id=u_...&days=30
Authorization: Bearer fc_...
```

Response:

```json
{
  "userId": "u_...",
  "days": 30,
  "daily": [
    {
      "day": "2026-06-26",
      "agentId": "agt_...",
      "model": "gpt-4.1-mini",
      "inputTokens": 100,
      "outputTokens": 50,
      "cacheReadTokens": 0,
      "cacheCreationTokens": 0,
      "requestCount": 1
    }
  ],
  "totals": {
    "inputTokens": 100,
    "outputTokens": 50,
    "cacheReadTokens": 0,
    "cacheCreationTokens": 0,
    "requestCount": 1
  }
}
```

### `PUT /v1/quota`

Sets paid-plan limits for a FastAgent user.

```http
PUT /v1/quota
Authorization: Bearer fc_...
Content-Type: application/json

{
  "user_id": "u_...",
  "monthly_token_limit": 5000000,
  "monthly_request_limit": 10000,
  "reset_day": 1
}
```

Limits of `0` mean no limit for that dimension. `reset_day` must be `1..28`;
invalid values are normalized to `1`.

### `GET /v1/quota`

```http
GET /v1/quota?user_id=u_...
Authorization: Bearer fc_...
```

Returns the configured quota and, when usage metering is enabled, current
status.

### `DELETE /v1/quota`

```http
DELETE /v1/quota?user_id=u_...
Authorization: Bearer fc_...
```

Removes explicit quota and reverts the user to unlimited FastAgent-side quota.

## Recommended Upstream Flow

1. Operator creates/configures an agent in FastAgent.
2. Operator creates an `agent` API key scoped to that agent.
3. Upstream backend stores the API key server-side.
4. When a product user starts:
   - call `POST /v1/users`, or use lazy provisioning via chat `user`
   - store the returned `user_id` if you need usage/quota lookups
5. For each conversation:
   - send `POST /v1/chat/completions`
   - set `agent_id`
   - set a deterministic `X-Fastagent-Session-Key`
   - set `user` to the upstream stable user ID
6. On subscription changes:
   - call `PUT /v1/quota`
7. For billing dashboards:
   - call `GET /v1/usage`

## Error Shape

FastAgent returns JSON errors compatible with OpenAI-style clients:

```json
{
  "error": {
    "message": "agent not found",
    "type": "not_found_error"
  }
}
```

Common statuses:

| Status | Meaning |
|---|---|
| `400` | Bad request body, missing messages, missing user_id, etc. |
| `401` | Missing/invalid API key. |
| `404` | Agent not found or not accessible by this API key. |
| `429` | Rate limited or quota exceeded. |
| `503` | Usage/quota subsystem not configured for that endpoint. |

## What To Give An Agent

When asking an AI coding agent to integrate an upstream app with FastAgent,
give it:

1. This document.
2. The FastAgent base URL.
3. API key type and scope.
4. Agent ID.
5. Your upstream user ID field.
6. Your conversation/session ID field.
7. Whether you need usage/quota billing integration.

If the agent supports Skills, install or load:

```text
skills/fastclaw-api-integration/SKILL.md
```

That skill is the short operational version of this API contract.
