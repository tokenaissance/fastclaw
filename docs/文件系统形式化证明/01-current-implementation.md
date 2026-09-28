# 01 · 当前实现实录

> 状态：as-built（对照 `fastagent` 当前代码逐处核过）· 最后核对：2026-09-17
> 作用：把"工作区文件到底存在哪、谁写它、什么时候同步"讲清楚。后面几篇的分析都以此为前提。

> **后记（2026-09-18 补；修正 §3–§8）。** 本篇是 2026-09-17 的代码快照。修复在次日落地，
> 改掉的正是本篇认定为缺陷的部分：`write_file` / `edit_file` / `apply_patch` 现在**都**会调用
> `writeThroughSignal` → `LifecyclePool.WriteThrough`（远程后端；`codingSubdir == ""` 这道门槛已取消，
> `mirrorCodingWriteToSandbox` 作为函数已不存在，被泛化成前者），因此 §3.1 的"条件性镜像"与 §3.5 的
> 路径错位**不再描述当前代码**。§3.4 的判据（`Stat().Size == len(data)` → 跳过）已换成
> [07 §3.11.3](./07-formal-rootcause-and-fix.md) 的无记忆 `size + mtime` 判定；§6 的日志缺口由逐键
> `BLOCKED` 行补上；§7 缺失的测试现在是 `lifecycle_sync_contract_test.go`。
>
> **§8 也修了（同日，第二轮）**：`apply_patch` 的 store 路径解析改回与 `write_file` / `edit_file` 同一个
> 函数（`scopeSessionID()` + `wsPath()`，6 个触点全改），并补了单测 + 真机 E2E；反证记录在 §8.1。
> 修的过程中在**同一族**里发现第二处作用域不一致（§8.2 / [10](./10-harness-state-audit.md) G17）：
> "项目会话把 session 折叠掉"这条规则 hydrate 有、sync 没有——真机已验证，属产品决策，仍开放。

## 1. 两份副本

同一个逻辑路径（例如 `byo-account-design.html`）在运行时最多同时存在于两个地方：

| 副本 | 位置 | 谁读它 | 谁写它 |
|------|------|--------|--------|
| **持久存储**（本文简称 store） | 生产：S3（DO Spaces，`prod/<agent>/sessions/<sid>/<path>`）；本地：`~/.fastagent/workspaces/<agent>/…` | UI 文件面板、下载、signed URL、`read_file`、`list_dir`、`/api/agents/{id}/files/...` | 宿主文件工具、沙箱回写通道 |
| **执行副本** | 沙箱内 `/workspace/<path>` | 沙箱里的 `exec`（脚本、构建、git） | `exec`、宿主工具的镜像（条件性，见 §3.3） |

store 的端口定义在 [internal/workspace/workspace.go](../../internal/workspace/workspace.go)：

```go
type Store interface {
    Put(ctx, agentID, projectID, sessionID, path string, r io.Reader, size int64, contentType string) error
    Get(ctx, agentID, projectID, sessionID, path string) (io.ReadCloser, error)
    Stat(ctx, agentID, projectID, sessionID, path string) (*ObjectInfo, error)
    List(ctx, agentID, projectID, sessionID string) ([]ObjectInfo, error)
    Delete(ctx, agentID, projectID, sessionID, path string) error
    Move(ctx, agentID, fromProjectID, fromSessionID, toProjectID, toSessionID string) error
    SignedURL(ctx, agentID, projectID, sessionID, path string, ttl time.Duration) (string, error)
}
```

作用域到路径的映射（两种后端一致，实现在 [localfs.go](../../internal/workspace/localfs.go) 的 `scopeDir` 与 [s3.go](../../internal/workspace/s3.go) 的 `key`）：

| projectID | sessionID | store 下的位置 |
|-----------|-----------|----------------|
| `""` | `""` | `<agent>/<path>`（agent 共享） |
| `""` | `s` | `<agent>/sessions/<s>/<path>` |
| `p` | `""` | `<agent>/projects/<p>/<path>` |
| `p` | `s` | `<agent>/projects/<p>/<s>/<path>` |

> 注意 `ObjectInfo` 里带 `Size`、`ModTime`、`Path`，**没有版本号、没有内容哈希、没有作者**。
> 这是后面所有仲裁困难的物质基础：store 自己也不知道某个对象是"宿主刚写的"还是"沙箱回写的"。

## 2. 沙箱后端及其物理事实

`Executor` 端口只有 7 个方法（[internal/sandbox/executor.go](../../internal/sandbox/executor.go)），
关于"`/workspace` 是什么"只字未提。这些事实由三个 marker 接口和三个后端实现分别表达：

| 后端 | `/workspace` 的物理形态 | `RemoteWorkspace`（`/workspace` 不与宿主共享） | `WorkspaceSnapshotter`（能给出快照字节） | `PortExposer`（能暴露端口） |
|------|------------------------|--------------------------------------------|--------------------------------------|---------------------------|
| **docker** | 宿主目录 bind mount：`-v <host dir>:/workspace:rw`（[docker.go](../../internal/sandbox/docker.go) 第 255 行） | 不声明 → **共享** | 实现：`filepath.WalkDir(宿主目录)`，跳过 `node_modules` 等构建目录（[docker_executor.go](../../internal/sandbox/docker_executor.go) 第 75 行） | 实现 |
| **e2b** | 沙箱内独立文件系统 | 声明（[e2b_executor.go](../../internal/sandbox/e2b_executor.go) 第 1565 行） | 实现：`tar -czf - -C /workspace . \| base64` 拉回远端副本 | 实现 |
| **boxlite** | 沙箱内独立文件系统 | 声明（[boxlite_executor.go](../../internal/sandbox/boxlite_executor.go) 第 741 行） | 实现：通过 Files API 下载 tar | 未声明 |

关键不对称：

- docker 的 `SnapshotWorkspace` **返回的就是宿主的那一份**（它 walk 的是 bind mount 的源目录），所以"快照"与 store 天然一致；
- e2b / boxlite 的 `SnapshotWorkspace` 返回的是**另一份**，且这份内容停留在上一次 hydrate（或上一次沙箱内写入）的时刻。

### 2.x 每后端对"一次写"的承诺（强度表，2026-09-19）

> 端口加了条件写 `PutIfVersion(expected Version)`（B 族，义务 L7）。**强度按后端不同，且必须声明**——
> 调用方只被允许依赖被声明的那部分。

| 后端 | `Version` 是什么 | `PutIfVersion` 的强度 | 依赖它的后果 |
|---|---|---|---|
| **S3 / Spaces（多副本生产）** | 对象 ETag | **在桶会计算 `If-Match` 时精确**（AWS S3、MinIO）：条件 PUT 与写在同一请求内，412 ⇒ `ErrVersionConflict`。Ceph RGW——本部署实际用的 DigitalOcean Spaces 就是它——只实现了 create-only 形式，对**每一个** `If-Match` 都回 412，哪怕 ETag 就是客户端刚读到的那个（2026-09-22 对 nyc3 Spaces 实测），所以那里的覆盖写是**比对后写**：`Stat` 对象，只有当它仍携带 `expected` 时才无条件落盘 | 桶精确时可以据此**拒绝**覆盖；在 Spaces 上只能"检测并拒绝已过期的期望"，而 create-only（`If-None-Match: *`）两种情形下都保持精确 |
| **LocalFS（单机/开发）** | `size:mtime_ns` | **尽力而为**：stat → 比对 → 写，其间无内核 CAS | 只能"检测并拒绝已过期的期望"，不能承诺并发安全；多副本必须用 S3/PG |
| **Metered** | 透传 | 与内层相同 | — |

写者姿态（谁在用什么前置条件）：

| 写者 | 前置条件 | 状态 |
|---|---|---|
| `write_file` / `edit_file` / `apply_patch` | 写前 `Stat` 取版本 ⇒ `PutIfVersion` | ✅ 已接（B7–B9） |
| 附件 | `VersionAbsent`（必须不存在）；名字被占则**改名保留两份** | ✅ 已接（B10）—— `internal/agent/attachments.go:178` 传 `workspace.VersionAbsent`；S3 上即 `If-None-Match: *`，LocalFS 上是 stat-比较（best effort，见上表）。**09-21 改了姿态**：旧写法先 `Put`、冲突时只 `slog.Warn` 却仍把**旧名字**写进 `[Attached: …]` 面包屑 —— 那是一个静默错答（面包屑指向别人的字节）。现在**先写 store**（名字由 store 决定），冲突就在 `<stem> (n)<ext>` 上保留两份（已存在的计数递增，不嵌套），返回**真正落地的名字**；连一个空位都拿不到时返回 `""`，调用方**什么也不声明**。见证 `internal/agent/attachments_store_posture_test.go`，反证实跑（`PutIfVersion`→`Put` ⇒ 3 红；去掉保留两份的循环 ⇒ 2 红） |
| 技能发布 | 读当前版本再条件写 | ✅ 已接（B10）—— `internal/skills/objectstore.go:113` 读版本后 `PutIfVersion`，并映射 `ErrVersionConflict` |
| 面板上传/删除 | 面板最后列出的版本 | ✅ 两侧均已接（B11）—— 服务端：上传读可选 `expectedVersion` 字段、冲突时 409 + `current{version,size,modified_at}`（`internal/setup/handlers_agents.go:1492-1531`）；cloud 面板：改为**一次请求一个文件**，把 409 变成具名冲突并给出**三答案**（保留两份＝按服务端命名自动改名 / 替换＝带 `expectedVersion` 重发 / 取消），落点见证 cloud `src/__tests__/fastagent/attachment-conflict-flow.test.tsx`。删除本身没有覆盖语义，它的第二半是 d1 镜像删除（`sandbox.LiveWorkspaceFileRemover`：面板 2026-09-18、工具路径 2026-09-20，见 [10 §4](./10-harness-state-audit.md) G7b） |
| 沙箱↔store（穿透/回写） | **不用条件写**：见证只 in-band 存在于副本的 mtime 戳里（L7 §3.1 的论证） | 保持 A 族 |

> **可核查的读数（2026-09-21 加；《认知哲学的数学原理》19-5 §19.5.8.8「拒绝的代价」的结构分量）**
> 后端强度表 **3 格** = 精确（买得到**阻止**）**1** · 尽力而为（只买得到**检测**）**1** · 透传 **1**；
> 写者姿态 **5 行** = 买得到阻止 **4**（**在 S3 上**；同一批在 LocalFS 上退化为检测）· **不买 1**（沙箱↔store 保持 A 族）。
> **两个结论**：① **这套系统里「拒绝」真正会被用到的地方只有 1 行**（沙箱覆盖 store 的 A 族回写）
> ＋ **1 个条件格**（租约存储不可用 ⇒ [12 §3.2](./12-lease-formal-design.md)）；
> ② 19.4.6.2 那条自检当场通过：**「阻止」的格数（1）不大于「校验同址」的格数（1）**——这张表还没有在说谎。

## 3. 写入者与写入路径

### 3.1 宿主文件工具 → store

注册在 [internal/agent/tools/file.go](../../internal/agent/tools/file.go)，路由由 `routeFor`（[route.go](../../internal/agent/tools/route.go)）决定：

| 工具 | 函数 | store 写入点 | 是否同时镜像进沙箱 |
|------|------|-------------|-------------------|
| `write_file` | `makeWriteFile` | 第 589 行 / 第 1035 行（`RouteWorkspaceStore` 分支） | **条件性**：第 1042 行 `mirrorCodingWriteToSandbox` |
| `edit_file` | `makeEditFile` | 第 703 行 / 第 1224 行 | **条件性**：第 1231 行 |
| `apply_patch` | `writeForPatchSandbox`（[apply_patch.go](../../internal/agent/tools/apply_patch.go)） | 同函数内 `workspaceStore.Put` | **从不** |

"条件性"指的是 [mirrorCodingWriteToSandbox](../../internal/agent/tools/file.go) 的两道门槛：

```go
if r.codingSubdir == "" || r.executor == nil { return }        // 必须是 coding 运行时会话
if _, ok := r.executor.(sandbox.RemoteWorkspace); !ok { return } // 必须是分离式后端
```

于是：

- **普通研究 / 交付物会话**（无 coding 运行时，`codingSubdir == ""`，即**默认形态**）：镜像不执行 → store 写了新的，沙箱里还是旧的；
- **coding 会话 + e2b**：镜像执行 → 两份一致，所以这类会话不容易看到回退；
- **`apply_patch` 在任何会话**：都不镜像 → 只要同路径在沙箱里存在，就埋下分叉。

`SetCodingSubdir("app")` 只由预览运行时的两条路径设置（[loop.go](../../internal/agent/loop.go) 第 224 行、
[runtime_tools.go](../../internal/agent/runtime_tools.go) 第 87 行）。因此
**镜像服务的对象是"coding 运行时会话里的 dev server 热重载"，而不是"保持 store 与沙箱一致"**——
它的引入 commit（`418ce82` / `ede73d3`，2026-06-14，"backend-agnostic project runtime + multi-template + preview UX"）
也是预览功能，不是同步功能。这个出身决定了它的路径选择，见 §3.5。

### 3.5 coding 镜像的路径不对称（重要）

镜像写入用的是 `"/workspace/" + path`，即**硬编码把 store 逻辑路径映射到 `/workspace` 的根**：

```go
// file.go，mirrorCodingWriteToSandbox
dest := "/workspace/" + strings.TrimPrefix(filepath.ToSlash(filepath.Clean(path)), "/")
```

而 hydrate 的方向完全按作用域前缀展开：E2B hydrate 把 store 对象按"list 时返回的相对路径"
打进 tar 并解到 `/`（[e2b_executor.go](../../internal/sandbox/e2b_executor.go) 第 633、783 行），
而 S3 `List` 返回的相对路径**对松散会话含 `sessions/<sid>/` 前缀**
（[s3.go](../../internal/workspace/s3.go) 的 `scopePrefix`/`List`）。

两者的对应关系因此随作用域漂移：

| 会话形态 | store 键（逻辑路径） | hydrate 后沙箱中的位置 | 镜像写入的位置 | 对齐？ |
|----------|---------------------|----------------------|---------------|--------|
| 松散会话（无 project） | `sessions/<sid>/report.html` | `/workspace/sessions/<sid>/report.html` | `/workspace/report.html` | ❌ **错位** |
| 项目会话 | `report.html`（项目根） | `/workspace/report.html` | `/workspace/report.html` | ✅ |

错位的后果有两个，都还没有被测量过：

1. **镜像修改的不是 hydrate 那份**：`write_file` 修好 store 后，沙箱里 `/workspace/sessions/<sid>/…`
   仍是旧版本（同步时因 store 该键大小未变而被跳过），而镜像产生的 `/workspace/report.html` 是一份**副本**。
   于是 agent 通过 `exec` 看到的路径内容与通过 `read_file` 看到的不一致。
2. **store 里多出重复对象**：镜像写入落到 `/workspace/<path>` 后，`lazyExecutor.WriteFile` 的
   `mirrorSandboxWrite` 会把它以**根键**（而非 `sessions/<sid>/…`）写回 store
   （[lifecycle.go](../../internal/sandbox/lifecycle.go) 第 784 行），
   于是同一逻辑文件在 store 里出现两个键。

结论：**"宿主写入镜像进沙箱"这条看似能防回退的路径，在默认（松散）会话里其实落到别处**。
它不能作为任何方案的正确性依据，见 [05-remediation-plan.md](./05-remediation-plan.md) §7。

### 3.1.1 全部 store 写者（2026-09-18 重盘补齐）

上表只列了**工具**这条链路。重盘后 store 的写者共有六条，作用域与穿透行为各不相同：

| 写者 | 位置 | 写什么键 | 是否同时写沙箱 |
|------|------|---------|---------------|
| 宿主文件工具（`write_file` / `edit_file` / `apply_patch`） | `tools/file.go`、`tools/apply_patch.go` | `scopeSessionID()` + `wsPath()`（三个工具**同一个**解析，2026-09-18 起；见 §8） | ✅ 远程后端穿透（`WriteThrough`） |
| 沙箱回写（`syncSnapshot` 批量 / `mirrorSandboxWrite` 单文件） | `sandbox/lifecycle.go:742`、`:1164` | 沙箱路径映射回 store 键 | —（方向相反） |
| **附件**（`WriteSessionAttachments`） | `agent/attachments.go:118` | 会话作用域 | ✅ 宿主目录 + store + **活沙箱**三处齐写 |
| **面板上传/删除**（`POST/DELETE /api/n`） | `setup/handlers_agents.go` `handleAgentFileUpload`/`handleAgentFileDelete` | 会话/项目作用域 | **上传：不穿透**（2026-09-18 决策 a，"面板＝文件库"）；**删除：穿透**（同日决策 d1，`Gateway.RemoveWorkspaceFile` → `LiveWorkspaceFileRemover`，只为活实例、绝不建实例）（见 10 §3.2 / §4 G21） |
| **技能镜像** | `skills/objectstore.go:105`、`:192` | `<owner>/skills/<skill>/<file>`（workspace 桶里的**第二个逻辑命名空间**） | 由 hydrate 带入沙箱 |
| ~~`WorkspaceSync`~~（死码，**2026-09-18 已删除**） | ~~`sandbox/workspace_sync.go`~~ | 旧的 userID-only 键 | — |

最后一条是这次重盘的发现：`WorkspaceSync` / `NewWorkspaceSync` **全仓零引用**，且它自带的
`WorkspaceStore` 接口（`List(ctx, userID)` / `Put(ctx, userID, path, r)`）与现行的
`workspace.Store`（`(agentID, projectID, sessionID, path, size, contentType)`）**签名不兼容**——
现行 `LocalFS`/S3 不满足它。它生于 `87c50ee`（2026-04-12 的多用户重构）、此后从未被修改，
也没有任何一个 commit 引用过 `NewWorkspaceSync`：**它从未被接上**，随后被 `LifecyclePool` +
`hydrateWorkspace` + `syncSnapshot` + `WriteThrough` 整条链路取代（详见 10 §9）。

### 3.2 exec → 沙箱副本

沙箱里的 `exec` 只能写 `/workspace`。这些写入要回到 store 只有一条路：`syncSnapshot`（见 §3.4）。

### 3.3 沙箱 → store（两条）

**a) 单文件镜像**：[lifecycle.go](../../internal/sandbox/lifecycle.go) 的 `mirrorSandboxWrite`（第 784 行）。
由 `lazyExecutor.WriteFile`（第 752 行）在 `RemoteWorkspace` 后端上、且路径以 `/workspace/` 开头时调用——
它服务的是"工具直接写沙箱绝对路径"这条路由（`RouteSandbox`，如 `ex.WriteFile`），
而不是 `write_file` 的常规路径（后者走 store）。

**b) 全量快照回写**：`syncSnapshot`（第 442 行），两个触发点：

```go
// 1) 每次 exec 之后（仅 RemoteWorkspace 后端）
if _, remote := ex.(RemoteWorkspace); remote {
    l.pool.syncSnapshot(ctx, l.scope, ex, "post-exec")
}
// 2) 空闲驱逐 / 睡眠之前
p.flushIfSupported(sc)   // 内部调用 syncSnapshot(..., "evict")
```

### 3.4 `syncSnapshot` 的判据

> **2026-09-28 修订（改动册第 87 行）——先读这段再看下面的代码。** 下面这段是 2026-09-17 的形状
> （"大小不同 ⇒ 覆盖"）：它落后了三处修复。当前判据是 [07 §3.11.3](./07-formal-rootcause-and-fix.md)
> 加上第 87 行的修订——store 没有该路径 ⇒ 推送；size+mtime 相同 ⇒ 跳过；否则两份副本是被**排序**而不是
> 只被比较（文件自己的 mtime 对 store 的写入时间，其中 guest 钟偏移在与列目录同一次 exec 里测出；
> 再加上字节的严格前缀包含关系），得出的判决是：推送 / **把 store 那份投递进沙箱** / 拒绝并上报。

```go
for path, data := range files {                    // files = 沙箱快照
    if info, err := p.workspace.Stat(...); err == nil && info.Size == int64(len(data)) {
        continue                                   // 大小相同 → 跳过
    }
    p.workspace.Put(..., path, bytesReader(data), int64(len(data)), "")
}
```

即：**store 里没有该路径 → 写入；大小不同 → 用沙箱内容覆盖。** 没有时间戳比较、没有来源标记、没有冲突处理。

## 4. hydrate：反方向的通道

沙箱创建（或被重建）时，从 store 单向灌入 `/workspace`：

| 后端 | hydrate 实现 |
|------|-------------|
| docker | 不需要：bind mount 本身就是宿主目录 |
| e2b | 内层池自 hydrate（一个 tar.gz 覆盖 `/skills` 与 `/workspace`），[lifecycle.go](../../internal/sandbox/lifecycle.go) 第 575 行附近会检测 `workspaceAware` 并跳过逐文件回退 |
| 其他 | 逐文件 `hydrateWorkspace`（[workspace_hydrate.go](../../internal/sandbox/workspace_hydrate.go)） |

hydrate 的时机决定沙箱副本的"出生版本"。之后沙箱长时间存活（跨 turn、跨 sleep/wake、跨 pod 采纳）时，
这个出生版本可能与 store 越差越远——它是后续冲突的源头。

## 5. 生命周期事实（与本次事故相关）

- 作用域键是 `(agentID, projectID, sessionID)`；`sandbox_leases` 表（[internal/store/sandbox_leases.go](../../internal/store/sandbox_leases.go)）
  按 `scope_key` 存 `sandbox_id`、`state`、`expires_at`、`paused_at`、`epoch`。
- 空闲驱逐默认 `idleTTL=10m`：可睡眠的后端走 `ScopeSleeper`（保留文件系统与 `hydrated` 标记），
  不可睡眠的走 `Release`（销毁，下次重建重新 hydrate）。见 [lifecycle.go](../../internal/sandbox/lifecycle.go) 第 320 行附近。
- `hydrate` 与 `syncSnapshot` 共同构成"关闭时尽量把沙箱内容留在 store"的意图，但两者的方向都是**后写覆盖前写**。

## 6. 可观测性现状

| 事件 | 日志 | 是否足够定位冲突 |
|------|------|-----------------|
| 沙箱 hydrate | `e2b sandbox hydrated … workspaceFiles=N tarBytes=…` | 是（能看到出生版本时间点） |
| 快照回写成功 | `sandbox synced to workspace store … cause=post-exec\|evict files=N` | **否**：只有文件数，没有路径、字节数、覆盖前的大小 |
| 快照超过体积上限 | `sandbox sync: snapshot failed … over the 32.0 MB cap` | 部分（能解释"某段时间没有回写"） |
| 单文件镜像失败 | `sandbox sync: write_file mirror failed … path=…` | 是（有路径） |

结论：**当前日志无法回答"这次回写覆盖了多少字节、覆盖了什么"**。这也是本次事故只能靠 S3 对象
`LastModified` 与逐条会话消息反推、而不能一眼看出的原因。

## 7. 测试覆盖现状

[internal/sandbox/lifecycle_test.go](../../internal/sandbox/lifecycle_test.go) 里与回写相关的只有：

- `TestLifecycle_FlushOnEvict`（第 842 行）：断言沙箱文件被推到 store（单向、只覆盖"沙箱有新文件"的情形）；
- `TestLifecycle_HydrateOnCreate`（第 742 行）：断言 store → 沙箱方向。

**没有任何测试覆盖"store 里是新版本、沙箱里是旧版本"这一情形**，也没有测试区分 docker（共享）
与 e2b（分离）两种后端语义。夹具 `fakeWorkspace.Stat` 返回的 `ObjectInfo` 中 `ModTime` 恒为零值，
`snapshottingExecutor` 只提供 `map[string][]byte`（无时间概念），所以"两份副本版本不同"这件事
在现有测试里**无法被表达**。

## 8. 附：路径解析——一条路径一个键（§8.1 已修），以及同一族的遗留（§8.2 仍开放）

### 8.1 已修（2026-09-18）：`apply_patch` 与另两个文件工具用同一个解析

修复前，`apply_patch` 的 store 路径解析与 `write_file` / `edit_file` **不一致**：

| 工具 | 传给 store 的 session 段 | 路径映射 |
|------|-------------------------|---------|
| `write_file` / `edit_file` | `r.scopeSessionID()`（coding-root-scope 模式下折叠为 `""`） | `r.wsPath(path)`（追加 `codingSubdir`） |
| `apply_patch`（修复前） | `r.sessionID`（原始值） | 原样 |
| `apply_patch`（现状） | `r.scopeSessionID()` | `r.wsPath(path)` |

当时折叠由一个标志决定：`a.registry.SetCodingRootScope(a.projectRuntime != nil && projectID != "")`
（[internal/agent/loop.go](../../internal/agent/loop.go) 第 214 行）。**2026-09-18（[10 §4 G23](./10-harness-state-audit.md)）
把该标志删除**：折叠规则收进 [`workspace.WriteScope`](../../internal/workspace/scope.go)
（判据 = "有没有项目"，不再看"有没有接运行时"），文件工具、沙箱回写、面板解析三处共用它。
于是修复前：

| 会话形态 | 两个工具是否分叉 | 说明 |
|----------|-----------------|------|
| 松散会话（无项目） | 否 | `projectID==""` ⇒ session 段保留，且 `codingSubdir=""`，两者解析结果相同 |
| 项目/coding 会话（有运行时） | **是** | `write_file` 落 `<codingSubdir>/foo.md`，`apply_patch` 落 `foo.md` |

修复覆盖 **6 个触点**（`apply_patch.go`）：host 模式 `readForPatch` / `writeForPatch` / `deleteForPatch`，
以及沙箱模式 `readForPatchSandbox` / `writeForPatchSandbox` / `deleteForPatchSandbox`。同时补上两件
与它同源的东西：

1. **镜像的 path 端与 store 端是同一条映射**。`writeThroughSignal` 用 `wsPath(path)` 同时算出
   store 键和 `/workspace/<键>`（[file.go](../../internal/agent/tools/file.go) 第 977 行一带）。
   它是这条映射的**第二个消费者**：只要写入端用别的解析，一次写入就自相矛盾——store 落到 A 键，
   镜像落到 B 路径，而 agent 在沙箱里读到的永远是 B。所以"镜像对不对"不能只看镜像那一行，
   要看**两个端点是否由同一个函数产生**。单测 `TestWriteThroughMirrorsOneKeyAndOnePath` 钉的就是
   这一对：`("app/notes.md", "/workspace/app/notes.md")`，两个工具都必须给出同样的二元组。
2. **沙箱模式的 `apply_patch` 以前根本不调用镜像**（这是本轮之前的实现事实，不是推断）：
   `registerSandboxedApplyPatch` 只写 store。现在它逐文件调用 `writeThroughSignal`，并把结果挂在该
   文件所在的工具结果上（`runApplyPatch` 本来就是一个路径一个回调，所以失败与信号都落在正确的文件上）。

**钉住它的测试**（每一条都做过反证：把修复行改回 `r.sessionID, path` → 变红）：

| 层 | 测试 | 反证时的输出 |
|----|------|-------------|
| host 模式键 | `apply_patch_path_scope_test.go`（3 条） | `keys = [app/notes.md sessions/sess-1/notes.md]` |
| 沙箱模式键 + 镜像二元组 | `write_through_signal_test.go::TestWriteThroughMirrorsOneKeyAndOnePath` | `keys = [app/notes.md sessions/sess_1/notes.md]` |
| 真机 E2B | `apply_patch_live_scope_e2e_test.go::TestE2BLiveOnePathIsOneKey` | —（真机，见该文件头部的运行命令） |

真机那条走的顺序与事故同形：seed → hydrate → `write_file` → `apply_patch`，断言 store 里**只有**
`app/notes.md` 一个键、沙箱读得到新内容、且 `/workspace/notes.md`（修复前会出现的根键在沙箱里的影子）
不存在。反证的意义在于：这三个断言分别在"解析分叉"和"镜像缺失"两种退化下都会红。

顺带清掉一处本轮引入的遗留：`Registry.sandboxSessionID` 全仓零引用，且注释指向一个不存在的
`sandboxScopeSession`——已删（[10](./10-harness-state-audit.md) §9）。

§8.1 **没有**声称、而 2026-09-22 这一趟必须补上的一点：`apply_patch` 是唯一不走 `routeFor` 的文件
工具——它保留着自己手写的后端梯子（`readForPatch` / `writeForPatch` / `deleteForPatch` 及其
`*ForPatchSandbox` 孪生）。§8.1 修好的 store 键解析是共享的，但 `routeFor` 的**策略**那一半（技能
规则）没有：那条梯子既不守任何 `SKILL.md`，也不路由任何 `skills/<name>/…`。于是
`Update File /skills/<name>/SKILL.md` + `*** Move to:` 能把操作者的清单文本带出只读挂载、落到一个
chatter 随后可以 `read_file` 的路径；而一条 `skills/<name>/…` 补丁会落进沙箱 `/workspace`（或 agent
home）而不是技能桶。两者现在都在工具入口、任何 op 规划之前被拒绝（`applyPatchRefusal`），命名空间那
条拒绝里点名 `write_file`——登记册第 50 行。这里要报的**不是**"应该用手搓梯子换掉 `routeFor`"（那是
更大的重构，而且未必对：apply_patch 需要逐路径做同样的宿主/沙箱分流）；而是：一个自持梯子的工具，
也就自持了"把共享梯子上每一条规则都带上"的责任，而它之前没带上。

### 8.2 已修（G17：可见性走 G+H、同步回写走 A）——下面是当时的 as-built 记录

> **2026-09-18 最终更新**：本节描述的错配**已经全部修掉**，三件事一起：
> **G** 预览容器按项目寻址（一个项目一个预览容器，两个入口同一个）；
> **H** 写入/删除广播到项目内所有活容器（保留每 chat 独立 shell）；
> **A** `syncSnapshot` 的回写**折叠到项目根**（`syncStoreScope`）——与 hydrate、与文件工具同一个键。
> 因此"沙箱新生的文件对工具不可见"和"每个项目文件在 chat 子目录多一份副本"都不再发生；
> 真机 `TestE2BLiveProjectSessionKeepsOneTree` 钉住新形状（无副本、exec 产物落项目根且工具可见、
> 沙箱改既有路径仍被拒）。**未做迁移**：折叠前已经产生的那些副本仍在库里（不再刷新、也无人清理）。
> 下面保留原文，作为"当时为什么会这样"的记录。

修 §8.1 时顺着"还有谁在用这条路径映射"往下查，发现同一族里的第二处，而且它比 §8.1 更靠底层：
同一个 coding 项目会话里，三处对作用域的看法并不一致。

| 谁 | 作用域 | 出处 |
|----|-------|------|
| 文件工具（`scopeSessionID()` → `workspace.WriteScope`） | `(agent, projectID, "")` —— session 被折叠 | `scopeSessionID()` |
| 沙箱池的实例键 | `(agent, projectID, chatSession)` —— 每个 chat 一个实例 | `internal/agent/loop.go` 第 244 行 `pool.Get(a.name, projectID, sessionID)` |
| **hydrate** | `projectID != ""` 时按 **session=""** 列整个项目 | `e2b_executor.go` 第 739–746 行（注释原文：*"so the chat sees sibling chats' files"*） |
| **syncSnapshot** | 一路用沙箱作用域的 `sc.sessionID` | `lifecycle.go` 第 671 行起（`Stat` / `Put` / `syncSnapshot` 的 List 全部） |

hydrate 那一半是**对的**（它把项目根的对象按 `session=""` 灌进沙箱，所以工具写的文件确实在
`/workspace/<path>`）。错配在 sync：它把沙箱的内容按 `<agent>/projects/<pid>/<chat>/…` 写回，
而工具读的是 `<agent>/projects/<pid>/…`。

真机验证（**当时**用 `TestE2BLiveProjectScopeIsNotTheSandboxScope` 记录，2026-09-18，逐行日志；
该测试在决策 A 之后改名为 `TestE2BLiveProjectSessionKeepsOneTree` 并改为断言新形状）：

| 步 | 观察 |
|----|------|
| 1 | seed 项目根一份 `app/notes.md` → hydrate `workspaceFiles=1` → 沙箱 `/workspace/app/notes.md` = `one\n` ✅ 与工具一致 |
| 2 | 第一次 `exec` 的 post-exec sync：该路径在 **chat 作用域**里"不存在" → 写第二份 `projects/<pid>/<chat>/app/notes.md`（日志 `new path from sandbox … session=chat-scope path=app/notes.md`） |
| 3 | 沙箱里再改同一路径（4 → 17 字节）→ sync 判 **BLOCKED**（`storeBytes=4 snapshotBytes=17`），信号 `[workspace] NOT synced …` 正常送达 agent |
| 4 | `read_file`（项目根那份）读到 `one\n`，沙箱的改动**永远进不了工具读的那一份** |

与 [07 §3.3](./07-formal-rootcause-and-fix.md) 拒绝的区别要写清：那条是"两份真的分歧、无法判定归属，
所以拒绝"，是**有价值**的保守；这条是**系统性**的——拒绝的对象是 sync 自己刚造出来的第二份，
每次改都会重演，且两份对象在 store 里长期并存（正是 §3.5 预告过的"重复对象"，现在有了真机证据）。

修法方向只有一行：让 sync 用与 hydrate **相同的折叠**（`projectID != ""` 时 `session=""`）。
但它改变"沙箱里新生的文件落在哪个键"，也就改变了项目内各 chat 之间的可见性（docker 侧的行为是
cwd 到 chat 子目录），所以它是**产品决策**而不是可观测性缺口——按 [10 §4](./10-harness-state-audit.md)
G17 记录，不擅自改。上面那条真机测试钉的是 as-built 事实：决策改变时它应当**同时**被改，
这样"变了"永远是一个显式动作。
