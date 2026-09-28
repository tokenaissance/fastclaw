# 07 · 根因的形式化描述、论证与修复

> 状态：根因已闭合（含活体沙箱直读证据）· 最后核对：2026-09-17
> 记号体系沿用 [mcp-oauth-design.md §13](../mcp-oauth-design.md) 的 Cordis 形式化语言
> （revertible effect、左逆、前置条件、keyed diff、系统边界）。本文只用该语言陈述，不引入新框架。
> 前置：[01](./01-current-implementation.md) · [04 事故链](./04-incident-workspace-2026-09-17.md) · [06 六条判据的初次复审](./06-cordis-review.md)
>
> **定位**：本篇是文档集里**三套形式化系统**中的 **F1（前置条件 / 零迁移）** 的权威定义——
> 它只回答一个问题：**这次迁移允不允许发生**（不允许 ⇒ 报错 + 零迁移）。
> 另外两套是 **F2 可观测性**（哪些变更必须说）与 **F3 投递**（说了怎样真的到达），都定义在
> [08 §2 / §2.2](./08-state-observability-principle.md)；三者的分工与总索引见 [00-formal-systems.md](./00-formal-systems.md)。

---

## 第一部分 · 根因确认（实证，非推理）

### 1.1 闭环证据

对事故会话 `OvDLzEPKcEK84Hpfq0U6LK`（agent `agt_cda27bbfbf4a84e2dfa6`），
按 `sandbox_leases` 的租约行定位活体沙箱 `iydyr4nz1rt1kxq57a1mh`，
经 E2B `POST /sandboxes/{id}/connect` 取 token 后直读 `/workspace`：

```
GET /workspace/byo-account-design.html : status=200 bytes=41262
GET /workspace/qc-email-draft.md       : status=200 bytes=5705
GET /workspace/todo.md                 : status=200 bytes=580
GET /workspace/sessions/OvDLzEPKcEK84Hpfq0U6LK/byo-account-design.html : status=404
```

沙箱内的三个文件**至今仍是旧版本**，且 store 中与沙箱中字节数逐一相等
（S3：41,262 / 5,705 / 580，`LastModified` 12:15:16–17）；第 4 条 404 同时排除了
"沙箱里另有 `sessions/<sid>/…` 一份副本"的可能。（读完后已把沙箱重新 pause。）

**字节级复核（2026-09-17 二次自校验补做）**：沙箱内
`md5sum /workspace/byo-account-design.html = 305a43137bf54d18235ef50647579413`，
与 S3 对象的 ETag `"305a43137bf54d18235ef50647579413"` **完全一致** ——
store 里现在那份就是沙箱那份，不是"另一个同长度版本"。

同次自校验读到的 `/workspace` 元数据（`stat`）：

```
birth  2026-09-17 07:56:53.453830420 +0000   byo-account-design.html   41262
birth  2026-09-17 07:56:53.453830420 +0000   qc-email-draft.md          5705
birth  2026-09-17 07:56:53.457830476 +0000   todo.md                     580
mtime  2026-09-17 07:41:04                   byo-account-design.html
mtime  2026-09-17 07:39:44                   qc-email-draft.md
mtime  2026-09-17 07:41:29                   todo.md
```

`birth` 全部等于 B 的 hydrate 时刻 07:56:53（[04](./04-incident-workspace-2026-09-17.md) §2.2），
而 `mtime` 保留了 store 当时的 `LastModified`（对照：`qc-lean-mcp/README.md` 在沙箱里是
`05:30:21`、在 S3 里也是 `05:30:21`）。这条元数据同时给出了两个结论：

1. **B 自出生起未收到过宿主工具的写入**——若镜像生效过，这些文件会出现新的 mtime 或新副本；
   实测沙箱里**不存在**任何 55,527 / 6,893 / 462 字节的副本（同目录下也没有镜像路径的残留）。
2. **mtime 不能用作跨副本判据**：hydrate 把 store 的 `LastModified` 写进沙箱文件的 mtime，
   所以"新内容"与"旧内容"在两侧看起来是同一次写入——这独立地否决了"用文件时间戳仲裁"的方案
   （与 [05](./05-remediation-plan.md) §8 的 ADR 一致）。
   > **2026-09-18 补（G22）**：正因如此，**穿透也必须盖章**（把 store 的 mtime 写到它镜像的那份上），
   > 否则同一路径的两份 mtime 天然不同、每次同步都要读整份字节来确认"一样"。
   > 这一条在项目会话里曾静默失效（盖章用沙箱作用域查 store，而工具写项目根）——已修，见
   > [10 §4 G22](./10-harness-state-audit.md)；真机实测该路径的整对象读取 **1 → 0**。

### 1.2 事件链（全部有日志或对象时间戳支撑）

| 时间 (UTC) | 事件 | 来源 |
|-----------|------|------|
| 05:25:33 | 沙箱 **A** `iknoadnp4ho8un3yhmjr2` 建立，`workspaceFiles=1` | `e2b sandbox hydrated` |
| 05:25:50 – 07:30 | A 的多次快照同步**全部失败**（`over the 32.0 MB cap`） | `sandbox sync: snapshot failed` ×42 |
| 06:26:14 / 07:38:05 / 07:52:58 | A 已死（`extend timeout … 404`） | `could not extend the sandbox timeout` |
| 07:56:44 | A 过期，开始重建 | `e2b sandbox expired, recreating` |
| 07:56:53 | 沙箱 **B** `iydyr4nz1rt1kxq57a1mh` hydrate，**`workspaceFiles=10`** | `e2b sandbox hydrated` |
| 07:59:21 | B 被另一 pod 采纳（本地缓存过期） | `e2b sandbox adopted (local cache stale)` |
| 08:04–08:19 | agent 用 `edit_file`/`apply_patch` 多次改写三个文件（**只写 store**） | `session_messages` seq 256–297 |
| 08:09:47 | **A 的 evict 同步**推 2 个文件 → 第一次回退 | `synced … cause=evict files=2` |
| 08:15:26 | agent 首次发现回退 | seq 280 |
| 08:15:44 / 12:07:00 | agent 两次修复（写回 6,893 / 55,527 / 462） | seq 283 / 318–320 |
| 08:24:18 | **B 的 evict 同步**推 3 个文件 → 第二次回退 | `synced … cause=evict files=3` |
| 12:05:26 | B 的 post-exec 同步推 1 个文件 | `synced … cause=post-exec files=1` |
| 12:15:17 | **B 的 evict 同步**推 3 个文件 → 最终态 = B 的 07:56 内容 | `synced … cause=evict files=3` |

关键补充：B 建立于 07:56:53，而三个文件第一次被宿主写入是 08:04 ——
**B 从出生那一刻起就是错的版本**，且在 session 余下的 4.5 小时里从未获得更新。

### 1.3 结论（不改动）

> 根因是 `syncSnapshot` 的判据：**它把"沙箱副本"当作权威，
> 用 `|X[p]| ≠ |S[p]|` 推断"沙箱更新了"，而唯一能支持这个推断的前提——
> "store 里同路径的内容只能来自沙箱"——在宿主文件工具存在时不成立。**

两个放大器（不是引入者）：A 期间快照因体积上限连续失败（掩盖了这条路径）、
B 因租约可跨 pod 采纳而长寿（旧副本存活 4.5 小时、跨 5 次同步）。

---

## 第二部分 · 形式化

### 2.1 论域

取一个 scope `σ`（本文中 `σ = (agt_cda27bbfbf4a84e2dfa6, "", OvDLzEPKcEK84Hpfq0U6LK)`，
即无 project 的松散会话）。

```
K          键（key）集合：工作区里的每条路径；本文关心
             P = {byo-account-design.html, qc-email-draft.md, todo.md}
Γ          digest 值域：可判等的内容摘要（size 或 size+hash）
Δ          digest 全集，含 ⊥（不存在）

S : K ⇀ Γ   store 的当前内容（权威副本）
X : K ⇀ Γ   沙箱 /workspace 的当前内容（缓存副本）
B : K ⇀ Γ   基线：最近一次"两侧观测等价"时的内容（当前实现中不存在）
```

动作（action）与它们的效果：

```
HostWrite(p,v)     S[p] := v            —— 宿主文件工具；本实现中不触碰 X
SandboxWrite(p,v)  X[p] := v            —— exec 在沙箱内写入
Hydrate            X := S|dom(S)        —— 沙箱诞生时的一次性复制
Sync(cause)        —— 待定义：当前实现与修复版本的差别就在此
```

系统边界（§13.1 原则 6）：

```
inside   S、X、Hydrate、Sync —— 系统可独占修改并恢复
outside  Read(t)：外部观察者（用户、agent 的后续推理、另一个 pod）在时刻 t 读到的 (S,X) 快照
         Emission：任何由 Read(t) 引发的、系统无法收回的行为
```

### 2.2 待证命题

> **命题 D（Defect）**：当前实现的 `Sync_cur` 不满足"前置条件错误 ⇒ 零迁移"，
> 因此在 `dom(S) ∩ dom(X) ≠ ∅` 且两侧内容被不同写入者更新过时，
> 存在一个使 `S` 失去**未被观察过**内容的执行。

### 2.3 `Sync_cur` 的定义与缺陷

当前实现（[lifecycle.go](../../internal/sandbox/lifecycle.go) 第 461 行）逐键为：

```
Sync_cur(p):
    if S[p] = ⊥            then  S[p] := X[p]        (T1)
    elif |S[p]| = |X[p]|   then  跳过                 (T2)
    else                        S[p] := X[p]         (T3)
```

分析：

| 分支 | 语义 | 是否正确 |
|------|------|---------|
| T1 | 沙箱产物落盘 | ✅ 正确：`p ∉ dom(S)` 是"这内容由沙箱产出"的充分条件 |
| T2 | 大小相同视为未变 | ⚠️ 仅在单写入者下成立；长度相同的异内容会漏写 |
| T3 | 大小不同 ⇒ 用 X 覆盖 S | ❌ **无前置条件**。两侧都可能是新的，而实现无条件选 X |

**缺陷的形式化陈述**：`Sync_cur` 没有任何前置条件检查（no precondition guard），
它从"状态差异"直接推断"意图方向"。按 §13.1 原则 5，`set(k,v)` 要求 `k ∉ dom`；
违反时报错且零迁移。`Sync_cur` 的 T3 分支等价于"对已存在的 key 静默覆盖"，
即**在任何前置条件都不成立的情况下执行迁移**。

### 2.4 证明（以本事故为例，构造性）

以下时间线逐条对应 §1.2 的证据。记 `p₀ = byo-account-design.html`。

```
t₀ = 07:56:53   Hydrate:      X := S|dom(S)               ⇒ X[p₀] = 41,262
t₁ = 08:04–08:19 HostWrite:   S[p₀] := 55,527 (经中间版本)  ⇒ X[p₀] = 41,262  (未更新)
t₂ = 08:24:18   Sync_cur(evict):
                  |S[p₀]| = 55,527 ≠ |X[p₀]| = 41,262
                  ⇒ 取 T3 分支 ⇒ S[p₀] := 41,262           ⇒ X = S，且 S 的 55,527 消失
```

于是：

1. **T3 在 `X[p₀] ≠ S[p₀]` 时无法区分两种世界**：
   `W₁`（沙箱编辑、store 未变）与 `W₂`（store 编辑、沙箱未变）产生**相同的观测**
   `(|S[p₀]|, |X[p₀]|)`。选择 X 在 `W₁` 正确、在 `W₂` 错误；
   本事故是 `W₂`（有 seq 259–264、297、318 的宿主写入为证）。∎(D)

2. **该执行含一次 outside 破坏**：`Read(t)` 在 `t₂` 之后读到的是 41,262，
   而 agent 在 `t₁` 之后已经基于 55,527 / 6,893 / 462 继续推理
   （seq 323 的报告、324 的用户决策）。被观察过的内容不可收回 ⇒ 属 emission，
   不在 inside 的可逆范围内。

3. **`Sync_cur` 不是 reconcile（违反 §13.1 原则 7）**：
   `Sync_cur² ≠ Sync_cur` 在一般情形下成立——每次执行都以当前 `X` 覆盖 `S`，
   若期间有第三次写入，再跑一次会再次改变 `S`。它既不幂等也不收敛，
   因此不能被称为"声明式收敛"。

### 2.5 为什么在 docker 上 D 不可触发（同一定理的推论）

在 docker 后端，`X` 的实现是"宿主目录的绑定视图"，即 `X ≡ S` 恒成立
（[01](./01-current-implementation.md) §2 的 docker 行；`SnapshotWorkspace` 直接 walk 宿主目录）。
于是对任意 `p`、任意时刻：`|S[p]| = |X[p]|` 恒真 ⇒ `Sync_cur` 永远走 T2 分支，
T3 不可达。**D 的成立需要一个"X 与 S 可分离"的领域，而 docker 不提供这个领域。**

这也给出"为什么早期版本没暴露"的精确说法：缺陷（T3 无前置条件）自 2026-04-20 起一直存在，
但 E2B 的 `SnapshotWorkspace` 直到 2026-05-01 才让 `X ≠ S` 成为可能
（详见 [04](./04-incident-workspace-2026-09-17.md) §6）。

---

## 第三部分 · 修复

### 3.1 定性：Sync 不是 effect，是 reconciler

按 §13.1，`effect = Γ → Γ×(Γ→Γ)` 适用于**有意图的动作**（add/remove/login…）。
`Sync` 没有意图方向，它是一次**声明式收敛**（keyed diff）。对 reconciler 的正确要求是：

> **R1（前置条件）** 每个 key 的迁移只在显式前置条件成立时发生；
> **R2（零迁移）** 前置条件不成立时不改变任何 key，并产生可观测的失败；
> **R3（收敛）** `Reconcile` 幂等：`Reconcile ∘ Reconcile = Reconcile`；
> **R4（局部性）** 一次 reconcile 不改变 `dom`（不新增/删除邻居 key）。

### 3.2 引入基线 B：把"谁改的"变成可判定事实（**已废弃**，见 §3.11.3）

> 这一节与 §3.3 的判定表、§3.3.1 的摘要结论、§3.8、§3.10 描述的是**同一个中间形态**：
> 用一个 scope 级（后来是进程内）的基线 `B` 记录"上次交给沙箱的那一份"。
> 2026-09-18 该形态被**整体下线**（跨副本裁决 + 进程内记忆不构成保证，见 §3.11.3），
> 保留在此作为演进记录：它解释了"为什么一开始需要它"，也解释了它为什么不够。

`B` 是 scope 级持久文档，每键一行（复用 `configs_kv`，`kind = ws_baseline`，
与 `mcp_undo` 同构；见 [mcp-oauth-design.md §13.2/§13.7](../mcp-oauth-design.md)）：

```
Hydrate:      ∀p∈dom(S):  B[p] := S[p]        （只写一次，随 hydrate 现场产出）
Reconcile(p): 迁移发生时才更新 B[p]
Release(X):   B 保留（scope 级，不随实例销毁）
```

`B` 的作用是把 §2.4 中"不可区分的两个世界"分开：

```
W₁ 沙箱编辑、store 未变  ⟺  X[p] ≠ B[p]  ∧  S[p] = B[p]
W₂ store 编辑、沙箱未变  ⟺  S[p] ≠ B[p]  ∧  X[p] = B[p]
W₃ 两侧都变（真冲突）   ⟺  S[p] ≠ B[p]  ∧  X[p] ≠ B[p]
```

### 3.3 写入穿透 + 前置条件（2026-09-17 落地；判定表部分已被 §3.11.3 取代）

在 §3.2/§3.3 的判据之前，先加**一个动作**，它把大部分冲突在源头消掉：

> **宿主工具写文件时，把内容同时写进该 scope 的沙箱**（`LifecyclePool.WriteThrough`，
> 由 `write_file` / `edit_file` / `apply_patch` **逐路径**调用）。

理由是形式化里那句：事故的第三个必要条件（"宿主写过、且两副本内容不同"）在 docker 上
**永远不成立**——因为 bind mount 让 `X ≡ S`。穿透就是把这条性质搬到远程沙箱上：
写入那一刻两副本就一致，不等到事后仲裁。

穿透**逐路径**进行（一次调用一个文件），因此失败也是逐路径的：某个路径镜像失败时，
只有那个路径进入 `staleWrites`，其余路径照常。**不做"整体成功才算成功"**——
`apply_patch` 改多文件时，一个失败不应该连坐其它文件的 `exec` 产物。

在穿透之上，同步的判定表当时变成（`decideReconcile`，五个分支）：

> **已下线（2026-09-18）**：这张表的三列判据里有两列（基线摘要、stale 记账）已经不存在，
> 当前判定表见 §3.11.3。它保留在这里是为了说明"当时代价换来了什么"。

| store | 沙箱 vs 基线摘要 | 判定 | 动作 |
|-------|----------------|------|------|
| 无该路径 | — | 沙箱产物 | **推送** |
| 有 | 该路径 mirror 失败（stale） | 沙箱的副本更旧 | **拒绝** + 告警 |
| 有 | 与基线摘要相同 | 沙箱未动 | 跳过 |
| 有 | 不同，且 store 仍等于基线 | **沙箱自己的编辑** | **推送** |
| 有 | 不同，且 store 已偏离基线 | 两侧都动过 = 事故形态 | **拒绝** + 告警 |

### 3.3.1 为什么写透之后仍需要摘要（当时的实测结论；摘要已于 2026-09-18 下线）

> **后记（2026-09-18）**：下面这条结论对"进程内/持久基线"那一版设计成立，但它依赖
> 一份**记住**的指纹。最终形态把指纹换成了**自描述的时间戳**：穿透写入后立刻用
> `touch -d @<store mtime>` 把沙箱文件的时间戳对齐到 store 对象（hydrate 时同样如此，
> E2B 的 tar 条目用 `obj.ModTime` 落盘），于是"沙箱还拿着交给它的那一份吗"不需要任何
> 记忆就能判——代价是**沙箱编辑既有路径不再自动回写**，见 §3.11.3。

**注意时序**：下面第二行不是终态——沙箱的内容会在**下一次 `exec` 之后的回写**里到达
store，前提是那一刻 store 的对象仍等于基线（§3.3 判定表第 4 行）。完整回路见 §3.10。

我原本推断"穿透可以让摘要退场"，实现后**被测试否决**，这里如实记录：

```
宿主写 notes.md（穿透成功） → 沙箱 = store = "HOST"
沙箱再改它                  → 沙箱 = "HOST+EDIT"，store 仍是 "HOST"
```

此时 store 对象**等于基线**（没有宿主再写过），而沙箱内容变了——与"沙箱没动"在
元数据上**完全一样**。要区分"沙箱编辑过"与"什么都没发生"，只有
「上次交给沙箱的那份内容」这个指纹可用。所以摘要是**载重件**，穿透删不掉它；
穿透真正删掉的是**"宿主写过 ⇒ 一律拒绝"这条过宽的判定**（那曾让沙箱侧的改动无法回写）。

两者分工因此很清楚：

| 机制 | 消除什么 |
|------|---------|
| 写入穿透 | 宿主写入造成的分叉（事故的形态） |
| 摘要基线 | 穿透之后的沙箱编辑与"未变"之间的歧义 |
| ~~staleWrites~~ | **已下线**（2026-09-18 删基线时一并移除）：现在不记忆「哪条路径的镜像失败过」——两份副本不一致本身就是那个状态，判据直接读它 |

---

### 3.3.2 早期版本（保留为演进记录）

```
Reconcile(p):
  (1) S[p] = ⊥  ∧  X[p] ≠ ⊥
        前置: p ∉ dom(S)                        [沙箱产物]
        迁移: S[p] := X[p] ; B[p] := X[p]

  (2) X[p] = B[p]  ∧  S[p] = B[p]
        迁移: 无（两侧一致）

  (3) X[p] ≠ B[p]  ∧  S[p] = B[p]
        前置: X 相对基线变化 ∧ S 未变            [沙箱编辑]
        迁移: S[p] := X[p] ; B[p] := X[p]

  (4) S[p] ≠ B[p]  ∧  X[p] = B[p]
        前置: S 相对基线变化 ∧ X 未变            [宿主编辑]
        迁移: X[p] := S[p] ; B[p] := S[p]        ← 拉，而非跳过

  (5) S[p] ≠ B[p]  ∧  X[p] ≠ B[p]  ∧  S[p] ≠ X[p]
        前置: 不成立（真冲突）                    [见 3.4]
        迁移: 无 ; 置 B[p] := ⟨CONFLICT, X[p], S[p]⟩ ; 报错

  (6) X[p] = ⊥  ∧  S[p] ≠ B[p]
        前置: 不成立（沙箱侧删除，逆操作需被删内容快照）
        迁移: 无 ; 报错

  (7) B 缺该键（历史 scope）
        迁移: 仅执行 (1)；其余保持不动（保守退化）
```

与原实现的对照：**T2 被拆成 (2)(3)(4)**，其中 (4) 是新增的方向（把宿主编辑同步进沙箱，
而不是跳过）；**T3 被 (5)(6) 取代：报错 + 零迁移。**

### 3.4 冲突的收敛出口（保持 R3/R4）

违反前置条件必须零迁移，但**零迁移会让同一冲突在下次 Reconcile 再次触发**。
按 R3/R4，出口只能是"改变声明式文档 B"，而不是新增文件：

```
第一次: B[p] := ⟨CONFLICT, X[p], S[p]⟩ ; 报错（fail loud，进 tool_result / session trace）
第二次: 读到 B[p] = ⟨CONFLICT, …⟩ ⇒ 按已声明策略收敛（默认：S 权威）
        X[p] := S[p] ; B[p] := S[p]
```

对比 [06](./06-cordis-review.md) 中我先前提出的 `shadow` 方案：它新增了一个 key
（违反 R4），且每次运行内容不同（违反 R3），因此被本文正式废弃。

### 3.5 边界声明（原则 6）

| 位置 | 分类 | 形式化理由 |
|------|------|-----------|
| `S` 写入 / `X` 写入 / Hydrate / Reconcile | inside | 系统独占修改，可恢复到上次观测等价状态 |
| 用户在文件面板/下载中已读到的 `S[p]` | outside | `Read(t)` 已发生，无法收回 |
| agent 基于已读内容产生的后续写入 | outside | 同上；只能由 agent 重做（compensation） |
| 跨 pod 采纳期间对端 pod 的写入 | outside | 采纳方无法回滚对端动作 |
| 沙箱侧删除（规则 6） | inside 但当前不可逆 | 逆需要被删内容快照；无快照即报错 |

**推论**：若要避免事故中"agent 已据此推理"这类 outside 破坏，
防回退必须发生在 `Read` 之前——即写入后立刻收敛（规则 4 的"拉"），
而不是等到驱逐时才 reconcile。这给"post-exec 同步 + 及时 reconcile"提供了必要性论证。

### 3.6 文件动作层面（原则 1–3，当前完全缺失）

按 §13.1 原则 1，文件写入也应以 `(新状态, 逆)` 的形式返回，逆在应用现场产出。
本仓库已有同构范例：`<mcp-undo>` marker（[mcp_undo.go](../../internal/agent/mcp_undo.go)）。

| action | 前置条件 | 逆（应用现场产出） | witness |
|--------|---------|------------------|---------|
| `edit_file` | `old_string` 唯一匹配（**已有**） | 反向替换 | `digest` 回到前值 |
| `write_file` / `apply_patch` 覆盖既存键 | 当前无 | 旧内容（或内容引用）随 tool_result 返回 | 回写后 `S[p] = B[p]` |
| `write_file` 新建键 | `p ∉ dom(S)`（可由 T1 保证） | 删除该键 | 键不存在 |

其中 `edit_file` 的 `old_string` 匹配是本系统里唯一已符合原则 5 的写法，
可作为"前置条件即错误语义"的落地样板。

### 3.7 验收（每条对应一个可证命题）

| 命题 | 检查方式 |
|------|---------|
| R2 零迁移 | 构造 (5)(6) 状态，断言 `S`、`X`、`dom` 均未变，且返回错误 |
| R3 收敛 | 对同一状态连续调用 `Reconcile` 两次，断言第二次为恒等 |
| R4 局部性 | 断言一次 reconcile 前后 `dom(S) ∪ dom(X)` 不变 |
| 方向正确 | 构造 `W₁`/`W₂`/`W₃` 三态，断言 `W₁→推`、`W₂→拉`、`W₃→报错` |
| 边界声明 | 断言 `Read` 之后的状态里，(4) 的"拉"已经完成（不存在"用户读到新、沙箱仍旧"） |
| 左逆 witness | 对每条文件 action 断言 `g(δ) = γ`（写后撤销回到原 digest） |

**落地状态（2026-09-17）**：[internal/sandbox/lifecycle_sync_contract_test.go](../../internal/sandbox/lifecycle_sync_contract_test.go)
已落成 10 个可执行测试，**全部通过**，逐条对应上面的命题：

| 测试 | 对应命题 | 断言 |
|------|---------|------|
| `StoreEditIsNotOverwritten` | 方向正确（`W₂`） | 宿主写过的键**不得**被沙箱覆盖，且零迁移 |
| `StoreEditSurvivesPostExecTrigger` | 同上 × post-exec | 连续两次 reconcile 也不得回退 |
| `NewPathIsFlushed` | 方向正确（`W₁`） | 沙箱产物必须回写 |
| `WriteThroughKeepsSandboxEditsPushable` | 穿透 + 摘要 | 穿透后沙箱再改同一文件 → 必须回写（摘要的唯一用处） |
| `StaleMirrorBlocksOnlyThatPath` | 逐路径失败语义 | 一个路径镜像失败，只锁该路径；无关产物照常回写 |
| `FailedMirrorIsRecordedNotFatal` | 失败非致命 | 镜像失败报给调用方并记账，store 不受影响 |
| `SecondReconcileWritesNothing` | R3 收敛 | 二次 reconcile 零写入 |
| `DomainUnchanged` | R4 局部性 | `dom` 不变 |
| `StoreOnlyKeySurvives` | §5.1 | store-only 键不被触碰 |
| `UnknownBaselineBlocksOverwrite` | 保守退化 | 无基线时不得授权覆盖 |

### 3.8 规则 (3) 的判据：hydrate 时的内容摘要（Phase 2；**已下线**，见 §3.11.3）

第一版修复只带"store 侧元数据"，因此把 `W₂`（宿主编辑）挡住的同时**也挡住了
规则 (3)**（沙箱编辑已 hydrate 的路径）——两者的痕迹在只有 store 元数据时不可区分：

```
W₂  宿主编辑 ⟺ store 对象不再等于基线（宿主写了它）
W₁' 沙箱编辑 ⟺ store 对象仍等于基线，但沙箱字节与之不同
```

补上**第三列事实**即可分开：`baselineEntry.digest` 记录 hydrate 时**复制过去的那份
内容**的 sha256（[lifecycle.go](../../internal/sandbox/lifecycle.go) 的
`refreshBaseline` / `digestOf`）。于是：

| store 对象 vs 基线 | 沙箱字节 vs 基线摘要 | 判定 | 动作 |
|---|---|---|---|
| 时间戳不同（宿主写过） | — | `W₂` | **BLOCK** + 告警 |
| 时间戳相同 | 相同 | 未变 | 跳过 |
| 时间戳相同 | 不同 | `W₁'` 沙箱编辑 | **推送** |
| 摘要缺失（超上限/读不到） | — | 不可判定 | **BLOCK** + 告警 |

代价与边界：

- 摘要只覆盖 `≤ baselineDigestMax`（2 MiB）的对象；更大的对象留空 → 走保守分支。
  理由是"需要在沙箱里编辑的文档"和"交付物/数据集"量级不同，而 hydrate 时全量读一遍
  大文件不划算；
- 摘要在 hydrate 时算、随实例存活，进程重启后重建——与沙箱实例同生命周期，
  因此不会拿旧实例的数据去判定新实例；
- 判定被抽成一个纯函数 `decideReconcile(statErr, info, base, data)`，
  六个分支一一对应，测试直接打在这张表上。

这一版**没有收紧任何原有能力**：规则 (1)（沙箱产物）与规则 (3)（沙箱编辑）都可用，
规则 (4)（宿主编辑）继续被拒。

### 3.9 端到端验证（真实 E2B）

[internal/sandbox/e2b_live_repro_test.go](../../internal/sandbox/e2b_live_repro_test.go)
两个用真实沙箱跑的验收测试（`FASTAGENT_E2B_LIVE=1` 门控，不进常规 CI）：

| 测试 | 验证什么 | 结果 |
|------|---------|------|
| `TestE2BLiveRepro` | 事故时序（Hydrate→宿主写→exec→同步）在真后端**不再复现**；且 `exec` 产物仍能落盘 | ✅ store 保住宿主版本；日志 1 条 `BLOCKED … storeBytes=54 snapshotBytes=40`；`from_exec.txt` 正常回写 |
| `TestE2BLiveHydrateKeepsStoreStamp` | hydrate 把 store 的 `LastModified` 写进沙箱文件 mtime（±1s，tar 整秒截断）、且沙箱与 store 是**分离**的两份 | ✅ |
| `TestE2BLiveLooseScopeLayout` | 松散会话的沙箱布局 = `/workspace/<key>`（01 §3.5 的对照表） | ✅ |
| `TestE2BLiveSandboxEditMigrates` | 规则 (3) 在真后端成立：沙箱就地改文件（无宿主写入）必须到达 store | ✅ |
| `TestE2BLiveWriteThroughReachesSandbox` | 穿透在真后端成立：宿主写入后沙箱**立刻读到新内容**，且随后的 reconcile 不动 store | ✅ |

第二条把 §1.1 那次一次性探针变成了可重复检查——它是"为什么 mtime 不能用来仲裁"的实物证据。

---

### 3.10 沙箱 → store 的回写机制（**基线版回路，已下线**，见 §3.11.3）

> 当前实现只把**新建路径**写回 store；判定表见 §3.11.3。下面这张回路图保留的是
> 摘要基线还在时的形状。

沙箱改动**会**回流到 store。回路如下：

```
沙箱内 exec 改了 /workspace/notes.md
        │
        ├─ 触发点 1：每条 exec 正常结束后（仅 RemoteWorkspace 后端）
        │             lazyExecutor.Exec → syncSnapshot(cause=post-exec)
        └─ 触发点 2：空闲驱逐 / 睡眠之前
                      flushIfSupported → syncSnapshot(cause=evict)
        │
        ▼
   SnapshotWorkspace()            e2b: tar -czf - -C /workspace . | base64
   · /skills 不在 /workspace 内，因此不进快照（它是只读挂载）
   · 整个 /workspace 打成一个 tar；有 32 MiB 上限（base64 后），超过则整次同步失败
        │
        ▼
   逐路径 decideReconcile（§3.3 的五行表）
   · store 无此路径            → 写
   · 该路径 mirror 失败(stale) → 不写
   · 沙箱内容 == 基线摘要       → 不写（沙箱没动）
   · 摘要不同 + store == 基线   → **写**（沙箱的编辑）
   · 摘要不同 + store ≠ 基线    → 不写（两侧都动过 = 事故形态）
        │
        ▼
   workspaceStore.Put(S3/本地) → 写成功后把该路径的基线重新锚定为刚写的这份
```

所以 §3.3.1 那个例子的**完整**时序是：

```
t0  宿主写 notes.md + 穿透     → store = 沙箱 = "HOST"，基线摘要 = H("HOST")
t1  沙箱 exec 编辑 notes.md     → 沙箱 = "HOST+EDIT"（store 此刻仍是 "HOST"）
t2  该条 exec 结束后的 post-exec 同步
      摘要 H("HOST+EDIT") ≠ 基线 H("HOST")，且 store 仍等于基线
      ⇒ 判定第 4 行 ⇒ Put("HOST+EDIT")，基线摘要 := H("HOST+EDIT")
t3  store 与沙箱一致，稳态
```

也就是说："store 仍是 HOST"只存在于 `t1 → t2` 之间，宽度等于**一条 exec 的时长**。
这就是为什么判据要能区分"沙箱动过"与"没动"——否则 `t2` 会走"跳过"，沙箱的编辑永远
到不了 store（旧实现里它靠"字节数不同就覆盖"蒙对，代价是宿主写入被覆盖，也就是事故）。

两个已知边界：

| 边界 | 后果 | 规避 |
|------|------|------|
| `/workspace` 超过 32 MiB（快照上限） | **整次同步失败**，什么都不写回；日志 `workspace snapshot is over the 32.0 MB cap` | 把大文件/日志/缓存放 `/tmp`（提示词已这么要求）；这也解释了事故当天 05:25–07:52 那段"没有回写"的空白 |
| 沙箱从未再运行 exec，直接被驱逐 | 触发点 2 仍会同步一次 | 无需处理 |

### 3.11 让状态变更可见，而不是给每种变更配一个兜底（2026-09-18 落地）

前面几版机制都建立在"拒绝 + 告警"上。告警只进日志，agent 看不到；而事故的教训恰恰是
**"没有人知道发生了不一致"**。最终结论是：把机制的重心从"防止每种覆盖"移到
**"让每次变更可见"**——可见之后，兜底机制就是重复保险，应当删掉。

#### 3.11.1 最终形态：**感知为主，判定为辅，不做兜底副本**

这一节走过三次形态，前两次都被推翻，理由值得保留：

| 形态 | 做法 | 为什么被推翻 |
|------|------|-------------|
| ① CAS | 覆盖前比对摘要，不一致就**拒绝写** | 制造死锁：`write_file` 重写会被同一 CAS 再拒，`exec` 写沙箱会被 reconcile 拒绝，两条路都堵死；而且选边不该由代码做 |
| ② 保全 | 覆盖前把沙箱那份**复制成 `<path>.sandbox-version`**，让 agent 之后决定 | 大文件复制代价高（几百 MB 只为留一个可能没人要的版本）；文件面板被副本污染；**而且它与"感知通道"重复**——agent 早已从 exec 信号知道沙箱改过什么 |
| ③ **感知**（当前） | **直接覆盖 + 报告**：覆盖前观测一次、把"替换了一个不同的版本（N 字节）"写进工具结果 | — |

第三次推翻的依据是 §3.12 那条原则：**只要状态变更已经让 agent 感知到，兜底副本就是重复保险**。
agent 在写之前就已经从 exec 结果里看到"脚本改过这个文件"，所以它这次写入是**知情决策**；
替它留一份副本、再给它一个选边工具，是把"防止犯错"错当成"必要机制"。

于是机制只剩四个：

| 机制 | 职责 | 形式符号 |
|------|------|---------|
| **写入穿透** | 宿主写的那一刻两副本一致（含时间戳对齐）——消除"陈旧副本"这个前提（唯一能让 store 不落后的机制） | 消除未来的 δ |
| **无记忆的版本判定** | `size + mtime` 判等（`statsFor`/`sameVersion`），元数据不同才读字节比对（`equalToStore`）；判据写在两份副本自己身上，跨副本有效 | δ 的判据 |
| **拒绝 + 信号** | 两副本都动过且判不出谁最新时，不许沙箱覆盖 store，并把事实交给 agent | δ → σ |
| **exec 变化信号** | 把沙箱侧变化（新路径已同步 / 既有路径被拒 / 快照失败）告诉 agent（§3.11.2 的感知通道，主力） | σ |

§3.2/§3.8/§3.10 里的基线 `B` 与 `staleWrites` 都已下线：判定不再需要"记住上次交给沙箱
的是什么"，因为**沙箱文件自己的 mtime 就是那条记录**（§3.11.3）。

**方法论备注（2026-09-20 补）：为什么上面这三行判定可以在出事之前写完。**
那次淘汰不是等事故等出来的，是**读一遍规格**推出来的——因为这套系统的分析单位是**规定的**，不是观察的：
谁 `produce`、谁 `place`、谁 `take`（不变量 I1），信号必须落在哪两个落点（`D₁` / `D₂`），
以及"∀δ ⇒ ∃σ"这条通道责任，都写在文档里，可以照着念。
于是：**遗漏是补集形式的断言，而补集只能对着规格说**——观测告诉你的永远是"发生了什么"，
要说"少了什么"，你必须先有那份规格；规格是谁写的，谁才有资格说"少了"。
**代价（同样必须写明）**：裁决只在**这套系统自己**的范围内有效——规格没写到的地方，这里同样看不见。
（这条判据的完整形态见《认知哲学的数学原理》19.4.9「单位是规定的还是观察的」。）

#### 3.11.3 当前实现：基线已下线，判据来自两份副本自己（2026-09-18 定稿）

下线基线的理由与"跨 pod 沙箱租约"直接相关：租约会被**另一个副本**采纳，而进程内的基线表
（pod 重启即空、另一副本从未见过）会让同一个沙箱被不同副本按不同规则裁决——
"同一次同步，结论取决于谁在处理"。所以现在**没有任何跨调用、跨副本的记忆**，
判据全部来自两份副本自带的元数据：

| 事实 | 来源 | 代价 |
|------|------|------|
| store 对象的 size + LastModified | `workspace.Store.Stat` | 每路径一次往返 |
| 沙箱文件的 size + mtime | 一条 `find /workspace -type f -printf '%P\t%s\t%T@\n'`（`statsFor`） | 每次同步一条 exec |
| "沙箱还拿着交给它的那一份吗" | `sameVersion`：两侧 size 相同且 mtime 相差 ≤1s（tar 整秒截断） | 元数据相同即判等，**不再算摘要** |
| 元数据不同时 | `equalToStore`：把 store 那份读回来比字节 | 只在这条分支付一次读 |

穿透是这套判据的**前提**：镜像写入后立刻用 `touch -d @<store mtime>` 把沙箱文件的时间戳
对齐到 store 对象；hydrate 也把 store 的 `LastModified` 写进沙箱文件（E2B 的 tar 条目带
`obj.ModTime`）。于是"同一版本"在两份副本里都是**自描述**的，不需要任何地方另记一份。

> **2026-09-28 修订（改动册第 87 行）。** 下表第三行——"不同 ⇒ 拒绝"——正是 §13.4 量到的那个吸收态
> （446 条拒绝，最久的一条 10 小时以上）：拒绝不写任何东西，于是同一对副本每次同步都原样回来。
> 现在两份副本是被**排序**，而不只是被比较，用的两件器具仍保持本节的"无记忆"姿态：
> 文件自己的 mtime 对 store 对象的写入时间，其中的 guest 钟偏移在**与列目录同一次 exec** 里测出
> （`statsFor`）；以及字节的严格前缀包含关系，它根本不需要钟。**能说话的器具必须一致**才成立判决，
> 而且判决必须在 mtime 的两种读法下都成立（沙箱自己的写，或本节描述的盖章）。表因此变成：

| store | 两份副本的元数据 | 判定 | 动作 |
|-------|----------------|------|------|
| 无该路径 | — | 沙箱产物 | **推送** |
| 有 | 相同（size + mtime） | 沙箱未动 | 跳过 |
| 有 | 不同，且能说话的器具一致 | 有一份是**后继**：钟排出两笔写的先后，或字节呈包含关系（严格前缀即血缘） | 后继是沙箱那份 ⇒ **推送**；后继是 store 那份 ⇒ **把 store 那份投递进沙箱**（新方向，像镜像一样盖章） |
| 有 | 不同，且没有器具能说话、或它们互相矛盾 | 无法判定谁最新 | **拒绝 + 信号**（若字节相同则静默跳过） |

两处必须如实记录的代价（都是这次简化换来的，不是意外）：

1. ~~**沙箱编辑既有路径不再回写**~~——**已被第 87 行作废**：当没有别人写过 store 时，§3.2 的 W₁ 说沙箱那份是后继，
   于是它被收集（而 §3.2 的 W₂——store 动过、沙箱没动——现在是**投递进沙箱**，不再让沙箱陈旧下去，
   那正是 `KeyError: 'current_pnl'` 的来源）。第 87 行仍然拒绝的是没有器具能排序的那一对：
   相隔约 3 秒以内的两笔写，或两件器具的盲点互相矛盾。
2. **删除不可检测**（G4）：判据的定义域是沙箱快照，被删掉的路径不在其中。要恢复检测
   需要 **store 侧的持久清单**，不能用进程内状态（[09](./09-sandbox-lifecycle-audit.md) §3）。

**删掉的**：保全副本（`.sandbox-version`）、`resolve_workspace_conflict`、
`ConflictResolver` 接口、以及由此产生的 `ErrTooLargeToCompare` 拒绝路径。
删除后不再有"被锁住的状态"，因此也不再需要解锁工具——**机制数量从 6 降到 4，且没有死锁**。

穿透的三种结果，对应工具结果里的三种（互斥）说法：

| 情况 | 穿透做什么 | 工具结果 |
|------|-----------|---------|
| 沙箱那份就是交给它的版本（size+mtime 相同） | 直接写，连读都不读 | 无（静默） |
| 沙箱那份**不同**（可比较时） | **覆盖**，并报告事实 | `[workspace]`：替换了一个不同的版本（N 字节），未留副本——如需该版本请重跑产生它的命令 |
| 沙箱那份与新内容不同、且没有可比对的期望值（例如 store 里本来没有这个 key） | **覆盖**，并说明**无法还原**那份是什么 | `[workspace]`：沙箱里那份是另一个版本（N 字节），没有可比对的旧副本 |
| 沙箱不可达 | 写失败（判定留在两份副本上） | `[workspace]`：写入安全，沙箱内可能读到旧内容（**不提"选择"**，因为没有可选项） |

关于"大文件会不会撑爆上下文"的三点保障不变：标记只含路径与字节数；所有工具结果被
`ClipAndLog` 裁剪到 64 KiB 头 + 64 KiB 尾；大文件本来就不复制。

覆盖带来的风险用**一条纪律**代替（写进提示词层面的建议，而不是代码）：
脚本修改了大文件之后，agent 应在下一次写入前先 `exec ls -l` 确认，因为那种情况下
穿透无法替它核对。

#### 3.11.2 感知通道（exec 信号）

```
exec("python3 build.py")
  ...命令原本的输出...
  [workspace] the sandbox changed generated.csv, report.html — synced to the workspace store.
  [workspace] NOT synced (the store's copy differs and neither side may be chosen automatically): notes.md — both versions are intact.
```

- **没有变化就不出现**（异常通道，否则会被忽略）；
- **路径用沙箱自己的写法**（与 `exec ls`、`read_file` 一致）；
- **被拒绝的路径同样列出**——那正是最需要知道、且没有任何其他信号的一类；
- **空闲驱逐时的同步也投递**：那条路径没有工具结果可挂，需要带走的信号会写进 scope 级的**耐久**载体（`SignalStore`，2026-09-18 前是进程内队列），由该 scope 的
  下一个工具结果，且只投递一次（`TestEvictionSignalReachesNextToolResult`）。

**第四类是删除——它没有信号，而且当前做不到**（2026-09-18 复核纠正）：同步的遍历定义域是
**沙箱快照**，被删掉的路径根本不在快照里，循环碰不到它。曾经靠基线判定
（"基线里有、快照里没有" ⇒ 沙箱删的），基线随 §3.11.3 下线后这条判据一起消失：
现在"沙箱删了它"与"这份文件本来只在 store 里（上传/附件）"**无法区分**。
恢复检测需要一个 **store 侧的持久清单**（不能用进程内状态），记为 G4（[09](./09-sandbox-lifecycle-audit.md) §3）。
所以本节的其余三条信号是 as-built，删除这一条是**已知缺口**，不是已解决项。

## 第四部分 · 完备性自检（对本文自身的三个反驳）

### 3.12 状态可观测性原则

本目录所有机制最终收敛到一条 harness 层面的原则：

> **harness 内的状态变更必须让 agent 可感知；否则 agent 会基于一个已经不存在的世界推理。**

它是独立于文件同步的通用约束，因此单独成文：
**[08-state-observability-principle.md](./08-state-observability-principle.md)**
（形式化表述、三条推论、当前 harness 的逐项审计、新机制的审查清单）。

本节保留它在本篇里的最短形式，作为 §3.11 四机制的取舍依据：

- agent 自己引起的变更 → 工具结果就是回执，无需额外机制；
- **harness 自己引起的变更 → 必须显式报告，且报告要进 agent 真正读的通道**；
- 没有信号 ≠ 没有变化（沉默即宣称"世界如你所想"）；
- 报告是异常通道（常态即噪音，模型会学会跳过）。

**反驳 1："B 也会过期/丢失，那时不还是错的吗？"**
规则 (7) 覆盖此情形：缺 B 时只允许 (1)（store 中不存在才写），
即退化为"绝不覆盖既存键"。这比现状严格更保守，不会重新引入 D。

**反驳 2："规则 (4) 的 X := S 会覆盖沙箱里未同步的编辑吗？"**
不会——(4) 的前置条件是 `X[p] = B[p]`，即沙箱自 hydrate 起未改过该键。
沙箱确有编辑时命中 (3) 或 (5)，前者推送、后者报错。

**反驳 3："那事故里 agent 的两次修复为什么还会失败？"**
因为修复只执行了 `S[p] := v`（inside 的一半），而 `X[p]` 未被更新，
于是下一次 reconcile 的前置条件仍按 `W₂`（S 相对 B 变化、X 未变）判定——
在**修复版**规则下这会触发 (4) 的"拉"，即把修复同步进沙箱并更新 B，
从而终止回退链。**换句话说：D 的修复不只是"别覆盖"，
还必须是"把权威内容推回缓存"，这两半缺一不可。**

---

## 第五部分 · 自校验中发现的新问题（诚实记录）

### 5.1 `Sync` 是"只增不减"的

二次自校验时对比了"沙箱文件集合"与"store 对象集合"，发现一个此前没有写下来的事实：

```
沙箱 /workspace 顶层：byo-account-design.html  qc-email-draft.md  todo.md
                      qc-lean-mcp/  quantconnect-gonogo.html  quantconnect.html
store（同 scope）：   以上全部 + mcp-oauth-authorization-flow.md + mcp-oauth-design.md
```

后两个文件是用户在 12:04 上传的附件（`LastModified 12:04:32`），存在于 store，**不在沙箱**。
它们能被 `read_file` 读到（读 store），但 `exec` 看不到。

原因是 `Sync` 的遍历方向：它以**沙箱快照为定义域**，只做 `S[p] := X[p]`，
从不删除 `S` 中沙箱没有的键。也就是说：

| 现象 | 原因 | 影响 |
|------|------|------|
| 附件在 store 但不在沙箱 | hydrate 之后才上传，且没有任何"补 hydrate"机制 | agent 的 `exec` 与 `read_file` 看到的世界不一致 |
| `Sync` 不会误删附件 | 遍历是"沙箱 → store"，只增不减 | 安全，但也意味着**沙箱侧删除永远不会传播** |

### 5.2 这暴露了修复规则的一个缺口

本文 §3.3 的规则 (1)–(7) 假设"两侧都存在的键"和"沙箱新增的键"，
但没有覆盖 **`p ∈ dom(B)` 而 `X[p] = ⊥`（沙箱侧删除）在 `S[p] = B[p]` 时** 的情形
——那是"沙箱明确删掉了它"。当前实现会静默保活该键（因为遍历不到它），
因此既不丢数据也不响应删除：**一个没有被声明的行为**。

需要补一条规则并在实施前决策：

```
 (8) X[p] = ⊥  ∧  S[p] = B[p]  ∧  p ∈ dom(B)
       语义: 沙箱侧删除（用户/agent 在 exec 里 rm 过）
       选项 A（保活，保守）: 无迁移，B[p] 保留 —— 即当前行为，但必须写进契约
       选项 B（传播删除）  : 前置 = 有删除动作的证据（需要 §3.6 的 undo marker 提供）
                            迁移 S[p] := ⊥ ; B[p] := ⊥
       禁止: 在无证据时删除 S[p]（那正是"静默销毁用户数据"的同族错误）
```

无论选哪个，**都必须显式声明**——这正是本文 §3.5 边界表要求的"不可逆部分要写明"。
我的倾向是选项 A 加上"向 agent 声明存在 store-only 文件"（让 `list_dir` 区分两类来源），
这样既保守又不制造新的不可逆动作；选项 B 等 §3.6 的删除快照（逆操作）就位后再开。

### 5.2.1 受控复现（已跑通，2026-09-17）

用真实 E2B 后端把 §2.4 的时序完整跑了一遍：新增
[internal/sandbox/e2b_live_repro_test.go](../../internal/sandbox/e2b_live_repro_test.go)
（`FASTAGENT_E2B_LIVE=1` 闸门，不进常规 CI），步骤与断言如下。

```
1. store 先写 v1（40 字节）→ 建沙箱（Hydrate 把 v1 带进 /workspace）
2. 宿主侧只改 store：v2（54 字节）        ← 模拟 write_file/edit_file/apply_patch
3. 沙箱内跑一次 exec（ls /workspace）
4. 驱逐同步 syncSnapshot(cause=evict)
```

结果（两次运行一致）：

```
step 1 ok: sandbox born holding v1
step 2 ok: store holds v2, sandbox still holds v1
store after sync: "OLD SNAPSHOT VERSION (1111111111111111)\n"     ← v1，v2 被销毁
per-key 'path differs' log lines: 1
  level=INFO msg="sandbox sync: path differs, taking sandbox copy"
    cause=post-exec path=report.txt storeBytes=54 snapshotBytes=40
REPRODUCED the incident
```

三条可以直接引用的事实：

1. **`cause=post-exec`** —— 触发它的**不是驱逐，而是第 3 步那次 exec 自带的回写**。
   这解释了事故里 `12:05:26 cause=post-exec files=1` 为什么能与 `12:15` 的 evict 并列出现：
   在这个后端上"随便跑一条命令"就足以成为覆盖窗口。
2. **`storeBytes=54 snapshotBytes=40`** —— 新开的逐键日志把两侧字节数直接写出来，
   §5.3 那条未闭合矛盾下一步就是靠这个字段定位。
3. 沙箱是**热复用**的（第二次运行没有重建实例，2.9 秒完成），
   与事故里"B 活了 4.5 小时、跨 5 次同步"的形态一致。

与事故的对应关系因此可以逐格对齐：

| 事故 | 本次受控复现 |
|------|-------------|
| A/B 从 hydrate 那刻起持有旧版本 | 沙箱出生即 v1 |
| 08:04–08:19 宿主工具只写 store | 第 2 步只写 store |
| 12:05:26 `cause=post-exec files=1` | 第 3 步的 exec 触发同一路径 |
| 12:15:17 `cause=evict files=3` | 第 4 步的显式 evict |
| S3 对象回到旧字节数 | store 回到 v1 |

（复现所用的 E2B 沙箱与事故会话无关；运行结束后已被释放，未在生产 `sandbox_leases` 留下任何行。）

### 5.3 一处曾记为"矛盾"的观测（已定性，仅剩日志缺失）

第二轮（五次复核中的第 3 次）复核时发现一个用现有证据无法解释的数据点，
按"调查清楚之前不硬套结论"的纪律，这里原样记录、不并入 §2 的事件链。

`list_dir` 是读 store 的，其输出中 `byo-account-design.html` 的字节数序列为：

| 时间 (UTC) | 字节 | 场景 |
|-----------|------|------|
| 08:19:32 | 50,282 | 两次修复（08:15）之后 |
| 12:06:17 | **41,262** | 本轮 `write_file`(12:05:45) + `edit_file`(12:06:13) **之后** |
| 12:07:06 | 55,527 | 本轮修复（12:07:00）之后 |
| 12:15:17 | 41,262（终态） | evict 之后（有日志） |

**结论（2026-09-17 复核修正）**：这里没有算术矛盾，我最初把两个工具混为一谈了。

- `write_file` 是**整篇替换**：工具回报的字节数就等于它写进 store 的字节数。
  12:07:00 的 `Written 6893 bytes` 与 `list_dir` 的 6,893 一致，即 store 写入是自洽的；
- 事故当事的 `byo-account-design.html` 在 12:06:13 走的是 `edit_file`（读改写），
  但 `list_dir` 在 12:06:17 报的是 **41,262**——而该值正是**沙箱 B 里的字节数**
  （§2.5 直读：41,262，md5 与 store 对象一致）。

所以三个文件在 12:06:17 都回到了**各自沙箱副本的字节数**
（41,262 / 5,705 / 580），说明这一轮开始时它们**已经**被回退过了
（`list_dir` 08:19:32 仍是健康的 50,282 / 6,893 / 462）。回退发生在
**08:19:32 与 12:04:33 之间**，而不是本轮的三次 exec 造成的。

**仅剩的缺口是日志**：这段时间内没有 `synced to workspace store` 记录
（`08:24:18` 之后到 `12:05:26` 之间为空），但能产出这些字节的写者只有沙箱快照通道——
字节序列本身（41,262 / 5,705 / 580）就是沙箱副本的内容。因此缺口是
**"这次同步没有留下日志"**，而不是"存在第三种写者"；新加的逐键 `BLOCKED` 日志
正是为堵这个缺口。

已排除的假设：

| 假设 | 检验 | 结论 |
|------|------|------|
| 沙箱里存在健康版本（50,282 / 55,527） | 直读 `/workspace` 全量 md5 | ❌ 不存在，只有 41,262 那一版 |
| 有第三条（非 lifecycle）的回写路径 | 全仓 `SnapshotWorkspace` 调用点仅 lifecycle 两处 | ❌ 代码中不存在 |
| 12:05:26 的 post-exec 同步是肇事者 | 该次 `files=1`；且三个文件在该次之前已是旧版本 | ❌ 不是 |
| 另一个 pod 持有第二个沙箱 | `sandbox_leases` 单行 + 直测该 scope 只有一个 sandbox id | ❌ 不成立 |

**结论**：这不影响 §2（D 只依赖"T3 无前置条件"与"两侧曾分叉"，有 md5、birth time、
四条同步日志三重佐证），也不改变修复。唯一缺口是那一次回退**没有留下日志行**；
新加的逐键 `BLOCKED` 日志即为堵此缺口而设。

---

## 附：本次确认所使用的一次性探针（可复现）

```bash
# 1) 取该 scope 的活体沙箱与状态
select sandbox_id, state, envd_token, epoch from sandbox_leases
 where scope_key = 'agt_…:s:OvDLzEPKcEK84Hpfq0U6LK';

# 2) 解锁 envd token（AES-256-GCM，key = sha256(FASTAGENT_OAUTH_SECRET)）
#    见 internal/mcp/oauth/adapter/cryptor.go

# 3) 连接（paused → running，返回新 token）并直读文件
curl -X POST "https://api.e2b.dev/sandboxes/$SBID/connect" \
     -H "X-API-Key: $E2B_API_KEY" -H 'Content-Type: application/json' -d '{"timeout":120}'
curl -H "X-Access-Token: $TOKEN" \
     "https://49983-$SBID.e2b.app/files?path=/workspace/byo-account-design.html&username=user"

# 4) 读完恢复：POST /sandboxes/$SBID/pause
```

该探针是只读 + 复原的；本文所有结论均可由 §1.2 的表与这段命令独立复现。
