# 11 · 改动点总册：形式系统 → 代码 → UT → 真机 e2e

> 状态：登记册（2026-09-18）· 回答一个问题：**基于文件形式化（F1/F2/F3）与状态可观测性原则，
> 我们改了哪些点，每个点的 UT 与真机 e2e 在哪。**
> **部署状态（2026-09-19 更正）**：本册 #1–#31（T1–T6）**已提交**在分支 `fastagent`（HEAD `8984c99`，
> `a0080a5` 是锚点），**dev 已部署** `8984c99`；**production 仍是 `a24c0a8`（落后 34 个提交），
> 所以对 production 而言"上线"列依然全是 ❌**。原文"只在工作区（未提交）"写于 09-18，已过期；
> 两个远端：`fastagent`（私有，已含 `8984c99`）与 `origin` = `tokenaissance/fastclaw`
> （**公开镜像，落后 34 个提交，按决定暂不推送**）。
> 对照表与理由见 [10 的"部署状态"横幅](./10-harness-state-audit.md)。
> 与其它文档的分工：**10 §4** 管"缺口的状态"，**05** 管"方案与决策记录"，**本文** 管"改动点 ↔ 证据"。

## 0. 怎么读

| 列 | 含义 |
|----|------|
| **形式（义务）** | [00](./00-formal-systems.md) 的三套形式系统：**F1** 前置条件/零迁移、**F2** 可观测性、**F3** 投递；括号里是 [08 §2.2](./08-state-observability-principle.md) 的义务 O1–O5。`—` 表示不属于这三套（产品决策 / 单一来源 / 协议合规 / 死码） |
| **代码锚点** | 函数名或 `文件:行`；行号会漂，函数名不会，所以以函数名为准 |
| **UT** | 进程内测试（`go test`）。标「反证」的，表示把修复改回去该测试会红 |
| **真机 e2e** | 需要 `FASTAGENT_E2B_LIVE=1 E2B_API_KEY=…` 的 E2B 测试；运行命令写在各自文件头部 |
| **上线** | ❌ = **未部署到生产**。它不说明代码是否存在：09-19/20/21 那几行**已提交**（见表格下方说明）。这一列里的 ✅ 等于声称发过一次版，所以没发布之前它保持 ❌ |

> **本册不收录「控制动作形态」。** 形态是**缺口**的属性（不提供／提供／顺序／时长），
> 逐格判定与可核查读数在 [10 §4 的表头](./10-harness-state-audit.md)；本册只做「改动点 ↔ 证据」。
> **同一条事实不写两份表达式**——要按形态查，去 10 §4；要按改动查，留在这里。
> （理由：改动与缺口不是一对一——#23 一次关掉两格，新改动可能还不属于任何既有缺口。
> 硬塞一列，那一格迟早会说谎。）

**四层归属**（Clean Architecture）：F1 组落在 **Use Case（对账策略）+ Frameworks（store / sandbox）**；
F2 组横跨 **Entities（回合收据）→ Use Cases（信号渲染）→ Interface Adapters（工具结果）**；
第 4 组的七条**全部**落在 **Interface Adapters ↔ Use Cases 的端口**上——那不是巧合，G21/G17/G22
三处缺陷的共同位置就是这条缝（谁拥有"这个键在哪个作用域"这个事实）。

**覆盖范围（2026-09-18 逐文件核对）**：本册对应工作区的**全部**改动 —— `git status --porcelain`
里的 81 个文件（47 改 + 34 增/改名/删）每一族都能找到一行：F1 组 #1–#4、F2 组 #5–#18、
F3 组 #19–#20、路径与作用域 #21–#27 与 #31、其余 #28–#30。**没有一族落在表外**；反向也成立 ——
表里没有"只写在文档里、代码里并不存在"的行（每条都给了函数名锚点）。

## 1. F1 · 前置条件 / 零迁移（事故根因与对账）

| # | 改动点 | 形式（义务） | 代码锚点 | UT | 真机 e2e | 上线 |
|---|--------|-------------|---------|----|---------|------|
| 1 | 同步判据从 **`Size == len(data)` 就跳过**（否则沙箱覆盖 store）换成 **size+mtime → 字节比较 → 分歧则 `BLOCKED` 拒绝并报告** | F1（R1/R2 零迁移） | `sandbox/lifecycle.go` `syncSnapshot` | `TestSyncContract_StoreEditIsNotOverwritten`、`_SurvivesPostExecTrigger`、`_NewPathIsFlushed`、`TestSyncVerdictDoesNotDependOnThePoolThatMakesIt`（**反证见 §10-1**） | `TestE2BLiveRepro`（**反证见 §10-2**） | ❌ |
| 2 | 删掉 **pod-local 基线**（内存判据跨副本失效）→ 无记忆对账 | F1（R3 收敛） | `sandbox/lifecycle.go`（baseline/digest 表删除）、[10 §9](./10-harness-state-audit.md) | `TestSyncVerdictDoesNotDependOnThePoolThatMakesIt`（两个世界：一个 hydrate 过、一个只是接管）、`TestEvictSignalOutlivesThePoolThatProducedIt`（**反证见 §10-3**） | `TestE2BLiveRepro` | ❌ |
| 3 | **交付戳**：hydrate 与穿透都把 store 的 mtime 写到沙箱副本上（廉价判据的前提） | F1（判据可重算） | `sandbox/e2b_executor.go` hydrate、`sandbox/lifecycle.go` `WriteThrough` 的 STAMP 段 | `TestWriteThroughStampsTheSandboxCopyWithTheStoreTime`（钉住**命令里那个时刻**，不是"若干读"；**反证见 §10-4**） | `TestE2BLiveHydrateKeepsStoreStamp`（**反证见 §10-5**） | ❌ |
| 4 | 磁盘布局与 hydrate 的一致性：`/workspace/<path>` 与 store 键一一对应（宽松会话不带 `sessions/<sid>/` 前缀） | F1（R4 局部性） | `sandbox/e2b_executor.go` hydrate | —（只有真机能证；**反证见 §10-6**） | `TestE2BLiveLooseScopeLayout` | ❌ |

## 2. F2 · 可观测性（谁必须说话）

| # | 改动点 | 形式（义务） | 代码锚点 | UT | 真机 e2e | 上线 |
|---|--------|-------------|---------|----|---------|------|
| 5 | 写入穿透泛化：三个写工具在**所有远程后端**都镜像（此前只在 coding 会话、且 `apply_patch` 不镜像） | F2（C1 进真正读的通道） | `agent/tools/file.go` `writeThroughSignal`、`agent/tools/apply_patch.go` | `TestWriteFileSignalsReplacedVersion`、`TestWriteFileSignalsUnreachableSandbox` | `TestE2BLiveWriteThroughReachesSandbox` | ❌ |
| 6 | `CompareResult` **四态**替代单个 `Compared bool`（G5：5 字节文件曾被报"超过 2 MiB"） | F2（O1 说真话） | `sandbox/lifecycle.go` `CompareResult`、`agent/tools/file.go` | `TestWriteFileSignalsUncheckedReplacement`、`TestWriteFileStaysQuietOnASharedBackend`、`TestSyncContract_WriteThroughWithoutExpectationClaimsNothing` | `TestE2BLiveWriteThroughReachesSandbox` | ❌ |
| 7 | **G6**：pre-image 随调用传下去，写工具因此第一次能说"替换了不同版本（N 字节）" | F2（O1） | `agent/tools/file.go` `previousStoreVersion`、`apply_patch.go` `plannedWrite.previous` | `TestWriteFilePassesThePreviousStoreVersionAsExpectation` | 同上 | ❌ |
| 8 | **G4/G7a**：`StoreOnlyLine` —— 同步与 `list_dir` 共用**一句话**点名"store 有、沙箱没有"，并给出补救；**拒绝声称归属** | F2（O1）+ F3（O2） | `sandbox/lifecycle.go`（`storeOnly` + 额外一次 List）、`agent/tools/file.go` `storeOnlySignal` | `TestSyncReportsPathsTheStoreHasAndTheSandboxDoesNot`、`TestSyncDoesNotReportTheSkillsNamespace`、`TestListDirNamesPathsTheSandboxDoesNotHave`、`TestListDirStaysQuietWhenStoreAndSandboxAgree`、`TestListDirStaysQuietWhenTheSandboxCannotBeAsked` | `TestE2BLiveSandboxEditOfStorePathIsRefusedAndReported`（拒绝路径同期覆盖） | ❌ |
| 9 | **G19**：未水合声明随**实例**持久化到 `sandbox_leases.unhydrated`，采纳时读回 | F2（O1） | `sandbox/lease.go`、`store/sandbox_leases.go`、`sandbox/e2b_executor.go` `publishUnhydrated` / `setWorkspaceUnhydrated`、`store/database.go`（老库 retrofit 迁移） | `TestSandboxLeaseUnhydratedRidesTheInstance`、`TestE2BPoolAdoptionCarriesTheUnhydratedFact`、`TestE2BPoolPublishesTheUnhydratedFactOnCreate` | `TestE2BLiveUnhydratedFactSurvivesPodHandoff` | ❌ |
| 10 | **G8**：身份文件指纹进回合采样（只说改了哪个文件，不泄内容） | F2（O1） | `agent/env_changes.go` `identityFingerprints` | `TestEnvSignalCarriesIdentityFileChanges` | 无（见 §7） | ❌ |
| 11 | **G9 → G20**：配置指纹的基线从进程内搬进**回合收据**（`run_receipt`），`envTracker` 整表删除；三条回合入口统一采样 + 盖戳 | F2（O1）+ F3（O4） | `agent/env_changes.go`、`session/manager.go` `SetRunReceipt`/`RunReceiptOf`、`session/store_adapter.go`、`agent/loop.go` | `TestEnvBaselineComesFromTheTurnReceipt`、`TestRebuiltAgentStatesTheChangeFromTheReceipt`、`TestReceiptCarriesEverySampledFamily`、`TestRunReceiptStampSurvivesAReload`、`TestEnvSignalStatesAConfigChangeThatOutlivedItsInstance` | 无（见 §7） | ❌ |
| 12 | **G10**：cron 清单进采样（排除 `LastRun`/`NextRun` 记账字段；读不到就声明读不到） | F2（O1） | `agent/env_changes.go` `cronFingerprint`、`cronLabels` | `TestEnvSignalCarriesScheduledJobChanges`、`TestCronFingerprintIgnoresRunBookkeeping`、`TestEnvSignalStatesUnreadableJobListWithoutClaimingDeletion` | 无（见 §7） | ❌ |
| 13 | 技能列表读不全 ⇒ 声明"这份列表不完整"（而不是让 agent 否认自己有某技能） | F2（O1） | `agent/skills.go`（`hydrateFailed`）、`agent/env_changes.go` | `TestEnvSignalCarriesIncompleteSkillListOnFirstTurn`、`TestEnvSignalStopsCarryingIncompleteSkillsAfterRecovery` | 无（见 §7） | ❌ |
| 14 | 工具输出被剪枝时**说清发生了什么**（不是只留一句 "truncated"） | F2（O1） | `agent/compaction.go` | 无 UT（纯文案，见 §7） | 无 | ❌ |
| 15 | **G11 stdio 半边**：MCP 通知有接收口（`NotificationSink`）→ 30s/服务闸门 → 复用 `mcpConfigNotify` 重建 → 回合级工具集信号 | F2（O1） | `mcp/client.go`、`mcp/stdio.go`、`mcp/manager.go` | `TestStdioClientHandsNotificationsToTheHandler`、`TestManagerWiresNotificationsThroughTheGate`、`TestManagerDoesNotWireATransportWithoutNotifications` | 无（HTTP 半边仍开放，见 §7） | ❌ |
| 16 | **G12**：自动回合被丢弃时逐条带全信息；用户创建的 cron 额外在该会话发一条注记 | F2（O2） | `gateway/deferred_turns.go`、`gateway/gateway.go`（注记出口，有界发送 `250ms`） | `TestDeferredTurnsAnnouncesADroppedScheduledTask`、`TestDeferredTurnsDropsMessagesPastBudget`、`TestDroppedCronNoteWithoutAJobName` | 无（见 §7） | ❌ |
| 17 | **G14**：heartbeat 与提示词读**同一个** `HEARTBEAT.md`（单一来源） | —（单一来源） | `agent/heartbeat.go` `loadHeartbeatTasks` | `TestHeartbeatReadsWhatThePromptShows`、`TestHeartbeatFallsBackToTheDiskCopy`、`TestHeartbeatWithNoFileSendsNothing` | 无（见 §7） | ❌ |
| 18 | **统一环境信号出口**：所有"你的世界变了"走**一个出口**（不是每个子系统各自发明一句），渲染在**提示词最后一段**（之前的各段逐字节不变，保住提示词缓存）；工具名与记忆进同一份快照；术语对齐 `notice → signal`（σ） | F2（C1 进真正读的通道 + C3 无变化不说话） | `agent/env_changes.go` `signalEnvironmentChanges` / `renderEnvDelta`、`agent/context.go` `SetEnvironmentSignal`、`agent/tools/registry.go` `ToolNames` | `TestEnvSignalCarriesRemovals`、`TestEnvSignalCarriesAdditionsAndEdits`、`TestEnvSignalIsSilentWhenNothingChanged`、`TestEnvSignalIsSilentOnFirstObservation`、`TestEnvSignalIsPerSession`、`TestEnvSignalIgnoresContentPreservingMemoryRewrite`、`TestReceiptCarriesEverySampledFamily`；改名后的 `TestWorkspaceSignalSurvivesTheLifecycleProxyAndTheMetaStrip` | 无（见 §7） | ❌ |

## 3. F3 · 投递（信号怎样真的到达）

| # | 改动点 | 形式（义务） | 代码锚点 | UT | 真机 e2e | 上线 |
|---|--------|-------------|---------|----|---------|------|
| 19 | **G3**：空闲驱逐产生的信号从**进程内队列**换成**耐久载体**（`configs_kv`，scope 级）；可重算的（拒绝/失败）不排队，只排队不可重算的（moved）；"沙箱被替换"的注记搭在发现它的那次调用上返回 | F3（O4）+ O2 | `gateway/sandbox_signals.go`、`gateway/userspace.go` / `gateway/reload.go`（`SetSignalStore` 接线，两个建池入口都接）、`sandbox/lifecycle.go` `parkSignal` / `takeSignals` / `takeReplacedNote` | `TestEvictSignalOutlivesThePoolThatProducedIt`、`TestReplacedSandboxNoteRidesTheCallThatFoundIt` | `TestE2BLiveRepro`（驱逐路径） | ❌ |
| 20 | 把"判据该落在哪里"形式化为**三落点**（可重算 / 复用既有耐久记录 / 新建载体），并据此把环境基线搬进回合收据（与 #11 同源） | F3（O4 的第二种形态） | `sandbox/lifecycle.go` `liveInstance`、[08 §2.2.3](./08-state-observability-principle.md)；代码见 #11 | 见 #11 | 见 #11 | ❌ |

## 4. F1-实体 · 路径与作用域不变式（G18 / G21+d1 / G17-G+H+A / G22）

> 这七条是同一句话的七次落地：**"这个键在哪个作用域"这个事实必须由知道的人声明，不能在仲裁点猜。**

| # | 改动点 | 形式（义务） | 代码锚点 | UT | 真机 e2e | 上线 |
|---|--------|-------------|---------|----|---------|------|
| 21 | **G18（01 §8）**：`apply_patch` 的 6 个 store 触点改用与 `write_file`/`edit_file` 同一个解析（`scopeSessionID()` + `wsPath()`） | F1（一条路径一个键） | `agent/tools/apply_patch.go` | `TestApplyPatchUsesTheSameStoreKeyAsWriteFile`、`TestApplyPatchDeleteUsesTheSameStoreKey`、`TestApplyPatchKeyInANonCodingSession`、`TestWriteThroughMirrorsOneKeyAndOnePath` | `TestE2BLiveOnePathIsOneKey` | ❌ |
| 22 | **G21 Fix 0**：面板删除与下载**同一路径约定**（带前缀的路径按 agent 相对解释；旧的无前缀形状继续可用） | F1（同一路径一个键） | `setup/handlers_agents.go` `handleAgentFileDelete`、`sandbox/workspace_paths.go` `StorePathScope` | `TestHandleAgentFileDelete_UsesThePathThePanelClicked`、`_ProjectPathTargetsTheChatSandbox`、`_ScopeRelativePathStillWorks`、`TestStorePathScope` | `TestE2BLivePanelDeleteSticks` | ❌ |
| 23 | **d1**：删除也穿透到**活**沙箱，且**绝不建实例**（`LiveExecutorPool` 只找活实例）；docker 那种单副本后端是 no-op | F1（写路径对称）+ 非投递 | `gateway/workspace_files.go`、`setup/handlers_agents.go`（面板侧的窄接口断言）、`sandbox/lifecycle.go` `RemoveLiveWorkspaceFile`、`sandbox/executor.go`、`sandbox/workspace_paths.go` | `TestRemoveWorkspaceFile_AsksTheExecutorsCapability`、`_NoPoolIsANoOp`、`_SingleCopyBackendIsANoOp`、`TestSandboxPathForStorePath`、`TestE2BPoolLiveExecutorDoesNotCreate`、`TestDockerPoolLiveExecutorDoesNotCreate` | `TestE2BLivePanelDeleteSticks` | ❌ |
| 24 | **G17-G**：预览容器统一按项目寻址（一个项目一个预览容器，两个入口同一个） | —（作用域不变式） | `runtime/runtime.go` `previewSandboxSession` | `TestPreviewSandboxSession` | 由 #25 的真机覆盖 | ❌ |
| 25 | **G17-H**：写入与删除**广播到项目内所有活容器**（保留每 chat 独立 shell） | —（作用域不变式） | `sandbox/lifecycle.go` `mirrorToProjectPeers` + 删除扇出、`sandbox/executor.go` `LiveProjectExecutors` | `TestWriteThroughReachesEveryContainerOfTheProject`、`TestWriteThroughCountsAContainerItCouldNotReach`、`TestRemoveLiveWorkspaceFileReachesEveryContainerOfTheProject` | `TestE2BLiveProjectWriteReachesSiblingContainer` | ❌ |
| 26 | **G17-A**：同步回写折叠到**项目根**（`syncStoreScope`）——不再产生 `<pid>/<chat>/…` 重复副本，`exec` 的产物工具立刻可见 | —（作用域不变式） | `sandbox/lifecycle.go` `syncStoreScope` | `TestSyncWritesBackToTheProjectRootNotTheChatSubdir`、`TestSyncScopeEqualsHydrateScopeForProjects` | `TestE2BLiveProjectSessionKeepsOneTree` | ❌ |
| 27 | **G22**：写入方把 store 作用域一起交下来（`sandbox.StoreScope`），盖章不再从容器推断 | F1（同一路径一个键） | `sandbox/executor.go` `StoreScope`、`sandbox/lifecycle.go`、`agent/tools/file.go` | `TestWriteThroughStampsWithTheStoreScopeItWasGiven`（**反证见 §10-7**）、`TestWriteThroughStampsTheSandboxCopyWithTheStoreTime` | `TestE2BLiveSyncReadsNoBodiesForStampablePaths`（**实测**：同一路径的整对象读取 **1 → 0**；但见 §10-8：这条读数是**测量不是反证**——±1 秒容差会把"没盖章"也盖住） | ❌ |
| 31 | **G23**：「项目会话 ⇒ 键落项目根」与**布局表**各收成**一个纯函数**（`workspace.ScopeSegments` / `workspace.WriteScope`）——文件工具、沙箱回写、面板解析、LocalFS、S3 全部改为调用它；`Registry.codingRootScope` / `SetCodingRootScope` 删除，`sandbox.StoreScope` 变成 `workspace.Scope` 的**别名** | —（作用域不变式，同一族的第四/第五处；F1 的"一条路径一个键"） | `workspace/scope.go`（新）、`workspace/localfs.go`、`workspace/s3.go`、`agent/tools/registry.go`、`agent/loop.go`、`sandbox/lifecycle.go` `syncStoreScope`、`sandbox/executor.go` `StoreScope` | `TestScopeSegmentsIsTheLayoutTable`、`TestWriteScopeCollapsesInsideAProject`、`TestAWriterScopeIsTheScopeItsKeysLandIn`、`TestLayoutWriteScopeAndParserAgree`、`TestProjectWritersAndTheSyncShareOneScope`、`TestAProjectChatSubdirKeyIsItsOwnPath`、`TestScopeSessionIDCollapsesInsideAProject`（**反证**：`scopeSessionID()` 改回 `r.sessionID`、`syncStoreScope` 改回"不折叠" ⇒ 各两条/三条变红，实测） | 作用域逻辑变了 ⇒ 复跑真机全套（`TestE2BLiveProjectSessionKeepsOneTree`、`TestE2BLiveOnePathIsOneKey`、`TestE2BLiveProjectWriteReachesSiblingContainer`、`TestE2BLivePanelDeleteSticks` 等都在这条路径上） | ❌ |

## 5. 不属于 F1–F3 的改动

| # | 改动点 | 类别 | 代码锚点 | UT | e2e | 上线 |
|---|--------|------|---------|----|-----|------|
| 28 | **G16**：技能密钥被自己的**遮罩**覆盖 —— 规则收成一个家（`mergeSkillEntry` + `mergeSkillEntries`），两条写入路径共用；深拷贝 `cloneSkillEntries` 修掉 JSON 解码器**复用 map** 的坑 | setup API 缺陷 | `setup/handlers.go` | `TestMaskedGlobalSkillSecretKeepsTheStoredValue`、`TestMaskedAgentSkillSecretKeepsTheStoredValue`、`TestMergeSkillEntriesSemantics`、`TestMaskedValueIsWhatThePanelSends` | 无（HTTP 层，见 §7） | ❌ |
| 29 | **死码与遗留配置清理**：`WorkspaceSync`（121 行，从未接线）、`makeExecTool`、整条 `BoxliteClientID` 链（config/env/管理端/web/池/执行器）、`generateRandomToken`、`filterAccounts`、`defaultIfEmpty`、`delta.deleted`、`Registry.sandboxSessionID` | 死码（CCP：没有变更原因就不该存在） | 见 [10 §9](./10-harness-state-audit.md) 的表 | 无 UT（删除本身）；验证 = 全仓引用计数 + `go build` / `vet` | 无 | ❌ |
| 30 | **取证与清理工具**：`scripts/workspace_revert_audit.py`（事故回退取证，离线 TSV 输入）、`scripts/workspace_project_chat_duplicate_cleanup.py`（A 之前产生的重复副本，`--selftest` 自检，只在"字节在项目根另有存活"时才列入删除） | 运维工具 | `scripts/` | 脚本自带 `--selftest`（6 种形状） | 无（离线；生产上按 §8 的顺序执行） | ❌ |

## 6. 复核命令

```bash
# 全量（每次改动后）
go build ./... && go vet ./internal/... && go test ./internal/... -count=1

# 真机全套（E2B；约 2 分钟）
FASTAGENT_E2B_LIVE=1 E2B_API_KEY=… go test ./internal/sandbox/ ./internal/agent/tools/ -run 'TestE2BLive' -count=1

# 分组（对应上面的编号）
go test ./internal/sandbox/ -run 'TestSyncContract' -count=1                     # #1 #2 #6
go test ./internal/sandbox/ -run 'Test(Evict|Replaced|SyncReports)' -count=1     # #8 #19
go test ./internal/store/   -run TestSandboxLeaseUnhydrated -count=1             # #9
go test ./internal/agent/   -run 'TestEnvSignal|TestEnvBaseline|TestReceipt|TestHeartbeat' -count=1  # #10–#17
go test ./internal/mcp/     -run 'TestStdioClient|TestManager' -count=1          # #15
go test ./internal/gateway/ -run 'TestDeferredTurns|TestDroppedCronNote|TestRemoveWorkspaceFile' -count=1  # #16 #23
go test ./internal/setup/   -run 'TestHandleAgentFileDelete|TestMasked|TestMergeSkillEntries' -count=1      # #22 #28
go test ./internal/sandbox/ -run 'TestWriteThrough|TestRemoveLiveWorkspaceFileReaches|TestSyncWritesBack|TestSyncScope|TestSandboxPath|TestStorePathScope|TestE2BPoolLiveExecutor|TestDockerPoolLiveExecutor' -count=1   # #5 #18 #25 #26 #27
go test ./internal/sandbox/ -run 'TestSyncContract|TestSyncVerdict|TestWriteThroughStamps' -count=1   # #1 #2 #3（含 §10 的反证目标）
go test ./internal/runtime/ -run TestPreviewSandboxSession -count=1               # #24
go test ./internal/session/ -run TestRunReceipt -count=1                          # #11
go test ./internal/sandbox/ -run 'TestLayoutWriteScopeAndParserAgree|TestProjectWritersAndTheSyncShareOneScope|TestAProjectChatSubdirKeyIsItsOwnPath' -count=1   # #31
go test ./internal/workspace/ -run 'TestScopeSegments|TestWriteScope|TestAWriterScope' -count=1  # #31
```

## 7. 没有真机 e2e 的条目，以及为什么

* **回合级环境信号（#10–#14、#17）**：机制完全在进程内 + DB，不引入沙箱。它们的 E2E 等价物是
  **真 sqlite 的往返测试**（#11 的 `TestRunReceiptStampSurvivesAReload`）与逐条渲染断言；用真机跑一遍
  只会增加沙箱开销，不会多证明一件事。
* **压缩占位符（#14）**：纯文案；由 08 的原则审查 + 代码阅读钉住。
* **技能遮罩（#28）**：HTTP 层缺陷，与沙箱无关；真机没有对应语义。
* **piiScrubbing 开关（#49）**：没有真机可跑。它问的是*agent 交给 provider 的东西是什么*，而两处见证里的假 provider 记录的
  正是这件事；真机跑一遍需要真实 provider 加会话里的 PII，只会把同一条断言再往外推一跳。
* **死码清理（#29）**：删除本身就是验证（引用计数 + 编译）。
* ~~仍然开放、因此没有 e2e 的~~ ——**两者都已在 2026-09-22 关闭，而且各自补上了缺的那次 e2e**：**G11 的 HTTP
  半边**是那条站着的 SSE 流（第 45 行，`httptest` 加上真机 `TestE2BLive*` 那一跑）；**G15** 是 `initialized` 通知
  加真机 `TestLiveMCPHandshakeReadsTheWholeToolList`（第 48 行）——也正是它把「可能是前提」变成了 12-vs-13 个工具的
  实测。MCP 族剩下的是边界 3（从不宣告的 server），而那一条是**已裁决**的限制（只走推送、不做每回合 re-list），
  不是开放项。

## 8. 上线顺序建议（按风险）

1. **#1–#4**：事故根因。线上现在**仍可复现**（HEAD 的判据是"size 相同就跳过，否则沙箱覆盖 store"）。
2. **#21–#27**：路径/作用域那一族。其中 **#22（面板删除静默无效）**与 **#26（重复副本仍在产生）**
   是线上正在发生的用户可见问题。
   **#31** 是这一族的收口（同一族第六处：把规则收成一个家）——它与这一组一起上线，没有独立风险，
   但**必须整组一起发**：它的行为差只存在于"有项目、没有 runtime manager"的部署（那种部署里项目
   也建不出来）。
3. **#5–#20**：信号与投递。它们不修数据，但决定"以后出问题时 agent 能不能自己看出来"。
4. **#30 的清理脚本**：必须在 **#26 上线之后**执行，否则旧行为会继续产生副本（脚本 docstring 顶部已写明）。

## 9. 评审发现 → 已落地（同日）

上一轮评审在 §4 那条缝上又找到两处重复表达式（[10 §4 的 **G23**](./10-harness-state-audit.md)）：
「项目会话 ⇒ 键落项目根」被写成两种判据、出现在三处（agent 侧 `scopeSessionID()` 的 `codingRootScope`、
沙箱侧 `syncStoreScope()` 的 `projectID != ""`、面板侧 `StorePathScope()` 的前缀判定）；顺这条线再查，
**布局表**（`pid/sid` → 目录）又在 `LocalFS.scopeDir` 与 `S3.key`/`S3.scopePrefix` 各写了一遍。

两条规则现在都在 [`internal/workspace/scope.go`](../../internal/workspace/scope.go)：`ScopeSegments`
（布局）与 `WriteScope`（写者折叠）。这就是上表 **#31**，反证与实测写在 #31 与 10 §4 的 G23 行里。

## 10. 反证记录（2026-09-18，逐条实测）

每一行 = 一次**受控反证**：把某处改动改回它修之前的样子（或改成明显错误的形状），跑指定测试，记录它变红。
全部做完后已撤回，仓库里没有 `FALSIFY` 残留（`rg FALSIFY internal/` 零命中）。

| # | 针对 | 把什么改回去 | 哪个测试变红 | 观察到的输出 |
|---|------|-------------|-------------|-------------|
| 10-1 | #1（离线） | `syncSnapshot` 的判据改回 HEAD 的 **"size 相同就跳过，否则覆盖 store"** | `TestSyncContract_StoreEditIsNotOverwritten`、`_StoreEditSurvivesPostExecTrigger`、`_SandboxEditOfStorePathIsRefused` | `the sandbox copy overwrote the host's version — store: "OLD SNAPSHOT VERSION"; want: "THE HOST WROTE THIS LONGER VERSION"`（事故第一次在**离线**被复现） |
| 10-2 | #1（真机） | 同上（判据改回 HEAD） | `TestE2BLiveRepro` | `the store lost the host's version — the incident is back`；`store after sync: "OLD SNAPSHOT VERSION (1111111111111111)"`，同步日志为空（旧逻辑静默覆盖） |
| 10-3 | #2 | 把 **pod-local 基线**以最小形态放回（hydrate 时记住"交给沙箱的是什么"，同步时据此判定） | `TestSyncVerdictDoesNotDependOnThePoolThatMakesIt` | ①事故场景：`{[] [] [] }`（有记忆的 pod **什么都不说**）vs `{[] [report.html] [] }`（接管的 pod 报 BLOCKED）；③沙箱改动场景：`{[report.html] [] [] }`（**直接把未归属的沙箱改动写回 store**）vs `{[] [report.html] [] }`。同一状态、两个 pod、两种裁决 —— 这就是删掉它的理由 |
| 10-4 | #3（离线） | 穿透盖章改成写**镜像自己的时钟**（`time.Now()`）而不是 store 对象的 mtime | `TestWriteThroughStampsTheSandboxCopyWithTheStoreTime` | `want a command: touch -d @1700000001 '/workspace/app/notes.md'` |
| 10-5 | #3（真机） | hydrate 传 **零时间**而不是 `obj.ModTime`（沙箱副本不再带 store 的时间戳） | `TestE2BLiveHydrateKeepsStoreStamp` | `sandbox file mtime 1789734932 is +8s from the store's LastModified 1789734924` |
| 10-6 | #4（真机） | hydrate 把**作用域前缀**也铺进沙箱（`/workspace/sessions/<sid>/<path>`，即镜像旧硬编码路径会产生的形状） | `TestE2BLiveLooseScopeLayout` | `a loose scope's file is not at /workspace/<path> (got "no…")` + `the sandbox has a /workspace/sessions tree`；同一轮里信号层正确地报出 `[workspace] 1 path(s) are in the workspace store but NOT in this sandbox …` |
| 10-7 | #27 | 盖章的 `Stat` 改回**沙箱作用域**（G22 的原缺陷） | `TestWriteThroughStampsWithTheStoreScopeItWasGiven` | `primary container was not stamped — the store scope the caller stated was ignored: []` |
| 10-8 | #27（边界） | **移除**穿透盖章（整条 `touch` 变 no-op） | `TestE2BLiveSyncReadsNoBodiesForStampablePaths` **没有变红** | `one sync in a project session: 0 whole-object reads, 2 stats` —— 判据的 ±1 秒容差（`sameVersion`）把"store 写和镜像写在同一秒"这件事盖住了，所以**这条读数是测量，不是反证**。真正能钉住盖章的是 10-4（命令）与 10-5（hydrate 的时间戳） |

**一条夹具发现（同样实测）**：`syncFixture` 原先**先制造分歧、后 hydrate**，而 hydrate 会把 store 的内容倒回沙箱 ——
分歧在同步看到它之前就被抹掉了，所以上面三条离线契约测试在 **HEAD 的旧判据下也照样绿**（10-1 第一次跑就没有红）。
夹具已改成"先交接（两边同字节）→ hydrate → 再制造分歧"，10-1 才成为真正的反证。

### 10.1 全仓扫描：还有没有同类的"空夹具"

判据是经验性的，不靠读代码：**把 #1 的判据改回 HEAD，整包跑 `./internal/sandbox/` 与 `./internal/agent/tools/`**，
看哪些测试会红。两次对照（都是实测）：

| 形态 | HEAD 判据下的红灯 |
|------|------------------|
| **旧夹具**（分歧先、hydrate 后） | **3 条**：`TestExecObservesRefusedPaths`、`TestBlockedPathsAreReDerivedRatherThanCarried`、本册新增的 `TestSyncVerdictDoesNotDependOnThePoolThatMakesIt` —— 而三条 `TestSyncContract_*` 事故断言**全绿**，即：事故在 HEAD 上**根本进不了 CI 的红灯** |
| **新夹具**（交接 → hydrate → 分歧） | **6 条**：上面 3 条 + `TestSyncContract_StoreEditIsNotOverwritten`、`_StoreEditSurvivesPostExecTrigger`、`_SandboxEditOfStorePathIsRefused` |

逐夹具核对其余会 hydrate 的测试，结论是**它们不属于这一类**（各条的理由）：

* `sync_project_scope_test.go`：直接调**内层** `pool.Get`，从不 hydrate；且它用真 `LocalFS`，因为假 store 的
  `scopeForKey` 会把 `(pid, sid)` 折叠成 `p:<pid>` —— 这本身是"项目根 vs 项目 chat"差别的盲点。
* `broadcastFixture`：hydrate 写的是同一条路径的**写前**内容，写穿透随后覆盖它；断言看的是最终内容与盖章。
* 真机各条：要么在**建池之前**就把 store 种好（`liveE2B` 与 `e2b_live_*`），要么在**出生之后**再改
  （`e2b_live_project_broadcast_test`、`e2b_live_stamp_cost_test`）。
* `internal/agent/tools/*`：用裸执行器（没有 lifecycle 池 ⇒ 不会 hydrate）。
* `lifecycle_test.go` 的 hydrate 系列：它们**就是要**测 hydrate，先种 store 是正确顺序。

**夹具的两个已知盲点**（不是缺陷，但决定了用例该放哪）：假 store **没有 mtime**（所以时间戳类断言只能钉命令或上真机，
见 10-4/10-5）；假 store 的 `scopeForKey` **折叠 (pid, sid)**（所以"项目根 vs 项目 chat"只能靠真 `LocalFS`
或真机，见 `sync_project_scope_test.go` 与 10-7）。

## 11. 上线分组清单

> §11.1–§11.4 是**计划**（评审时定的边界）；**§11.7 是实际落地**（七笔提交、每笔的 SHA 与验证），
> 两者不一致处以 §11.7 为准，差异与原因也列在那里。§11.5 是每笔要过的门槛，§11.6 是文档入库的结果。

### 11.1 计划：六笔提交

> 2026-09-18 追加两笔：**T0**（给联网测试加闸门，先让 CI 可信）与 **T5**（形式化文档入库，见 §11.6）。
> 它们与 T1–T4 无代码依赖，顺序上 T0 放最前、T5 放最后即可。

| 提交 | 内容 | 形式 | 本册行 | 文件 | 能单独上线吗 |
|------|------|------|--------|------|-------------|
| **T0** | 测试卫生：联网测试加闸门 | —（测试卫生） | §11.5 的外部依赖 | 6 个文件（两个包各一个 `live_net_test.go` helper + 4 个挂了闸门的测试文件） | ✅ 不含产品代码 |
| **T1** | 沙箱对账与作用域 | F1 + 作用域不变式 | #1–#4、#21–#27、#31 | 25 个整文件 + 10 个跨组文件的 T1 部分 | ✅ 即 §8 的第 1、2 步（根因 + 路径/作用域），事故修复本身 |
| **T2** | 信号与投递 | F2 + F3 | #5–#20 | 36 个整文件 + 10 个跨组文件的 T2 部分 | ✅ 但**必须紧跟 T1**：其中几条 σ 描述的是 T1 引入的行为 |
| **T3** | 死码与配置清理 | —（CCP） | #29 | 7 个整文件 + 跨组文件的 T3 部分 | ✅ 纯删除，随时可发 |
| **T4** | 脚本与仓库内文档 | —（运维） | #30 | 3 个整文件 | ✅ 离线工具，无运行时影响 |
| **T5** | 形式化文档入库 | —（文档，GEB 同构） | §11.6 | 26 个文档文件 + 13 处引用旧路径的注释/脚本 | ✅ 纯文档 |

**为什么 T1、T2 要分成两笔**：T1 是"数据不再被覆盖"，T2 是"以后出事 agent 能自己看出来"。两者可以分开发布
（§8 的第 1、3 步），但**顺序不能反**——先发 T2 的话，那些 σ 描述的是一次仍然会覆盖的同步。

### 11.2 整文件归属

**T1（25）**

* `internal/workspace/{scope.go, scope_test.go, localfs.go, s3.go}`
* `internal/sandbox/{workspace_paths.go, workspace_paths_test.go, executor.go, docker_executor.go, lifecycle_sync_contract_test.go, sync_project_scope_test.go, project_broadcast_test.go, live_executor_test.go, e2b_live_repro_test.go, e2b_live_panel_delete_test.go, e2b_live_project_broadcast_test.go, e2b_live_stamp_cost_test.go}`
* `internal/runtime/{runtime.go, preview_scope_test.go}`
* `internal/gateway/{workspace_files.go, workspace_files_test.go}`
* `internal/setup/handlers_agent_file_delete_panel_test.go`
* `internal/agent/tools/{apply_patch_path_scope_test.go, apply_patch_live_scope_e2e_test.go, coding_scope_test.go, apply_patch_test.go}`

**T2（36）**

* `internal/agent/{env_changes.go, env_changes_test.go, heartbeat_source_test.go, receipt_baseline_test.go, context.go, heartbeat.go, skills.go, compaction.go, workspace_signal_e2e_test.go（改名自 workspace_notice_e2e_test.go）}`
* `internal/session/{manager.go, store_adapter.go, run_receipt_test.go}`
* `internal/store/{database.go, sandbox_leases.go, sandbox_leases_unhydrated_test.go}`
* `internal/sandbox/{lease.go, lease_pool_test.go, signal_carrier_test.go, exec_change_signal_test.go, e2b_live_unhydrated_handoff_test.go}`
* `internal/gateway/{sandbox_signals.go, deferred_turns.go, deferred_turns_test.go, reload.go, gateway.go, sandbox_pool_lease_test.go}`
* `internal/mcp/{client.go, manager.go, stdio.go, notification_test.go}`
* `internal/setup/{handlers.go, skill_entry_mask_test.go}`
* `internal/agent/tools/{write_through_signal_test.go, store_only_signal_test.go, workspace_signal_test.go（改名自 workspace_notice_test.go）, bash_session_test.go}`

**T3（7）**：`internal/sandbox/workspace_sync.go`（删除）、`internal/agent/tools/exec.go`、
`internal/config/{config.go, env.go}`、`internal/setup/{handlers_admin.go, handlers_agent_channels.go}`、`web/src/lib/api.ts`

**T4（3）**：`scripts/{workspace_revert_audit.py, workspace_project_chat_duplicate_cleanup.py}`、`docs/sandbox-scope-leak.md`

### 11.3 跨组文件（10 个，需要按 hunk 拆）

| 文件 | T1 部分 | T2 部分 | T3 部分 |
|------|---------|---------|---------|
| `internal/sandbox/lifecycle.go` | `syncSnapshot`、`statsFor`、`sameVersion`、`equalToStore`、`delta.changed`、`syncStoreScope`、`WriteThrough`、`mirrorToProjectPeers`、`lazyExecutor.WriteThroughScope`、`liveInstance`、`RemoveLiveWorkspaceFile` | `SetSignalStore`、`parkSignal`、`takeSignals`、`signalsFor`、`StoreOnlyLine`、`movedLine`、`takeReplacedNote`、`getInner` 的未水合段、`lazyExecutor.execOnce` 的渲染段 | — |
| `internal/sandbox/e2b_executor.go` | `LiveExecutor`、`LiveProjectExecutors`、结构体字段 | `publishUnhydrated`、`adoptFromLease`、`reconcileLocalLease` | — |
| `internal/sandbox/lifecycle_test.go` | `fakeExecutor.commands` 记录器（§10 的 10-4 用它） | `snapshottingExecutor` 的单副本一致性说明、`fakeLeaseStore` 的未水合字段 | — |
| `internal/agent/tools/file.go` | 5 处 `StoreScope` 传递 | `writeThroughSignal`、`storeOnlySignal`、`list_dir` 的渲染 | — |
| `internal/agent/tools/apply_patch.go` | 6 个 store 触点的键解析 | 沙箱模式的镜像 + 信号 | — |
| `internal/agent/tools/registry.go` | `scopeSessionID`、`codingRootScope` 删除 | `ToolNames`、`declareUnhydratedWorkspace` / `withWorkspaceSignal` | `sandboxSessionID` 字段（死码） |
| `internal/agent/loop.go` | `bindSession` 里的折叠接线 | 三条回合入口的采样 + `refreshSkills` 的未水合段 | — |
| `internal/gateway/userspace.go` | — | `SetSignalStore` 接线（两个建池入口） | `BoxliteClientID` 接线删除 |
| `internal/setup/handlers_agents.go` | `handleAgentFileDelete` + 窄接口 | — | onboard 的 boxlite 字段 |
| `internal/sandbox/boxlite_executor.go` | `LiveExecutorPool` 实现（no-op 语义） | — | `clientID` 整条链 |

### 11.4 两种做法，选一个

| 做法 | 提交数 | 代价 |
|------|--------|------|
| **A（推荐）** | 4 笔（上表） | 10 个文件要按 hunk 拆；每笔都必须**能编译、能跑绿**；T1 那一笔里不能出现 T2 的测试（它们引用 T2 的符号） |
| **B（省事）** | 3 笔：T1+T2 并成一笔"修复主体"，再加 T3、T4 | 不需要拆 hunk；代价是这一笔 60+ 文件，review 单位大、回滚粒度粗（想只回滚"信号"做不到） |

### 11.5 每笔的验证门槛

| 提交 | 门槛 |
|------|------|
| T1 | `go build ./... && go vet ./internal/...` + `go test ./internal/{sandbox,agent/tools,workspace,runtime,setup}/ -count=1` + 真机 `-run 'TestE2BLive(Repro\|PanelDeleteSticks\|ProjectWriteReachesSiblingContainer\|OnePathIsOneKey\|ProjectSessionKeepsOneTree\|SyncReadsNoBodiesForStampablePaths)'` |
| T2 | 离线全量 `go test ./internal/... -count=1` + 真机全套 `-run TestE2BLive` |
| T3 | 离线全量（删除类改动：引用计数 + 编译 + 测试） |
| T4 | `python3 scripts/workspace_project_chat_duplicate_cleanup.py --selftest` |

**外部依赖（已在 T0 处理）**：离线全量套件里原有**两处没有闸门的联网测试**，都让"全绿"不可靠：

| 包 | 测试 | 之前 | 现在 |
|----|------|------|------|
| `internal/skills` | `TestInstallFromSkillsSh_RepoField`（下真 tarball）+ 4 条 `TestDiagnose_*` 诊断 | 156 秒、1 失败（`probe HTTP 404`） | **0.55 秒全绿**（闸门后） |
| `internal/setup` | `TestRunInstall_GitHub_ReturnsRepoInResult`（真装一个 skill） | 122 秒、偶发失败（`runInstall github: …404`） | **7.8 秒全绿**（闸门后） |

**闸门做法**：两个包各有一个 `requireLiveNet(t)` helper（`internal/skills/live_net_test.go`、
`internal/setup/live_net_test.go`），要求显式 `FASTAGENT_NET_LIVE=1` —— 与真机 E2B 套件的
`FASTAGENT_E2B_LIVE=1` 同一约定，skip 消息里写清要设哪个变量。**实测**：带 `FASTAGENT_NET_LIVE=1`
时两条安装测试仍能复现上游 404，即失败被**显式化**（要跑就看得见），而不是被藏起来。

### 11.6 形式化文档已入库（2026-09-18 已落）

**问题**：本册与 00–10 的中英文档原先在 `/Users/reina/Project/tokenaissance/docs/fastagent/`，而那个目录
**不属于任何 git 仓库**（工作区根下挂着几十个独立仓库）—— 文档没有任何版本记录。这既解释了本次会话早先
"交付物被回退"只能靠数字节发现，也让"代码改动与文档更新同笔提交"（GEB 同构）在物理上做不到。

**已落**：整套文档进入本仓库，两个语言仍是兄弟目录：

* `docs/文件系统形式化证明/`（中文，origin）
* `docs/fs-formal-proof/`（英文 1:1 译本）

搬动时改了链接的**深度**，因为新位置离仓库根只有两级：指向代码的 `../../../fastagent/...` 改成
`../../internal/...`（共 **126 处**），指向仓库内其它文档的 `../../../fastagent/docs/X` 改成 `../X`。
文档之间的相互链接（`../fs-formal-proof/...`）不变，因为它们本来就是兄弟目录。

**校验**：搬完跑链接检查 —— `docs/文件系统形式化证明/*.md` 与 `docs/fs-formal-proof/*.md` 的相对链接
**0 条死链**；13 个引用旧路径的代码注释/脚本同步改过（`rg 'docs/fastagent/文件系统形式化证明'` 零命中）。

**因此从这一笔起，规则是**：改代码的那一笔里必须有对应的文档改动（或反之），否则视为未完成；
发布前用 §11.5 的门槛逐笔核对。

## 11.7 实际落地（as-shipped，2026-09-18）

分支 `ship/incident-2026-09-17`（从 `fastagent` 的 `16a7532` 起），**七笔**：

| 提交 | SHA | 内容 | 该笔自身的验证 |
|------|-----|------|---------------|
| **T0** | `e278e6b` | 给两个包的联网测试加闸门（`FASTAGENT_NET_LIVE=1`） | build ✓；`internal/skills` 0.59s、`internal/setup` 7.38s 全绿（此前 156s+失败 / 122s+偶发失败） |
| **T1** | `8fde5da` | 对账（size+mtime → 字节比较 → `BLOCKED` 零迁移）、删 pod 本地基线、交付戳、作用域不变式（#1–#4、#21–#27、#31） | build ✓；`internal/{sandbox,agent/tools,workspace,runtime,setup}` 全绿；真机子集 ✓（sandbox 110.1s） |
| **T2** | `39da597` | 信号与投递（#5–#20）：穿透泛化、`CompareResult` 四态、store-only、G19、G3 载体、统一环境信号出口、G11/G12/G14/G16 | 离线 **34/34 包**；真机全套 ✓（sandbox 169.5s / tools 50.0s） |
| **T3** | `175c8d6` | 死码与配置清理（#29） | build ✓；离线 34/34；pre-commit 的 eslint 通过 |
| **T4** | `260ec1e` | 离线脚本 + `docs/sandbox-scope-leak.md`（#30） | `--selftest` 通过（三类副本判定符合预期） |
| **T5** | `a087ec4` | 形式化文档入库（26 文件） | 链接检查 0 死链（见 §11.6） |
| **T6** | 本笔 | 把本节（实际切分）写进册子，使清单与落地一致 | 文档改动，无代码影响 |

**最终状态校验（T5 之后的 HEAD 上）**：`git status` 干净；对 BK3 基线 `verify.sh <repo> HEAD` →
`verify OK: 112 files match`（拆分没丢任何文件）；离线全量 34/34；真机全套 sandbox 179.0s、
agent/tools 42.9s 全绿。**当时的**线上是 `16a7532`。（2026-09-19 实测：dev 已部署 `8984c99`——
这七笔都在里面；production 仍是 `a24c0a8`。）

### 11.7.1 与 §11.3 计划的差异（六处，每处都写在对应 commit message 里）

| 文件 | 计划 | 实际 | 原因（实测） |
|------|------|------|-------------|
| `internal/sandbox/lifecycle.go` | T1/T2 按函数拆 | **整份随 T1** | 它的对账、写穿透与**信号端口**挤在同一个 diff 区段；部分切分三次都编不过（`errors` 未使用、`strconv`/`movedLine`/`takeReplacedNote` 未定义）。所以信号端口作为**定义**落在 T1，**接线与渲染**在 T2（`gateway/userspace.go` 的 `SetSignalStore` 等） |
| `internal/sandbox/boxlite_executor.go` | 全部 T3（`clientID` 链） | **随 T2** | 它唯一的调用方 `gateway/userspace.go`（`NewBoxliteExecutorPool`，去掉 `clientID` 参数）在 T2；拆开编不过 |
| `internal/agent/tools/apply_patch_live_scope_e2e_test.go` | T1 | **随 T2** | 其中的 `TestE2BLiveOnePathIsOneKey` 断言沙箱**镜像**收到了写，而镜像是 T2 的工具层（T1 里它真的红：`the write-through did not reach the sandbox`）。G18 的三条离线单测仍在 T1 |
| `internal/setup/handlers_agents.go` | 拆（删除处理 T1 / boxlite 字段 T3） | **整份随 T1** | 它的 T3 部分实际不在这个文件（在 `handlers_admin.go`/`handlers_agent_channels.go`，已在 T3） |
| `internal/agent/tools/registry.go` 的 `sandboxSessionID` | T3（死码） | **随 T1** | 它与 T1 删掉的 `codingRootScope` 是同一个字段 hunk |
| `internal/gateway/userspace.go` 的 boxlite 接线 | T3 | **随 T2** | 与同文件的 `SetSignalStore` 接线相邻，且必须与 `boxlite_executor.go` 的签名一起走 |

除这六处，§11.2 的整文件归属与 §11.3 的其余函数级归属与实际完全一致。

## 12. 跨副本轮次完整性（2026-09-19 采纳的设计，**已于 2026-09-19/20 实现**）

> 本节与上面所有节的区别：**它们是设计，不是改动**。经用户决定"先把设计做完，再一次性实现"
> （[../session-turn-integrity.md](../session-turn-integrity.md) 的 Adopted 节）。
> 所以每一行的"上线"列都是 ❌ —— 本册这一列的含义是**未部署到 production**（§0 的图例），
> 不是"待批准"。当时登记在这里，是为了让"设计 → witness"的缺口可见，而不是预支完成度。
> 形式化依据见 [12](./12-lease-formal-design.md)。

> **状态更正（2026-09-20）：它已经实现了。** 上面那段写的时候为真，现在是假的 —— A1-a…A1-e、
> 按显式签名落地的围栏（`session.WriteScope` + 内层自有的 `ErrSessionFenceLost`）、A2 的投影词汇、
> 取消契约（`chat/cancel` 返回 `{canceled, wasRunning, isRunning}`，`409 already_started` 已退役）、
> 取消路径的端到端测试（`TestCancelledTurnStopsAndSignalsOnce` —— 对端的印章是在租约端口上**模拟**的，
> 不是双副本那一跑），以及 cloud 那一半（X7：停止入口读事实并调服务端）
> 都在 2026-09-19/20 落地，各自带见证。**其中两项已在 2026-09-21 关闭，而且当初"做不了"的理由本身是错的**：
> ① 工具路径删除的 **E2B 真机证明**（[10 §4](./10-harness-state-audit.md) G7b）**不需要任何存下来的密钥**——
> 密钥按运行现场从集群 Secret `fastagent-secrets` 取出、只存在于那一次命令的环境里（[10 §7](./10-harness-state-audit.md)）：
> 通过 23 s、去掉镜像调用 31 s 变红（2026-09-21 复现，姊妹对照 `TestE2BLivePanelDeleteSticks` 65 s 绿）；
> ② 客户端那半的 **chat e2e**：:3000 上跑**当前检出** + fixture 账号，`e2e/tests/fastagent` 全量 130 例
> **125 通过 / 0 红 / 5 skipped**（7.0 分钟；5 个 skip 都有声明理由：`app-shell-ssr-seed.spec.ts:22` 要
> `E2E_VERIFY_EMAIL/PASSWORD`，`app-uiux-sweep.spec.ts:649`/`:686` 是只在移动端存在的抽屉形态）。
> 此前同一套的 **31 红是环境造的**，不能读成产品状态：没有凭据（空 storageState 让每个需要登录的用例都读到
> 匿名视图），且开发服务器在 :3100 而 `.env.local` 把 `NEXT_PUBLIC_APP_URL`/`AUTH_URL` 钉在 :3000（浏览器端
> auth 请求跨域 ⇒ CORS 报错被"renders cleanly"记成红）；另有一个 cwd 指向**已删除**工作树的旧 dev server
> （`/` 与 `/app` 都 404），它会被误读成"应用丢了很多控件"。
> **本节最后一件挂空的事 —— #32 后面的双副本那一跑 —— 已于 2026-09-21 关闭。** 这里写的缺口是真的，现在没了：
> 租约适配器没有导出构造器，所以骨架只能造出两个**服务器**、造不出两个副本。`gateway.NewStoreSessionLease`
> 补上了它，`internal/setup/cross_replica_turn_e2e_test.go` 跑的就是这个形状 —— 一个 store、两个除数据库外
> 什么都不共享的 server：第二次 `chat/stream` POST 排在同一会话正在跑的回合后面（并报出持有者与它的 ETA），
> 而在**另一个副本**上 `POST /api/chat/cancel` 会让持有者在下一个边界停下：恰好一条停止 σ，租约行被释放。
> 三处反证都实跑（见 #32 的真机 e2e 格）。它还顺手抓到一个没人去找的缺陷：边界 `break` 会掉进"强制终局交付"，
> 于是一个用户已经停掉的回合又花了一次模型调用、并给回包盖上 `iterationCapReached`（#41）。
>
> **#42（2026-09-21）**：扇出的心跳此前不报名字，面板只能猜它属于哪一行工具——而那个猜法（"第一个还没有结果的
> `delegate_task`"）除第一次调用之外全是错的：退场心跳先于它自己的调用返回、且不带结果，于是"第一个还没有
> result 的"在扇出里始终指着第一条。现在每条
> 心跳都报出自己属于哪次调用，客户端只在被点名的那一行画它。
>
> **真机 E2B 重跑（2026-09-22）**：`TestE2BLive*` 全套打到真 E2B 沙箱，密钥按运行现场从集群
> Secret 取出（`FASTAGENT_E2B_LIVE=1`、`FASTAGENT_E2B_TEMPLATE=fastclaw-sandbox`）——
> **12/12 通过，0 跳过**（`./internal/sandbox` 295 s：repro、盖章、宽松布局、拒绝沙箱改 store 路径、
> write-through、面板删除的**两半**、兄弟容器、同步不读正文、未水合事实跨 pod 交接；
> `./internal/agent/tools` 95 s：工具路径删除、一路径一键、一个项目会话一棵树）。删除那两半正是重点：
> "只删 store ⇒ 下一次同步把删除撤销"和"store + 活沙箱两半 ⇒ 站得住"是**两条**用例，不是一条断言加脚注。
>
> **第 42 行的另一半（2026-09-22）**：给那一行点出名字只是"一个事实一种线上形状"的一半；另一半是客户端把
> 退场事实也丢掉了。每次子代理**退场**的 `subagent_progress` 本来就带 `phase:"done"` **和**那次调用的 id，
> 客户端两者都丢（`setSubagentProgress(null)`）。而退场事实先于这次调用自己的结果到达，所以
> 在一次调用退场与它的结果落地之间，那一行只有两种说法，而两种都是假的：对一条**没有东西在等**的调用说
> `Queued (waiting on prior sub-agent)…`；以及在一次退场与下一次心跳之间的缝里、由"位置兜底"接管的
> `Executing...`，而那条调用其实**已经退场**。现在 cloud 把退场过的 id 记下来（`subagentFinishedIds`，
> 回合结束时清空），那一行读"子代理已完成 —— 结果随本回合一起返回"。**没有新的线上形状**：这个事实本来就
> 在发，是取用侧把它丢了。见证：cloud `subagent-finished-ids.test.tsx`（4，记账）、
> `message-list-tool-status.test.tsx`（+3，行的措辞）、`chat-streaming-parity.test.tsx`（+1，事实穿过真页面）。
> 反证均已实跑：不记 id ⇒ 3 红；行忽略"已完成" ⇒ 2 红；位置兜底不跳过已退场的调用 ⇒ 1 红；页面不往下传 ⇒ 1 红。

> **第 47 行（2026-09-22）**：B7–B9 覆盖的那三个写工具被**注册了两次**——`registerFile`（没有 executor）
> 与 `registerSandboxedFile`（`SetExecutor`，云上带 E2B 沙箱的回合跑的就是它）——而两者并不共用同一个函数体。
> 为 B7–B9 写的那批见证只搭了第一个，所以它们一直绿着，而生产走的那条分支仍在调用裸 `Put`：subject 写着
> "conditional writes for every tool→store writer" 的那个提交（`3f3c02e`）只给宿主分支加了守卫，把沙箱分支里的
> `write_file` 和 `apply_patch` 留成了盲写。**同一分支里已被守卫的 `edit_file`** 正是让这个缺口读起来像"基本做完了"
> 的原因——也正因此，本行的见证改为先断言"**根本没有读过版本**"（"a peer's write cannot be noticed here at all"），
> 而不是断言某个下游症状。同一轮复核还发现守卫的第二个缺陷，也由同一个提交引入：`putGuarded` 对**非**版本冲突的
> 错误一律 `return nil`，于是 store 答 500 时工具照样报 `Written 3 bytes to notes.md`。这正是这一族要消灭的静默丢失
> ——它替换掉的那个裸 `Put` 本来是会上报的。

> **第 48 行（2026-09-22）**：G15 被当作「协议合规、得先真机验证」挂了一个星期。而它点名的那次真机验证，
> 把问题答向了另一边：用我们的 client 驱动**参考实现** SDK server（`npx @modelcontextprotocol/server-everything`，
> 协议 2024-11-05），`tools/list` 在**不发** `notifications/initialized` 时返回 **12 个工具**、**发了**返回 **13 个**
> ——那个 server 从自己的 `oninitialized` 钩子里注册 `simulate-research-query`。所以缺的这条通知不是「少一句客套」，
> 而是 client 对一个 server 还没建完的清单下了断言，而下游每个信号（包括每回合的工具集增量）都会一致地描述
> 这份——本来不存在的——工具集。现在两个传输都在 initialize 之后发送它；server 定下来的修订被**读进来**而不是
> 假设（此前声明被钉在 `2024-11-05`，却在讲 Streamable HTTP），并且在确实定义了该头的修订上回带
> `MCP-Protocol-Version`。同一趟还裁决了边界 3：暂时以推送通道为准，不采用每回合 re-list——那条限制从此是
> **已裁决**，而不是「开放」。

> **第 49 行（2026-09-22）**：`privacy.piiScrubbing.enabled` 是一个**在生产实际走的那条路上没有任何读者**的开关，而它的规则
> 只落在**十一处** provider 调用点里的**三处**。那个标志唯一的赋值在 `NewAgentWithFullCfg`——一个**零调用者**的构造函数
> （Manager 构建的每个生产 agent 都走 `newAgentWithActor`）——所以量出来的结果很直白：行开着时
> `cfg.Privacy.PIIScrubbing.Enabled` 读出 `true`，而网关真正构建出来的 agent `piiScrubEnabled=false`。把标志在调用点上强行打开，
> 才看得见底下还剩什么：`HandleMessage` 会脱敏，而 `HandleMessageStream`（OpenAI 兼容的 `/v1` + `stream: true` 那一回合）
> 把 `user|my card is 4111 1111 1111 1111 and mail casey.rivera@example.com` **原样**交给了 provider，`delegate_task`
> 自己的 ReAct 循环同样如此。压实的摘要器和自动记忆抽取器直接拿 `a.provider`，也从来不在规则的覆盖范围里；而字段表还少两项：
> `ScrubMessages` 改写了 `Content` 与 `ContentParts` 里的文本，却没有改写 `Thinking`（它以 `reasoning_content` 出去）
> 和 `ToolCalls[].Function.Arguments`（会话后续每一次请求都会重放）。本次改动把规则放到**唯一**一个 provider 能进入 agent 的地方
> ——`Agent.setProvider`，`Agent.provider` 的唯一写入者，构造与两条热重载路径都走它——并把字段表从"结构体的"改成"线上真发出去的"。
> **五条反证全部实跑**：`setProvider` 不包 ⇒ 6 红，五个入口各一条、加热重载那条；热重载绕过 `setProvider` ⇒ **只有**重载见证变红；
> 装饰器只脱敏 `Chat` 不脱敏 `ChatStream` ⇒ web 与 `/v1` 两条变红；把 `agent.WithPrivacy(cfg.Privacy)` 从网关的选项表里删掉
> ⇒ 网关见证变红；字段表缩回去 ⇒ `Thinking` / `Arguments` 两条断言变红。**本次没有关掉、且写明白的限制**：
> `Message.RawAssistant` 为了前缀逐字节一致（prompt cache、DeepSeek thinking 模式）必须原样重放，因此**模型自己上一轮的回复里**
> 若回显了 PII，那份回显仍然会出去。这是本开关与一条既有不变式之间的冲突，需要你来裁：要么重放一致性优先（即上面这条限制），
> 要么被脱敏的回合放弃逐字节重放。这里把它**登记为限制**，而不是悄悄吞掉。

| # | 改动点 | 形式（义务） | 代码锚点 | 计划 UT（含反证） | 真机 e2e | 上线 |
|---|--------|-------------|---------|------------------|---------|------|
| 32 | **A1** 跨副本轮次租约：store 新增 `session_turns`（键 = `sessions` 主键），`Acquire/Renew/Release/Get` CAS + 单调令牌；准入失败 ⇒ 既有 `queued` 事件 | F1（L1/L3/L5）+ F2（L6） | **已落地（工作区）**：`internal/store/database.go`（DDL + 四方法）、`internal/store/session_lease_test.go`（2 条）、`internal/agent/sessionlease.go`（端口 + `Turn` + `NopSessionLease`）、`internal/gateway/sessionlease.go`（适配器，持有者由适配器生成）。**已全部落地（工作区）**：两个准入入口（`loop.go` 的 `HandleMessage`/`HandleMessageStream`：先租约、后本地 FIFO 槽、最后释放租约）、`admission.go` 的 IfIdle 改读 `Live`、续租/释放/丢失信号（`internal/agent/turnlease.go`）、装配（`manager.go` 的 `WithSessionLease` + `gateway/userspace.go` 注入 `storeSessionLease`） | UT 已绿：store 侧 3 条（8 并发唯一赢家、过期移交、陈旧令牌不能续租/释放）+ agent 侧 3 条（等待并上报持有者与 ETA、自动回合延迟不入队、被接管即停并提示）。**两条反证已实跑**：把租约从准入拿掉 ⇒ `no queued event`；把 `Live` 从 IfIdle 拿掉 ⇒ `RunTurn error = <nil>` | cancel 路径已在**回合循环的边界**上端到端覆盖（`TestCancelledTurnStopsAndSignalsOnce`），反证已实跑 —— 但**那一跑**的对端印章是在**租约端口上模拟**的（假租约记录下来），所以它本身不是双副本那一跑。**双副本那一跑已于 2026-09-21 落地**（`internal/setup/cross_replica_turn_e2e_test.go`）：一个 store、两个 hub 与两个 manager 各自独立的 `Server`，两个 agent 都接在 store 支撑的租约上 —— 正是这层装配让一个 server 成为**副本**，所以它属于本改动（`gateway.NewStoreSessionLease`；适配器本身仍未导出）。① 准入：副本 A 停在工具里，第二个 `chat/stream` POST 打到 B ⇒ B 报出 `queued`（含它等的持有者与该 possession 的 ETA），且模型轮数保持 1，直到 A 放手。② 取消：在**没跑这个回合**的副本上 `POST /api/chat/cancel` ⇒ 200 `{canceled:true, wasRunning:true}`，持有者在下一个迭代边界停下，流里恰好一条 `stopped at your request`，租约行被释放。**三处反证都实跑**：拿掉 `WithSessionLease` ⇒ B 自己开跑（`call_slow_probe_2`，一条 queued 都没有），对端取消答 `canceled:false`；拿掉 `lease.Cancelled` 边界 ⇒ 停止 σ 计数为 0；恢复掉进终局交付 ⇒ 假的上限徽章回来了（#41）。两个副本**只**共享数据库，所以没写进去的东西对端一概看不见。这一跑仍未覆盖的：两个独立进程/pod（副本之间的传输是 hub/subscribe 那批测试的主题，不是这一条） | ❌ |
| 33 | **A1 围栏**：`AppendSessionMessage` / `SaveSession` 在**有围栏时**带 `EXISTS(session_turns …)` 谓词；令牌对不上 ⇒ 拒绝写入（`ErrSessionFenceLost`） | F1（L4a：围栏在**资源侧**、与写入同一原子步骤） | **已落地（工作区）**：`internal/store/sessionfence.go`（`SessionFence` + ctx 盖章 + `ErrSessionFenceLost`）、`database.go` 两条写语句（两个方言；`SaveSession` 加在 `ON CONFLICT … DO UPDATE … WHERE`，`AppendSessionMessage` 加在 `HAVING`）、`internal/session/manager.go`（`TurnFence` + `Set/ClearTurnFence`，由 `ctx()` 盖章）。**按 review 决定的形态**：`session.SessionStore` 的两个写方法加 `*session.TurnFence` 参数（端口只命名自己那层的类型），适配器翻译成 `store.SessionFence`；store 侧保留原方法不动、新增 `SaveSessionFenced`/`AppendSessionMessageFenced`（与 A3 计划中的 `PutIfVersion` 同形——带前置条件的形式单独成一个方法）。未用 ctx 隐式传递 | 已绿：`TestSessionFenceRefusesASupersededWriter`（活令牌两侧都落、接管后陈旧令牌两侧都返回 `ErrSessionFenceLost`、新持有者照常写入、无 fence 路径不变）；**反证：把 `EXISTS` 去掉 ⇒ 陈旧写者的断言不再失败** | 同 #32 场景下，被接管的回合不得再落一行 | ❌ |
| 34 | **A2** 投影不再断言"被打断"：三形态文案（持有者已死 / 持有者还活着 / 无事实），术语常量与"没有证据不许说 interrupted"的测试 | F2（O1 说真话） | **第 1 步已落地（工作区）**：`internal/provider/provider.go`（三句词表 + `SyntheticToolPads`）、`internal/agent/normalize.go`（当前一律用"无事实"句）。**两步都已落地（工作区）**：`internal/agent/turnlease.go` 的 `openCallAnswer`（租约读不到 ⇒ 无事实；对端持有 ⇒ 仍在运行；无其他持有者 ⇒ interrupted 可证），两个投影点改调 `normalizeForPromptWith`。| 已绿：`TestProjectionDoesNotClaimInterruptedWithoutEvidence`、`TestOpenCallAnswerFollowsTheLeaseFacts`（3 个子例）、3 条既有测试改为查整表；**两条反证已实跑**：把 `StoppedToolResult` 放回无条件路径 ⇒ 对端持有/读不到两个子例都红 | 无（纯投影，见 §7 的判据） | ❌ |
| 35 | **A3** 工具写 store 的覆盖保护 —— **已决定走 B 族（版本条件写）**，改动清单 B1–B11 见 [../session-turn-integrity.md](../session-turn-integrity.md) A3.1：`ObjectInfo.Version`（不透明令牌）、`PutIfVersion` + `ErrVersionConflict`、S3 用 ETag（`SetMatchETag`，minio-go v7.3.0 已核）、LocalFS 用 `size:mtime_ns` 并**声明为尽力而为**、每后端强度表、7 个写者接入 | F1（G24：`tool→store` 那条路的前置条件）+ F4 的首个声明片段（**B1–B5 已落地**：版本令牌 + LocalFS 尽力而为的条件写 + S3 的 ETag 条件 PUT + `Metered` 透传；UT `TestLocalFSPutIfVersionRefusesAStaleExpectation`，反证已实跑。**B6–B11a 已落地**（强度表 `01 §2.x`；三个文件工具、附件、技能发布；面板上传冲突返回 409 + 当前版本）；**B11-b 两侧均已落地** —— 服务端半接受可选 `expectedVersion`（⇒ 按版本替换；缺失 ⇒ 仅创建），cloud 半改为**一次请求一个文件**并提供三答案（保留两份 / 替换 / 取消）与自动改名。**09-21 补齐"投递点见证"**（`tools/write_guarded_peer_test.go`、`workspace/s3_version_test.go`、`agent/attachments_store_posture_test.go`、`skills/objectstore_posture_test.go`、cloud `attachment-conflict-flow.test.tsx`），每条反证均已实跑；附件姿态**已改**：由"拒绝 + 告警"改为"改名保留两份"，因为旧路会把一个**并未写入**的文件名放进 `[Attached: …]` 面包屑） | `internal/workspace/{workspace,s3,localfs,metering}.go`、`internal/agent/tools/{file,apply_patch}.go`、`agent/attachments.go`、`skills/objectstore.go`、`setup/handlers_agents.go`；cloud `src/features/chat/{upload-attachments,attachment-conflicts}.ts` + `upload-conflict-dialog.tsx` | 每个写者**两条**见证：规则（对端在读写之间写入 ⇒ 拒绝 + σ）与投递点（事实真正搭上的那个调用点）。**反证：去掉条件谓词 ⇒ 对端版本被静默覆盖** —— 已逐个实跑：`file.go:592`（2 红）、`file.go:702`（1）、`apply_patch.go:549`（1）、S3 的 `SetMatchETag` 分支（2）、附件 `PutIfVersion`→`Put`（3）、保留两份的循环（2）、技能发布的"先读再条件写"（1），以及 cloud 半的 4 处 | 面板上传 vs 回合并发（B11） | ❌ 未上线（代码已落地工作区；本册这一列＝未部署，见 §0） |
| 36 | **A4** 客户端不再从自己的 socket 推断服务端状态：三元（interrupted / unknown / running）+ `turnActive{holder, epoch, expiresAt}` + `queued{holder, ETA}` + `subagent_progress.id` | F3（取用侧不得回答只有产生侧能回答的问题） | **服务端已落地（工作区）**：`internal/setup/handlers.go` 的 `handleChatSubscribe`（`event: turn_active`）与 `handleChatHistory`（`turnActive` 字段）、`internal/agent/turnlease.go` 的 `queued{holder, expires_at}`。**2026-09-19/20 起 cloud 侧也已落地**：四态（running / interrupted / unknown / idle）由 `selectTurnState` 驱动加载气泡、工具行**与**停止入口（`chat-composer.tsx:94` 走 `turnIsRunning({turnState, locallyStreaming: streaming})`——本地 `streaming` 只是这个视图自己的正面证据，"谁在跑"由服务端事实回答；Stop 调 `chat/cancel`）；消息行改为接收 `turnState` 属性、不再由 `msg.streaming` 自行推导；队列 σ 的时效被遵守（`isQueuedTurnLive`，`use-stream-pipeline.ts:161`），且线上两处都写 `expiresAt`（上面的 `expires_at` 已退役）。**2026-09-21 已落地（见第 42 行）**：`subagent_progress.id`——每条心跳都报出自己属于哪次调用，只有它点名的那一行才画它 | 已绿：三段序列在 `chat-composer-stop.test.tsx`，行级用例在 `message-list-tool-status.test.tsx`，四态映射在 `turn-state.test.ts`；**反证真跑（2026-09-21）：把“行”还原成只看气泡的措辞 ⇒ 2 条红（`message-list-tool-status.test.tsx:145` 与 `:155`）——被对端持有的回合同样重新渲染成 *Interrupted*** | 中途断开 SSE、再发一条 ⇒ UI 显示"仍在运行/已排队" | ❌ |
| 37 | **G25 修法**：沙箱租约的抢占分支 `epoch = epoch + 1`（令牌永不回到 1） | F1（L4c：令牌逐次唯一） | **✅ 已落地（工作区）**：`internal/store/sandbox_leases.go:69-80` | 已绿：`TestSandboxLeaseEpochNeverResetsAcrossTakeover`（严格递增 + 老令牌释放被拒 + 活行仍在）；**反证已实跑**：改回 `epoch = 1` ⇒ `gen1=1 gen2=1` 失败 | 无需真机（纯 store 语义） | ❌ |
| 38 | **O7**：客户端自己那份回答里带上 pod 发出的两条事实 —— 拒绝的稳定 **code**，以及"**已发布**的 skill 在 MCP 这一侧读起来不一样"的 **warning**（`_meta["com.tokenaissance/skills/warnings"]`，外加 `list_skills` 文本里一段）。出口自己那几条拒绝也给码：`no_name` / `name_mismatch` / `no_files` 刻意复用 pod 的拼法（一个事实一套词汇），扩展自己的上限则用 `egress_*` | **O7**（新增，[08 §10.7](./08-state-observability-principle.md)）+ 线上形状那半属 C1 | **✅ 已落地（工作区）**：tokenaissance-cloud `src/shared/services/mcp-oauth-server/{catalog,skills-service,skills-policy,policy,tools-service,index}.ts` | 已绿：`skills-list-chain.test.ts`（**投递点**：pod 应答 → 适配器 → 用例 → `_meta`），加上 `catalog.test.ts`、`skills-service.test.ts`、`policy.test.ts`、`tools-service.test.ts`；**反证已实跑**：还原 `{path, reason}` 重建 ⇒ 2 条红，返回空 warnings ⇒ 2 条红，去掉 `_meta` 键 ⇒ 3 条红 | 无（那一面目前只有清单里的实测覆盖；MCP 一致性套件仍无 SEP-2640 场景 —— 清单 E1） | ❌ 未部署（落在工作区） |
| 39 | **O1**：pod 的 `{baseDir}` 诊断改为在技能的 **manifest** 带 token 时触发，而不是"任何一个文件带 token"。这条 warning 携带的句子陈述的是**两个读者之间的差异**，而只有 `SKILL.md` 会被替换——随包脚本没有任何人替换，agent 同样读到字面量，"客户端读到的不一样"对它就是假的。`Files` 仍是**测量**（列出每个携带者），这才让剩下那句"in every file listed"可核对 | **O1**（只说真话）；在 O7 自己的实例投递面被扩大之后重读它时发现（[08 §10.5](./08-state-observability-principle.md) D-4） | **✅ 已落地（工作区）**：fastagent `internal/skills/catalog.go`（`skillManifestName` + `BuildCatalog` 的触发条件与句子）、`internal/skills/catalog_scan.go` | 已绿：`TestBaseDirTokenInABundledFileIsNotAReaderDifference`、`TestBaseDirWarningListsEveryCarrierNotOnlyTheManifest`、`TestCatalogHandlerCarriesTheCodesAndTheWarnings`（**wire**）、`TestScanAndCatalogReportTheBaseDirTokenAsAWarning`（句子点名 `SKILL.md`）；**反证已实跑**：触发条件改回"任何携带者" ⇒ 2 条红（规则 + wire），句子改回"replaced when this agent loads the skill" ⇒ 1 条红 | 无需真机（pod 自己那份应答；两个消费面由第 38 行的链式测试覆盖） | ❌ 未部署 |
| 40 | **O6 的第二个面**：面板与 MCP 应答都在报告"用户哪些 skill 能到达客户端"。pod 只能答它看得见的那一半，所以面板——通过原始透传读 pod 的目录——报不出由 pod 没有的规则做出的拒绝（扩展自己的限制：没有 `SKILL.md`、超过 512 个文件、超过 16 MiB、digest 不是 sha256）。一个客户端永远装不下的 skill，在那里仍然列在"已发布"里，没有任何地方说明原因——而组件自己的注释承诺的正是这条差异 | **O6**（[08 §10.3](./08-state-observability-principle.md)）+ **D-5**（[08 §10.5](./08-state-observability-principle.md)）：两个面读的是不同的**生产者**，这正是两边测试都绿的原因 | **✅ 已落地（工作区）**：cloud `src/shared/services/mcp-oauth-server/skills-service.ts`（`skillCatalogView`，即 `listSkills` 已经做的那份分区的第二个投影）、`src/routes/api/fastagent/$.ts`（`agent-skills/catalog` 不再是透传）、`src/shared/blocks/fastagent/skill-diagnostics.tsx` + `src/config/locale/messages/{en,zh}/fastagent/skills.json`（四个 `egress_*` 码有了句子，句子里写的就是规则实际执行的限额） | 绿：`skills-service.test.ts`（视图自己的两条规则 + 两个投影相等），cloud `fastagent-proxy-route.test.ts`（**投递点**：面板自己那条路径，pod 夹具里放一个 20 MiB 的 skill ⇒ `egress_too_large` 与 pod 自己的拒绝并列，`huge` 不再出现在 `skills`，读 pod 用的是调用者自己的 key）、`skill-diagnostics.test.tsx`（`buildSkillEntry` 能做出的每一条拒绝都渲染成句子而不是生产者的英文，且文案带着规则实际执行的数字）。**反证均已实跑**：去掉分支 ⇒ 3 红，第一条报 `['good','huge']`（透传把它列了出来）；面板的"码→句子"表少一条 ⇒ 1 红；句子里写 `1024` 而规则是 `512` ⇒ 1 红；把"列出的 skill"整份投影而不是可发布的那些 ⇒ 3 红 | 无（pod 的契约没动；清单里那次 MCP 手测也没动） | ❌ 未部署（已落地在工作区） |

| 41 | 以**边界决定**结束的回合——用户的停止请求，或对端接管了这个会话——不再掉进**强制终局交付**：循环记下理由后返回，于是这个回合既不多花一次模型调用，也不给自己的回包盖上 `iterationCapReached` | **O1**（只说真话，[08 §2.2](./08-state-observability-principle.md)）+ C1（一个事实一种线上形状：命名真实理由的 σ 在边界上就已经发出，上限徽章是同一件事的第二种、而且是假的形状） | **✅ 已落地（工作区）**：`internal/agent/loop.go` —— 四个边界断点上的 `stopReason`（两个 ReAct 循环里的 `lease.Lost()`/`sess.FenceLost()` 与 `lease.Cancelled()`），以及 `HandleMessage`（返回 `""`）与 `HandleMessageStream`（返回 `a.stringStream("")`）里强制终局交付之前的那道守卫 | UT 已绿：`TestCancelOnAnotherReplicaStopsTheRunningTurnE2E` 打的是一次 web `chat/stream` POST，也就是**非流式**那一份（`HandleMessage`）——恰好一条停止 σ、模型调用数仍为 1（按每一次咨询计——只看要工具的那几轮看不见掉进终局交付多出来的那一次）、流里没有 `iterationCapReached`、租约行已释放；同一份的单元见证 `TestCancelledTurnStopsAndSignalsOnce` 仍绿。**流式**那一份（`HandleMessageStream`，生产上只有 OpenAI 兼容的 `/v1` 挂载点会走到）有自己的见证 `TestCancelledTurnStreamsNothingAndSignalsOnce`：reader 产出空串、停止 σ 恰好一条、provider 轮数为 1——那条 E2E 从不调用它，所以"E2E 是绿的"从来不是关于它的证据。**反证已实跑，两份被区分开**：关掉非流式边界 ⇒ E2E 在 `cross_replica_cancel_e2e_test.go:76` 变红（停止 σ 计数 0）；关掉非流式那道强制终局交付守卫 ⇒ 在 `stopped at your request` 之后流里出现 `{"content":"done","metadata":{"iterationCapReached":true,"iterationCapValue":4}}`、`:85` 变红（多出来的那次模型调用被计到数了）、紧接着 `:88` 的上限徽章检查也红，而流式见证仍绿；关掉流式边界或它的守卫 ⇒ 流式见证变红（`streamed content = "done", want ""`），而 E2E 仍绿 | 无独立真机项（它就是聊天流本身的形状；观察到它的地方是 #32 的双副本那一跑） | ❌ 未上线（落在工作区） |

| 42 | 面板不再**猜**一条子代理心跳属于哪一行 `delegate_task`：每条 `subagent_progress` 都报出自己的调用（tool_use id），只有它点名的那一行才画它 | **C1**（一个事实一种线上形状）+ **D₃/F3**（产生侧回答；取用侧不得用一个本身就错的规则去回答——"第一个还没有结果的调用"） | **✅ 已落地（工作区）**：fastagent `internal/agent/tools/toolcall.go`（`ToolCallIDInputKey`、`WithToolCallID`/`ToolCallID`）、`internal/agent/sdkbridge.go`（把 id 盖到这次调用上——SDK 的 `ToolUseContext` 是整批共用的一枚指针，所以调用自己的参数是唯一的按调用通道——并在工具看到参数之前把它摘掉）、`internal/agent/subagent.go`（`subagentHeartbeat`，四处发射点）；cloud `use-stream-pipeline.ts`（`SubagentProgress.id` + 两个 setter）、`message-list.tsx`（`heartbeatOwnerId`：被点名的那一行拥有它；没有 id 的心跳沿用旧规则；点名在本组找不到活行时什么都不画，而不是把计数器画到别人的行上） | 绿：`TestToolCallIDReachesTheToolAndNotItsArguments`（id 到得了工具，保留键到不了它的参数）、`TestSubagentHeartbeatsNameTheirOwnCallE2E`（3 次调用 ⇒ 3 个名字、每次调用一块连续心跳、块序等于调用序）、`message-list-tool-status.test.tsx`（"lands the heartbeat on the row it names" 与 "draws no heartbeat on a group the named call is not in"）；**两条反证都真跑**：去掉参数注入 ⇒ 规则见证与 e2e 同时红；把客户端还原成"第一个未返回的调用" ⇒ cloud 两条新用例红 | 无自己的（这条是客户端读的那份接线；扇出本身是 `TestDelegateTaskFanOutTurnE2E`） | ❌ 未部署（在工作区） |

| 43 | HTTP MCP client 补齐它实际在用的那套传输的两条 MUST：每个请求都声明两种内容类型，且"以 SSE 流形态回来的应答"也能读 | **不属于 F1–F3**：协议合规，与 G15 同一族——client 会因为一种规范要求它必须支持的应答形态而丢掉整条连接 | **✅ 已落地**：fastagent `internal/mcp/http.go`——每个请求带 `acceptHeader`、`parseResponseBody`（JSON 对象**或** SSE 帧：`:` 注释、多行 `data:`、CRLF）、`isEventStream`、`sseDataFrames`；搭在流上的 server 主动消息现在走 `handleServerMessage`——与站着的流同一扇门（第 45 行） | 已绿：`TestHTTPClientAdvertisesBothContentTypes`、`TestHTTPClientReadsAnSSEReplyToARequest`（一条 keep-alive、一条不属于我们的消息、再是拆成两行 `data:` 且以 CRLF 结束的应答）、`TestHTTPClientDoesNotTakeAServerMessageForTheReply`、`TestMessagesOnAReplyStreamGoThroughTheSameDoor`；**三条反证真跑**：去掉 `Accept` 头 ⇒ 1 红；只按 JSON 解析 ⇒ 2 红；跳过应答流上的 server 消息 ⇒ 1 红 | 无：这套传输用 `httptest` 验证，不涉及真机 MCP server。**站着**的通道已在第 45 行落地；G15 仍开放 | ❌ 未部署（已提交：fastagent `24a984e`） |

| 44 | 被丢弃的 user space 把它的 MCP client 交还：丢弃先把空间**退休**，扫尾在"退休满 `releaseGrace`（5 分钟）且没有回合在跑、也没有回合在排队"后收回它的 client | **不属于 F1–F3**：资源归属。此前没有任何东西拥有 stdio 子进程或站着的流，而 G11 HTTP 半边的**任何一种**修法都会往同一个洞再加一份资源 | **✅ 已落地**：fastagent `internal/session/manager.go`（`Session.TurnInFlight`、`Manager.AnyTurnInFlight`）、`internal/agent/loop.go`（`Agent.TurnInFlight`）、`internal/agent/manager.go`（`Manager.AnyTurnInFlight`、`Manager.CloseMCPClients`）、`internal/gateway/userspace.go`（`UserSpace.Close`/`AnyTurnInFlight`、`retiredSpace`、三条丢弃路径都走 `retireLocked`、驱逐器 ticker 上调 `releaseRetired`、一个 `now` 缝） | 已绿：`TestADroppedSpacesMcpClientsAreReleasedAfterTheGrace` 与 `TestASpaceWithATurnInFlightKeepsItsClientsPastTheGrace`——两条都通过真实的 `UserSpace` 驱动一个**真** stdio MCP server（会报出自己 pid 的 shell 脚本），断言的是那个 OS 进程，不是"某个方法被调过"。**三条反证真跑**：丢弃即关 ⇒ 2 红；扫尾无视"有回合在跑" ⇒ 1 红；扫尾无视宽限期 ⇒ 1 红 | 无：释放不碰沙箱、也不碰真机 server——子进程就是 `/bin/sh`，与线上 stdio server 同一形状 | ❌ 未部署（已提交：fastagent `c44aefb`） |

| 45 | G11 的 HTTP 半边：client 打开传输层**站着的 GET 流**，把 server 发来的每条消息交给 sink，于是 HTTP 也有 stdio 那样的通道让"无人请求的变化"到达 | **F3 / O1**（投递：变化必须能到 agent；以及"不假装"——405 被照字面接受） | **✅ 已落地**：fastagent `internal/mcp/http.go`——`SetNotificationHandler` 在有 sink 时打开这条流（没有 handler ⇒ 不建连接）、`readStream`/`consumeStream` 实时读 SSE 帧、`handleServerMessage` 是两条流唯一的门、`answerServerRequest` 对服务端请求给应答（`ping` ⇒ `{}`，其余 ⇒ `-32601`）而不是忽略、`Last-Event-ID` 让重连能续上、`Close` 结束它 | 已绿：`TestServerNotificationArrivesOverTheStandingStream`（通知是在**流仍然开着**的时候到达的——缓冲到 EOF 也能过一种更弱的形状，而那仍然是老行为）、`TestManagerHearsAnHTTPNotificationOverTheStandingStream`（真实 `Manager` + 真实端点，穿过闸门）、`TestTheStandingStreamIsNotOpenedWithoutAHandler`、`TestAnEndpointWithoutAStreamIsNotAskedTwice`、`TestAServerRequestOnTheStreamIsAnswered`、`TestClosingTheClientEndsTheStandingStream`、`TestAReconnectResumesWithTheLastEventID`、`TestMessagesOnAReplyStreamGoThroughTheSameDoor`；**五条反证真跑**：不开流 ⇒ 2 红；把流缓冲到结束再读 ⇒ 2 红；把 405 当可重试 ⇒ 1 红；忽略服务端请求 ⇒ 1 红；跳过应答流上的 server 消息 ⇒ 1 红——另外一条：`Close` 不结束流时 `Close` 卡在 `streamWg.Wait()`，包在 60 s 超时上红 | 无：只用 `httptest`，不涉及真机 MCP server——也不需要，因为这里钉的是我们自己这一侧的线（哪些头、哪些帧、哪种应答）。G15（10 §3.4 边界 2）与"从不宣告的 server"（边界 3）仍开放 | ❌ 未部署（已提交在 `fastagent` 分支） |
| 46 | 一个回合在**每个调用返回时**就报出它自己的 `tool_result`，而不是等到整批的汇合点——已经跑完的调用不再被读成"排队中（等待前一个子代理）"（那是对一个没有任何东西在等的调用下的断言） | **O1 / F3**（已完成调用的事实必须到达读者；"排队中"是它的错误形状） | **✅ 已落地（工作区）**：fastagent `internal/agent/sdkbridge.go`（`executeToolsConcurrently` 增加 `onResult` 回调，逐调用各自启动——分区与执行器自身一致：并发安全的一起并行、其余严格逐个——并把 `convertToolResponses` 抽出来）、`internal/agent/loop.go`（`Agent.finishToolCall` 是单个调用的收尾：裁剪、hook、失败记账、索引、媒体、事件；`HandleMessage` 从 `onResult` 调用它，并在声明顺序那一趟里按声明顺序装配历史，因此移动的只是事件的时机——日志仍是模型的顺序，这是每个 provider 都要求的） | 已绿：`TestARoundEmitsEachToolResultWhenItsCallFinishes`——一个回合里一个快调用 + 一个会阻塞的调用：快调用的 `tool_result` 是在**慢调用还停在自己的工具里**时就从线上读到的，而历史里两条 tool 消息仍按声明顺序；**反证已实跑**：改回在整批的汇合点上报（不给逐调用回调）⇒ 同一条见证在"the finished call's tool_result never arrived before its blocked sibling"处变红 | 无独立真机项（它钉的是聊天流的形状；它修的那一行是 delegate_task 行，即第 42 行）。它让 cloud 侧的 `tool_delegate_finished` 补丁（第 42 行的配套）变得不必要，但退掉那是客户端改动，此处不作声明 | ❌ 未部署（落在工作区） |
| 47 | family B 的守卫覆盖文件工具的**两个注册**——宿主那个，和 `SetExecutor` 装上的沙箱那个——并且不再把"写失败"读成"写成功" | **L7 / F1**（12 §3.1：在读写之间被对端抢了先的写必须被拒绝，不能默默照办）+ **O1**（工具结果不得声称一个没有发生的效果） | **✅ 已落地**：fastagent `internal/agent/tools/file.go`——`registerSandboxedFile` 的 `RouteWorkspaceStore` 分支改为与宿主分支一样调用 `putGuarded`；`putGuarded` 在失败**不是**版本冲突时把 store 自己的错误交回调用者（此前是 `return nil`）。`internal/agent/tools/apply_patch.go`——`writeForPatchSandbox` 同改 | 已绿：`write_guarded_peer_test.go` 补上沙箱那一半——`TestSandboxWriteFileRefusesAWriteThatLostItsRace`、`TestSandboxEditFileRefusesAnEditThatLostItsRace`、`TestSandboxApplyPatchRefusesAPatchThatLostItsRace`（每条先断言"到底有没有读过版本"，再断言对端的字节存活），外加 `TestAGuardedWriteReportsAStoreFailureInsteadOfSuccess`（3 个工具 × 2 个注册，断言失败**及其原因**都到达调用者）。**三处反证已实跑**：把沙箱 `write_file` 改回裸 `Put` ⇒ 1 红（"the sandbox registration read no version before writing notes.md"）；把 `writeForPatchSandbox` 改回裸 `Put` ⇒ 1 红（同一句）；把错误交回改回 `return nil` ⇒ 6 红，每条都带着工具自己那句成功话术（"Written 3 bytes to notes.md"） | 无：`LocalFS` 搭在 `t.TempDir()` 上 + 假 executor——这里钉的是"分支到底调了哪一个"，不是后端 | ❌ 未部署（已提交在 `fastagent` 分支） |
| 48 | MCP 握手收口：两个传输都在 initialize 之后发送 `notifications/initialized`，并且**读回** server 定下来的修订（属于 Streamable-HTTP 修订时，作为 `MCP-Protocol-Version` 回带） | **O1**（client 不得断言一个比 server 实际更小的世界）+ 协议合规（G15，10 §3.4 边界 2） | **✅ 已落地（工作区）**：fastagent `internal/mcp/client.go`（`initializeResult`、`jsonRPCNotification`）、`internal/mcp/http.go`（`Connect` 记录版本并调用 `notify`；`notify` POST 一条无 id 的消息、把 202 当作应答；`applyHeadersFor` 只在 `streamableHTTPRevision` 成立时加版本头）、`internal/mcp/stdio.go`（把 `handshake()` 从 `Connect` 拆出来，好让协议那一半能在假管道上被驱动；`notify` 写一行无 id 的消息） | 已绿：`TestHTTPClientSendsInitializedAfterTheHandshake`（先 initialize、再那条无 id 通知，且记录的是 **server** 的版本而不是我们请求的）、`TestHTTPClientEchoesTheNegotiatedVersionOnlyForStreamableRevisions`（2025-06-18 ⇒ 有头，2024-11-05 ⇒ 无头）、`TestStdioClientSendsInitializedAfterTheHandshake`、`TestANotificationDoesNotConsumeARequestID`，以及真机的 `TestLiveMCPHandshakeReadsTheWholeToolList`——真实参考 server，日志为 `negotiated 2024-11-05; 13 tools, including the one gated on initialized`。**反证已实跑**：把 `handshake` 里的 `notify` 去掉 ⇒ 真机测试在「tools = [12 个名字] (12): the server offers simulate-research-query only once the handshake says initialized」处变红，离线见证则因缺第二行变红 | **有，而且它正是本行等的那个闸门**：`FASTAGENT_MCP_LIVE=1 go test ./internal/mcp/ -run TestLiveMCP`，对 `npx @modelcontextprotocol/server-everything` 跑——与 12-vs-13 那次实测同一机制。QC / Quandora 本身仍需交互式 OAuth 授权，而 quandora skill 明令本 agent 不得经手这些凭证；握手这个问题不取决于由哪台 server 回答 | ❌ 未部署（落在工作区） |
| 49 | `piiScrubbing` 开关：规则**只装一次**，装在 agent 持有的那个 provider 上——于是回合、`/v1` 流、`delegate_task`、压实、记忆抽取器全都发送脱敏后的副本——并把写死的字段表换成"线上真的发出去的那些字段" | **O8**（配置开关的契约：行开着，发给 provider 的东西就是脱敏过的）+ **00 §5.1**（链条每个投递点各一条见证：行 → cfg → Manager 选项 → provider） | **✅ 已落地（工作区）**：fastagent `internal/privacy/scrubprovider.go`（`ScrubbingProvider`、`Wrap`）、`internal/privacy/scrub.go`（`ScrubMessages` 覆盖 `Content`、`ContentParts[].Text`、`ToolCalls[].Arguments`、`Thinking`）、`internal/agent/loop.go`（`Agent.setProvider`；`piiScrubEnabled` 与三处调用点脱敏已删）、`internal/agent/manager.go`（`WithPrivacy`；`UpdateProvider` / `UpdateProviderResolved` 都走 `setProvider`）、`internal/gateway/userspace.go`（`managerOptions`，抽成命名函数就是为了让 cfg → agent 这一跳有见证） | 已绿：agent 侧 `pii_scrub_cloudpath_e2e_test.go` 把同一个回合用**五种方式**驱动（`HandleMessage`、`HandleMessageStream`、`RunSubagent`、`CompactMessages`、`AutoPersistMemory`），每种都同时断言 provider 没看到原始值**且** `[EMAIL]` 确实到达——没能到达 provider 的用例不能靠沉默通过；另有热重载用例与关闭开关的用例。网关侧 `pii_scrub_cloudpath_e2e_test.go`：真实 sqlite 里的设置行 → `assembleConfig` → `managerOptions` → `agent.NewManager`。`TestScrubMessagesCoversEveryFieldThatLeavesForTheProvider` 钉住字段表、未被改写的工具名、以及 `RawAssistant` 这个例外 | 无（本开关问的是"发给 provider 的是什么"，而这些用例里的 provider 本来就是假的；真机跑一遍需要真实 provider 加会话里的 PII，只会把同一条断言再往外推一跳） | ❌ 未部署（提交在 `fastagent` 上） |

| 50 | `apply_patch` 保留着自己手写的后端梯子，没有走 `routeFor`；这条梯子**两条**技能规则（其余文件工具在入口都执行）都没有：它允许 `Update File /skills/<name>/SKILL.md` + `*** Move to:` 把操作者的清单文本带出只读挂载、落到一个 chatter 能用 `read_file` 读的工作区路径，也接受只该由 `write_file` 落地的聊时 `skills/<name>/…` 命名空间（宿主桶 + store 镜像） | **O6**（拒绝必须发声，不能静默）+ **08 §1**（harness 必须可感知）——它是 `skillManifestBlocked` 的兄弟（`read_file`/`write_file`/`edit_file` 都在入口守 SKILL.md，`apply_patch` 什么都不守），也是 `skills/…` 单写者规则的兄弟 | **✅ 已落地（工作区）**：fastagent `internal/agent/tools/apply_patch.go`（`applyPatchRefusal`，由**两个**注册在任何 op 规划之前调用，故混合补丁不留半提交状态）、`internal/agent/tools/registry.go`（`SkillNamespaceRefusal`） | green：`apply_patch_skill_gate_test.go` —— `TestApplyPatchRefusesToMoveABundledSkillManifestOutOfTheMount`（记录型 executor：正文不得到达 `leaked.md`、全程不写、结果带拒绝）与 `TestApplyPatchRefusesTheChatSkillNamespace`（两个注册；拒绝里点名 `write_file`，沙箱 executor / agent home 都未被触碰；**管理员同样被拒**，因为这是路由限制而非 `skillManifestBlocked` 的保密闸门）。**两次反证真跑**：沙箱注册去掉闸门 ⇒ 2 红，第一条直接点名泄露（`the SKILL.md body was written out to "leaked.md"`）；宿主注册去掉 ⇒ 只有宿主命名空间那条红 | 自身无：闸门走的就是身份闸门修复已用的 executor 缝（真读 `/skills` 挂载是操作者 IP，不在单测里取） | ❌ 未部署（已落地在工作区） |
| 51 | `skillsLearner` 开关在生产路径上终于有了读者——Manager 依据解析出的 `skillsLearner` 命名空间构建后台抽取器——并且学到的 `SKILL.md` 由 `skills/…` 的**唯一写者**落地（宿主桶 + store 镜像），而不是自己 `os.WriteFile` 到 `<workspace>/skills/…` | **O8**（配置开关的契约：行开着，agent 就真的会学到技能）+ `skills/…` 的单写者规则（第 50 行的兄弟）+ **00 §5.1**（每个投递点各有见证：行 → cfg → Manager 选项 → learner） | **✅ 已落地（工作区）**：fastagent `internal/agent/loop.go`（`enableSkillsLearner`、`skillDirs`；`setProvider` 回写 learner，使抽取调用自动继承 piiScrubbing 包装）、`internal/agent/manager.go`（`WithSkillsLearner` + `buildAgent` 里的调用，放在最后，因为它需要技能路由与 provider 都已就位）、`internal/agent/skills_learner.go`（`SkillWriter` 缝、`skillTarget`、slug 单段校验）、`internal/agent/tools/file.go`（`Registry.WriteSkillFile` + `SkillsRoot`，即聊时路径早已在用的那个写者）、`internal/gateway/userspace.go`（`managerOptions`） | green：`internal/agent/skills_learner_wiring_test.go` —— 行关着时根本不建 learner；开着时学到的 `demo` 技能落在 `<FASTAGENT_HOME>/users/<uid>/skills/demo/SKILL.md` **且**进了对象存储，而 `<agent home>/skills` 保持空（最后这条就是第二写者的断言）；另一条把抽取调用钉成脱敏的 provider 调用点（原始地址到不了 provider，`[EMAIL]` 能到）。`internal/gateway/skills_learner_cloudpath_e2e_test.go` —— 设置行 → `assembleConfig` → `managerOptions` → 真回合（一次工具调用）→ 回合后抽取 → 对象存储。**四次反证真跑**：去掉 `SetWriter` ⇒ 红（`learner was built without the skills namespace's writer`）；把原始 provider 交给 learner ⇒ 红（`with piiScrubbing on, the extraction call still carried the raw address`）；去掉 `buildAgent` 那行 ⇒ 红（`the skillsLearner row did not reach the agent: no learner was built`）；去掉 `managerOptions` 那行 ⇒ 红（等满 15 秒后 store 仍为空） | 自身无：抽取调用的 provider 是假的、设置行是真 sqlite——正是本改动关心的两件事 | ❌ 未部署（已落地在工作区） |
| 52 | `memory` 命名空间（auto-persist）在生产路径上有了读者：Manager 把解析出的行作为**默认层**交给共享构造函数，per-agent 的 `agents.defaults.autoPersist` 再盖在它上面（nil = 继承，`false` = 一票否决） | **O8**（配置开关的契约：per-agent 字段自己写着 "nil = inherit system MemoryCfg.AutoPersist.Enabled"）+ **00 §5.1**（投递点见证：行 → cfg → Manager 选项 → 闸门） | **✅ 已落地（工作区）**：fastagent `internal/agent/manager.go`（`WithMemory` + `newAgentWithActor` 调用）、`internal/agent/loop.go`（`newAgentWithActor` 接收 `memoryCfg`；per-agent override 仍在其上；EveryNTurns 默认值在 override 之后落）、`internal/gateway/userspace.go`（`managerOptions`）、`internal/config/config.go`（AutoPersist 注释改成陈述优先级，不再描述一条死路径） | green：`internal/agent/memory_autopersist_row_test.go` —— 四种优先级（行关+无 override ⇒ 关、节奏默认 5；行开+无 override ⇒ 继承，含 `everyNTurns`/`model`；行开+per-agent `false` ⇒ 否决；行关+per-agent `true` ⇒ 单独打开），外加蒸馏本身真的往 MEMORY.md / USER.md 追加；`internal/gateway/memory_autopersist_cloudpath_e2e_test.go` —— 设置行 → `assembleConfig` → `managerOptions` → 真回合 → 蒸馏开火，而同样一个回合在没有该行时不触发。**两次反证真跑**：去掉 `managerOptions` 那行 ⇒ 红（`the memory row reached no reader: a real turn fired no auto-persist pass`）；让系统层盖掉 per-agent override ⇒ 否决那条与单独打开那条都红 | 自身无：蒸馏 provider 是假的、设置行是真 sqlite——正是本改动关心的两件事 | ❌ 未部署（已落地在工作区） |
| 53 | FTS5 记忆索引**整体删除**：`MemoryCfg.FTS`、`store.FTSStore`、`memory_search` 的 searcher 缝、以及两处索引调用点 | **O8**（开关的承诺必须与效果同构——这一条从未成立）：死代码（CCP：没有理由改就没有理由存在），外加"只有一个实现"这条读数：**两端从未接线**——store 只在零调用者的构造函数里被构造，也从来没有任何调用者传过 searcher，所以真正跑过的只有文件扫描那一份实现 | **✅ 已落地（工作区）**：fastagent —— 删除 `internal/store/fts.go`；`internal/config/config.go`（`FTSCfg` 与该字段）；`internal/agent/loop.go`（字段、初始化块、两处索引点）；`internal/agent/tools/memory_search.go`（`FTSSearcher`、可变参数缝、FTS 分支、`formatFTSResults`）；以及四处仍把 FTS 列为 `Origin` 消费者的注释（`loop.go`、`store/database.go`、`provider/provider.go`、`origin_tag_test.go`） | 验证是删除形态（第 29 行的规矩）：全仓引用计数 + `go build ./...` + `go vet` + 各套件；`memory_search` 仍由文件扫描作答，面板"只在镜像里存在的命名空间"那个夹具已从 `memory.fts.*` 挪到活字段上，免得它在演示一个已经不存在的字段 | 无（删除本身就是改动） | ❌ 未部署（已落地在工作区） |
| 54 | **被拒的 `If-Match` 不等于有人抢先改了**：Spaces 对**每一个** `If-Match` 都回 412，于是 `PutIfVersion` 对任何已存在路径的覆盖都报 `ErrVersionConflict`，文件工具把它翻成"another writer changed X"，模型按设计重读再试，三轮之后本轮丢掉工具（09-22 dev 会话 `MImz6pYfMoZLJabEHJRI4p`：文档 §7–§12 从未落到文件，模型把它们贴进了对话）。现在在相信一个 412 之前先问对象本身：若它携带的版本**正是**我们发出去的那个，就没有人抢先——只是这个桶不评估该头——于是写入无条件落盘，比对发生在片刻之前，这正是 `LocalFS` 一直具备的强度。每进程一行 WARN 点名这个桶，因为那是**存储**的属性而不是某次写入的属性。被抢先的期望仍然回 `ErrVersionConflict`，create-only（`If-None-Match: *`）在迄今遇到的每个后端上都保持精确，所以新路径的防覆盖保证未被触动 | F1（防覆盖保证必须在它被声称的地方成立）+ O1（提示对 Spaces 也得是真的） | **✅ 已提交在 `fastagent` 分支**（`242a4b3` 重读与条件重试、`7be8c24` 说明"在 Spaces 上属正常"的 WARN、`cfbbe8e` 端口写明哪种 S3 是精确的、`c25c1b1`/`ccc682d` 强度表）：`internal/workspace/s3.go` `PutIfVersion`、`internal/workspace/workspace.go` | 绿，先红后绿：`internal/workspace/s3_version_test.go` —— `TestS3OverwriteLandsOnABackendWithoutIfMatch`、`TestS3NoIfMatchWarningSaysItIsNormalOnSpaces`、`TestS3CreateOnlyUsesIfNoneMatch`、`TestS3VersionIsTheETagAndTheConditionRidesTheRequest`（`7fc39f3`、`09a28de`）。那个假后端对每个 `If-Match` 都回 412（Ceph RGW 的行为），并且会剥掉 aws-chunked 分帧——不剥的话"字节落盘了"根本无法断言。**两条反证真跑过**：把被拒的前置条件改回立即冲突 ⇒ 覆盖用例红；把提示改成通用文案 ⇒ 提示用例红 | `internal/workspace/s3_live_test.go` `TestS3LiveOverwriteOfAnExistingPath`（`b87e156`）——对真实桶跑 新建 → 覆盖 → 读回 → 被抢先者被拒，未设 `FASTAGENT_S3_LIVE=1` 则跳过，一个临时对象，用完删掉。这才是早先那条声称本该有的证人：假后端忠实于 AWS，因而恰好对唯一要紧的那件事是瞎的 | ❌ 在分支上，未发版 |
| 55 | **文件的判决不等于实例的判决**：envd 对不存在的路径回的是它自己的 404——与边缘用来表示"我从没有过这个实例"的状态码完全相同——而 `ReadFile`/`WriteFile` 信了它并调用 `recreateIfCurrent`，于是每次读取一个不存在的文件都会替换掉一个活着的沙箱（新实例、79 个技能文件、1.13 MB tar、重新 hydrate），重试再从"只剩已持久化内容"的工作区作答。这就是"模型刚写完的文件被告知不存在"的来历（09-22 dev 会话 `MImz6pYfMoZLJabEHJRI4p`）。`sandboxGoneOnFileAPI` 把这个区分留在词汇该在的地方：502 仍是实例，消息里点名路径的 404 是文件，无法识别的 404 仍算实例判决——未知继续只花一次重建，而不是丢一次调用。exec 路径未动：exec 没有可搞错的路径 | F2（O1 —— 判决必须是关于它所指的主体的判决） | **✅ 已提交在 `fastagent` 分支**（`4e95d19`、`7587ac2` 重建日志点名触发它的那个应答、`686f832` §10）：`internal/sandbox/e2b_executor.go` `sandboxGoneOnFileAPI`、`recreateIfCurrent` | 绿，先红后绿：`internal/sandbox/e2b_error_classification_test.go` —— `TestE2BReadOfAMissingPathKeepsTheSandbox`、`TestE2BReadOfAGoneInstanceStillRebuilds`、`TestE2BExecDoesNotRebuildOnNonGoneFailures`、`TestE2BCloseReadsTheAnswer`（`84a8c95`）；假 envd 现在回文件 API 自己的 404 而不是 200（`92267be`，`missingPaths`）。**反证两个方向都观测到**：把文件端点交给 `sandboxGone` 分类 ⇒ "a missing path cost 1 sandbox rebuild(s); want 0"；把每个 404 都当路径判决 ⇒ 实例已消失那条失败 | ——（形状是生产证据：07:29:40.193 两次 `read_file` 调用、07:29:40.710 "e2b sandbox expired, recreating"、1.6 s 后重建完成；同一会话 21 分钟内重建三次，每次都在某次文件工具调用之后 0.7 s 内） | ❌ |
| 56 | **工具调用后的钩子读的是这次调用自己设的时钟**：after 半被递了一个全新的 `HookContext`，于是 `time.Since(time.Time{})` —— `2562047h47m16.854775807s` —— 成了生产里每次工具调用的"耗时"，09-22 那次会话只能靠时间戳反推。`fireBeforeToolCalls` 现在触发本轮的 before 钩子并按 tool-call id 返回每次调用的开始时刻；`finishToolCall` 为它正在收尾的那次调用取该值，web 流里内联的 after 钩子从同一张表读。`StartTime` 在钩子运行**之前**设好，所以没有 `LoggingHook` 的 registry（测试、裁剪过的安装）也给 after 半一个真时钟。两处 before 循环此前已经漂成了彼此的副本，现在是一个函数，这也正是两个 after 半读同一来源的原因 | F2（O2 —— 日志里的数字必须量的是它名字所指的东西） | **✅ 已提交在 `fastagent` 分支**（`425b990`）：`internal/agent/loop.go` `fireBeforeToolCalls`、`finishToolCall`、`HandleMessageStream` 里内联的 after 钩子 | 绿，先红后绿：`internal/agent/tool_hook_timing_test.go` `TestAfterToolCallReportsThisCallsDuration`（`6b7c9ae`），表驱动覆盖两个入口，因为各自构造自己的 after 上下文。**反证**：把 `StartTime` 从 `finishToolCall` 里拿掉 ⇒ `HandleMessage` 子用例红而流的子用例仍绿——两个不同的代码路径，不是同一个测了两遍。模型调用那一对在同一用例里作为对照断言；它显式传 `StartTime`，从来没坏过，这正是问题属于**工具**那一对而不属于 `LoggingHook` 的依据 | 无——度量就是那行日志本身（09-22 dev 会话的 33 次工具调用全是 `2562047h47m16.854775807s`） | ❌ |
| 57 | **无法续期的凭据被命名，它的跳过日志点名补救动作**：刷新路径对一个**从不签发** refresh token 的授权服务器回的是 `oauth: no refresh token stored`——一个缺失字段，读起来像存储损坏。QuantConnect 就是产生这条的案例：它的 RFC 8414 元数据声明 `grant_types_supported = ["authorization_code"]`，没有 `scopes_supported`、没有 `revocation_endpoint`，而它签发的令牌不带 `refresh_token`、两小时过期，于是沙漏一漏完 `quantconnect` 就从工具列表里消失，之后每一轮都在没有它的情况下跑（dev，2026-09-22 07:28:11）。`domain.ErrReauthRequired` 给这个状态命名，刷新路径把它包在可执行动作里，Manager 对这一类原因的跳过行点名重新授权以及两个能做这件事的地方（agent 的 MCP 设置，或 `mcp login <server>`），而其余原因保持原行。控制台本来就能表达这件事——`/api/mcp/oauth/servers` 报 `expired`，面板会渲染——这里是同一事实的日志半边，给正在读网关而不是面板的运维 | ——（协议合规加上 O1：报告调用方**能够据以行动**的状态） | **✅ 已提交在 `fastagent` 分支**（`e24918a`、`e77c1ce`、`00e9cb8`、`5923b6d`）：`internal/mcp/oauth/domain/oauth.go`（`ErrReauthRequired`）、`internal/mcp/oauth/usecase/refresh.go`、`internal/mcp/manager.go`、`docs/mcp-oauth-design.md`（§0.1.1、§11） | 绿，先红后绿：`internal/mcp/oauth/usecase/reauth_required_test.go`（`781a947`）—— `TestExpiredCredentialWithoutRefreshTokenDemandsReauthorization`、`TestValidCredentialWithoutRefreshTokenIsStillUsed`；`internal/mcp/manager_reauth_log_test.go`（`18ffafa`）`TestManagerLogsTheRemedyWhenAServerNeedsReauthorization`，它把两种失败走过同一个 Manager 并断言的是**区分**而不是文案。**反证**：把 `refresh.go` 改回裸错误 ⇒ 用例红；把 Manager 的那条分支撤掉 ⇒ 日志用例红 | 无（证据是 09-22 07:28:11 的 dev 轮次，加上服务器发布的元数据，URL 与日期记在 `docs/mcp-oauth-design.md`） | ❌ |
| 58 | **预热离开沙箱池，由 shim 接管**：`camoufox-cli` 的 shim 现在是构建上下文里的一个文件，而不是 Dockerfile 里的 `printf` 块——逐字节相同的脚本，但这次测试能跑它。它把 daemon 冷启动的竞态等过去（客户端拉起自己的 daemon 后只为 socket 等 5 s；全新沙箱的首次启动可能更久，于是客户端在发出命令之前就 exit 1，而它启动的 daemon 还在继续开机），上限 `CAMOUFOX_SHIM_WAIT_SECS=20`，只重放一次同一组 argv，且只在**没有 socket** 时重放，因为活着的 daemon 已经应答过的命令绝不能再跑一遍。provisioning 不再跑 `camoufox-cli open about:blank` 并等最多 120 s：在 dev 上那是 26 s 冷启动里的 24 s，由每个 agent 买单，包括从不碰浏览器的那些；而且它从未覆盖重建/接手路径——那恰恰是最可能撞上这个竞态的实例 | F3（投递保证必须在每一个可能用得上它的路径上成立）+ O1 | **✅ 已提交在 `fastagent` 分支**（`034aaf3`、`d06467a`、`86db198`、`e935e65`、`67e049e`、`0c446aa`）：`deploy/docker/sandbox/camoufox-cli-shim.sh`、`deploy/docker/sandbox/Dockerfile`、`internal/sandbox/e2b_executor.go`（provision 期预热删除）、`docs/sandbox-pool-leases.md` | 绿，先红后绿：`internal/sandbox/camoufox_cli_shim_test.go`（`5f92523`）—— 四个用例拿真脚本对着假客户端跑：`TestCamoufoxShimInjectsProxyOnce`、`TestCamoufoxShimRetriesOnceWhenTheDaemonMissedItsOwnFiveSecondWait`、`TestCamoufoxShimDoesNotRetryWhenTheDaemonIsUp`、`TestCamoufoxShimGivesUpWhenNoDaemonEverAppears`。四条在修复前都红，而且理由很诚实：出厂的 shim 既没有重试，也没有任何办法把它指向假客户端，所以假客户端根本到不了 | `internal/sandbox/e2b_live_browser_cold_start_test.go` `TestE2BLiveBrowserColdStart`（`67e049e`）—— 全新 scope 的第一次 `camoufox-cli open` 必须仍然开出页面；它还读镜像里的 shim，因为"网关先滚上去、模板还没重建"这个组合没有别的护栏。已接进 credentials job 与无凭据 job 的跳过清单 | ❌ —— 并且它同时定下了这次改动隐含的发布顺序：先重建并重注册 e2b 模板，再发不再预热的网关 |
| 59 | **输出被截断的回合在它被切断的地方被命名，被截断的工具调用不再执行**：流里丢掉了 `finish_reason`，所以 09-22 那两次止于 `output_tokens = 8192` 的生产 `write_file` 调用没有留下任何痕迹。现在 Done chunk 携带它，`FinishReasonLength` 是 OpenAI 的 `length` 与 Anthropic 的 `max_tokens` 的同一个词，循环在 Done 处 WARN 一次，带上 agent、模型、上限以及是否同时带着一次工具调用——足以把"模型在工具调用中途被切断"和"工具拿到了坏参数"分开。桥此前把解析失败的调用参数以 `_raw` 塞给工具照发不误，于是一个在字符串中途被切掉的 `write_file` 回的是 `path is required`——而那次调用明明带了 path；模型把这读成自己漏了参数，把整份约 17 KB 文档重发一遍，再次撞上同一个上限（生产里两次）。现在解析不了就在解析处失败：工具不跑，结果点名工具、成因与字节数，调用保留自己的 id 所以批次仍然配对，空参数仍然是合法调用，并且这条失败也是 WARN。`write_file` 的描述现在说出该怎么做（先写第一节，再用 `edit_file`），共用的空路径错误改成给出**能用的**路径，而不是把调用方早已照做的规则再念一遍 | F2（O1 失败必须点名成因，O2 上限必须在它发生的地方可见） | **✅ 已提交在 `fastagent` 分支**（`d91165c`、`a6e515c`、`fb88240`、`a915329`、`94df285`、`7ac69e2`）：`internal/provider/provider.go`（`FinishReasonLength`）、`internal/provider/openai.go`、`internal/provider/anthropic.go`、`internal/agent/loop.go`（Done 处的 WARN）、`internal/agent/sdkbridge.go`（`parseToolCallArguments`、`uncallableArgumentsMessage`）、`internal/agent/tools/file.go`（`writeFileDescription`、`writeFileSchema`、空路径错误） | 绿，每步都先红：`internal/agent/tool_arguments_test.go`（`cb0c58c`：`TestToolCallWithUnparseableArgumentsNeverReachesItsTool`、`TestATruncatedCallDoesNotStopItsSiblings`、`TestAToolCallWithNoArgumentsIsStillACall`）、`internal/provider/stream_finish_reason_test.go`（`b6a4a31`）、`internal/provider/anthropic_stream_stop_reason_test.go`（`bdf7a86`）、`internal/agent/output_cap_log_test.go`（`5836127`：`TestACappedReplyIsLoggedWhenTheStreamEnds`、`TestAnOrdinaryEndingIsNotLoggedAsACap`）、`internal/agent/tools/write_file_guidance_test.go`（`7ec235f`：`TestWriteFileSchemaSaysHowToWriteSomethingLong`、`TestWriteFileSchemaStaysSmall`、`TestTheEmptyPathErrorShowsAPathThatWorks`）。给模型的那句自带缰绳——schema 每轮请求都随行，所以这句话必须挣到它的字节——"正常结束"两个用例的存在，则是因为一条总在响的 WARN 等于什么都没说 | 无——证据是生产（09-22 两次 `write_file`，`finish_reason` 为 `length`，`output_tokens` 8192；WARN 存在之前只能从 `token_usage_log` 反推） | ❌ |
| 60 | **九个工具注册了两次，现在只描述一次**：host 那套注册与 `SetExecutor` 安装的沙箱那套，文案是手抄的，而云端一轮读的是沙箱那套——于是 `write_file` 的"分段写"那句话只落在 host 那份上，偏偏就在沙箱那份被指责的同一天；`exec` 是更老的同一形状（`stdin` 的"给技能脚本喂 JSON 参数"示例只在 host 那份，`allow_long_wait` 的后果只在 host 那份，`run_in_background` 已经各自演化过一次）。`exec` 现在两份都从 `execSharedParams` 构建、取事实的并集，只有 host 注册多一个 `sandbox` 开关；`read_file` 与 `list_dir` 共用一份 `singlePathSchema`，就像 `edit_file` 与 `write_file` 早已各用一份 schema | ——（单一来源：一个事实只有一种表达） | **✅ 已提交在 `fastagent` 分支**（`c0b5b84`、`5f2af5f`、`dd4f22e`）：`internal/agent/tools/registry_parity_test.go`、`internal/agent/tools/exec.go`（`execSharedParams`、`execHostParams`）、`internal/agent/tools/file.go`（`singlePathSchema`、`writeFileSchema`、`editSchema`） | 绿，先红后绿：`internal/agent/tools/registry_parity_test.go` —— `TestBothRegistriesOfferTheSameTools`、`TestNoToolDescriptionIsOneSided`、`TestNoToolParameterIsOneSided`、`TestOnlyTheNamedDifferencesExist`。**许可清单本身就是重点**：只允许 `exec` 的描述、它独占的 `sandbox` 开关、以及每路径各自的 `path` 提示不同，且各带理由；第十处差异必须被人**刻意**加进去。**两条反证真跑过**：把旧的短 `stdin` 加回 `execHostParams` ⇒ 红并点名 `stdin`；手写一份漂移的 schema ⇒ 红并点名那个工具，这正是"重构后的四份 schema 仍被比对"的证据 | 无（两个 registry 都在进程内） | ❌ |
| 61 | **O8 的写者半边：三个只有读者的开关拿到了控制器**。Runtime 页面长出 `piiScrubbing`、`skillsLearner`、`memory`（auto-persist）三行——也就是第 49/51/52 行给了读者的那三行——并且 PII 那个控件把自己的限制一并带上（模型自己早先回复里回放的 `Message.RawAssistant` 仍然会出走，所以那个冲突依旧是运维的决定）。`08` 增加四个开关行的常设表（读者、写者、承诺、限制），三个网关测试的头部不再声称"没有面板能写这些行" | O8（开关的承诺必须与效果同构——而"谁能动它"是效果的一部分） | **✅ 已提交在 `fastagent` 分支**（`e611f3c`、`70fa91e`、`02ff391`、`6cba21a`、`d5f667f`、`feb010d`）：`web/src/app/settings/runtime/page.tsx`、`web/src/lib/api.ts`、`docs/fs-formal-proof/08-state-observability-principle.md`、三个 `internal/gateway/*_cloudpath_e2e_test.go` 的头部 | 绿：`internal/setup/runtime_settings_memory_learning_e2e_test.go` `TestRuntimePage_CanSetMemoryAndSkillLearning`（UI 里设的那一行，经 `assembleConfig` → `managerOptions` 到达一个真回合）、`web/src/__tests__/runtime-settings-memory-learning.test.tsx` | 无（上面那条网关 e2e 就是云端形状：settings 行 → 配置 → 真回合） | ❌ |
| 62 | **三份文档不再携带其证据从未覆盖的声称**：ext-skills 一致性清单原来把能力那一行记成"已实测"，用的却是一个能容忍**真客户端会拒绝的信封**的客户端（该行现在写明是哪个客户端测的什么，并指向可重复的检查），turn-integrity 文档写明 B3 的证人"忠实因而失明"——那个假后端按 AWS 的方式实现 `If-Match`，所以它不可能显示真实桶对每一个 `If-Match` 都回 412——而 MCP 出口的那几份说明（决策记录加三份设计/issue）描述的是一个**从未上线**的 per-agent 端点，现在各自在头部写明这一点 | O1（只说真话），这次用在文档而不是运行时信号上 | **✅ 已提交在 `fastagent` 分支**（`37983e0`、`1569253`、`b6ae1d9`）：`docs/issues/ext-skills-conformance-checklist.md`、`docs/session-turn-integrity.md`、`docs/issues/mcp-egress-decisions.md` 与三份 MCP 出口说明 | 无 —— 文档类 | 无 | ❌ 不适用（文档）；每处更正的**主体**各自在上面已有自己的行 |
| 63 | **丢失通知也算一次写入，所以 `Stop` 必须 join 续期协程**：A1.4 承诺被接管的回合"不再继续写"，而守卫发出的那条 σ 就是写——可它由续期协程发出，而 `Stop`（回合的最后一句，`defer lease.Stop()`）当时只是关掉 stop 通道就返回了。每个调用方都在回合返回之后关掉自己的事件通道，于是活过 `Stop` 的 emit 就是向已关闭通道发送。守卫现在带一个 `done` 通道，`renewLoop` 退出时关闭它，`Stop` 在碰围栏和租约之前先 join 它；join 有上界（2 s，一行 WARN），因为 emit 尊重回合的 context——停止读取的读者是由 context 结束的，不是靠耐心等掉的。本包唯一那条 `-race` 失败是同一事实从测试那一侧看到的：`TestSupersededTurnStopsAndSignals` 关闭 `events` 与 `renewLoop` 里 `emitEventChecked` 的发送相撞，那个测试也必须先停掉自己的生产者再关闭 | F2（O1 —— 发生过的 σ 必须在读者还能读到的时刻说出来） | **✅ 已提交在 `fastagent` 分支**（`489d1eb` join、`54568bb` 测试的顺序、`dd62fd9` 用例、`2d8e697` §A1.4）：`internal/agent/turnlease.go`（`turnLeaseStopJoin`、`renewLoop` 的 `done`、`Stop`）、`docs/session-turn-integrity.md` §A1.4 | 绿，先红后绿：`internal/agent/turnlease_test.go` `TestStopWaitsForTheNoticeItStarted` —— 用测试自己的回调把守卫的 emit 挂住，调用 `Stop`，断言 `Stop` 尚未返回；之后才放开 emit，`Stop` 必须返回。刻意做成确定性的：它红在断言上，不红在检测器上。**反证真跑过，一次同时看到两面**：去掉 join ⇒ 该用例红，并且 `TestSupersededTurnStopsAndSignals` 再次报出数据竞争 | `go test ./internal/agent/ -race` 绿；`internal/setup`、`internal/gateway`、`internal/sandbox` 在 `-race` 下同样绿（同一"返回后再关闭"的形状也在 SSE 与聊天处理器里） | ❌ |
| 64 | **README 里的 e2b 模板流程点名了存在的文件**：它的前置条件把读者送到 `e2b template build --config deploy/docker/sandbox/e2b.toml`，而那个文件从 `0f9ed98` 迁移到 E2B Build System 2.0 起就不存在了——那次提交把 TOML 与 Dockerfile 改名为 `.old`，却把 README 留在原处指着它们（它的 diff 只动了五个文件，README 不在其中）。现在步骤写的是真实路径：先推送底图（`build.sh --push`，也就是模板 `fromImage` 的源）、再用 SDK 定义注册模板（`build.prod.ts` / `build.dev.ts`，需要 `e2b` SDK——本仓没有根 `package.json`）、最后**在模板之后**发布网关。结尾的说明也不再只说 `sandbox.image`：面板的 E2B Template 字段写的是 per-backend 的 `e2bTemplate`，两者都有时它优先；而 `sandbox.image` 填的是 chart 映射到 `FASTAGENT_SANDBOX_IMAGE` 的旧槽位，只在 `e2bTemplate` 为空时才被读取 | O1（只说真话）+ F3（发布顺序是投递的一部分，不是建议） | **✅ 已提交在 `fastagent` 分支**（`1670d9d`）：`README.md` 的 "Enable E2B sandbox" 一节 | 无 —— 文档类 | 无 | ❌ 不适用（文档） |

| 65 | **一个向外部读者自我宣传的界面没有给它留交付点（F1）**——MCP 任务界面的状态设计是纯拉取式的，而这个事实只写在散文里：`status`/`outcome` 存在于一份没人有义务来问的回复中，于是"客户端没在看的时候任务结束了"对那套设计唯一的读者（一个进程外的 MCP 客户端）是不可见的——测试曾是它实际上的唯一读者，这也是为什么一切一直是绿的。裁定（2026-09-26）：两半都给——拉取仍是权威（`read_task` 的回复**就是**交付点，所以 §5.1 的 C 族豁免适用），再补一条原生**推送**，只当**提示**（每个任务作为资源被订阅；`notifications/resources/updated` 只说"变了"，从不携带事实；客户端据此重读）。`notifications/progress` 刻意不用——它的作用域是在飞请求，而提交调用早已不再阻塞。正确性不得依赖通知：通知是尽力而为的，任何一段空隙之后拉取一次即可对账 | **O9**（新增，已提升进 [08 §10.10](./08-state-observability-principle.md)）——形式化方法的第 4 条假设读者在 harness 内部，而这是第一个读者不在内部的实例。（同一次审计的 F2——`status` 必须恰好有一个生产者，因为线上读取路径从归档形状推断它、准入读的却是租约——是同一条界面上的实现约束，记在那节里） | **仅设计，尚无代码**：cloud `docs/mcp-task-submission.md` §14.2/§14.8/§14.9（`read_task` + 资源订阅；`run_task` 异步；`status`/`outcome`）；本行不碰 fastagent 侧（出口面是 cloud worker） | 尚未：本行的见证已写明——拉取：cloud `mcp-surface-e2e.test.ts`（回复携带事实）加 `read_task` 的行为用例；推送：一条测试，断言被订阅的客户端在状态变化时收到 `notifications/resources/updated`，反证要让**那条投递**变红，而不是让规则变红 | 无——还没有真机 e2e（该界面目前只在 dev，推送通道与它一起落地） | ❌ 未实现（2026-09-26 裁定设计） |
| 66 | **蒸馏器的提问不再被它自己的截断弄坏**：`AutoPersistMemory` 的问题由三次截断拼出来——最近 20 条消息的正文截到 300 字节，当前 `MEMORY.md` / `USER.md` 各截到 500——而真正送到模型的那两处用的是字节切片（`content[:300]`、`truncateStr` 里的 `s[:n]`）。这两个数都不是字符边界：UTF-8 里一个汉字占 3 字节，500 = 3×166+2 会切在第 167 个字的中间，而 300 只在它之前全是 ASCII 时才成立。这个功能自己的文件就是中文，所以从第 166 个字的记忆起，每一次蒸馏提问都带着一段被劈开的序列——而且它以唯一要紧的方式**静默**失败：模型照常作答，下游没有任何东西能看出是 harness 弄坏了自己的提问。`0a5a6b5`（"rune-based truncation for **all** UTF-8 text fields"）扫掉了会话侧的截断和**同一个文件里**解析失败日志的那个 preview，唯独漏掉真正发给模型的这两处——那个 preview 的邻居有用例，这两处什么都没有 | O1（只说真话），用在一次提交自己的"横扫"声称上——"all" 只覆盖了那个文件里三处截断中的一处——而不是用在某个运行时信号上 | **✅ 已提交在 `fastagent` 分支**（`088f41c`、`96c8c51`）：`internal/agent/memory_prompt_utf8_test.go`（新增）、`internal/agent/memory.go`（`AutoPersistMemory` 的消息 preview、`truncateStr`） | 绿，红先行：`TestTheDistillerPromptSurvivesItsOwnTruncation`——把超过 500 字节的中文 `MEMORY.md` 与超过 300 字节的中文消息喂给真的 `AutoPersistMemory`，断言落在**离开进程的那些字节**上（`utf8.ValidString`、无 `U+FFFD`），并要求两段中文开头都在，未到达或为空的 prompt 不能靠沉默通过。**证伪真跑**：把这两行改回去 ⇒ 红，`not valid UTF-8 (first bad byte at 702): "户要求所\xe6…"` | 无（蒸馏用的 provider 按构造就是假的；真机跑需要一份超过 166 个字的中文 `MEMORY.md`，只会把同一条断言再往外推一跳） | ❌ |
| 67 | **MCP 与回合身份两次审计反复撞到的三条**（2026-09-27）：(a) **降级声明**是一个登记状态——义务 + 降级的承诺 + 解锁物——于是"说了但没做到"不再被记成 未做；(b) **O9′**，**进程内**的投递也要点名生产者与消费者；(c) **O10**，欠外部读者的义务必须落在那个读者看得到的面上（实例："追加一条指令要换新的 `idempotencyKey`"该写在 `run_task` 的描述里） | O1/O9 用在**义务**与**进程内 hop** 上 | **✅ 已落地（工作区）**：`08-state-observability-principle.md` §10.11/§10.12/§10.13（中英双版）；`00-formal-systems.md` §5 增加 O9′/O10 两行 | 每条的反证写在 §10.11–§10.13 里；实例见 cloud `docs/mcp-task-submission.md`（F2；§14.2/§3.1）与 `docs/fastagent/design/14-turn-identity.md`（§3 I1） | 中文镜像已同步 | ❌ |

> **行号说明（2026-09-27）**：EN 的第 **65** 行（O9／MCP 交付点）此前缺镜像，当天补上，两表编号重新对齐。
> 新行仍按 EN 的编号落笔，而不是在缺行的位置顶一格——两表行号一旦错位，之后每一行都对不上号。
> 同一天还把这条行引用的落点补上了：`08 §10.10` 与它前面的 §10.9 已镜像进中文 08（`45c0ede`），所以这个链接不再悬空。

> **第 54–62 行（2026-09-22，第三趟）**：九行，覆盖本册上次更新（`aa67a2c`）之后落地的 51 个提交，
> 按 `git log aa67a2c..HEAD` 逐个家族读出来的。每格写明它自己的提交，因为它们确实存在；❌ 仍然只有一个
> 含义——在分支上，没发版。九行里有两行是文档行（第 54 行的提示与第 62 行的三处更正），它们如实说明，
> 而不是被安上一条从未有过的 UT。第 58 行是唯一一行"发布顺序本身是约束、而不是陈述"：必须先重建并重注册
> 模板，再发不再预热的网关。

> 表中 `tokenaissance-cloud:` 前缀的路径相对 cloud 仓库（[tokenaissance/tokenaissance-cloud](https://github.com/tokenaissance/tokenaissance-cloud)，分支 `develop`）。

> **状态列更正（2026-09-21）**：09-19/20/21 那一批（第 32–39 行）里仍写着"已落地（工作区）"的格子，
> 写的时候为真，现在则**低报**了——代码已提交：fastagent 在 `fastagent` 分支（`c4c4220` 栅栏、
> `ce6ffb4` 租约/投影/epoch 修法、`3f3c02e` 条件写、`95204aa` 附件保留双方、`bc1d9d6` 第 39 行），
> cloud 在 `develop`（`27736153` 工具行、`b453fef8` 输入框、`7af1224b` 会过期的回合事实、`29a2a23c`
> 附件应答、`81bc9296` 第 38 行、`74c69858` 第 39 行的客户端那份文案）。这些格子读作"在分支上、未部署"
> ——上面的 ❌ 只有一个含义。逐格写明 commit 是另一趟文档活儿（这里的锚点正是函数名，因为它不漂移）；
> 本注拒绝的是另一种错，即用一个 ✅ 声称发过版。

> **状态列更正（2026-09-22）**：同一条更正现在适用于第 **43、44、45** 行——它们"已落地（工作区）"写时为真，
> 现在是**低报**：三条都已提交在 `fastagent` 分支（第 43 行 `24a984e`、第 44 行 `c44aefb`、第 45 行随本轮），
> 且都没有部署，所以上面的 ❌ 只有一个含义。第 45 行是同一类格子里新的一格——它的 commit 刻意不写在这里，
> 因为一个引用自己所属提交的格子不可能永远正确。

> **状态列更正（2026-09-22，第二趟）**：同一条更正现在适用于第 **46、48、50、51、52、53** 行——它们
> "已落地（工作区）"写时为真，现在是**低报**：六条都已提交在 `fastagent` 分支（第 46 行 `af3b65c`、
> 第 48 行 `756814c`、第 50 行 `798b380`、第 51 行 `17f3a3f`、第 52 行 `4ecadc7`、第 53 行 `86c38b9`）；
> 都没有部署，所以上面的 ❌ 只有一个含义——在分支上，不是发过版。第 47、49 行落地时就逐格更正过，这里不重复。
> 这一趟是在逐行拿各自符号去 `git log -S` 核对之后写的，所以它能写出六个 hash 而不是猜一个。
