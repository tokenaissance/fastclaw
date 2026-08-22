# configs_kv：per-(user, agent) 独立 scope 适配（给上游 PR 的文档）

> 状态：fork 已落地（commit `ce6f2b1`）。本文档用于向**上游 fastclaw** 提交 PR，
> 说明 fork 对 configs_kv 四层 scope 模型的适配理由与最小改动。若上游采纳，
> 请把代码 diff + 本文档的「PR 提案」章节一起提交。

## 背景：fork 的四层 scope 模型

fork 的配置按 `(user_id, agent_id)` 所有权分成四层（见 `internal/scope/scope.go`
头注释），从全局到局部：

```
system          (user='',  agent='')   全局默认
user            (user=X,  agent='')    某用户的个人配置
agent           (user='', agent=Y)     某 agent 的配置
per-(user,agent) (user=X,  agent=Y)    某用户在某 agent 上的专属配置
```

读路径按 `system → user → agent → per-(user,agent)` 逐层覆盖，**最内层 wins**。
这四层是 fork 多租户架构的基础：一个用户可以用多个 agent，每个 agent 有独立
的 provider key / 模型 / 渠道绑定，互不泄漏。

## 问题：上游 configs_kv 只实现三层

上游 `refactor(configs): flatten JSON blobs into configs_kv`（fcb63be）新增的
`kvScopeFromOwnership` 把 per-(user, agent) 折叠到 user 层：

```go
// 上游（3 层）
func kvScopeFromOwnership(userID, agentID string) (scope, scopeID string) {
    switch {
    case userID != "" && agentID != "":
        return "user", userID   // ← 折叠：丢 agent 维度
    case userID != "":
        return "user", userID
    case agentID != "":
        return "agent", agentID
    default:
        return "system", ""
    }
}
```

`(X, Y)` 与 `(X, '')` 都落 `(user, X)`，带来两个具体故障：

### 故障 1：跨 agent key 泄漏

一个用户有 agent A、B。为 A 绑定 provider key（per-(user,A) 层）→ 落到
`(user, X)`。之后**任何**该用户的 agent（含 B）在 `GetValue` 读路径都会命中
这个 key。**A 的私有 key 泄漏给 B**——多租户下这是数据泄露。

### 故障 2：迁移时同用户多 agent key 碰撞丢数据

`migrateConfigsToKV` 把存量四层 configs 行扁平化到 configs_kv。同一用户下
agent A、B 各自有 `openai.api_key`（per-(user,A) 与 per-(user,B)）→ 扁平化后
**都写到 `(user, X)` 同一行**，last-write-wins，先写的一个静默丢失。

## fork 的适配（最小改动）

保留 per-(user,agent) 作为 configs_kv 的独立 scope，`scope_id = userID + "/" + agentID`。

### 1. 新常量 `internal/scope/scope.go`

```go
const (
    System    = "system"
    User      = "user"
    Agent     = "agent"
    UserAgent = "user-agent"   // 新增
)
```

### 2. `kvScopeFromOwnership` 加第四层

```go
func kvScopeFromOwnership(userID, agentID string) (scope, scopeID string) {
    switch {
    case userID != "" && agentID != "":
        return UserAgent, userID + "/" + agentID   // 新增，不再折叠
    case userID != "":
        return User, userID
    case agentID != "":
        return Agent, agentID
    default:
        return System, ""
    }
}
```

### 3. 读路径加最内层

`GetValue`（单值）与 `GetValues`（前缀扫描）在 `agent` 层之后、`user` 层之内
增加 per-(user,agent) 层查询；`GetValue` 语义 = innermost wins：

```go
// GetValue：tryGet 顺序 system → user → agent → user-agent（最内层覆盖）
if userID != "" && agentID != "" {
    if err := tryGet(UserAgent, userID+"/"+agentID); err != nil {
        return "", true, nil
    }
}
```

### 4. 迁移映射 `internal/store/database.go`

```go
case cfg.UserID != "" && cfg.AgentID != "":
    kvScope   = "user-agent"
    kvScopeID = cfg.UserID + "/" + cfg.AgentID   // 不再折叠到 user 层
```

## 回归护栏（测试）

- `internal/scope/configs_kv_test.go` → `TestProviderPerUserAgentIsolation`：
  绑定 per-(user,A) provider key，断言 B（兄弟 agent）读不到、不落 user 层。
- `internal/store/configs_kv_test.go` → `TestMigrateConfigsToKV`：四层迁移后
  per-(user,agent) 落 `(user-agent, X/Y)` 独立行，不碰撞。
- `internal/setup/configs_kv_e2e_test.go` → `TestProviders_CloudPathE2E`：
  真实 handler 路径下兄弟 agent 不继承 agent-scope key。

## 对上游的 PR 提案

**标题建议**：`fix(configs-kv): preserve per-(user, agent) scope instead of folding to user`

**正文要点**：
1. 上游 `kvScopeFromOwnership` 把 `(X, Y)` 折叠到 `(user, X)`，两个故障：
   a. 同用户多 agent 时，per-(user,A) 的 provider key 泄漏给该用户的其他 agent（数据泄露）；
   b. 迁移 `migrateConfigsToKV` 时同用户多 agent 的 key 撞到 `(user, X)` 同一行，last-write-wins 静默丢数据。
2. 改动：`configs_kv` 增加第四层 scope `user-agent`，`scope_id = userID/agentID`；
   `GetValue`/`GetValues` 读路径加最内层；迁移映射不再折叠。
3. 破坏性：无。`user-agent` 是新 scope 值，存量数据要么没有该层、要么迁移前已折叠
   （可重跑迁移恢复）。写路径上游本身只在 3 层内产生数据，此改动只影响
   **既有 per-(user,agent) 行**的读写，属修复性增强。
4. 附带回归测试 3 组（隔离 / 迁移 / e2e）。

**可选**：如果上游不想要第 4 层，替代方案是把 `(X, Y)` 拒绝/回退（fail-closed）
而非静默折叠——至少避免泄漏；但四层模型才是语义正确的修复。
