# fastagent MCP OAuth 客户端接入设计

> 状态：Draft · 作者：fastagent · 日期：2026-08-30
> 目标读者：fastagent 后端工程师、tokenaissance 全栈
> 原则：遵循 Clean Architecture 四层模型与依赖规则；功能完整、交互友好、安全优先；对既有代码改动最小。

> **实施状态（2026-09-01）**：fastagent 侧已按本文落地于 `internal/mcp/oauth/`（domain / port / usecase / adapter / bootstrap）+ 框架接线（config / mcp HTTP 客户端 / agent loop / setup handlers / CLI `mcp login|status|logout`）。已实现并修复评审项 P0/P1/P2：进程级 refresh 锁单例、注册 store 按 (serverName, callbackURL) 键、公开回调路径 `/oauth/mcp/{callbackID}/callback`、refresh 不旋转时保留旧 refresh token、discovery 缓存 TTL 1h、**pending auth 加密持久化**（含 code_verifier 密文存储）。
>
> **多实例共享存储（2026-09-01，部署目标：多租户 + 多实例 + 共享存储）**：
> - token / pending / registration 三个 store 全部提供 SQL 实现（`mcp_oauth_tokens` / `mcp_oauth_pending` / `mcp_oauth_clients` 三张表）。共享 DB 存储适用于**所有部署形态**：多实例/生产走 Postgres，单实例/本地走 SQLite——两者共用同一套 SQL adapter（按 dialect 生成占位符），**部署不回退文件 store**；bootstrap 通过 `DBProvider`（`*store.DBStore`）选择 SQL 实现，文件 store 仅保留为无 DB 的测试夹具。
> - pending `Take` 用 `DELETE ... RETURNING` 原子消费，两个实例不可能同时消费同一 state；表含 `user_id` 分区索引。
> - refresh 跨实例互斥：可选 Redis 分布式锁（SET NX + TTL，复用 `FASTAGENT_REDIS_*` 配置）+ 进程内锁 + **乐观重试**（旋转竞态失败方 reload 到对方已刷新的 token 直接返回）。Redis 不可用时降级为进程锁 + 重试，仍安全。
> - **pending 容量上限**（威胁模型「待授权会话堆积」）：per-user 上限默认 20（`oauth.Options.MaxPendingPerUser` 可调），start 前 `CountActive`（惰性清理过期）+ 超限拒绝。
> - **跨实例失效广播（无 Redis 也成立）**：授权/吊销完成后本实例 `ReloadAgents` + 写共享 DB epoch（`configs_kv` kind=`mcp_oauth_reload`）+ Redis pub/sub（`rediscoord.Invalidator`，channel `{prefix}:reload-agents`）。其它 replica 无 Redis 时靠 30s epoch 轮询收敛；有 Redis 时 pub/sub 即时通知并 `Sync` epoch 防重复 reload。任一路径都能让 LB 路由到的实例丢弃授权前的旧 UserSpace 快照。
> - **授权边界收紧**：start/revoke/status 只允许 agent owner 或平台 admin（actAs 只读模式与公开 agent 访客拒绝），凭证归属键取 `agents.user_id`。
> - **方案 A：owner-only 使用门控（2026-09-03）**：OAuth 凭证只供 **agent owner 自己的会话**使用。会话主体（UserSpace 用户）随 agent 构建传入 `TokenProvider`（`RefreshInput.ActorUserID`）；actor ≠ credential owner 时在**任何 store 读取/HTTP 请求之前**拒绝（`ErrCredentialOwnerOnly`）。跨 UserSpace 访客（公开链接 / apikey 共享用户 / super_admin）因此**拿不到 owner 的 Quandora 凭据**：这些 session 里 OAuth server 连接被跳过、工具不注册。注意：owner 自己频道内的共享会话（群聊成员）当前仍可用——本实现是 **UserSpace 租户级**门控，非 per-message 门控（见 §7.4 草案）。静态 header server 与单用户本地模式（无多租户分隔）行为不变。对比：方案 B（owner 白名单放行指定用户）与方案 C（每人独立授权）未采用。
> - **严格 A：per-turn 发起人门控（2026-09-03，设计草案，代码未改）**：当前方案 A 是 **UserSpace 租户级**门控，在「owner 自己 UserSpace 内的群聊/共享会话」里，非 owner 成员也能触发 owner 的 OAuth 工具（烧 owner 配额、读 owner 数据）。拟升级为 **per-turn 发起人门控**：发起人判定在 routing seam 完成，principal stamp 进 run ctx，再沿 `CallTool → sendRequest → auth` 修复链透传到 MCP auth()（现状这两处会丢 ctx），owner 等价谓词之外的消息一律拒绝。设计详见 §7.4，**待确认两点后实现**：IM owner 身份来源、群成员拒绝 UX。
> - **L2 host 级发起器（agent 工具 `mcp`，2026-09-03 阶段 1 + 2026-09-04 动作补齐 + per-key 存储）**：agent 可用 `mcp add|remove|login|status|check|refresh|logout` 管理 MCP server 声明与 OAuth 授权；owner 门控沿用 scheme-A。`add`/`remove` 持久化写 **`agent_mcp_servers` per-key 行**（2026-09-04 从 `agents.config` JSON 迁出，见 §5.6/§13），落库成功后才 notify 跨实例 reload，二者互为逆操作；`login` 只产出 authorization_url，完成由宿主回调负责（cloud `/oauth/mcp/{id}/callback` 或 CLI），code 不进对话；`check` 用真实 `tools/list` 端到端验证连接与凭证（本地 authorized ≠ 服务端 token 有效）；`refresh` 主动走 RefreshToken usecase（与 401 自动刷新同源，fresh 时幂等 no-op）；`logout` 吊销。完整 action 清单与协议映射见 §5.6，可逆性设计与论文合规审计见 §13。服务端部署需配置 `FASTAGENT_OAUTH_CALLBACK_BASE`（helm `--set oauth.callbackBase=<origin>/oauth/mcp`，走 ConfigMap，非 secret）。`update`、SKILL.md 自动声明（阶段 2）、allow/deny 审批等其余 L2-B 能力仍为设计（§5.7）。
> - **同步通知（2026-09-04）**：§5.6/§5.7 已按当前实现更新（add/remove 落地、per-key 存储）；「MCP 操作可逆性」新设计与论文合规审计见 §13（原独立文档 `mcp-reversibility.md` 已合并删除）。
> - 部署要求：所有实例必须同一 `FASTAGENT_OAUTH_SECRET`（轮换 = 全量重新授权）；生产用 Postgres；Redis 可选但推荐（分布式锁 + 跨实例失效广播）。k8s helm 部署通过 `--set oauth.secret=<value>` 注入（写入 `fastagent-secrets` Secret → `FASTAGENT_OAUTH_SECRET` 环境变量），不要放进 ConfigMap/明文文件。
>
> 测试：domain 纯函数单测、usecase 集成测试（含容量上限）、adapter 单测（cryptor/token/registration/pending + SQL 共享实现 + discovery `WWW-Authenticate auth-issuer` 回退）、`internal/mcp` HTTP 客户端单测（Bearer 注入、401 恰好一次重放、无 auth 时静态 header 不变）、`rediscoord` 失效广播单测、`gateway` reload epoch 跨实例单测、`setup` HTTP handler 集成测试（start/status/servers/callback/revoke 全链路 + 归属 403 + 未配置 503，`servers` 覆盖动态 OAuth server 枚举与授权状态）、`e2e/` 本地 mock provider 全链路（web 回调 / CLI loopback / 刷新旋转 / 吊销）+ **多实例共享存储 e2e**（A 实例 start → B 实例 complete/读 token；双实例并发刷新在 Redis 锁下 provider 恰好收到一次 refresh）。**Postgres 真库多实例 e2e**（`Test8PostgresMultiInstanceSharedStorage`）：`FASTAGENT_E2E_PG=1` 显式启用，DSN 只取 `FASTAGENT_E2E_PG_DSN`（无 `.env.development`/`DATABASE_URL` 隐式回退），未启用/未提供则 skip；已于本地临时 PostgreSQL（2026-09-03）验证通过：完整 AutoMigrate（含 BYTEA DDL）+ start(A)→complete(B)→双实例读 token + state 重放拒绝。新增偏差：
> AutoMigrate 兼容性（2026-09-03）：OAuth 三表 DDL 按 dialect 生成——SQLite `BLOB` / Postgres `BYTEA`（`migrationSQLForDialect`），并有**纯函数 dialect 回归 UT**（无需真库、常跑）；跨实例 reload epoch 落 `configs_kv`（kind=`mcp_oauth_reload`, scope=`user`, scope_id=userID, name=`epoch`），专用 `agent_reload_epochs` 表已从迁移中移除。
> 测试（方案 A 门控）：usecase UT（owner 放行 / visitor 在 **store 读取与 refresh 之前**被拒 / 空 actor 单用户兼容 / 无凭证时返回 owner-only 而非 not-found）；framework e2e（真实 `oauth.Init` + 加密 DB store(SQLite) + 真实 MCP manager：owner 会话 connect/list/call 全通，visitor 会话 **0 次 HTTP 请求**、工具不注册；静态 header server 不变）；Manager 契约 UT（UserSpace 用户 → `mcpActorUserID`，owner 与 visitor 各验一遍）；bootstrap UT（传 DB 时单实例也走 SQL store、重启后 token/pending 仍在、Home 下零文件）+ adapter SQL dialect 占位符 UT。
> - `start` 支持 `callbackBase`（如 `https://app.tokenaissance.com/oauth/mcp`），fastagent 自动追加 `/{callbackID}/callback`，云前端无需复刻哈希；
> - `revoke` 允许空 `serverURL`（CLI logout 只删本地凭证）；
> - `status` 只要求 `agentId + serverName`（不再需要 serverUrl）；
> - 加密密钥为 `FASTAGENT_OAUTH_SECRET`，未配置时 MCP OAuth 端点返回 503、agent loop 对声明 oauthResource 的 server 报错并跳过；
> - 纯内存的 pending/registration store 已删除（无调用方）；持久化统一走共享 SQL 密文落库（单实例 SQLite / 多实例 Postgres 同一套 adapter），文件 store 仅为无 DB 测试夹具。
>
> **剩余工作（tokenaissance-cloud）**：① 在 cloud 侧新增公开回调路由 `/oauth/mcp/{callbackID}/callback`，收到 code+state 后以 admin key 转发 fastagent `POST /api/mcp/oauth/callback` 并 302 回 SPA；② SPA 增加「授权 Quandora」按钮（调 start → `window.location.assign`）与状态/吊销入口；③ 用真实 quandora 端到端验证（先 CLI loopback，再 Web 回调）。

---

## 0. 背景与目标

fastagent 的 MCP 客户端（`internal/mcp/`）目前只支持 **静态 header 鉴权**（`$ENV` 展开），无法接入 OAuth 保护的远程 MCP server（如 Quandora `https://mcp.quandora.ai/quant`）。

### 0.1 对 Quandora 服务端的实测结论（2026-08-30）

| 项目 | 实测值 | 影响 |
|---|---|---|
| `grant_types_supported` | `authorization_code` + `refresh_token` | **无 device flow**，无法走设备码 |
| `token_endpoint_auth_methods` | `none` | 公开客户端，无 client secret |
| `code_challenge_methods_supported` | `S256` | PKCE 强制 |
| `registration_endpoint` | 存在，开放注册 | 动态客户端注册可用 |
| redirect_uri 类型 | **HTTPS 与 loopback 均接受** | 服务器侧可用公网 HTTPS 回调 |
| `authorization_response_iss_parameter_supported` | **未声明** | 不能依赖 iss 参数，须用 callback-specific 模式（state/路径绑定） |
| token 生命周期 | access 7 天 + refresh 自动旋转 | 客户端必须实现刷新 |
| `revocation_endpoint` | 存在 | 支持主动吊销 |

**核心矛盾**：fastagent 是无头服务器，浏览器在用户机器上；OAuth 回调的 redirect 永远落在**浏览器那端**，code 必须经一条安全通道回到服务器。

### 0.2 目标

1. fastagent 成为**标准 MCP OAuth 客户端**（authorization-code + PKCE S256 + refresh 旋转）。
2. 在 headless 服务器部署下，用户能**完整走完授权流程**（三种路径，见 §6）。
3. 遵循 Clean Architecture：领域纯逻辑、用例编排、端口解耦、框架只接线。
4. 多用户多 Agent 凭证隔离；凭证永不进入 Agent 上下文。
5. 对既有代码零破坏：静态 header 的 MCP server 继续可用。

---

## 1. 架构总览（四层映射）

```
┌─────────────────────────────────────────────────────────────────────┐
│ Framework & Drivers    net/http · config · internal/store · agent loop │
│   config.MCPServerConfig(+oauth)   internal/mcp/http.go(改造)          │
│   internal/agent/loop.go(接线)     cmd/fastclaw(CLI)  internal/setup   │
│        │  依赖（实现端口）                │                              │
│        ▼                                ▼                              │
├─────────────────────────────────────────────────────────────────────┤
│ Interface Adapters     实现 Ports，不依赖 Use Case 以上              │
│   internal/mcp/oauth/adapter/                                          │
│     http_exchange.go   → AuthorizationCodeExchanger / RefreshClient   │
│     metadata_fetcher.go→ MetadataFetcher                              │
│     token_store.go     → TokenStore（加密落盘, per user+agent）        │
│     pending_store.go   → PendingAuthStore                             │
│     registration_store.go → ClientRegistrationStore                   │
│     browser.go         → BrowserOpener（含 no-browser/粘贴模式）      │
│     loopback.go        → CallbackReceiver（CLI 本地回调）             │
│   internal/setup/handlers_oauth.go（HTTP 薄适配，复用 Use Case）      │
│        │  只依赖端口接口                                              │
│        ▼                                                              │
├─────────────────────────────────────────────────────────────────────┤
│ Application (Use Cases + Ports)  编排，声明端口接口，不 import 适配器 │
│   internal/mcp/oauth/usecase/                                         │
│     start.go      StartAuthorization(…)→ authURL                     │
│     complete.go   CompleteAuthorization(callbackParams)→ok           │
│     refresh.go    RefreshToken(…)→ accessToken                       │
│     revoke.go     RevokeToken(…)                                     │
│   internal/mcp/oauth/port/      ← 所有端口接口（见 §3.2）             │
│        │  只依赖 Domain                                              │
│        ▼                                                              │
├─────────────────────────────────────────────────────────────────────┤
│ Entities (Domain)  纯 Go，零 I/O，零外部 import                       │
│   internal/mcp/oauth/domain/                                         │
│     tokens.go        OAuthTokens / PendingAuth / ClientRegistration   │
│     discovery.go     DiscoveryMetadata + 解析                          │
│     authurl.go       授权 URL 构建（纯函数）                          │
│     callback.go      state/issuer/error 校验（纯函数）               │
│     pkce.go          code_verifier/challenge(S256)（纯函数）         │
└─────────────────────────────────────────────────────────────────────┘
```

**依赖规则**：所有源码依赖**向内**。Domain 零依赖；Use Case 只依赖 Domain + 端口接口；Adapter 实现端口；Framework 依赖端口（通过接口注入），绝无反向依赖。

---

## 2. 领域层（Entities）— 纯逻辑

位置：`internal/mcp/oauth/domain/`。零外部依赖（只 `crypto/rand`、`crypto/sha256`、`encoding/base64`、`net/url`、`time`）。

### 2.1 核心模型

```go
type OAuthTokens struct {
    AccessToken  string    `json:"access_token"`
    RefreshToken string    `json:"refresh_token,omitempty"`
    ExpiresAt    time.Time `json:"expires_at"`   // access token 过期时刻
    Scopes       []string  `json:"scopes,omitempty"`
    Issuer       string    `json:"issuer"`        // RFC 9700：token 归属的 issuer
}

type PendingAuth struct {
    State          string    // crypto-random，一次性
    CodeVerifier   string    // PKCE，只在服务器内存/短时存储，绝不下发
    Scopes         []string
    UserID         string    // 绑定发起者
    AgentID        string
    ServerName     string    // 绑定的 MCP server 名
    CallbackMode   CallbackMode // issuer-bound 或 callback-specific
    CreatedAt      time.Time
    ExpiresAt      time.Time  // TTL 默认 10min
}

type ClientRegistration struct {
    ClientID         string
    RedirectURIs     []string
    TokenEndpointAuthMethod string // 恒为 "none"
    RegisteredAt     time.Time
}

type DiscoveryMetadata struct {
    Issuer                    string
    AuthorizationEndpoint     string
    TokenEndpoint             string
    RevocationEndpoint        string
    ScopesSupported           []string
    CodeChallengeMethods      []string
    IssParamSupported         bool   // authorization_response_iss_parameter_supported
}
```

### 2.2 纯函数

| 函数 | 职责 |
|---|---|
| `NewPendingAuth(userID, agentID, serverName, scopes)` | 生成 state + PKCE verifier，计算 TTL |
| `BuildAuthorizationURL(md, reg, pending)` | 拼接 `authorization_endpoint?response_type=code&client_id=…&redirect_uri=…&code_challenge=S256&code_challenge_method=S256&state=…&scope=…&resource=<serverURL>` |
| `ValidateCallback(state, expectedState, callbackParams)` | state 匹配（缺失即拒）；`error` 参数失败关闭；**不回显 error_description**（攻击者可控） |
| `ValidateIssuer(md, iss)` | 仅当 `IssParamSupported` 时校验 iss 与 metadata.issuer 一致 |
| `NewPKCE()` / `S256Challenge(verifier)` | 生成与计算 challenge |
| `TokenNeedsRefresh(tokens, now)` | 过期判定（含缓冲，默认提前 60s） |

> 领域层不碰网络、不碰存储、不碰浏览器。所有规则可单测（纯函数，表驱动测试）。

**RFC 8707（Resource Indicators for OAuth 2.0）**：所有与受保护资源相关的 OAuth 请求都携带 `resource=<MCP server URL>`（绝对 URI，取自该 server 的 `oauthResource`/连接 URL）——**authorize、token exchange、refresh、revoke 一律带**。这是标准扩展而非供应商私有参数：Quandora 的 AS 在 authorize 缺 `resource` 时返回 422，在 token endpoint 缺 `resource` 时返回 `invalid_request: missing token request field`（2026-09-04 实测）；不支持该参数的 AS 按 OAuth 约定忽略未识别参数。早期"资源在授权阶段已绑定、token/refresh 不携带"的假设已被实测推翻，实现按 RFC 8707 全程透传。发现阶段若根 `.well-known` 缺失，回退解析资源 401 的 `WWW-Authenticate`（`auth-issuer`，后续可扩展 RFC 9728 `resource_metadata`）。`IssuerBound` 模式（AS 声明 `authorization_response_iss_parameter_supported`）下 authorize 请求额外回带 `iss`（RFC 9207 混淆防护）。

---

## 3. 应用层（Use Cases + Ports）

位置：`internal/mcp/oauth/usecase/`（用例）+ `internal/mcp/oauth/port/`（端口接口）。

### 3.1 用例清单

```go
// StartAuthorizationUC：发起授权
// 输入 userID, agentID, serverName, scopes, callbackURL
// 流程：解析 discovery → 注册/复用 client → 建 PendingAuth → 构建 authURL
// 返回 { authURL, state }（供前端跳转/CLI 打印）
func (uc *StartAuthorization) Execute(ctx, in StartAuthInput) (StartAuthOutput, error)

// CompleteAuthorizationUC：完成授权
// 输入 callbackParams(code, state) 或粘贴的 redirect URL
// 流程：校验 state → 校验 issuer → 用 code+verifier 换 token → 加密存储 → 删 PendingAuth
func (uc *CompleteAuthorization) Execute(ctx, in CompleteAuthInput) error

// RefreshTokenUC：刷新 access token
// 输入 userID, agentID, serverName
// 流程：加载 tokens → 判过期 → refresh（带锁，防并发刷新）→ 存回 → 返回
func (uc *RefreshToken) Execute(ctx, in) (string, error)

// RevokeTokenUC：吊销
// 输入 userID, agentID, serverName
// 流程：调 revocation_endpoint → 删除本地 token
func (uc *RevokeToken) Execute(ctx, in) error

// TokenProvider：供 MCP 客户端请求时取可用 token（内部先刷新再返回）
func (p *TokenProvider) AccessToken(ctx, userID, agentID, serverName) (string, error)

// StatusUC：查询某 agent 某 server 是否已授权（供 UI 展示）
func (uc *Status) Execute(ctx, in) (StatusOutput, error)
```

### 3.2 端口接口（Ports）

```go
type MetadataFetcher interface {
    Fetch(ctx, serverURL string) (*domain.DiscoveryMetadata, error)
}
type AuthorizationCodeExchanger interface {
    Exchange(ctx, tokenEndpoint string, code, verifier, redirectURI string, reg domain.ClientRegistration) (*domain.OAuthTokens, error)
    Refresh(ctx, tokenEndpoint string, refreshToken string, reg domain.ClientRegistration) (*domain.OAuthTokens, error)
    Revoke(ctx, revocationEndpoint string, token, clientID string) error
}
type TokenStore interface {   // 加密存储，per (userID, agentID, serverName)
    Save(ctx, key string, t *domain.OAuthTokens) error
    Load(ctx, key string) (*domain.OAuthTokens, error)
    Delete(ctx, key string) error
}
type PendingAuthStore interface {
    Save(ctx, p *domain.PendingAuth) error
    Take(ctx, state string) (*domain.PendingAuth, error)  // 读取即删除（一次性）
}
type ClientRegistrationStore interface {
    Get(ctx, serverName string) (*domain.ClientRegistration, error)
    Save(ctx, serverName string, r *domain.ClientRegistration) error
}
type BrowserOpener interface {
    Open(ctx, url string) (handled bool, err error)  // false=未打开浏览器（no-browser）
}
type CallbackReceiver interface {   // CLI loopback 专用
    Listen(ctx, port int) (<-chan CallbackResult, error)
}
type KeyFunc interface {   // 派生加密密钥（Adapter 提供，由 config secret 派生）
    Encrypt(ctx, plaintext []byte) ([]byte, error)
    Decrypt(ctx, ciphertext []byte) ([]byte, error)
}
```

> 所有适配器都实现这些接口；Use Case 通过构造注入接口，**可整体替换**（内存 store ↔ DB store ↔ keyring）。

---

## 4. 接口适配层（Adapters）

位置：`internal/mcp/oauth/adapter/`。实现 §3.2 端口；**不 import** Use Case 以上。

| 适配器 | 实现 | 备注 |
|---|---|---|
| `http_exchange.go` | `AuthorizationCodeExchanger` | `POST {token_endpoint}`，`Content-Type: application/x-www-form-urlencoded`，解析 `access_token/refresh_token/expires_in/scope` |
| `metadata_fetcher.go` | `MetadataFetcher` | `GET {issuer}/.well-known/oauth-authorization-server`；失败回退从 `WWW-Authenticate: Bearer auth-issuer=…` 取 issuer 再 fetch |
| `token_store.go` | `TokenStore` | 密文落盘 `~/.fastagent/oauth/{userID}/{agentID}/{serverName}.json`，用 KeyFunc 加密；原子写（tmp+rename）；权限 0600 |
| `pending_store.go` | `PendingAuthStore` | 内存 TTL 缓存 + DB 兜底；`Take` 读即删保证一次性 |
| `registration_store.go` | `ClientRegistrationStore` | 首次注册后持久化 client_id，避免重复注册 |
| `browser.go` | `BrowserOpener` | 分平台 `open`；headless 返回 `handled=false` → 走粘贴模式 |
| `loopback.go` | `CallbackReceiver` | `net.Listen("tcp","127.0.0.1:0")` 空闲端口，路径 `/callback[/{callbackID}]`（codex callback-specific 模式，多 server 防串） |

HTTP 薄适配层：`internal/setup/handlers_oauth.go`（见 §5.4）。

---

## 5. 框架层（Frameworks & Drivers）

### 5.1 config 扩展（`internal/config/config.go`）

```go
type MCPServerConfig struct {
    Type    string            `json:"type"`
    URL     string            `json:"url,omitempty"`
    Headers map[string]string `json:"headers,omitempty"`
    Command string            `json:"command,omitempty"`
    Args    []string          `json:"args,omitempty"`
    Env     map[string]string `json:"env,omitempty"`

    // —— 新增（可选，仅 OAuth 保护的服务需要）——
    OAuthResource string   `json:"oauthResource,omitempty"` // 默认取 URL
    Scopes        []string `json:"scopes,omitempty"`        // 默认取 scopes_supported
    CallbackURL   string   `json:"callbackURL,omitempty"`   // 默认：HTTP 回调用公网 URL / 走 CLI loopback
}
```

声明的逻辑形状（统一存 `agent_mcp_servers` 每行 `config` 列，JSON 形状如下；`agent.json` 已废弃，配置全部走 DB）：

```json
"mcpServers": {
  "quandora": {
    "type": "http",
    "url": "https://mcp.quandora.ai/quant",
    "oauthResource": "https://mcp.quandora.ai/quant"
  }
}
```

兼容性：`oauthResource` 缺省 = 无 OAuth → 走既有静态 header 路径，**零行为变化**。

### 5.2 MCP HTTP 客户端改造（`internal/mcp/http.go`）

最小侵入（已实现）：
- `HTTPClient` 持有可选 `auth func(ctx context.Context) (string, error)`，由 `SetAuthProvider` 注入；`nil` = 静态 header 路径不变。
- 每次请求发送前调用 `auth` 取 token 并注入 `Authorization: Bearer <token>`——认证发生在传输层，**不进工具参数、不进 agent 上下文**。
- 收到 `401` 时强制调用 `auth` 一次（内部走预刷新/refresh 旋转）并重放**恰好一次**；第二次 401 直接失败，防环路。
- `HTTPClient` 捕获 initialize 响应头的 `Mcp-Session-Id`，后续 `tools/list` / `tools/call` 自动附带（Streamable HTTP 2025-11-25；Quandora 缺失时返回 400 / `-32600 Invalid Request`）。

```go
// SetAuthProvider wires an OAuth bearer-token provider. When set, every
// request gets Authorization: Bearer <token> and a 401 triggers exactly
// one refresh-and-replay (never a loop).
func (c *HTTPClient) SetAuthProvider(f func(ctx context.Context) (string, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.auth = f
}
```

身份（userID / agentID / serverName / actorUserID）不通过 HTTPClient 参数传递，全部收进 provider closure，由框架层在 agent 构建时绑定（见 §5.3）；HTTPClient 只负责「拿到字符串 → 加 Bearer → 401 单次重放」。

### 5.3 Agent 回路接线（`internal/agent/loop.go`）

agent 构建时经 ManagerOption 为声明了 `oauthResource` 的 server 注入 Bearer provider（scheme-A owner 门控在 provider/use case 层，不在接线层）：

```go
// 对每台 OAuth server（mcpOAuthManagerOptions 内）：
opts = append(opts, mcp.WithAuth(serverName, func(ctx context.Context) (string, error) {
	return ob.Provider.AccessToken(ctx, usecase.RefreshInput{
		UserID:      rc.UserID,      // 凭证归属 = agent owner
		AgentID:     rc.ID,
		ServerName:  serverName,
		ServerURL:   resource,       // RFC 8707 resource / discovery 入口
		ActorUserID: actorUserID,    // 会话主体（UserSpace 用户）
	})
}))
mcpMgr := mcp.NewManager(rc.MCPServers, mcpOpts...) // 内部对每台 http client 调 SetAuthProvider
```

要点：
- `actorUserID != rc.UserID`（跨 UserSpace 访客）时 `AccessToken` 在**任何 store 读取 / HTTP 请求之前**返回 `ErrCredentialOwnerOnly`——门控是用例行为，不是接线层的 if。
- `mcp_<server>_<tool>` 工具代理由老代码在 server 连接成功后逐个注册；本方案只新增 auth 接线与门控，静态 header server 不加 auth、行为零变化。

### 5.4 HTTP 薄适配层（`internal/setup/handlers_oauth.go`）

复用既有 `requireSuperAdmin` / `auth` 中间件约定：

| 路由 | 方法 | 鉴权 | 行为 |
|---|---|---|---|
| `/api/mcp/oauth/start` | POST | auth（user session） | 返回 `{authURL, state}` |
| `/oauth/quandora/callback` | GET | **公开**（无 session） | 转发 code+state 到 `/api/mcp/oauth/callback`（admin key）→ 302 回 SPA |
| `/api/mcp/oauth/callback` | POST | super_admin | 完成授权，返回重定向目标 |
| `/api/mcp/oauth/status` | GET | auth | 已授权/未授权/过期（供 UI） |
| `/api/mcp/oauth/revoke` | POST | auth | 吊销 |
| `/api/mcp/oauth/refresh` | POST | super_admin | 手动刷新（运维用） |

> 公开 callback 路由不依赖 cookie/session（浏览器无登录态），安全性由 **state + 一次性 + TTL + PKCE** 保证（§7）。cloud 转发到 `/api/mcp/oauth/callback` 用 `FASTAGENT_ADMIN_API_KEY`（`requireSuperAdmin`），防绕过 provider 伪造回调。

### 5.5 CLI（`cmd/fastclaw/cmd_mcp_login.go`）

```
fastclaw mcp login <name> [--no-browser] [--oauth-resource <url>]
fastclaw mcp status <name>
fastclaw mcp logout <name>
```

- 默认：起 loopback 监听 → 开浏览器 → 自动完成。
- `--no-browser`（headless/SSH）：打印 auth URL，提示用户**把浏览器里重定向后的完整 URL 粘贴回来**（对齐 `claude mcp login --no-browser`），解析出 code+state 完成。

### 5.6 agent 工具 `mcp` action 清单（L2 host 级发起器）

`mcp` 是 agent 侧的管理工具（built-in，注册于 `internal/agent/loop.go`），语义是 **host 级发起器的模型映射**：code exchange 永远由宿主回调 / CLI loopback 完成，code 不进对话；所有动作先过 **owner 门控**（actor ≠ agent owner 直接拒绝，见方案 A）。注册条件与可见性分离：`oauth.Global() != nil`（MCP OAuth 启用）即注册——**即使该 agent 尚无任何 server**（否则模型无法 `mcp add` 第一条）；每轮按 prompt mode 过滤暴露（agent mode 全量 built-in；chatbot/customize 的 allowlist 不含 `mcp`，MCP server 工具本身仍常驻）。server 声明存 **`agent_mcp_servers` per-key 行**（见 §13.2），add/remove 互为逆操作（可逆性设计见 §13）。

| action | 参数 | 语义 | 宿主/协议对应 | 状态 |
|---|---|---|---|---|
| `add` | serverName, url, oauthResource?, scopes? | 在 `agent_mcp_servers` 插入一行（`AddMCPServer`，PK 前置条件：同名不存在）；拒绝重复名/非 http(s)/scopes 无 oauthResource；落库成功后 `mcpConfigNotify` 跨实例 reload。**可逆：`remove`** | `internal/agent/mcp_config_tool.go` + store adapter | ✅ 2026-09-04 |
| `remove` | serverName | 删除该 server 行（`DeleteMCPServer`，前置条件：存在）；不删除/不吊销 OAuth 凭证（凭证按 `K_cred` 键独立保留）。**可逆：`add`（同名同字段重加即恢复授权）** | 同上 | ✅ 2026-09-04 |
| `undo` | 无（可重复调用） | 从**当前会话**的 `session_events` trace（tool_result 携带 `<mcp-undo>` 逆参数 marker）读取最近未消费的 add/remove，按 LIFO 自动重放逆操作；消费游标存 `configs_kv`（kind=`mcp_undo`，name=`undo:<sessionKey>`）。**范围限定当前会话**（方向 A：活跃会话记录必然完整，删除的其它会话不影响本会话 LIFO）；只自动重放边界内声明操作；login/logout 涉及 consent 不走自动 undo | `internal/agent/mcp_undo.go` + store `ListSessionEventsSince` | ✅ 2026-09-04 |
| `login` | serverName | 生成 authorization_url（RFC 8707 `resource` + PKCE S256 + state + scope），由宿主回调完成换码 | setup `POST /api/mcp/oauth/start` + 宿主 callback | ✅ |
| `status` | serverName? | **只读本地**凭证状态（none/authorized/expired），不触网 | setup `GET /api/mcp/oauth/status`（同一 usecase） | ✅ |
| `check` | serverName | 真实 MCP `initialize` + `tools/list`（带 Bearer，401 自动 refresh 一次重放），端到端验证「连上了」 | MCP HTTP 客户端（`internal/mcp/http.go`）+ `TokenProvider` | ✅ 本轮补齐 |
| `refresh` | serverName | 主动执行 RefreshToken usecase：fresh 幂等 no-op，stale 旋转 refresh token；只回报有效期与 scope 数，不透出 token | `usecase/refresh.go`（自动刷新同源） | ✅ 本轮补齐 |
| `logout` | serverName | 调 revocation endpoint 吊销 + 删除本地凭证 | setup `POST /api/mcp/oauth/revoke`（同一 usecase） | ✅ |

> `check` 与 `status` 的差别是设计要点：`status` 只证明「本地有凭证」，服务端吊销 / issuer 迁移后可能已失效；`check` 才证明「带 token 真实可用」。agent 在需要确认连接时用 `check`，不要只信 `status`。

**L2-B（剩余未落地项）**：

| action | 语义 | 依赖 |
|---|---|---|
| `update` | 原地修改 server（url/oauthResource/scopes）——实现时必须满足 §13.8：携带旧值快照（返回旧 entry 或版本回退），逆 = 恢复旧值 | 配置变更面、安全评审 |
| `allow`/`deny` | owner 审批非 owner 的使用请求 | 方案 B/C（每用户授权 / owner 白名单）产品决策 |

> `add`/`remove` 已按 §13 最小路径落地（per-key `agent_mcp_servers` + notify，无目录源/审批/自动声明依赖）；SKILL.md 自动声明仍见 §5.7。

**与 MCP 2026-07-28 RC 六个 OAuth SEP 的映射**：SEP-2468（`iss` 校验）与 SEP-2351（discovery 后缀）落在 complete/discovery 内部，不是 action；SEP-837（DCR `application_type`）在 `adapter/client_registration.go`（当前声明 `native`，云 web 回调形态可评审是否切 `web`）；SEP-2207（OIDC 风格 refresh）即 `refresh` 动作与自动刷新路径；SEP-2350（scope 累积）影响 `login` 的参数语义（增量 scope 需设计，当前取 config/metadata 全量）；SEP-2352（注册凭据绑定 issuer + 迁移重注册）当前未实现，注册按 (serverName, callbackURL) 键复用。

### 5.7 L2-B：SKILL.md / catalog 声明的 MCP server 自动接入（阶段 1 已落地；阶段 2 设计未实现）

> **状态（2026-09-04）**：阶段 1 的模型驱动路径已按 **最小实现** 落地——agent 工具 `mcp add/remove` 直接读写 **`agent_mcp_servers` per-key 行**（`internal/agent/mcp_config_tool.go` + store adapter），落库成功后才经 `mcpConfigNotify`（InvalidateAgent + configs_kv epoch + Redis pub/sub）跨实例收敛；未为单一调用方引入四层 usecase（YAGNI，见 §13.7）。阶段 2（SKILL.md frontmatter 自动声明）仍未实现，下述四层映射保留为阶段 2 / 出现第二个安装方时的蓝图。

**目标**：SKILL.md（或未来 catalog）声明 `{name, url, oauthResource?, scopes?}` → fastagent 把该 server 写入 **声明存储**（`agent_mcp_servers` 每行一 server；`agent.json` 已废弃，配置统一 DB）→ `notifyAgentChanged`（ReloadAgents + configs_kv epoch + Redis pub/sub，与 OAuth 完成同一条广播）→ 新会话里 `mcp_<name>_<tool>` 注册可用。skill 只负责“声明 + 使用说明”，连接仍走既有 `mcp.NewManager`。

**字段语义**（与 §5.1 一致）：`url` = MCP 传输端点（HTTP 必填）；`oauthResource` = RFC 8707 resource + discovery 入口，仅 OAuth 保护 server 需要（通常与 url 相同）；`scopes` 可选——缺省取 provider `scopes_supported` 全集。无鉴权 / 静态 header server 只需要 `url`（+headers）。

**四层映射（阶段 2 蓝图）**：

| 层 | 内容 | 位置 |
|---|---|---|
| Entities | `DeclaredMCPServer{Name, URL, OAuthResource, Scopes, Source}`；纯校验（http(s)、name 非空、oauthResource 与 url 一致性） | domain 纯函数，零依赖 |
| Use Cases | `InstallDeclaredServer.Execute(In{AgentID, OwnerID, Declared})`：读现有声明行 → 冲突拒绝（不覆盖）→ 写 `agent_mcp_servers` → 返回变更摘要 | 新 usecase，依赖 port |
| Interface Adapters | ① SKILL.md frontmatter 解析器（复用现有 frontmatter 读取，只新增声明字段解析）② store adapter（读写 `agent_mcp_servers`，复用 `AddMCPServer` 等） | adapter 层 |
| Frameworks & Drivers | 触发入口：agent 工具 `mcp` 的 `add` action（当前由最小路径直写 store 提供）；安装链路；`notifyAgentChanged` 广播 | agent loop / setup |

**端口（DIP）**：`AgentConfigPort`（GetMCPServers / UpsertMCPServer）、`AgentNotifyPort`（ReloadAgent，复用 OAuth 失效广播）、校验阶段 `OAuthStatusPort`（ServerRequiresAuth，判断是否需要 consent 前置）。

**两条触发路径**：
1. 模型驱动（阶段 1，**已落地**）：SKILL.md 文本指导模型调 `mcp add <serverName> --url … [--oauth-resource …] [--scopes …]`（owner 门控）；add 插行 → notifyAgentChanged → 新会话工具出现。当前为最小实现（无 usecase/domain 分层），语义与本蓝图一致。
2. 自动声明（阶段 2，未实现）：解析 SKILL.md frontmatter（`metadata.fastagent.mcp.servers[]`），skill 加载/安装时把声明交给 usecase——此时出现第二个安装方，才落地上面的四层抽象。

**关键规则**：add 拒绝非 http(s)、name 已存在（PK 前置条件 `ErrMCPServerExists`）、scopes 无 oauthResource；只写持久化不做内存态；add/remove 落库成功后才走 `notifyAgentChanged` 跨实例收敛。secret 未启用时 `mcp` 工具本身不注册（`oauth.Global() == nil`），故 add 侧天然写不进 OAuth server——阶段 2 自动声明路径仍需显式检查该前提。安全备注（暂不实现）：SSRF url 预检复用 blocked-addr；任意 skill 自动装 server 需 owner 审批，不静默执行。

---

## 6. 交互流程设计（用户友好）

### 路径 1：Web 控制台（主路径，零粘贴）

```
[SPA] 点「授权 Quandora」
  └ POST /api/mcp/oauth/start → {authURL}
  └ window.location.assign(authURL)         ← 整页跳转
  └ [app.quandora.ai] 登录 + consent
  └ 302 → https://app.tokenaissance.com/oauth/quandora/callback?code=…&state=…
  └ [Cloud] 转发(admin key) → POST /api/mcp/oauth/callback
  └ fastagent 换 token 存密文 → 302 → /settings?mcp=authorized
```

### 路径 2：CLI loopback（本机/开发者，自动完成）

```
fastclaw mcp login quandora
  └ 起 loopback 监听(空闲端口, /callback/<hash(server_url)>)
  └ 动态注册 client（redirect_uris 含该 loopback）
  └ 开浏览器 → 授权 → 浏览器回跳 127.0.0.1 监听器捕获 code+state
  └ 校验 → 换 token → 加密存储 → 显示 "✓ Authorized"
```

### 路径 3：CLI `--no-browser`（SSH / headless 服务器）

```
fastclaw mcp login quandora --no-browser
  └ 打印: Open the following address in your browser. After authorizing,
  └       paste back the FULL redirected URL from the address bar:
  └   https://mcp.quandora.ai/oauth/authorize?client_id=…&redirect_uri=http%3A%2F%2F127.0.0.1%3A54321%2Fcallback…
  └ 用户浏览器授权 → 浏览器跳到 127.0.0.1:54321（用户机器上显示无法连接）
  └ 用户把 URL 栏的完整地址粘贴 → fastclaw 解析 code+state → 换 token → 存储
```

### 路径 4：agent 会话内（`mcp login` 管理工具，2026-09-04）

agent 侧 `mcp` 工具是 **host 级发起器**（§5.6）：模型产 URL、宿主完成，code 不进对话。

```
[agent 会话] 用户："用 Quandora 跑个回测"
  └ 模型调 mcp status（无 serverName → 列出 OAuth servers + 状态）
  └ 若 server 未配置 → mcp add quandora <url> [oauthResource] [scopes]
  └     （owner 门控；写入 agent_mcp_servers 行；与 remove 互为逆，见 §13）
  └ 若 none/expired → mcp login quandora
  └     StartAuthorization.Execute（同路径 1：discovery→DCR→pending）
  └     callbackBase = FASTAGENT_OAUTH_CALLBACK_BASE（服务端部署必配）
  └ 模型把 authUrl 交给 owner → owner 浏览器 consent
  └ 302 → https://app.tokenaissance.com/oauth/mcp/{id}/callback?code&state
  └ [Cloud] 转发(admin key) → POST /api/mcp/oauth/callback → 换 token 存密文
  └ notifyAgentChanged：ReloadAgents + epoch + Redis pub/sub（跨实例收敛）
  └ agent 重建/新会话后注册 mcp_quandora_* 工具代理

[授权后每次业务调用] 模型调 mcp_quandora_quant(args)
  └ registry → mcpMgr.CallTool → HTTP client 注入 Authorization: Bearer
  └     （TokenProvider.AccessToken：owner 门控 → Load → 过期则锁内刷新旋转）
  └ 401 → 恰好一次 refresh 重放（绝不循环）→ 仍失败则提示 mcp check / 重新 login
```

> 四条路径共享同一 Use Case（start/complete），只是触发面（SPA / CLI / agent 工具）与 CallbackReceiver/BrowserOpener 适配器不同 —— 这就是端口解耦的价值。完整时序见 tokenaissance-cloud `docs/fastagent/guides/auth/mcp-oauth-authorization-flow.md` §2.1/§2.2。cloud chat 会把路径 4 的授权 URL 渲染成授权按钮卡（popup + 轮询状态，不展示长链接），交互细节见该文档 §4/§7（2026-09-05）。

---

## 7. 安全性设计

### 7.1 威胁模型 → 对策

| 威胁 | 对策 | 落地位置 |
|---|---|---|
| code 被截获 | **PKCE S256**：verifier 只存服务器，code 单独无价值 | usecase/complete |
| 回调注入 / CSRF | **state**：crypto-random、一次性（`Take` 读即删）、绑定 userID+agentID | usecase/complete |
| 回调被劫持到别处 | **redirect_uri 校验**：provider 只重定向已注册 callback；动态注册时仅注册受控 URI | usecase/start |
| Mix-up（code 发错 token endpoint） | RFC 9700：`iss` 支持则校验 issuer；否则 **callback-specific** 路径 `hash(server_url)` | domain/callback |
| 长期凭证泄露 | refresh token **加密落盘**（KeyFunc，密钥派生自 config secret）、0600、原子写 | adapter/token_store |
| 跨用户/跨 Agent 越权 | TokenStore/PendingAuth 键均含 `userID/agentID`；handler 从 session 取身份 | adapter/token_store |
| 访客会话借用 owner 凭证（成本 + 数据边界） | **方案 A 门控**：会话主体（actor）≠ agent owner 即拒绝，先于任何 store/HTTP；访客连 OAuth server 的权限都没有 | usecase/token_provider + agent/loop |
| 伪造回调直达 fastagent | cloud→fastagent 用 admin key（`requireSuperAdmin`），public 路由只薄转发 | setup/handlers_oauth |
| 凭证进 Agent 上下文 | 刷新仅发生在 MCP client 层；skill/agent 提示词零接触 token | agent/loop + §5.2 |
| 并发刷新（token 旋转竞态） | `refresh_lock`：per (user,agent,server) 单飞刷新 | usecase/refresh |
| error_description 攻击 | **不回显** provider 返回的 error 描述（Codex 同款） | domain/callback |
| 待授权会话堆积 | pending_auth TTL 10min + 数量上限 + 兜底清理 | usecase/start |
| 日志泄露 | 日志/URL 永不落 token/code；scrub 复用 `env_scrub` 机制 | framework |

### 7.2 凭证生命周期

```
authorized ── access 过期 ──> refresh（带锁） ──> authorized
authorized ──> revoke（调 revocation_endpoint + 删本地）
authorized ──> logout ──> 删本地（可选远端）
```

### 7.3 安全边界声明

- 浏览器授权页属于 provider 域（app.quandora.ai），consent 页由 provider 展示 scopes。
- fastagent 只做标准 OAuth 客户端职责，**不接触**用户密码/API key。
- 使用边界 = **agent owner**：`(user, agent, server)` 凭证只有 owner 自己的 session 能解析（方案 A）。owner 把 agent 公开分享，不等于把 Quandora 账户授权给**跨 UserSpace 访客**（公开链接 / apikey / super_admin）——这类访客 session 里这些工具直接不可用；owner 频道内的共享会话（群聊成员）当前仍可用（UserSpace 租户级语义，见 §7.4 草案）；owner 若要给他人使用须走方案 B/C（另行设计）。
- 所有 token 操作走端口接口，便于未来换 keyring / KMS。

### 7.4 严格 A：per-turn 发起人门控（设计草案，未实现）

**问题**：现有方案 A 的门控锚点是 **UserSpace**（agent 构建期静态 actor = Manager.uid），不是消息发起人。群聊/频道消息统一路由到 channel owner 的 UserSpace（`routeDM`/`routeGroup` 都执行 `g.users.getOrLoad(msg.OwnerUserID)`），于是：

- agent 实例 actor = owner → 门控通过；
- 群里任何成员触发 MCP 工具都带 owner 的 Bearer token。

一般协作产品的最佳实践是：**把「聊天所在的空间（tenant）」与「触发操作的个人（principal）」解耦**；个人账户类工具（Quandora 属此类：个人数据 + 个人配额）默认只代表发起人本人，否则显式拒绝——不能隐式借用 bot owner 的账户。

**目标语义（严格 A）**：`(user, agent, server)` 凭证只允许**等价于 agent owner 本人的消息发起人**触发；owner 频道内的其他成员（tenant-chatter）与跨租户访客（foreign）一律拒绝。

#### 7.4.1 身份模型与 owner 等价谓词

现有可用的 owner 判定只覆盖 web/api/cron：`msg.UserID == rc.UserID`（FastAgent UUID 相等）。IM 侧 `resolveChatter` 会把包括 owner 本人内的所有发送者铸成 channel owner 名下的 app_user，因此不能靠 ID 相等。

设计在 routing seam 新增显式解析，输出三类 principal：

| principal | 判定 |
|---|---|
| `owner` | web/api/cron：`msg.UserID == agent owner`；IM：发送者外部 ID ∈ **owner 本人平台 ID 清单**（见配置） |
| `tenant-chatter` | channel owner 空间内的其他成员（外部 ID 不在清单内） |
| `foreign` | 跨 UserSpace 会话（公开链接访客 / apikey app_user / super_admin 操作他人 agent） |

owner 本人平台 ID 清单：agent 级新增 `oauthOwnerIdentities[channel] = [平台ID...]`（**不复用 `admins[channel]`**——admin = "可管理机器人"，不等于授权访问 owner 的 Quandora 数据；复用等于变相实施方案 B）。清单默认空：

- IM 渠道：空清单 → 所有 IM 消息拒绝 OAuth（fail-closed）；
- web/api：不受清单影响；
- 本地单用户模式（无多租户、无 DB）：保持"actor 缺省 = owner"兼容路径。

#### 7.4.2 数据流：principal 一路透传到 auth()

澄清：§7.4.1 的 owner 等价判定在 **routing seam** 完成并写入消息身份字段；但 routing 经 bus 投递消息、本身不持有 ctx 链，因此 **principal 的 ctx 值在 agent loop 入口（runOnce）stamp**，而不是"从 routing 透传 ctx"。真正要修的 ctx 断点有两处：`mcp.Manager.CallTool(_ context.Context, …)` 丢弃 ctx；`HTTPClient` 的 `auth(context.Background())` 再丢一次。改动链（未实施）：

```
runOnce(msg)
  └─ stamp ctx: withSessionPrincipal(owner | tenant-chatter | foreign)
       └─ registry tool call → mcpMgr.CallTool(ctx, …)
            └─ HTTPClient.CallTool(ctx, …) → sendRequest(ctx, …)
                 └─ auth(ctx)                          // 不再 background
                      └─ loop.go closure: principal := FromContext(ctx)
                           └─ TokenProvider.AccessToken(RefreshInput{
                                UserID: rc.UserID,
                                ActorUserID: principal })   // policy 不变
```

分层约束：`TokenProvider` policy 不变（actor ≠ owner → `ErrCredentialOwnerOnly`），ctx 键是 framework 概念，由 loop 闭包解包成参数——usecase 不感知 ctx，依赖方向不变。

#### 7.4.3 两级门控

| 阶段 | 检查 | 结果 |
|---|---|---|
| build（agent 实例创建） | UserSpace actor == owner（现状，保留） | 跨 UserSpace 附加：不连 server、工具不注册 |
| per-turn 工具调用 | ctx principal == owner | owner 放行；tenant-chatter / foreign 拒绝，错误回模型并解释"仅 owner 可用" |

可选项（暂不做）：owner 空间的 OAuth server 连接/工具列举仍发生在 agent build（可能由群成员第一条消息触发，用 owner token 只取元数据）。若确认 server `initialize` 也算敏感，再加"首个 owner 消息才懒连接"。

#### 7.4.4 行为矩阵（补完后）

| 场景 | 结果 |
|---|---|
| owner web/api 会话 | ✅ 工具注册 + 调用通过 |
| 访客公开链接（跨 UserSpace） | ⛔ build 期即拒，0 HTTP |
| owner 本人 IM（ID 在清单） | ✅ 通过 |
| owner 频道群聊其他成员 | ⛔ 工具可见但调用拒绝，模型明确告知（待确认 UX） |
| 单用户本地（无身份系统） | ✅ 兼容路径（actor=owner） |

#### 7.4.5 测试与文档计划（实现时执行）

- usecase UT：现有 gate 测试不变；补 principal 枚举 → `ActorUserID` 映射的拒绝路径。
- agent UT：`oauthOwnerIdentities` 清单匹配（命中 / 未命中 / 空清单 / 单用户回退）。
- e2e：owner 空间内两条消息（owner 本人 vs 群成员）→ 前者 `tools/call` 带 Bearer 成功，后者拒绝且不带 owner token；HTTP client `CallTool(ctx)` ctx 透传断言。
- doc：本节约束下把方案 A 更新为"租户级 build 预筛 + per-turn 发起人门控"。

**待确认（2026-09-03）**：
1. IM owner 身份来源：新增 `oauthOwnerIdentities[channel]`（推荐）还是复用 `admins[channel]`？
2. 群成员拒绝 UX：工具保持可见、调用报"仅 owner 可用"（推荐），还是对非 owner 消息直接隐藏工具定义？

---

## 8. 功能完整性清单

> 状态说明：权威状态以顶部实施状态与 §13 为准；下表保留历史清单，已实现项已勾选。

- [x] MCP server 声明 OAuth：`oauthResource`/`scopes`/`callbackURL`；存储 = `agent_mcp_servers` per-key 行（2026-09-04 从 `agents.config` 迁出；`agent.json` 已废弃，配置统一 DB）
- [x] agent 工具 `mcp add/remove`：owner 门控、per-key 持久化、落库后 notify、add↔remove 互为逆（可逆性见 §13）
- [ ] 动态客户端注册 + 持久化 client_id
- [ ] 授权发起（start）：discovery → authURL（PKCE/state/scopes）
- [ ] 授权完成（complete）：code→token、加密存储、一次性 state
- [ ] 自动刷新：请求前预刷新 + 401 单次重放 + refresh_lock
- [ ] 吊销（revoke）+ 状态查询（status）
- [ ] 三种交互路径（Web / CLI loopback / CLI --no-browser）
- [ ] 多用户多 Agent 凭证隔离
- [ ] 静态 header MCP server 完全兼容（零行为变化）
- [ ] 测试：domain 纯函数单测 + usecase 集成（fake 适配器）+ e2e（mock token endpoint）
- [ ] 观测：`/api/mcp/oauth/status` 聚合展示；日志无敏感信息

---

## 9. 反过度设计（Musk 五步）

1. **Question**：需要接口抽象吗？—— 需要。TokenStore/PendingAuth 至少两个真实变体（内存/DB、密文/明文），BrowserOpener 有真实的分支（开浏览器/不开）。这是已验证的变化轴，不是臆想。
2. **Delete**：砍掉「多 provider 抽象」「通用 OAuth 引擎库」「自研 PKCE 平台」—— 只做 fastagent 需要的标准流。
3. **Simplify**：不引入第三方 OAuth SDK（Go 生态无统一权威库，自带 ~400 行标准流可控可测）；不建独立 microservice。
4. **Accelerate**：domain 纯函数表驱动测试；usecase 用 fake adapter 全内存快测。
5. **Automate**：CI 加 e2e（本地 mock token endpoint 起真实 HTTP 流转）。

**结论**：单仓库内 `internal/mcp/oauth/` 子包即可，不建新顶层目录、不引新服务、不引重量级依赖。

---

## 10. 依赖规则与代码骨架

### 10.1 目录

```
internal/mcp/oauth/
  domain/      # 纯领域（零依赖）
  port/        # 端口接口
  usecase/     # 用例编排
  adapter/     # 适配器实现
```

### 10.2 关键骨架（≤50 行）

```go
// domain/oauth.go —— 纯函数，无 I/O
func BuildAuthorizationURL(md *DiscoveryMetadata, reg *ClientRegistration, p *PendingAuth) string {
    q := url.Values{}
    q.Set("response_type", "code")
    q.Set("client_id", reg.ClientID)
    q.Set("redirect_uri", reg.RedirectURIs[0])
    q.Set("code_challenge", S256Challenge(p.CodeVerifier))
    q.Set("code_challenge_method", "S256")
    q.Set("state", p.State)
    if len(p.Scopes) > 0 { q.Set("scope", strings.Join(p.Scopes, " ")) }
    return md.AuthorizationEndpoint + "?" + q.Encode()
}

// usecase/start.go —— 依赖端口接口，不依赖实现
type StartAuthorization struct {
    Meta MetadataFetcher
    Reg  ClientRegistrationStore
    Pending PendingAuthStore
    Discovery domain.DiscoveryMetadata
}
func (uc *StartAuthorization) Execute(ctx, in StartAuthInput) (StartAuthOutput, error) {
    reg, err := uc.Reg.Get(ctx, in.ServerName)      // 复用已注册 client
    p, _ := domain.NewPendingAuth(in.UserID, in.AgentID, in.ServerName, in.Scopes)
    _ = uc.Pending.Save(ctx, p)
    return StartAuthOutput{AuthURL: BuildAuthorizationURL(&uc.Discovery, reg, p), State: p.State}, nil
}
```

> 依赖方向：`usecase` → `domain` + `port`；`adapter` → `port`；`framework` → `usecase`。无任何反向依赖。

---

## 附：参考实现对照

| 来源 | 可借鉴点 |
|---|---|
| MCP TypeScript SDK `CliOAuthClientProvider` | loopback 端口 `listen(0)`、host 校验 state、`isSafeBrowserUrl` |
| Codex `oauth_callback.rs` | callback-specific 路径 hash(server_url)（mix-up 防护）、issuer 绑定 |
| Codex `oauth.rs` | keyring + 文件回退、refresh_lock、startup 预刷新 |
| Claude CLI `mcp login --no-browser` | 打印 URL + 粘回完整 redirect URL（headless 范式） |

---

## 11. 实现代码（Go，落盘草案）

> ⚠️ **历史草案（2026-08-30 快照）**：§11–§12 是早期实现草案与评审记录，可能与最终代码不一致（例如 §11.4 文件 store 代码、旧 wiring 行号）。**权威内容以顶部实施状态与 §1–§10 为准**；当前实现见 `internal/mcp/oauth/` 及其测试。

> 模块路径：`github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/…`。按四层分包；依赖方向严格向内。

### 11.1 领域层 `internal/mcp/oauth/domain/oauth.go`

```go
package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"time"
)

// —— 领域模型 ——
type OAuthTokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"` // access token 过期时刻（UTC）
	Scopes       []string  `json:"scopes,omitempty"`
	Issuer       string    `json:"issuer,omitempty"`
}

type CallbackMode int

const (
	CallbackSpecific CallbackMode = iota // 路径绑定（默认，quandora 走此模式）
	IssuerBound                          // RFC 9700 iss 校验
)

type PendingAuth struct {
	State        string
	CodeVerifier string
	Scopes       []string
	UserID       string
	AgentID      string
	ServerName   string
	ServerURL    string
	CallbackURL  string
	Mode         CallbackMode
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

func (p *PendingAuth) Expired(now time.Time) bool { return now.After(p.ExpiresAt) }

type ClientRegistration struct {
	ClientID               string
	RedirectURIs           []string
	TokenEndpointAuthMethod string // 恒 "none"
	RegisteredAt           time.Time
}

type DiscoveryMetadata struct {
	Issuer                  string
	AuthorizationEndpoint   string
	TokenEndpoint           string
	RegistrationEndpoint    string
	RevocationEndpoint      string
	ScopesSupported         []string
	CodeChallengeMethods    []string
	IssParamSupported       bool // authorization_response_iss_parameter_supported
}

// —— 领域错误（fail closed）——
var (
	ErrInvalidState  = errors.New("oauth: state mismatch or missing")
	ErrDenied        = errors.New("oauth: authorization denied")
	ErrMissingCode   = errors.New("oauth: missing authorization code")
	ErrIssuerMismatch = errors.New("oauth: issuer mismatch")
)

// —— 纯函数 ——
const pendingAuthTTL = 10 * time.Minute

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// NewPKCE 生成 code_verifier（43 字符，RFC 7636）。
func NewPKCE() (verifier string, err error) {
	return randomToken(32) // 32 字节 → base64url 43 字符
}

func S256Challenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func NewPendingAuth(userID, agentID, serverName, serverURL, callbackURL string, scopes []string, mode CallbackMode) (*PendingAuth, error) {
	state, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	verifier, err := NewPKCE()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	return &PendingAuth{
		State:        state,
		CodeVerifier: verifier,
		Scopes:       scopes,
		UserID:       userID,
		AgentID:      agentID,
		ServerName:   serverName,
		ServerURL:    serverURL,
		CallbackURL:  callbackURL,
		Mode:         mode,
		CreatedAt:    now,
		ExpiresAt:    now.Add(pendingAuthTTL),
	}, nil
}

// BuildAuthorizationURL 拼接授权 URL（PKCE S256 + state + scope）。
func BuildAuthorizationURL(md *DiscoveryMetadata, reg *ClientRegistration, p *PendingAuth) (string, error) {
	if md.AuthorizationEndpoint == "" || reg.ClientID == "" || p.CallbackURL == "" {
		return "", errors.New("oauth: missing authorization endpoint, client id or callback url")
	}
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", reg.ClientID)
	q.Set("redirect_uri", p.CallbackURL)
	q.Set("code_challenge", S256Challenge(p.CodeVerifier))
	q.Set("code_challenge_method", "S256")
	q.Set("state", p.State)
	if len(p.Scopes) > 0 {
		q.Set("scope", strings.Join(p.Scopes, " "))
	}
	return md.AuthorizationEndpoint + "?" + q.Encode(), nil
}

// CallbackParams 是回调携带的授权参数。
type CallbackParams struct {
	Code       string
	State      string
	Issuer     string
	OAuthError string
}

// ParseCallbackURL 解析用户粘贴回的完整重定向 URL（headless 模式）。
func ParseCallbackURL(raw string) (CallbackParams, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return CallbackParams{}, err
	}
	q := u.Query()
	return CallbackParams{
		Code:       q.Get("code"),
		State:      q.Get("state"),
		Issuer:     q.Get("iss"),
		OAuthError: q.Get("error"),
	}, nil
}

// ValidateCallback 校验回调。error_description 一律不回显（攻击者可控）。
func ValidateCallback(p CallbackParams, expectedState string) error {
	if p.OAuthError != "" {
		return ErrDenied
	}
	if p.State == "" || p.State != expectedState {
		return ErrInvalidState
	}
	if p.Code == "" {
		return ErrMissingCode
	}
	return nil
}

// ValidateIssuer 仅在 provider 声明 iss 支持时校验；否则回退 callback-specific。
func ValidateIssuer(md *DiscoveryMetadata, iss string) error {
	if !md.IssParamSupported {
		return nil
	}
	if iss == "" || iss != md.Issuer {
		return ErrIssuerMismatch
	}
	return nil
}

// TokenNeedsRefresh 过期判定（含 buffer）。
func TokenNeedsRefresh(t *OAuthTokens, now time.Time, buffer time.Duration) bool {
	if t == nil || t.AccessToken == "" || t.ExpiresAt.IsZero() {
		return true
	}
	return now.Add(buffer).After(t.ExpiresAt)
}

// StoreKey 凭证存储键：oauth/{user}/{agent}/{server}.json。
func StoreKey(userID, agentID, serverName string) string {
	return "oauth/" + url.PathEscape(userID) + "/" + url.PathEscape(agentID) + "/" + url.PathEscape(serverName) + ".json"
}

// CallbackID 派生自 server URL（Codex 同款，防 mix-up 的路径绑定）。
func CallbackID(serverURL string) (string, error) {
	u, err := url.Parse(serverURL)
	if err != nil || u.Host == "" {
		return "", errors.New("oauth: invalid server url")
	}
	u.Fragment = ""
	h := sha256.Sum256([]byte(u.String()))
	return base64.RawURLEncoding.EncodeToString(h[:9]), nil
}

// AppendCallbackID 在 redirect_uri 路径末尾追加 callback id。
func AppendCallbackID(redirectURI, id string) string {
	if redirectURI == "" {
		return redirectURI
	}
	if strings.HasSuffix(redirectURI, "/") {
		return redirectURI + id
	}
	return redirectURI + "/" + id
}
```

### 11.2 端口层 `internal/mcp/oauth/port/ports.go`

```go
package port

import (
	"context"
	"errors"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
)

var ErrNotFound = errors.New("oauth: not found")

type MetadataFetcher interface {
	Fetch(ctx context.Context, serverURL string) (*domain.DiscoveryMetadata, error)
}

type ClientRegistrar interface {
	Register(ctx context.Context, registrationEndpoint string, redirectURIs []string) (*domain.ClientRegistration, error)
}

type AuthorizationCodeExchanger interface {
	Exchange(ctx context.Context, tokenEndpoint, code, codeVerifier, redirectURI string, reg *domain.ClientRegistration) (*domain.OAuthTokens, error)
	Refresh(ctx context.Context, tokenEndpoint string, tokens *domain.OAuthTokens, reg *domain.ClientRegistration) (*domain.OAuthTokens, error)
	Revoke(ctx context.Context, revocationEndpoint, token, clientID string) error
}

type TokenStore interface {
	Save(ctx context.Context, key string, t *domain.OAuthTokens) error
	Load(ctx context.Context, key string) (*domain.OAuthTokens, error)
	Delete(ctx context.Context, key string) error
}

type PendingAuthStore interface {
	Save(ctx context.Context, p *domain.PendingAuth) error
	Take(ctx context.Context, state string) (*domain.PendingAuth, error) // 读取即删除（一次性）
}

type ClientRegistrationStore interface {
	Get(ctx context.Context, serverName, callbackURL string) (*domain.ClientRegistration, error)
	Save(ctx context.Context, serverName, callbackURL string, r *domain.ClientRegistration) error
}

type BrowserOpener interface {
	Open(ctx context.Context, url string) error
}

type CallbackReceiver interface {
	Listen(ctx context.Context) (<-chan domain.CallbackParams, error)
}

type Cryptor interface {
	Encrypt(ctx context.Context, plaintext []byte) ([]byte, error)
	Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error)
}
```

### 11.3 用例层 `internal/mcp/oauth/usecase/`

#### start.go

```go
package usecase

import (
	"context"
	"errors"
	"fmt"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/port"
)

type StartAuthorization struct {
	Meta      port.MetadataFetcher
	Registrar port.ClientRegistrar
	Regs      port.ClientRegistrationStore
	Pending   port.PendingAuthStore
}

type StartAuthInput struct {
	UserID, AgentID, ServerName, ServerURL, CallbackURL string
	Scopes                                             []string
}

type StartAuthOutput struct {
	AuthURL string
	State   string
}

func (uc *StartAuthorization) Execute(ctx context.Context, in StartAuthInput) (StartAuthOutput, error) {
	md, err := uc.Meta.Fetch(ctx, in.ServerURL)
	if err != nil {
		return StartAuthOutput{}, fmt.Errorf("oauth: discovery: %w", err)
	}

	// 复用已注册 client；未注册则动态注册（redirect_uri 绑定 callback）。
	reg, err := uc.Regs.Get(ctx, in.ServerName, in.CallbackURL)
	if errors.Is(err, port.ErrNotFound) {
		reg, err = uc.Registrar.Register(ctx, md.RegistrationEndpoint, []string{in.CallbackURL})
		if err != nil {
			return StartAuthOutput{}, fmt.Errorf("oauth: register client: %w", err)
		}
		if err := uc.Regs.Save(ctx, in.ServerName, in.CallbackURL, reg); err != nil {
			return StartAuthOutput{}, err
		}
	} else if err != nil {
		return StartAuthOutput{}, err
	}

	mode := domain.CallbackSpecific
	if md.IssParamSupported {
		mode = domain.IssuerBound
	}
	p, err := domain.NewPendingAuth(in.UserID, in.AgentID, in.ServerName, in.ServerURL, in.CallbackURL, in.Scopes, mode)
	if err != nil {
		return StartAuthOutput{}, err
	}
	if err := uc.Pending.Save(ctx, p); err != nil {
		return StartAuthOutput{}, err
	}

	authURL, err := domain.BuildAuthorizationURL(md, reg, p)
	if err != nil {
		return StartAuthOutput{}, err
	}
	return StartAuthOutput{AuthURL: authURL, State: p.State}, nil
}
```

#### complete.go

```go
package usecase

import (
	"context"
	"fmt"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/port"
)

type CompleteAuthorization struct {
	Meta     port.MetadataFetcher
	Pending  port.PendingAuthStore
	Regs     port.ClientRegistrationStore
	Tokens   port.TokenStore
	Exchange port.AuthorizationCodeExchanger
}

type CompleteAuthInput struct {
	Callback domain.CallbackParams
}

func (uc *CompleteAuthorization) Execute(ctx context.Context, in CompleteAuthInput) error {
	// state 查找即取即删（一次性）。
	p, err := uc.Pending.Take(ctx, in.Callback.State)
	if err != nil {
		return fmt.Errorf("oauth: invalid or expired authorization request")
	}
	if err := domain.ValidateCallback(in.Callback, p.State); err != nil {
		return err
	}
	md, err := uc.Meta.Fetch(ctx, p.ServerURL)
	if err != nil {
		return err
	}
	if err := domain.ValidateIssuer(md, in.Callback.Issuer); err != nil {
		return err
	}
	reg, err := uc.Regs.Get(ctx, p.ServerName, p.CallbackURL)
	if err != nil {
		return err
	}
	tokens, err := uc.Exchange.Exchange(ctx, md.TokenEndpoint, in.Callback.Code, p.CodeVerifier, p.CallbackURL, reg)
	if err != nil {
		return err
	}
	tokens.Issuer = md.Issuer
	return uc.Tokens.Save(ctx, domain.StoreKey(p.UserID, p.AgentID, p.ServerName), tokens)
}
```

#### refresh.go + token_provider.go

```go
package usecase

import (
	"context"
	"sync"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/port"
)

// RefreshLocks 进程内 per-key 刷新锁（防 token 旋转竞态）。
type RefreshLocks struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func NewRefreshLocks() *RefreshLocks { return &RefreshLocks{locks: map[string]*sync.Mutex{}} }

func (l *RefreshLocks) Lock(key string) func() {
	l.mu.Lock()
	m, ok := l.locks[key]
	if !ok {
		m = &sync.Mutex{}
		l.locks[key] = m
	}
	l.mu.Unlock()
	m.Lock()
	return m.Unlock
}

type RefreshToken struct {
	Meta     port.MetadataFetcher
	Regs     port.ClientRegistrationStore
	Tokens   port.TokenStore
	Exchange port.AuthorizationCodeExchanger
	Locks    *RefreshLocks
}

type RefreshInput struct {
	UserID, AgentID, ServerName, ServerURL string
}

func (uc *RefreshToken) Execute(ctx context.Context, in RefreshInput) (*domain.OAuthTokens, error) {
	key := domain.StoreKey(in.UserID, in.AgentID, in.ServerName)
	unlock := uc.Locks.Lock(key)
	defer unlock()

	tokens, err := uc.Tokens.Load(ctx, key)
	if err != nil {
		return nil, err
	}
	if tokens.RefreshToken == "" {
		return nil, fmt.Errorf("oauth: no refresh token stored")
	}
	md, err := uc.Meta.Fetch(ctx, in.ServerURL)
	if err != nil {
		return nil, err
	}
	reg, err := uc.Regs.Get(ctx, in.ServerName, "") // refresh 不需要 callbackURL
	if err != nil {
		return nil, err
	}
	fresh, err := uc.Exchange.Refresh(ctx, md.TokenEndpoint, tokens, reg)
	if err != nil {
		return nil, err
	}
	if fresh.RefreshToken == "" { // provider 未旋转则保留旧 refresh token
		fresh.RefreshToken = tokens.RefreshToken
	}
	fresh.Issuer = md.Issuer
	if err := uc.Tokens.Save(ctx, key, fresh); err != nil {
		return nil, err
	}
	return fresh, nil
}

// TokenProvider 供 MCP HTTP client 请求时取可用 access token（自动预刷新）。
type TokenProvider struct {
	Tokens  port.TokenStore
	Refresh *RefreshToken
}

func (p *TokenProvider) AccessToken(ctx context.Context, in RefreshInput) (string, error) {
	key := domain.StoreKey(in.UserID, in.AgentID, in.ServerName)
	tokens, err := p.Tokens.Load(ctx, key)
	if err != nil {
		return "", err
	}
	if domain.TokenNeedsRefresh(tokens, time.Now().UTC(), 60*time.Second) {
		tokens, err = p.Refresh.Execute(ctx, in)
		if err != nil {
			return "", err
		}
	}
	return tokens.AccessToken, nil
}
```

> 注：`RefreshInput` 中的 `ServerURL` 仅用于 discovery。若同一 (user, agent, server) 的 discovery 结果可缓存（TTL 1h），可减少每个请求一次 HTTP 探测。

#### revoke.go

```go
package usecase

import (
	"context"
	"fmt"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/port"
)

type RevokeToken struct {
	Meta     port.MetadataFetcher
	Regs     port.ClientRegistrationStore
	Tokens   port.TokenStore
	Exchange port.AuthorizationCodeExchanger
}

func (uc *RevokeToken) Execute(ctx context.Context, in RefreshInput) error {
	key := domain.StoreKey(in.UserID, in.AgentID, in.ServerName)
	tokens, err := uc.Tokens.Load(ctx, key)
	if err != nil {
		return err
	}
	md, err := uc.Meta.Fetch(ctx, in.ServerURL)
	if err != nil {
		return err
	}
	reg, _ := uc.Regs.Get(ctx, in.ServerName, "")
	_ = uc.Exchange.Revoke(ctx, md.RevocationEndpoint, tokens.RefreshToken, reg.ClientID)
	return uc.Tokens.Delete(ctx, key)
}
```

### 11.4 适配层 `internal/mcp/oauth/adapter/`

#### metadata.go / exchange.go

```go
package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
)

type HTTPMetadataFetcher struct{ Client *http.Client }

func (f *HTTPMetadataFetcher) Fetch(ctx context.Context, serverURL string) (*domain.DiscoveryMetadata, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, err
	}
	u.Path = "/.well-known/oauth-authorization-server"
	u.RawQuery, u.Fragment = "", ""
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discovery: http %d", resp.StatusCode)
	}
	var raw struct {
		Issuer               string   `json:"issuer"`
		AuthorizationEndpoint string  `json:"authorization_endpoint"`
		TokenEndpoint        string   `json:"token_endpoint"`
		RegistrationEndpoint string   `json:"registration_endpoint"`
		RevocationEndpoint   string   `json:"revocation_endpoint"`
		ScopesSupported      []string `json:"scopes_supported"`
		CodeChallengeMethods []string `json:"code_challenge_methods_supported"`
		IssParamSupported    bool     `json:"authorization_response_iss_parameter_supported"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	return &domain.DiscoveryMetadata{
		Issuer:                 raw.Issuer,
		AuthorizationEndpoint:  raw.AuthorizationEndpoint,
		TokenEndpoint:          raw.TokenEndpoint,
		RegistrationEndpoint:   raw.RegistrationEndpoint,
		RevocationEndpoint:     raw.RevocationEndpoint,
		ScopesSupported:        raw.ScopesSupported,
		CodeChallengeMethods:   raw.CodeChallengeMethods,
		IssParamSupported:      raw.IssParamSupported,
	}, nil
}

// HTTPCodeExchanger 实现授权码/刷新/吊销。
type HTTPCodeExchanger struct{ Client *http.Client }

func (e *HTTPCodeExchanger) Exchange(ctx context.Context, tokenEndpoint, code, codeVerifier, redirectURI string, reg *domain.ClientRegistration) (*domain.OAuthTokens, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("code_verifier", codeVerifier)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", reg.ClientID)
	return e.postToken(ctx, tokenEndpoint, form)
}

func (e *HTTPCodeExchanger) Refresh(ctx context.Context, tokenEndpoint string, tokens *domain.OAuthTokens, reg *domain.ClientRegistration) (*domain.OAuthTokens, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", tokens.RefreshToken)
	form.Set("client_id", reg.ClientID)
	return e.postToken(ctx, tokenEndpoint, form)
}

func (e *HTTPCodeExchanger) Revoke(ctx context.Context, revocationEndpoint, token, clientID string) error {
	if revocationEndpoint == "" {
		return nil // provider 未声明 revocation 时只删本地
	}
	form := url.Values{}
	form.Set("token", token)
	form.Set("client_id", clientID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, revocationEndpoint, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := e.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func (e *HTTPCodeExchanger) postToken(ctx context.Context, endpoint string, form url.Values) (*domain.OAuthTokens, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := e.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oauth: token endpoint http %d", resp.StatusCode)
	}
	var raw struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	if raw.AccessToken == "" {
		return nil, fmt.Errorf("oauth: empty access token")
	}
	expires := time.Now().UTC().Add(time.Duration(raw.ExpiresIn) * time.Second)
	var scopes []string
	if raw.Scope != "" {
		scopes = splitFields(raw.Scope)
	}
	return &domain.OAuthTokens{AccessToken: raw.AccessToken, RefreshToken: raw.RefreshToken, ExpiresAt: expires, Scopes: scopes}, nil
}
```

#### client_registration.go

```go
package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
)

// HTTPClientRegistrar 动态客户端注册（POST registration_endpoint）。
type HTTPClientRegistrar struct{ Client *http.Client }

func (r *HTTPClientRegistrar) Register(ctx context.Context, endpoint string, redirectURIs []string) (*domain.ClientRegistration, error) {
	body, _ := json.Marshal(map[string]any{
		"client_name":                "fastagent",
		"redirect_uris":              redirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"application_type":           "native",
		"token_endpoint_auth_method": "none",
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oauth: registration http %d", resp.StatusCode)
	}
	var raw struct {
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	if raw.ClientID == "" {
		return nil, fmt.Errorf("oauth: empty client_id")
	}
	return &domain.ClientRegistration{ClientID: raw.ClientID, RedirectURIs: redirectURIs, TokenEndpointAuthMethod: "none"}, nil
}
```

#### token_store.go（加密落盘）

```go
package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/port"
)

// FileTokenStore 密文落盘 ~/.fastagent/oauth/…，原子写 + 0600。
type FileTokenStore struct {
	Root  string // 默认 os.UserHomeDir()/.fastagent
	Crypt port.Cryptor
}

func (s *FileTokenStore) Save(ctx context.Context, key string, t *domain.OAuthTokens) error {
	plain, err := json.Marshal(t)
	if err != nil {
		return err
	}
	enc, err := s.Crypt.Encrypt(ctx, plain)
	if err != nil {
		return err
	}
	path := filepath.Join(s.Root, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, enc, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *FileTokenStore) Load(ctx context.Context, key string) (*domain.OAuthTokens, error) {
	enc, err := os.ReadFile(filepath.Join(s.Root, filepath.FromSlash(key)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, port.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	plain, err := s.Crypt.Decrypt(ctx, enc)
	if err != nil {
		return nil, err
	}
	var t domain.OAuthTokens
	if err := json.Unmarshal(plain, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *FileTokenStore) Delete(ctx context.Context, key string) error {
	return os.Remove(filepath.Join(s.Root, filepath.FromSlash(key)))
}
```

#### pending_store.go / registration_store.go

```go
package adapter

import (
	"context"
	"sync"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/port"
)

// MemoryPendingStore 内存待授权会话（TTL 惰性清理 + Take 即删）。
type MemoryPendingStore struct {
	mu      sync.Mutex
	pending map[string]*domain.PendingAuth
}

func NewMemoryPendingStore() *MemoryPendingStore {
	return &MemoryPendingStore{pending: make(map[string]*domain.PendingAuth)}
}

func (s *MemoryPendingStore) Save(ctx context.Context, p *domain.PendingAuth) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[p.State] = p
	return nil
}

func (s *MemoryPendingStore) Take(ctx context.Context, state string) (*domain.PendingAuth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[state]
	if !ok {
		return nil, port.ErrNotFound
	}
	delete(s.pending, state) // 一次性
	if p.Expired(time.Now().UTC()) {
		return nil, port.ErrNotFound
	}
	return p, nil
}

// MemoryRegistrationStore 持久化 client_id，键 = (serverName, callbackURL)。
type MemoryRegistrationStore struct {
	mu   sync.Mutex
	regs map[string]*domain.ClientRegistration
}

func (s *MemoryRegistrationStore) Get(ctx context.Context, serverName, callbackURL string) (*domain.ClientRegistration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.regs[serverName+"|"+callbackURL]; ok {
		return r, nil
	}
	return nil, port.ErrNotFound
}

func (s *MemoryRegistrationStore) Save(ctx context.Context, serverName, callbackURL string, r *domain.ClientRegistration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.regs[serverName+"|"+callbackURL] = r
	return nil
}
```

#### browser.go / loopback.go / cryptor.go

```go
package adapter

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
)

// SystemBrowserOpener 分平台打开系统浏览器。
type SystemBrowserOpener struct{}

func (b *SystemBrowserOpener) Open(ctx context.Context, url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return errors.New("unsupported platform for browser open")
	}
	return cmd.Start()
}

// FreeLoopbackPort 由 OS 分配空闲 loopback 端口。
func FreeLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port, nil
}

// LoopbackCallbackReceiver 本地回调监听；路径 /callback/{id} 绑定到特定 server（防 mix-up）。
type LoopbackCallbackReceiver struct {
	Port       int
	CallbackID string
}

func (r *LoopbackCallbackReceiver) Listen(ctx context.Context) (<-chan domain.CallbackParams, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", r.Port))
	if err != nil {
		return nil, err
	}
	ch := make(chan domain.CallbackParams, 1)
	go func() {
		defer ln.Close()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			req, err := http.ReadRequest(bufio.NewReader(conn))
			if err != nil {
				conn.Close()
				continue
			}
			path := req.URL.Path
			expected := "/callback/" + r.CallbackID
			if r.CallbackID != "" && !strings.HasSuffix(path, expected) {
				conn.Write([]byte("HTTP/1.1 404 Not Found\r\n\r\n"))
				conn.Close()
				continue
			}
			params, _ := domain.ParseCallbackURL(req.URL.String())
			conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/html\r\n\r\n<html><body>Authorization received. You can close this window.</body></html>"))
			conn.Close()
			ch <- params
			return
		}
	}()
	return ch, nil
}

// AESGCMCryptor 用派生自 secret 的 32 字节密钥做 AES-256-GCM 加密。
type AESGCMCryptor struct{ key []byte }

func NewAESGCMCryptor(secret string) (*AESGCMCryptor, error) {
	if secret == "" {
		return nil, errors.New("oauth: encryption secret is required (FASTAGENT_OAUTH_SECRET)")
	}
	sum := sha256.Sum256([]byte(secret))
	return &AESGCMCryptor{key: sum[:]}, nil
}

func (c *AESGCMCryptor) Encrypt(ctx context.Context, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, nil), nil
}

func (c *AESGCMCryptor) Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return nil, errors.New("oauth: ciphertext too short")
	}
	nonce, data := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	return gcm.Open(nil, nonce, data, nil)
}
```

### 11.5 框架层接线

#### `internal/mcp/http.go` 增量

```go
// HTTPClient 增加可选 OAuth 注入。
type HTTPClient struct {
	// …既有字段…
	auth func(ctx context.Context) (string, error) // 新增
}

// SetAuthProvider 注入 OAuth token 提供者（由 agent loop 在 NewManager 后设置）。
func (c *HTTPClient) SetAuthProvider(f func(ctx context.Context) (string, error)) {
	c.auth = f
}

func (c *HTTPClient) sendRequest(ctx context.Context, method string, params interface{}) (…, error) {
	// 每次请求前注入 Bearer；401 单次重放由 CallTool 层处理。
	if c.auth != nil {
		tok, err := c.auth(ctx)
		if err != nil {
			return nil, fmt.Errorf("mcp: oauth token unavailable: %w", err)
		}
		c.headers["Authorization"] = "Bearer " + tok
	}
	// …原有逻辑…
}
```

> `Manager` 增加 `SetAuth(serverName string, f func(ctx) (string, error))`，在 `CallTool` 分发到对应 server 前调用。

#### `internal/agent/loop.go` 增量

```go
// 在 NewManager 之后、注册工具之前：
for name, cfg := range rc.MCPServers {
	if cfg.OAuthResource == "" {
		continue // 静态 header 路径，零变化
	}
	provider := oauthBootstrap.TokenProviderFor(userID, rc.ID, name, cfg.OAuthResource)
	mcpMgr.SetAuth(name, func(ctx context.Context) (string, error) {
		return provider.AccessToken(ctx, usecase.RefreshInput{
			UserID: userID, AgentID: rc.ID, ServerName: name, ServerURL: cfg.OAuthResource,
		})
	})
}
```

> `oauthBootstrap` 是进程级单例（gateway 启动时装配一次），保证 `RefreshLocks` 跨多个 agent loop 共享 —— 见 §12 评审项 2。

#### `internal/setup/handlers_oauth.go` 骨架

```go
// POST /api/mcp/oauth/start —— auth（user session）
func (s *Server) handleMcpOAuthStart(w http.ResponseWriter, r *http.Request) {
	userID := auth.MustUserIDFromContext(r.Context())
	var req struct {
		AgentID    string   `json:"agentId"`
		ServerName string   `json:"serverName"`
		ServerURL  string   `json:"serverUrl"`
		CallbackURL string  `json:"callbackUrl"`
		Scopes     []string `json:"scopes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	out, err := s.mcpOAuth.Start.Execute(r.Context(), usecase.StartAuthInput{
		UserID: userID, AgentID: req.AgentID, ServerName: req.ServerName,
		ServerURL: req.ServerURL, CallbackURL: req.CallbackURL, Scopes: req.Scopes,
	})
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"authUrl": out.AuthURL, "state": out.State})
}

// POST /api/mcp/oauth/callback —— super_admin（cloud 以 admin key 转发 code+state）
func (s *Server) handleMcpOAuthCallback(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code  string `json:"code"`
		State string `json:"state"`
		Iss   string `json:"iss"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := s.mcpOAuth.Complete.Execute(r.Context(), usecase.CompleteAuthInput{
		Callback: domain.CallbackParams{Code: req.Code, State: req.State, Issuer: req.Iss},
	}); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// GET /oauth/mcp/{callbackID}/callback —— 公开（provider 重定向落点，薄转发）
// 实现：解析 callbackID → 映射 serverName，然后把 code/state 以 admin key POST
// 给 /api/mcp/oauth/callback，最后 302 回 SPA。具体映射见 §12 评审项 3。
```

#### `cmd/fastclaw/cmd_mcp_login.go` 骨架

```go
// fastclaw mcp login <name> [--no-browser] [--oauth-resource <url>]
func runMcpLogin(…, noBrowser bool) error {
	resource := … // cfg.OAuthResource 或 --oauth-resource
	port, err := adapter.FreeLoopbackPort()
	if err != nil { return err }
	callbackID, _ := domain.CallbackID(resource)
	callbackURL := fmt.Sprintf("http://127.0.0.1:%d/callback/%s", port, callbackID)

	in := usecase.StartAuthInput{UserID: …, AgentID: …, ServerName: name, ServerURL: resource, CallbackURL: callbackURL}
	out, err := start.Execute(ctx, in)
	if err != nil { return err }

	if noBrowser {
		// headless：打印 URL，用户授权后粘回完整重定向 URL。
		fmt.Printf("请在你的浏览器打开以下地址，授权完成后，把跳转到的完整地址粘贴回来：\n\n  %s\n\n> ", out.AuthURL)
		var pasted string
		fmt.Scanln(&pasted)
		params, err := domain.ParseCallbackURL(strings.TrimSpace(pasted))
		if err != nil { return err }
		return complete.Execute(ctx, usecase.CompleteAuthInput{Callback: params})
	}

	recv, err := (&adapter.LoopbackCallbackReceiver{Port: port, CallbackID: callbackID}).Listen(ctx)
	if err != nil { return err }
	if err := (&adapter.SystemBrowserOpener{}).Open(ctx, out.AuthURL); err != nil {
		fmt.Printf("浏览器打不开，请手动打开：\n\n  %s\n", out.AuthURL)
	}
	params := <-recv
	return complete.Execute(ctx, usecase.CompleteAuthInput{Callback: params})
}
```

---

## 12. 评审：设计与代码

> 评审视角：功能完整性 / 交互 / 安全 / 一致性 / 反过度设计。

### 12.1 结论（P0/P1 摘要）

| 级别 | 问题 | 影响 | 处置 |
|---|---|---|---|
| P0 | refresh 锁是进程内的，而 agent loop 会并发建多个 Manager/Provider | token 旋转竞态 → 一次刷新失败/串号 | 进程级单例装配（§12.2-1） |
| P1 | 注册 store 键只含 serverName，忽略 callbackURL | Web(HTTPS) 与 CLI(loopback) 用同一 client 冲突 | 键升级为 (serverName, callbackURL)（代码已改，待确认） |
| P1 | 公开回调路径写死 `/oauth/quandora/callback` | 多 OAuth server 冲突 + 丢失 mix-up 路径绑定 | 改 `/oauth/mcp/{callbackID}/callback`（§12.2-3） |
| P2 | refresh 返回新 refresh_token 时可能被覆盖为空 | 旋转后丢 refresh token | 保留旧值兜底（代码已含） |
| P2 | discovery 每请求一次 | 高扇出下放大网络 | discovery 结果缓存 TTL 1h |

### 12.2 评审项

**1.【P0】refresh 锁必须跨 agent loop 共享。**
`internal/agent/loop.go:406` 每个 agent 每次循环都 `mcp.NewManager(rc.MCPServers)` → 各建一个 `TokenProvider` 与 `RefreshLocks`。两个并发 loop 同时触发刷新 → 同一 refresh_token 被用两次，旋转后前一个失效。**修**：`RefreshLocks` + TokenStore 由 gateway 装配成进程级单例注入；或对 refresh 用 store 级文件锁。此点 codex 只在单进程 CLI 成立，fastagent 多 loop 场景必须升级。

**2.【P1】ClientRegistrationStore 键需含 callbackURL。**
同一 `serverName` 可能同时有 Web 路径（HTTPS 回调）和 CLI 路径（loopback 回调），两者注册的是不同 client（不同 redirect_uri）。按 `serverName` 单一键会互相覆盖，导致 authorize 时 redirect_uri 不匹配。代码 §11.2 已改为 `Get(serverName, callbackURL)`，与 §11.3 start.go 对齐。**另**：refresh 时 `Get(ctx, serverName, "")` 语义需定义——建议注册 store 按 (serverName, callbackURL) 存、按 serverName 聚合可列举，refresh 取任一有效 reg 即可（client_id 属公开信息）。

**3.【P1】公开回调路径应通用 + 绑定 server。**
写死 `/oauth/quandora/callback` 的问题：
- 多 OAuth MCP server 无法区分（每个都要一条路径或靠 state）；
- **丢失 mix-up 路径绑定**：callback-specific 模式的本质是「回调路径 = hash(server_url)」，HTTPS 共享路径会让多个 server 混在一起。
**修**：`GET /oauth/mcp/{callbackID}/callback`；cloud 路由按 callbackID 解析 serverName → 以 admin key 转发。callbackID 由 `domain.CallbackID(oauthResource)` 计算（已提供）。该路径需注册进 client 的 redirect_uris。

**4.【P2】refresh 旋转兜底。**
quandora 可能在 refresh 响应中不再下发 refresh_token（仅旋转 access）；或下发新 refresh_token。代码已兜底：`if fresh.RefreshToken == "" { fresh.RefreshToken = tokens.RefreshToken }`。**补充**：`ExpiresAt` 若响应无 `expires_in` 时置零 → 每次预刷新，需容忍（或在 postToken 给默认 60s）。

**5.【P2】discovery 缓存。**
`TokenProvider.AccessToken` 每次都要 `Refresh.Execute` → `Meta.Fetch`（HTTP）。高扇出下放大。**修**：`MetadataFetcher` 加内存 TTL 缓存（1h），或 TokenProvider 内缓存 DiscoveryMetadata。

**6.【交互】headless 粘贴体验。**
路径 3 让用户粘贴**完整 redirect URL**（对齐 claude/codex）。需在 CLI 里明示：浏览器显示「无法连接」是预期现象，复制地址栏的完整地址。文档 §6 已描述，但 CLI 输出文案要写清楚。

**7.【交互】Web 控制台路径的 state 展示。**
SPA 拿到 `{authURL, state}` 后 `window.location.assign`。若用户中途关闭浏览器再回，`state` 无二次校验 UI。**建议**：回调落地页（302 回 SPA）带上 `?mcp={serverName}&result=ok|denied`，SPA 据此展示「已授权/已拒绝」，不依赖后台推送。

**8.【安全】加密密钥来源需显式。**
`AESGCMCryptor` 依赖 `FASTAGENT_OAUTH_SECRET`（或复用现有 master secret）。**必须 fail-closed**：未配置 secret 时拒绝保存 refresh token（宁可不接，不落明文）。部署文档需写明密钥轮换 = 全部 token 重新授权。

**9.【安全】callback 公开端点滥用面。**
公开 `GET /oauth/mcp/{id}/callback` 仅转发 code+state。攻击者无有效 pending state 时 `Complete` 直接失败（state 一次性 + TTL）。**确认**：该端点不应携带任何可被枚举的副作用；转发目标（admin key）绝不可被公开端点读取。

**10.【一致性】go 语法核对。**
- `adapter/cryptor.go` 需补 import：`crypto/aes`、`crypto/cipher`、`crypto/sha256`、`crypto/rand`、`io`、`io/readfull`（`io.ReadFull`）——骨架略；
- `adapter/loopback.go` 需补 `bufio` import；
- `exchange.go`/`registration.go` 需补 `strings`、`fmt` import；
- `complete.go` 中 `reg` 变量已用（无未使用变量）。

**11.【反过度设计】端口抽象规模合适。**
7 个端口接口均有 ≥2 个真实变体（内存/文件、HTTPS/loopback、默认/自实现 browser），符合「已验证变化轴」；未引入第三方 OAuth SDK（可控、可测、~500 行），判断成立。

### 12.3 建议实施顺序

1. domain + port + 纯函数单测（无依赖，先行）
2. usecase + fake adapter 集成测试（内存 store、mock token endpoint）
3. adapter 落地（http/metadata/token_store/cryptor）
4. framework 接线（config → loop → handlers → CLI）
5. e2e：本地 mock provider（authorize + token 双端点）走通三路径
6. 接真实 quandora 验证（先 CLI loopback，再 Web 回调）

---

## 13. MCP 操作可逆性设计（Cordis 论文落地 + 合规审计，2026-09-04）

> 本节由原独立文档 `docs/mcp-reversibility.md` 合并而来（原文件已删除）。依据：_A Programming Paradigm for Spatiotemporal Composability_（arXiv:2608.25512，Peking University × DeepSeek-AI，92 页全文）。§11–§12 是历史草案；本节与顶部实施状态为当前实现的设计权威。

### 13.1 论文原则摘要（页码按论文）

论文的核心不是「所有操作都要可逆」一句话，而是一套精确机制：

1. **revertible effects（§3.1, p.9–16）**：effect = Γ → Γ×(Γ→Γ)——作用于上下文后返回（新状态, 显式逆操作）；逆在应用现场产出、由运行时持有（Definition 8, p.12）。
2. **左逆、按应用现场保证（§3.1.1/3.1.2）**：只要求 `g∘f`（从不要求 `f∘g`）；witness = `g(δ)=γ`（Definition 8）。witness 是组件作者义务，运行时只持有回放、不验证（§5.1.1, p.59）→ **UT/e2e 即 witness**。
3. **LIFO 反序撤销（Definition 1; Theorem 16, p.15）**：逆按应用反序合成；反序撤销时每个逆都拿到自己应用产出的状态。
4. **撤销一次性（Algorithm 1, p.59）**：armed/dispose 只能触发一次；重复触发无保证。
5. **恢复是观测等价 ≃（§3.3.2, p.23）**：不承诺物理还原（free/malloc 例子），只承诺观察者不可区分。
6. **系统边界（§6.1, p.70–71）**：可「独占修改+恢复」的位置在边界内（可逆）；跨出边界的 emission 不可逆，只能 withholding 或 compensation（同样 LIFO 合成）。
7. **coeffect 表带前置条件（Definition 20, p.17）**：`set(k,v)` 要求 `k∉dom`；restriction 要求 `k∈dom`；违反 = 报错、零迁移。
8. **声明式 loader = entry 列表 + keyed diff（§5.2, p.64–69）**：每组件一 entry `{id,url,config,disabled}`；reconcile 收敛到「最终配置决定的状态」（Theorem 80）；单 entry 重建不影响邻居（Corollary 69）。

### 13.2 上下文模型：per-key 声明存储

| key 空间 | 绑定值 | 物理存储 |
|---|---|---|
| `K_conf = (agentID, serverName)` | `config.MCPServerConfig` 整份 JSON | `agent_mcp_servers`，PK `(agent_id, server_name)`，config TEXT（dialect 无关） |
| `K_cred = (userID, agentID, serverName)` | 密文 token record | `mcp_oauth_tokens`（SQLite `BLOB` / PG `BYTEA`） |
| `K_pending = (userID, callbackID)` | 一次性授权待办 | `mcp_oauth_pending` |
| `K_reg = (serverName, callbackURL)` | DCR client | `mcp_oauth_clients` |

`mcpServers` 已于 2026-09-04 从 `agents.config`（TEXT JSON 单文档）迁出为 per-key 行——同一 agent 的不同 server 写不同 PK，key 级操作物理可交换；`agents.config` 不再承载 mcpServers，`agent.json` 已废弃，**本地与云端统一 DB**（sqlite/PG）。访问统一过 owner 门控 + `requireAgentOwner`（stale rc 归属校验）。

### 13.3 Action 逆映射（左逆 / 应用现场 / LIFO）

| action f | 前置条件 | 逆 g | witness | 边界 | redo |
|---|---|---|---|---|---|
| `add` | `k∉dom`（PK DO NOTHING，0 行=`ErrMCPServerExists`） | `remove` | 删同一条行，声明集回 add 前 | inside | 再次 add |
| `remove` | `k∈dom`（DELETE 0 行=`ErrNotFound`） | `add`（**逆参数在应用现场随输出返回**：`<mcp-undo>` marker 携带完整被删 entry，落进 tool_result trace） | 重建声明；不碰 `K_cred` | inside（无外部副作用） | 再次 remove |
| `undo`（机器回放） | **当前会话** trace 有未消费的 add/remove marker | 本会话最新未消费操作的反向动作（LIFO；消费集存 `configs_kv`，name=`undo:<sessionKey>`） | 逐条恢复声明集；只覆盖边界内操作；跨会话操作不可达 | inside | 再次 undo 回放本会话更旧操作 |
| `login` | server 已声明且 `oauthResource` 非空 | `logout` | 本地凭证删除；provider 授权关系不可撤销（补偿） | acquisition inside + consent outside | 重新 login |
| `logout` | `K_cred∈dom` | `login`（补偿） | 重新授权后可用 | local delete inside + revoke emission outside | 再次 logout |
| `refresh` | `K_cred∈dom` | 无（幂等自愈） | — | local rotate inside + 旧 token 远端失效 outside | 最终撤销走 logout |
| `status`/`check` | 只读 | 无 | — | inside 读 + check 探测 emission | — |

要点：逆只在应用现场成立（`remove(add(v))` 恢复 add 那一刻，不是任意历史）；LIFO 示例 `add A → login A → add B ⟹ remove B → logout A → remove A`；撤销一次（二次 remove 报错）。

### 13.4 前置条件即错误语义

重复 add / remove 缺失 / 非 http(s) / scopes 无 oauthResource / owner 门控失败 / 归属不一致：全部在写库/触网前报错、零迁移。写库成功后才 notify（withholding：状态先持久再广播，通知失败由 epoch 轮询兜底）。

### 13.5 notify = reactive coeffects

add/remove 行写成功（或 dashboard `ReplaceMCPServers` 事务完成）→ `mcpConfigNotify` → `NotifyAgentReload`（InvalidateAgent + configs_kv epoch + Redis pub/sub）→ 下一个 build 工具代理激活/停用。与 OAuth callback 共用同一广播链。

### 13.6 系统边界与补偿结论

- **`remove` 绝不自动吊销**：吊销是跨边界 emission，一旦执行不可撤销；绑进 remove 会让 `add` 不再是它的逆。凭证记录保留，重 add 即恢复授权；彻底下线 = `logout → remove`。
- **`login ↔ logout` 不是严格互逆**：本地半部可逆，远端 consent/revocation 只能补偿。
- 业务 MCP 工具调用（factor-mining 等）是 emission，不在本管理面的可逆性承诺内。

### 13.7 实现落点与测试（witness）

- store：[agent_mcp_servers.go](/Users/reina/Project/tokenaissance/fastagent/internal/store/agent_mcp_servers.go)（List/Add/Delete/Replace，PK 前置条件）；DDL [database.go:1447](/Users/reina/Project/tokenaissance/fastagent/internal/store/database.go:1447)；DeleteAgent/DeleteUser 级联。
- agent 工具：[mcp_config_tool.go](/Users/reina/Project/tokenaissance/fastagent/internal/agent/mcp_config_tool.go)（per-key 写 + `requireAgentOwner`；旧 JSON helper 已删；add/remove 输出追加 `<mcp-undo>` 逆参数 marker，remove 携带完整被删 entry）。
- undo 机器回放：[mcp_undo.go](/Users/reina/Project/tokenaissance/fastagent/internal/agent/mcp_undo.go)（**限定当前会话**：`ListSessionEventsSince(user, agent, session)` 倒序读 tool_result marker，LIFO 自动重放；消费集存 `configs_kv` kind=`mcp_undo`、name=`undo:<sessionKey>`）。
- fail-loud：[events.go](/Users/reina/Project/tokenaissance/fastagent/internal/agent/events.go) `emitEventChecked`——mcp add/remove 的 tool_result 持久化失败或无 chat journal 时，结果附加 `[undo journal warning]`；`markUndoCursor` 写失败在 undo 结果中显式报错（已应用但未记账）。
- 读路径：gateway layer-3 loader（[gateway.go:801](/Users/reina/Project/tokenaissance/fastagent/internal/gateway/gateway.go:801)）从表 overlay；GET config 与 oauth servers 枚举从表读。架构备注：`config.AgentFileConfigLoader` 全局间接层当前**单写点**（仅 gateway 启动接线）+ **单读点**（`MergedAgentConfig`）；若未来出现第二个 agent 构建入口，应改为显式入参注入，不再新增全局写点。
- 前端：编辑器保存前 refetch + [mergeMCPServersForSave](/Users/reina/Project/tokenaissance/fastagent/web/src/lib/mcp-servers.ts)（保留并发新增、不复活用户删除）。
- dev 存量数据：一次性幂等脚本 [backfill_agent_mcp_servers.sql](/Users/reina/Project/tokenaissance/fastagent/scripts/backfill_agent_mcp_servers.sql)（已在 dev PG 执行：`agt_e586…/quandora` 1 行）。
- 测试：store CRUD/前置条件/Replace、级联删除、dialect 回归、agent e2e（真 SQLite）、owner gate/schema/零 server 注册、**nil 依赖安全（全 action × nil bootstrap/agent 不 panic）**、gateway loader overlay（含空 JSON + 表行回归）、undo e2e（LIFO 顺序、remove 逆参数重放、消费游标、**会话隔离**、**游标写失败注入**）、fail-loud sink e2e（可注入失败 EventSink + warning 决策矩阵）、**tool 文案 copy 契约 golden**（`testdata/mcp-copy/`，`FASTAGENT_UPDATE_GOLDEN=1` 显式更新）、前端 merge（bun 4 用例）。

### 13.8 未来动作的可逆性门槛

- `update`：必须带旧值快照（A：update 返回旧 entry；B：多版本回退）。无快照的 update 不允许。
- `remove --revoke` / `allow`/`deny` / SKILL.md 自动声明 / pending 容量上限：落地前逐条过 13.11 清单；自动声明装载器按 per-entry keyed diff reconcile。

### 13.9 论文合规审计矩阵（2026-09-04 代码扫描）

| 论文原则 | 当前实现 | 结论 |
|---|---|---|
| effect 返回 (新状态, 逆)，运行时持有 | add/remove 的逆参数在**应用现场**随 tool 输出返回（`<mcp-undo>` marker）；agent loop 把 tool_result 持久化进当前会话 `session_events`；`mcp undo` 从**本会话** trace 读 marker 自动回放，消费游标存 `configs_kv`（name=`undo:<sessionKey>`） | ✅ 合规（机器持有限定边界内声明操作 + 当前会话；login/logout 的 consent/revoke 仍人工，见 §13.6） |
| 左逆、按应用现场 `g(δ)=γ` | per-key 行：remove 只删 add 的那条；**remove 的逆参数（完整被删 entry）在删除现场随输出返回**；不承诺 `f∘g`（remove 不吊销、可逆性不跨历史） | ✅ 合规 |
| 前置条件错误 = 零迁移 | `ErrMCPServerExists`/`ErrNotFound`/输入校验/owner 门控，写库前拒绝 | ✅ 合规（PK 原生实现） |
| LIFO 反序撤销 | `mcp undo` 从 trace 自动回放最新未消费操作，重复调用逐条回退（LIFO）；文档给出反序示例 | ✅ 合规（自动机覆盖 add/remove；login/logout 序列需人工按序补偿） |
| 撤销一次 | 二次 remove 报错 | ✅ 合规 |
| 恢复 = 观测等价 ≃ | 承诺声明集/授权可用性等价，不承诺 token 字节回滚 | ✅ 合规 |
| 系统边界 + 补偿 | remove 不 revoke；logout 远端吊销按补偿处理；业务调用不承诺逆 | ✅ 合规 |
| key 级独立（Σ 偏函数） | `agent_mcp_servers` per-key 行，不同 server 并发写不同 PK | ✅ 合规 |
| notify 驱动激活/停用 | 写后 notifyAgentChanged → 工具下个 build 激活/停用，跨实例 epoch/Redis | ✅ 合规 |
| 声明式 loader 单编排方 + keyed diff | 编排方有两个：agent 工具（per-entry）与 dashboard（整表 PATCH）；dashboard 保存前 refetch+merge 保留并发新增 | ⚠️ 部分偏差（API 整表替换仍是「最后写者胜」契约；毫秒级竞态窗口；彻底修复=服务端 per-entry ops） |
| witness 作者义务 | store/agent/gateway/setup/web 全套 UT/e2e | ✅ 合规 |
| 撤销的撤销 = redo | add 是 remove 的逆且可重做；undo 后可再次 undo 回放更旧操作或手动重放 | ✅ 合规 |

### 13.10 已知边界与开放风险

1. dashboard 整表 PATCH 的 API 契约仍是最后写者胜；UI 已用 merge 缓解，毫秒级窗口存留（详见 13.9）。
2. **undo journal 的范围与持久性边界（方向 A，2026-09-04）**：`mcp undo` 限定**当前会话**（`undo:<sessionKey>` 消费游标），因此其它/已删除会话不会破坏本会话的 LIFO 链——完整性与「会话删除即清历史」自洽。剩余边界：journal 写失败或运行上下文无 chat journal 时 **fail-loud**（mcp add/remove 结果附显式警告，见 §13.7）；消费游标写失败在 undo 结果中显式报错（已应用但未记账，重复 undo 会显式失败而非静默）；只覆盖 agent loop 调用（dashboard 整表 PATCH 不入 journal）；只自动回放 add/remove（边界内），login/logout 涉及 provider 侧 consent/revoke 需人工补偿；跨会话操作**有意不可达**（不会误撤销别的聊天）。
3. `agent.json` 已废弃（2026-09-05）：agent 配置与 MCP 声明全部走 DB（本地 sqlite / 云端 PG）；无 DB 的运行形态不提供 agent 声明管理（`mcp add/remove/undo` 需要 store）。
4. pending/registration 不暴露为 action，其逆由一次性/TTL/原子 `Take` 承担。
5. `refresh` 的 provider 侧旧 token 失效不可逆——按 §13.6 归类为幂等自愈而非「有逆操作」。

### 13.11 回归护栏

新增/评审 MCP 管理 action 时逐条核对：

- [ ] 变更的 key 与前置条件是否显式（不存在才能 add、存在才能 remove/refresh…）？
- [ ] 逆操作是否命名、同一 owner 门控、同一 notify 链？是否只承诺左逆（应用现场）而非 `f∘g`？
- [ ] LIFO 反序示例是否在文档给出？
- [ ] 越界部分（consent/revocation/业务调用）是否只声明 compensation？
- [ ] 撤销是否最多一次、失败是否无半提交？
- [ ] 是否有 UT/e2e 充当 witness（add→remove→add、login→logout→login、重启/并发收敛）？
- [ ] 是否引入需要旧值快照的 update 却没做快照？
- [ ] 多写者语义是否明确（per-entry vs 整表替换），同 key 并发是否串行化/显式冲突？

### 13.12 量化 / 交易 agent 中的价值与边界（Quandora 语境，2026-09-04）

本节把可逆性设计放到 tokenaissance 的实际产品语境（factor-mining / strategy / paper-trading MCP）里，明确「什么可逆、什么只补偿」：

**三层边界**：

| 层 | 例子 | 可逆性 |
|---|---|---|
| 声明层 | 接 factor-mining/strategy/paper-trading 哪个 server、用哪些 scope | ✅ 机器回放（add↔remove、`mcp undo`） |
| 凭证层 | OAuth consent、token、provider 账号 | ⚠️ 本地可逆（remove 不吊销）+ 远端只能补偿（logout → 窄 scope 重新 consent） |
| 执行层 | 提交因子挖掘、创建回测、paper run、未来真实下单 | ❌ emission——undo 不负责；只能 withholding（先演练/先确认）或领域级补偿（stop/cancel/close） |

**管理面的实际价值**：

1. 动态接入数据/研究工具的试错可干净退出：接错 URL、scope 配宽、名字冲突 → `mcp undo` 从持久化 trace 精确回退，不留半配置 server；per-key 行保证试 A/试 B 互不干扰。
2. 凭证是高价值资产，撤销分层：`plugins.upload`/`runs.create` 这类 scope 给宽后，正确路径是 `logout → login（窄 scope 重 consent）`，不是悄悄 remove；scope 收紧/换数据源全程可审计。
3. 审计与回滚 = 合规要求：每个 mcp 变更都有持久化 trace（session_events + undo 游标），出问题直接回放而非靠回忆。
4. 并发改动不会静默破坏配置：undo 遇到状态已变显式报错（§13.10-2），避免自动化交易中静默的半配置状态。
5. owner 门控对逆操作同样生效：访客不能借 undo 触碰 owner 的 provider 配置。

**诚实边界与执行层指引**：`mcp undo` 不撤销已执行的 factor run / paper run / 真实订单——执行层应把可逆性设计为领域能力：paper-trading 本身就是真下单前的演练（withholding 的产物）；执行动作提供 `runs.stop`/取消/平仓等**补偿 action**；未来接真实 broker 时，不可逆下单默认走人工确认。执行层新增 action 也应过 §13.11 门槛（正/补偿成对、越界明示）。
