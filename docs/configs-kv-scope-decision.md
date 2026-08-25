# FastAgent configs 设计决策 — 维持现状，不纠偏

> 状态：已决策（2026-08-25）。本记录归档「是否全盘纠偏 fork 的 configs_kv
> 双列 + 4 层 scope 设计、回退到上游 fastclaw 单列方案」的完整讨论，供后续
> 重启纠偏时复用调研结论。

## 决策

**不改变任何代码。** fork 的 `configs_kv` + `user_id/agent_id` 双列 + 4 层
scope 设计**全部保留**。如需后续再评估再提。

## Context（为什么曾考虑纠偏）

- fork 自建「configs_kv KV 表 + 双列 + 4 层 scope」，上游 fastclaw 用
  「scope_id 单列 + JSON blob」。曾考虑**全盘纠偏**回退到上游方案。
- 起初依据「产品无共享 agent 场景」判定 fork 的 per-(user,agent) 层属过度设计。
  **该前提已被推翻**——用户确认**共享 agent 场景仍需要**。

## 为什么维持现状（上游实测 origin/dev）

1. **上游也有 4 层**：上游 scope.go `Providers`/`Setting` 读路径同样遍历
   `system→user→agent→per-(user,agent)`（`scope_id='U/X'`）。但上游的
   user-agent 是**隐式字符串编码**——`ScopeFromOwnership` 注释自述
   「Today the dashboard only reads scope=system/user/agent; the new
   compound keeps the door open for the multi-tenant view」。
2. **fork 双列是显式共享 agent 支持**：`user_id/agent_id` 双列可直接 SQL
   查询 per-(user,agent) 行，无需拆字符串；`configs_kv` 提供 KV 读路径。
   共享 agent 场景下这是更直接的能力承载。
3. **「完全同步上游」= 丢显式性**：scope_id='U/X' 编码下查共享 agent 需拆
   字符串，且上游 dashboard 都不渲染该层。用户明确「不要完全同步上游」。

## 调研沉淀（后续若重启纠偏可复用）

- **blob↔KV 恒同步**：`dualWriteSettingKV`(scope.go:820)/`dualWriteProviderKV`
  (scope.go:842) 每次 `SaveConfig` 同步写 blob+KV，KV 方法调用方全在 scope.go
  （无 KV-only 写路径）→ 若未来要删 configs_kv，可直接 DROP 表（blob 即事实源），
  **无需** unflatten-merge。
- **迁移顺序约束**：`migrateChannelsFromConfigs`(database.go:3809，调用点
  database.go:165) 必须在删 configs 双列**之前**跑（读 channel 行依赖
  user_id/agent_id/credential_key）。
- **上游终态 schema**：`(id, kind, scope, scope_id, name, enabled, data,
  created_at, updated_at)` + `UNIQUE(kind, scope_id, name)` +
  `idx_configs_scope`；`computeScopeID`：user-agent→`U/X`。
- **5 个 KV 方法 + configs_kv 表 + migrateConfigsToKV** 若未来删，需同步改：
  scope.go KV 辅助、10 处 `LookupChannelByCredential` 降级分支
  （handlers_agent_channels.go ×7、handlers_scoped.go ×3）、configs_kv 测试 ×3
  （internal/scope/configs_kv_test.go、internal/setup/configs_kv_e2e_test.go、
  internal/store/configs_kv_test.go）。

## 技能审查

- **clean-architecture**：维持现状 = 无改动，架构保持 fork 既有形态
  （configs_kv 位于 Frameworks 层，scope.go 经 `store.Store` 接口读，DIP 未破坏）。✓
- **vercel-react-best-practices**：无 web/React 改动 → **n/a**。✓

## 后续

- 纠偏议题归档于此决策记录；产品若出现共享 agent 需求变化，或 configs_kv
  造成维护负担，再评估。

## 关联文档

- `fastagent/docs/configs-kv-scope-adaptation.md`：fork 对 configs_kv 四层
  scope 的适配说明（给上游 PR 的文档）。
