# fastagent/docs · 顶层索引（L2）

> 状态：索引 · 最后核对：2026-09-28（改动册新增第 69–75 行：回合身份 / 幂等键 / 结局字段 / 队列计数）· 维护规则见文末
> 用途：一份**能一眼看全**的目录索引——每篇文档做什么、什么状态、是不是某主题的**唯一有效来源**、
> 被代码引用多少、以及它被哪套形式化系统约束。
> **形式化推理的唯一入口**：[文件系统形式化证明/00-formal-systems.md](./文件系统形式化证明/00-formal-systems.md)
> （英文镜像 [fs-formal-proof/00](./fs-formal-proof/00-formal-systems.md)）——它 §4.1 的"形式系统 → 机制 →
> 被约束的设计文档"反向索引，是**所有形式化相关文档的汇总入口**。

## 1. 按用途分桶

| 桶 | 是什么 | 入口 |
|---|---|---|
| **A. 形式系统** | 三套形式化（F1 前置条件/零迁移 · F2 可观测性 · F3 投递）+ 机制层（租约 L1–L7）+ 子系统契约 | [形式化集 00](./文件系统形式化证明/00-formal-systems.md)（§1 系统 / §4.1 反向索引 / §7 全量清点 / §11 交付索引） |
| **B. 设计与决策** | 某块机制的 as-built 设计与决策记录 | 见 §2 表 |
| **C. 事故与排查** | 一次真实事故的取证与结论 | [04](./文件系统形式化证明/04-incident-workspace-2026-09-17.md) · [sandbox-scope-leak.md](./sandbox-scope-leak.md) |
| **D. 一次性分析 / 运维** | 一次性扫描、清理、发布准备 | 见 §2 表（多为零引用） |
| **E. 提案** | 尚未实现的提案与清单（MCP / skills 出口） | [issues/mcp-egress-decisions.md](./issues/mcp-egress-decisions.md)（**该主题的唯一有效决策来源**；出口面的**审计判据 I1–I5 与循环**在 cloud `docs/audits/README.md`——规则只有那一个家，这里只记决定与指向） |

## 2. 每篇一行

**形式化集**（中文 origin）：[00](./文件系统形式化证明/00-formal-systems.md) 索引 · [01](./文件系统形式化证明/01-current-implementation.md) 实现实录 · [02](./文件系统形式化证明/02-semantics-and-architecture.md) 四层语义 · [03](./文件系统形式化证明/03-state-machine-and-timing.md) 时序 · [04](./文件系统形式化证明/04-incident-workspace-2026-09-17.md) 事故 · [05](./文件系统形式化证明/05-remediation-plan.md) 方案 · [06](./文件系统形式化证明/06-cordis-review.md) Cordis 复审 · [07](./文件系统形式化证明/07-formal-rootcause-and-fix.md) 根因/证明/修复（**F1 权威**）· [08](./文件系统形式化证明/08-state-observability-principle.md) 可观测性（**F2+F3 权威**）· [09](./文件系统形式化证明/09-sandbox-lifecycle-audit.md) 沙箱生命周期审计 · [10](./文件系统形式化证明/10-harness-state-audit.md) 全 harness 审计 · [11](./文件系统形式化证明/11-change-register.md) 改动点总册（**交付唯一索引**）· [12](./文件系统形式化证明/12-lease-formal-design.md) 租约形式化设计（**F1 机制层**）。英文镜像逐篇对应：[fs-formal-proof/](./fs-formal-proof/README.md)。

| 文档 | 类型 | 状态（原文摘） | 权威性 | 形式 | 代码引用 |
|---|---|---|---|---|---|
| [session-turn-integrity.md](./session-turn-integrity.md) | 设计 + 事故 | `Status: P0–P6 landed, plus Q4` | 会话轮次完整性（W/P/O/T）唯一来源；**A1–A4 已落地（工作区，见 §A3.1 落地日志与各条的 landing log；改动册的状态格里仍是「待批准」）** | F1（A1/A3）· F2（A2）· F3（A4） | **24** |
| [sandbox-scope-leak.md](./sandbox-scope-leak.md) | 事故排查 | `状态：事故记录，根因已修（G17 族）` | 该次容器作用域泄漏的唯一取证 | F1（作用域同一性） | 7 |
| [configs-kv-scope-adaptation.md](./configs-kv-scope-adaptation.md) | 设计 + 实现对照 | `状态：fork 已落地` | configs 数据域的实现对照 | 子系统契约（scope） | 2 |
| [configs-kv-scope-decision.md](./configs-kv-scope-decision.md) | 决策 | `状态：已决策（08-25 维持；09-13 保留并继续演进）` | **该数据域唯一决策来源** | 子系统契约（scope） | 0 |
| [prompt-inventory.md](./prompt-inventory.md) + [prompt-inventory/](./prompt-inventory/) | 参考资料 | `状态：清单已生成（改提示词须重跑脚本）` | 模型可见文本的唯一清单（A–F 六份） | —（提示词资产） | 2 |
| [chat-event-delivery.md](./chat-event-delivery.md) | 设计 | `Status: ✅ landed` | 跨副本事件投递唯一来源 | F3（机制 3） | 1 |
| [chat-event-delivery-placement.md](./chat-event-delivery-placement.md) | 决策 | `Status: decided · 2026-09-24 · 未实现` | **订阅落点（session 键亲和 vs fan-out 中继）唯一决策来源** | —（放置 / 容量） | 0 |
| [sandbox-pool-leases.md](./sandbox-pool-leases.md) | 设计 | `Status: implemented, unreleased` | 沙箱池租约 U/A/I 唯一来源（含 G25 备注） | F1（机制 3 的 as-built 格） | 1 |
| [mcp-oauth-design.md](./mcp-oauth-design.md) | 设计 | `状态：Draft` | MCP OAuth 客户端（协议合规族） | 子系统契约（OAuth 安全条款） | 1 |
| [coding-agent-runtime.md](./coding-agent-runtime.md) | 设计 | `状态：as-built 契约` | coding 会话与预览运行时 | F1（作用域） | 1 |
| [tool-output-limits.md](./tool-output-limits.md) | 设计 | `状态：已实现` | 工具输出有界 + delivered-frames 条款 | F2（机制 3） | 0 |
| [per-chatter-files.md](./per-chatter-files.md) | 设计 | `Status: fixed` | 每-chatter 文件路由 | 子系统契约（身份） | 0 |
| [sandbox-background-exec.md](./sandbox-background-exec.md) | 设计 | `状态：已实现` | 沙箱后台执行与 tail 排空 | F3（机制 1/2） | 0 |
| [sandbox-secret-rotation.md](./sandbox-secret-rotation.md) | 运维 runbook | `状态：可执行的运维手册` | 密钥轮换步骤 | —（运维） | 0 |
| [upstream-pr-split.md](./upstream-pr-split.md) | 发布准备 | `Status: analysis done, nothing ported yet` | 租约实现拆 PR 的分析 | —（交付） | 0 |
| [dependency-alert-triage.md](./dependency-alert-triage.md) | 一次性分析 | `状态：✅ 已落（09-15/09-16）` | 164 条依赖告警的结论 | —（依赖） | 0 |
| [webui-lint-cleanup.md](./webui-lint-cleanup.md) | 一次性分析 | `状态：✅ 已落（09-15）` | webui lint 清零记录 | —（前端） | 0 |
| [agent-commit-checks.md](./agent-commit-checks.md) | 一次性清单 | `状态：已生效` | 提交前检查清单（**当前无人引用**） | —（流程） | 0 |
| [upstream-api.md](./upstream-api.md) | 接口文档 | `状态：接口契约` | 上游 App 集成 API（**当前无人引用**） | —（接口） | 0 |
| [query_optimization/QUERY_OPTIMIZATION.md](./query_optimization/QUERY_OPTIMIZATION.md) | 参考资料 | 无状态行 | SQL 优化指南 | —（性能） | 0 |
| [REBRAND/REBRAND_PLAN.md](./REBRAND/REBRAND_PLAN.md) | 历史记录 | 无状态行 | FastClaw → FastAgent 改名记录 | —（历史） | 0 |
| [issues/](./issues/)（9 篇） | 提案/决策/清单 | 全部有 `状态 + 日期` | 见各自文件；[mcp-egress-decisions.md](./issues/mcp-egress-decisions.md) 为 MCP 出口的唯一有效决策来源 | 协议合规族 | 见单篇 |

> "代码引用" = 仓库内 `internal/` `cmd/` 里提到该文件名的 Go 文件数（2026-09-19 实测）。
> 0 引用不等于无用（清单/运维/参考类本来就不被代码指向），但它说明**这些文档没有"机械保护"**：
> 改代码的人不会因此被提醒去更新它。

## 3. 按目的读

| 你要做的事 | 先读 |
|---|---|
| 第一次理解这套系统 | [形式化集 README](./文件系统形式化证明/README.md) → [00](./文件系统形式化证明/00-formal-systems.md) → [01](./文件系统形式化证明/01-current-implementation.md) |
| 改文件同步 / 沙箱 / 存储相关代码 | [00 §4.1 反向索引](./文件系统形式化证明/00-formal-systems.md) → [07](./文件系统形式化证明/07-formal-rootcause-and-fix.md) · [12](./文件系统形式化证明/12-lease-formal-design.md) |
| 改任何会改变 agent 可见状态的东西 | [08 §6 审查清单](./文件系统形式化证明/08-state-observability-principle.md) → [10 §4 缺口表](./文件系统形式化证明/10-harness-state-audit.md) |
| 想知道"改了哪些、测了哪些、上没上线" | [11 改动点总册](./文件系统形式化证明/11-change-register.md)（§12 是已采纳未实现的设计） |
| 排查一次相似的事故 | [04](./文件系统形式化证明/04-incident-workspace-2026-09-17.md) · [session-turn-integrity.md](./session-turn-integrity.md)（09-18 跨副本双轮次）· [sandbox-scope-leak.md](./sandbox-scope-leak.md) |
| 做 MCP / skills 出口相关的事 | [issues/mcp-egress-decisions.md](./issues/mcp-egress-decisions.md)（唯一有效来源） |

## 4. 目录树

```
docs/
├── README.md                        ← 本文件（L2 索引）
├── 文件系统形式化证明/                 ★ 中文 origin（00–12 + README，14 篇）
├── fs-formal-proof/                 ★ 上面 14 篇的 1:1 英文镜像
├── <顶层 19 篇>                      设计/决策/事故/一次性分析（见 §2 表）
├── issues/                          9 篇：MCP/skills 提案、决策、对照清单
├── prompt-inventory/                6 篇 + 同名顶层 md：模型可见文本清单
├── query_optimization/              1 篇
└── REBRAND/                         1 篇（历史）
```

## 5. 维护规则（三条，尽量小）

1. **新文档必须带状态行**（借用 `issues/` 的格式：`状态 · 日期 · 是否仍有效`）。**2026-09-19：顶层原缺状态行的 8 篇已补齐**（agent-commit-checks · coding-agent-runtime · prompt-inventory · sandbox-background-exec · sandbox-scope-leak · sandbox-secret-rotation · tool-output-limits · upstream-api）；剩下 `query_optimization/`、`REBRAND/` 与 `prompt-inventory/` 的子文件仍是参考/历史类，暂不补。
2. **某主题的唯一有效来源要显式声明**（先例：[issues/mcp-egress-decisions.md](./issues/mcp-egress-decisions.md) 开头"本文档是当前唯一有效的决策来源；冲突时以本文为准"）。同一主题的其他文档降级为"推导过程"。
3. **形式化相关的文档必须挂到 [00 §4.1 反向索引](./文件系统形式化证明/00-formal-systems.md)**（形式系统 → 机制 → 文档，中英两份同步）。

> **不要搬文件。** 63 篇的相对链接、24 处代码注释都指向现路径；规整靠"加索引 + 加状态行"，
> 不靠移动目录（Musk 顺序：先删/简化，最后才谈重构结构）。
