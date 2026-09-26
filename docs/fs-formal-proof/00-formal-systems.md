# 00 · The three formal systems: an index

> Status: index · last verified: 2026-09-24 (F3 mechanism 3 now carries its **boundary**: live-only events are outside the log's guarantee)
> This directory (`文件系统形式化证明`, formerly `文件系统`) actually carries **three** formal systems:
> its name only describes the first one's application area; the other two constrain the whole harness.
> This document is their **single entry point** — what each defines, how they compose, which code each
> symbol lands on, and which test pins each obligation.
> **It is also the single entry point for formal reasoning across the repository**: §4.1 turns "formal
> system → mechanism → the design documents it constrains" (including files outside this directory —
> `../session-turn-integrity.md`, `../sandbox-pool-leases.md`, `../chat-event-delivery.md`, …) into one
> inverse index.
> **Full inventory** (A formal systems / B mechanism layer / C subsystem contracts / D models / E unformalised) is §7.

## 1. Three systems, three questions

| # | Formal system | Defined in | Question it answers | Form of the judgement | Cost of violating it |
|---|---------------|-----------|--------------------|-----------------------|---------------------|
| **F1** | **Preconditions / zero migration** (Cordis: reconciler, left inverse, keyed diff, system boundary) | [06](./06-cordis-review.md) · [07 part 2](./07-formal-rootcause-and-fix.md) | **May this migration happen at all?** And what happens when it may not? | Declarative: one migration per key + an explicit precondition; a violation ⇒ **error + zero migration** (R1/R2); idempotent convergence (R3), locality (R4) | Silent corruption: overwriting what someone else wrote (the 2026-09-17 incident) |
| **F2** | **Observability** (`δ` / `σ`, `Belief` / `World`) | [08 §2, §2.1, §3](./08-state-observability-principle.md) | Which changes **must be stated**? And must the statement be true? | A universal proposition: **∀ δ triggered by the harness, ∃ a σ the agent can consume**; corollaries C1/C2/C3 | A missing signal → data loss + **belief corruption** (the agent reasons about a world that no longer exists) |
| **F3** | **Delivery** (produce / place / take, O1–O5, P1) | [08 §2.2](./08-state-observability-principle.md) | How does σ **actually arrive** at the agent? | Constructive: three roles + invariant **I1** + five obligations + "only the pull shape is reachable" (**P1**) | A signal exists but never arrives (§6.1's idle eviction, G3's in-process queue, G12's slog-only drop) |

**In one line**: **F1 decides whether the mechanism may act, F2 decides whether it speaks, F3 decides
whether the speech arrives.** The three are a **conjunction** — if any one fails, the change is not done.

## 2. How they compose: the three systems as one decision procedure

```
For a mechanism M that changes the agent's world:

① F1  What is this migration's precondition? (the tables in 07 §3.3 / §3.11.3)
         does not hold → error + zero migration, and **say it correctly** (refusing ≠ being silent)
② F2  Who made the change?
         the agent itself → the tool result is the receipt; stop
         the harness      → a σ is mandatory, and it must be true (O1)
③ F3  Who places it, on D₁ (the call receipt) or D₂ (the turn entry)? (O2)
④ F3  Who takes it, and when? (O3: on the consumer's next read)
⑤ F3  Can it be lost between place and take? (O4: in-process state means yes)
⑥ F2  Silent when there is no δ? (C3 = O5)
```

## 3. Symbols (all three, with their code)

| Symbol | System | Meaning | Where it lives |
|--------|--------|---------|----------------|
| `World(t)` / `Belief(t)` | F2 | the world's real state / the agent's belief about it | `envSnapshot` |
| `δ` | F2 | one fact that the world changed | `sandbox.delta`, `sandbox.WriteThroughOutcome`, the diffs inside `envSnapshot` |
| `σ(δ)` | F2 | δ rendered as one sentence the agent can read | `signalsFor(delta)`, `Registry.writeThroughSignal`, `renderEnvDelta` / `Agent.signalEnvironmentChanges` |
| C1 / C2 / C3 | F2 | a channel it really reads / silence ≠ no change / an exception channel | the three exits + the witnesses in §5 |
| `produce` / `place` / `take` | F3 | produce / place / take | the role table in [08 §2.2.1](./08-state-observability-principle.md) |
| **I1** | F3 | the separation of the three powers: `place` belongs to the change side, `take` to the consumer | — |
| **D₁ / D₂** | F3 | the call receipt (a tool result) / the turn entry (the turn prompt) | the `lazyExecutor.Exec` result / the end of the `ContextBuilder` prompt |
| **O1–O5** | F3 | true statements / landing on a delivery point / the moment of taking / no loss / no noise | the obligation table in [08 §2.2.3](./08-state-observability-principle.md) |
| **P1 / P1′** | F3 | only the pull shape is reachable; every δ must land on D₁ or D₂ | proof by absence: the code has **no** `AgentInbox`-style interface |
| precondition / zero migration | F1 | `set(k,v)` requires `k∉dom`; a violation ⇒ error and no state changes | the branches in `LifecyclePool.WriteThrough` / `syncSnapshot` |
| R2 / R3 / R4 | F1 | zero migration / convergence / locality | `lifecycle_sync_contract_test.go` |
| inside / outside (emission) | F1 | recoverable vs compensatable-only | the boundary table in [07 §3.5](./07-formal-rootcause-and-fix.md) |
| left inverse `g(δ)=γ` | F1 | the inverse is produced **at the application site** | `edit_file`'s `old_string` match; the `<mcp-undo>` pattern |

## 4. Which document carries which system

| Document | Carries | Key sections |
|----------|---------|--------------|
| [01](./01-current-implementation.md) | the factual basis (the premise all three share) | §2 backend physical facts, §3.5 and §8 path mapping |
| [02](./02-semantics-and-architecture.md) | the **layer attribution** for F1/F3 | §1 the four layers, §5 the ownership declaration, §7 Musk's five steps |
| [03](./03-state-machine-and-timing.md) | F1's timing | §3 two registers, §7 the trigger conditions |
| [04](./04-incident-workspace-2026-09-17.md) | the evidence | §2 the evidence chain, §6 historical attribution (leases did not introduce it) |
| [05](./05-remediation-plan.md) | F1's **historical plan** | §2 P0 (corrected by 06), §5 the T1–T7 test matrix |
| [06](./06-cordis-review.md) | where F1's criteria come from | §1 the seven principles, §4 the corrected design |
| [07](./07-formal-rootcause-and-fix.md) | F1's **authoritative definition** | part 2 (domain and constructive proof), §3.3, §3.11.3 |
| [08](./08-state-observability-principle.md) | **F2 + F3** | **§1.1 the two-layer statement (the interface and its tension)**, §2/§2.1/§3 (F2), **§2.2 (F3)**, §5 the audit, §6 the checklist, §9.1 the exits |
| [09](./09-sandbox-lifecycle-audit.md) | F2/F3 applied cell by cell to the **sandbox lifecycle** | §2 the transition verdicts, §3 G1–G4 |
| [10](./10-harness-state-audit.md) | F2/F3 applied to the **whole harness** | §1 the whole picture, **§4 the gap table (with the duty column)** |
| [**11**](./11-change-register.md) | the **delivery index for all three systems**: every change ↔ code anchor ↔ UT ↔ live e2e ↔ deployment status | the row-by-row register (F1 #1–#4 · F2 #5–#18 · F3 #19–#20 · path/scope #21–#27) |
| [12](./12-lease-formal-design.md) | **F1's mechanism layer**: the contract induced from the four existing lease implementations (L1–L7), the as-built classification, the G25 counterexample, `session_turns`' instantiation and its four-layer placement | §3 the obligations, §4 the classification, §5 the counterexample (measured), §6 the design rules |

### 4.1 The inverse index: formal system → mechanism → the design documents it constrains

> The table above is "document → system"; this one is its **inverse**, and it is **the single entry
> point for formal reasoning**: find out which system constrains whatever you are about to touch, then
> read the design documents listed under that mechanism. Links inside this directory are relative;
> documents outside it use `../` or a repository path. **Register a new mechanism or document here first.**

```
F1 preconditions / zero migration (criteria from 06, authoritative definition in 07)
├── mechanism 1 · the reconcile (reconciler + BLOCKED + zero migration)
│     └─ 05 §2/§5 · 07 §3.3 · register #1–#2 · code: sandbox/lifecycle.go syncSnapshot
├── mechanism 2 · write-through + the delivery stamp (the host write also lands in the sandbox copy, stamped with the store's time)
│     └─ 07 §3.3/§3.11.3 · register #3–#4 · code: lifecycle.go WriteThrough
├── mechanism 3 · leases (one writer per key across replicas; the L1–L7 contract)
│     ├─ 12 here (induction + the G25 counterexample + session_turns' instantiation)
│     ├─ ../session-turn-integrity.md (A1 admission / A1.4 the fence / the IfIdle path)
│     └─ ../sandbox-pool-leases.md (the as-built cell: clauses U/A/I)
└── mechanism 4 · conditional writes (**decided: family B — versioned conditional writes**; the change list B1–B11 is in ../session-turn-integrity.md A3) → 10 §4 G24
      (the family is chosen by **L7, preconditions must be evaluable**: witness co-located with the effect's actor ⇒ B; only in-band inside a copy ⇒ A. See [12 §3/§3.1](./12-lease-formal-design.md))

F2 observability (defined in 08 §2/§2.1; **the layer statement and its tension, §1.1**)
├── mechanism 1 · the turn receipt (the baseline travels with the turn, not in the process) → 10 §4 G9/G20 · register #11
├── mechanism 2 · the unified environment-signal exit (identity files / config / cron / skills and tools)
│     └─ 08 §5 · 10 §4 G8/G10/G14 · register #10/#12/#13/#18
├── mechanism 3 · the tool result is the receipt (write-through, "replaced a different version", store-only, the unhydrated declaration)
│     └─ register #5–#9 · ../tool-output-limits.md (bounded + the delivered-frames clause)
└── mechanism 4 · the projection does not lie (three pad shapes: holder dead / holder alive / no fact)
      └─ ../session-turn-integrity.md A2/A2.1 · 10 §4 G5/G6

F3 delivery (defined in 08 §2.2)
├── mechanism 1 · separation of powers + the four exits (tool result D₁ / turn prompt D₂ / exception channel) → 08 §2.2/§9.1 · register #19–#20
├── mechanism 2 · durable carriers (a signal rides the instance/turn into the store, never an in-process queue)
│     └─ 09 §3 G3 · 10 §4 G3/G19 · register #9
├── mechanism 3 · the event log as the transport of record (cross-replica delivery + seq dedup) → ../chat-event-delivery.md
│     └─ boundary: live-only events (`content_delta`) are outside the log's guarantee — **placement** carries them → ../chat-event-delivery-placement.md
└── mechanism 4 · the client reads facts only (turnActive / queued{holder,ETA} / progress.id + the five states)
      └─ ../session-turn-integrity.md A4 · [tokenaissance-cloud › design/09-delegate-task-design.md](https://github.com/tokenaissance/tokenaissance-cloud/blob/develop/docs/fastagent/design/09-delegate-task-design.md)

Subsystem contracts (same style, their own alphabets; instances of the three above, not a fourth system)
├── W/P/O/T session turn integrity → ../session-turn-integrity.md §Invariant
├── U/A/I sandbox pool lease → ../sandbox-pool-leases.md
├── bounded output + delivered frames → ../tool-output-limits.md
├── path and scope identity (one path, one key) → 01 §3.5/§8 · 02 §5
├── the project-tree invariant (one project, one tree) → 10 §4 G17
├── the data-domain scope contract → ../configs-kv-scope-decision.md · ../configs-kv-scope-adaptation.md
├── identity and per-chatter routing → ../per-chatter-files.md
└── the protocol-compliance family (not F1–F3) → ../mcp-oauth-design.md · ../issues/ext-skills-conformance-checklist.md

**Single source / one expression family** (**a peer contract, not a fourth system**; its judgement form is
"how many expressions does this one fact have", unlike any of F1–F3. Basis: §7's verdict on S1/S2/S3 —
"recompute from the authority, do not remember" is F2's dual, i.e. a C)
├── one rule must not have a second expression (layout table / writer folding, each one pure function) → 10 §4 G23 · code `workspace/scope.go` `ScopeSegments` / `WriteScope`
├── one input must not have two sources (the heartbeat file: the prompt reads the store, the tick read the disk) → 10 §4 G14 · code `agent/heartbeat.go` `loadHeartbeatTasks`
├── one guard must not be written twice (the skill-mask merge) → 10 §4 G16 · code `setup/handlers.go` `mergeSkillEntry` / `mergeSkillEntries` / `cloneSkillEntries`
├── a judgement must not invent a second copy (ask first: does this fact already have an owner?) → 08 §2.2.3 landing 2 · 10 §4 G20 (the turn receipt carries the whole snapshot; the counterexample is the `cfg_seen` row)
├── the scope contract (blob authoritative / KV a faithful mirror — a **temporary invariant during migration**; the exit condition is flipping authority) → ../configs-kv-scope-decision.md · ../configs-kv-scope-adaptation.md
└── one exit per category (within one package, mechanisms must not each assemble their own sentence for the model) → 08 §9.1

```

> **The difference from the "subsystem contracts" block above is the cut**: that block cuts by **domain**
> (who may touch this key); this family cuts by **form** (how many expressions does this fact have).
> So G17/G22/G23 appear in both — the gap table records the domain, this records the form.
> **One boundary left to decide**: §7 draws the A/C line as "a C row asks the same question in the same
> form as A", and this family's form is different. Filing it as a C follows §7's verdict on S1/S2/S3,
> not that line. See the note in §7.

```

Covered by none of them → §7's bucket E (the F4 candidate: concurrency and visibility — not adopted)
```

## 5. Obligation ↔ gap ↔ witness (the checkable index)

| Obligation | The gaps that violated it | The tests that pin it |
|------------|---------------------------|-----------------------|
| **O1** true statements | ~~G5~~ (a false σ), ~~G6~~ (never rendered), ~~G8~~, ~~G10~~, ~~G11~~ (both halves), ~~G4~~ (signal half), ~~G19~~; the `{baseDir}` diagnostic's trigger (register row 39) | `TestWriteFileSignalsUncheckedReplacement`, `TestEnvSignalCarriesIdentityFileChanges`, `TestCronFingerprintIgnoresRunBookkeeping`, `TestStdioClientHandsNotificationsToTheHandler`, `TestServerNotificationArrivesOverTheStandingStream`, `TestE2BLiveUnhydratedFactSurvivesPodHandoff`, `TestBaseDirTokenInABundledFileIsNotAReaderDifference`, `TestBaseDirWarningListsEveryCarrierNotOnlyTheManifest`, `TestCatalogHandlerCarriesTheCodesAndTheWarnings` |
| **O2** placement | ~~G12~~, G1/G2 | `TestDeferredTurnsAnnouncesADroppedScheduledTask`, `TestEvictionSignalReachesNextToolResult` |
| **O3** the moment of taking | no violation; **G13 is its positive instance** (a pull σ: the criterion is recomputable, so the consumer's next read IS the delivery point) | `TestBashOutputTool_DrainsTailOnExit`, `TestSandboxJobOutputReturnsDeltaThenStatus` |
| **O4** no loss | ~~G3~~, ~~G9~~, ~~G20~~ | `TestEvictSignalOutlivesThePoolThatProducedIt` (delivered by a different pool instance), `TestReplacedSandboxNoteRidesTheCallThatFoundIt`, `TestRunReceiptStampSurvivesAReload` |
| **O5** no noise | — (no "spoke without a change" instance yet) | `TestExecIsQuietWhenNothingChanged`, `TestWriteFileStaysQuietOnASharedBackend` |
| **O7** a delivered fact whose meaning changed | the 2026-09-21 egress audit (no G number: recorded as [11](./11-change-register.md) row 38) | `skills-list-chain.test.ts` (the delivery point) + `catalog.test.ts`, `skills-service.test.ts`, `policy.test.ts`, `tools-service.test.ts` |
| **O6** an absence must speak (promoted [08 §10.3](./08-state-observability-principle.md)) | the 2026-09-21 egress audit: two consumers of one fact, and only one could see it — they read different producers (no G number: [11](./11-change-register.md) row 40) | `skills-list-chain.test.ts` (MCP) + cloud `fastagent-proxy-route.test.ts` (**the panel's delivery point**) + `skills-service.test.ts` (one partition, two projections) |
| **O8** a switch must have a reader on the production path, and its promise must be isomorphic to its effect (promoted [08 §10.8](./08-state-observability-principle.md)) | register rows 49 (`piiScrubbing`), 51 (`skillsLearner`), 52 (`memory.autoPersist`), 53 (`memory.fts` — resolved by deletion), plus the cloud panel's auto-remember switch, which is the render half (2026-09-22) | `TestThePiiScrubbingRowReachesEveryAgentProvider`, `TestTheSwitchRedactsEveryModelCallTheTurnMakes`, `TestTheSkillsLearnerRowReachesTheLearnerAndWritesThroughTheSingleWriter`, `TestTheSkillsLearnerRowReachesTheSingleWriter` (cloud path), `TestTheMemoryRowIsTheDefaultLayerAndThePerAgentFlagOverridesIt`, `TestTheMemoryRowIsWhatTurnsAutoPersistOn`, cloud `src/__tests__/fastagent/auto-persist-inherited-state.test.tsx`, and — for the writer half, all four rows — `TestRuntimePage_CanSetMemoryAndSkillLearning` plus web `src/__tests__/runtime-settings-memory-learning.test.tsx` |
| **O9** an EXTERNAL consumer must be given a delivery point — or the contract must name who reads, and when (promoted [08 §10.10](./08-state-observability-principle.md), 2026-09-26) | the 2026-09-26 MCP surface audit (finding F1): the state design was pull-only, and the checklist's item 4 assumes a reader *inside* the harness ("the agent is reading something") — an out-of-process MCP client has no forced read, so "the event happened and the client never learns" was a design consequence rather than a bug (cloud `docs/mcp-task-submission.md` §14.8; register row 65) | pull side: cloud `mcp-surface-e2e.test.ts` (the tool reply **is** the delivery point) + the `read_task` behaviour witnesses; push side: the resource-subscribe notification witness (to be written with the channel). The falsification must redden the **delivery**, not only the rule (00 §5.1) |
| **F1** preconditions / zero migration | ~~the incident, D~~ | `TestSyncContract_StoreEditIsNotOverwritten`, `TestSyncContract_SecondReconcileWritesNothing`, `TestSyncContract_DomainUnchanged`, `TestE2BLive*` |
| **F1** boundary (inside / outside) | **G4** (a deletion is irreversible; no snapshot) | — (a missing witness is itself part of that gap) |

> D₃ was promoted in [08 §10.2](./08-state-observability-principle.md) after this table was written
> and has no row here yet. O6 was promoted the same way and **got its row on 2026-09-21**, when the
> egress audit produced its second consumer and a witness for it; O7 was added with its row, and **O8
> got its row on 2026-09-22** when the switch audit found four rows whose reader lived in a constructor
> no production path calls. So the index does not drift further.

### 5.1 The witness column has two halves (2026-09-21, brought back from the cloud re-audit)

> **This changes only the third column above. It adds no obligation, no gap family, and no design
> work.** It is a rule about *what counts as pinned* — a **verification-side** rule, not a statement
> about the systems. Nothing in F1–F3 or O1–O7 changes meaning.

That column can answer exactly one question today: **was the rule tested?** But *producing* a fact and
*taking* it are two different events, and one obligation usually has several delivery chains (a
render, a store, the wire, another view). That leaves a state where everything is green and the user
still sees nothing: the rule is tested, the copy is in place, and the one **call site** that carries
the fact to its destination has never been touched by a test.

The criterion:

> **An obligation has landed ⇔ there is a rule witness, *and* every delivery point has one; and the
> falsification must be able to redden the delivery point's test** — reddening only the rule witness
> is no evidence about that point at all.

Where it applies (otherwise it degenerates into "write more tests"): only to obligations that **send a
fact out** (O1–O7 and D₃'s family). Wire-shape and field-spelling contracts (the C family) have no
separate delivery point — **the rule witness *is* the delivery-point witness**.

**The retired failure shape (this round's instance)**: `toolRowLabelKey` (cloud
`src/features/chat/turn-state.ts:143`) is the rule witness, and its delivery point is the call site at
cloud `message-list.tsx:61`. That call site used to pass only the local `turnOver`, so the render layer
had **zero hits** for `grep tool_running_elsewhere` and the row always read "interrupted", while the
group header — an independent derivation — was right. The tests asserted the header, so the suite was
green. The same `grep` still returns zero hits today, with the opposite meaning: the call site
now hands the fact down, and `describe('the live-turn fact reaches the tool ROW')` has four cases
(including "an unusable fact says unknown in the row AND does not let the header say stopped"). One
grep fact, two opposite meanings — the difference is only whether the fact reached the point.

(This is the other side of [08 §6.1](./08-state-observability-principle.md): that section states
"a signal that is *produced* is not a signal that *arrives*" from the producing side, and this is its
mirror on the verification side.)

### 5.2 The reader may be outside the harness: O9 (2026-09-26, promoted from the MCP surface audit)

Every clause so far silently assumed the reader is **inside** the harness: item 4 of 08 §6 asks "when this
change happened, was the premise *the agent is reading something* true?", and the answer's remedy is
always "attach it to the next tool result / put it in the next prompt". That premise does not hold for an
**external consumer** — a process outside the loop, with no tool result to attach anything to and no
prompt of ours to ride. An MCP client is exactly that reader: it can call, it can subscribe, and it can
also simply never ask again.

> **O9 — an external consumer must be given a delivery point, or the contract must name who reads and
> when.** "It can pull whenever it wants" is a delivery point only if the contract says the pulling is
> the mechanism; silence is not delivery for a reader whose loop never forces it.

The shape that satisfies it (worked out on the MCP task surface, cloud
`docs/mcp-task-submission.md` §14.2/§14.8, and now the shape this repo's MCP egress uses):

1. **a pull point that is authoritative** — a call the consumer makes and whose reply carries the fact
   (for MCP: `read_task`'s reply; the reply *is* the delivery point, so the C-family exemption in §5.1
   applies: its rule witness is also its delivery-point witness);
2. **a push point that is only a hint** — a native notification the consumer subscribed to, carrying
   "something changed", never the fact itself, so that push can never become a second source
   (for MCP: `resources/subscribe` + `notifications/resources/updated`, then re-read);
3. **reconciliation by re-reading** — notifications are best-effort (lost, duplicated, or delivered
   after a reconnect), so correctness must not depend on them: one pull after any gap restores the
   consumer's picture. A design that needs `notifications/progress` to be reliable is a design with no
   pull point.

No obligation is owed by the *harness* until something external consumes it: O9 binds a surface that
advertises itself to an outside reader, and its witness is a test that **the reply/notification actually
left** (reddening only the rule witness is no evidence about the delivery point — §5.1). Its first
instance and the reason it exists are register row 65.

## 6. What is still open, sorted by formal system

| Item | Belongs to | Status |
|------|-----------|--------|
| ~~**G11** the HTTP side of MCP notifications~~ | F2 · O1 (the transport had no channel at all) | **fixed 2026-09-22 (register row 45)**: the client opens the spec’s standing GET stream, so on HTTP too a server’s unprompted change lands in the same sink → gate → rebuild path the stdio half uses. It could only land after row 44, which gave the stream an owner. **What is left of the same gap is not transport-specific**: a server that changes its list *without* announcing is invisible on both transports (10 §3.4 boundary 3) — seeing that needs a pull (re-list per turn), which is a decision, not a defect fix |
| **G4** the *attribution* of a sandbox-side deletion | F2 · O1 (**fixed as far as "the fact is stated"**) + F1's boundary (an irreversible action with no snapshot) | **decided: no attribution (2026-09-18)** — the consequence is already delivered, and a manifest would buy only the cause at thousands of rows per hydrate; see the decision log in [05 §8](./05-remediation-plan.md) |
| **G7b** uploads/deletes and the live sandbox | **not F1–F3**: write-path symmetry | **upload half: decided a (no write-through, "the panel is the file library")**; **delete half: decided d1 and fixed** (write through to the live sandbox, never creating one) |
| ~~**G21**~~ the panel delete was a silent no-op (the path/scope convention applied twice) | **belongs to F1** ("one path, one key") | **fixed (2026-09-18)**: Fix 0 (delete uses the download endpoint's path convention) + d1 (also drop the live sandbox's copy), landed as a pair; both halves pinned on real E2B (without d1 it comes back; with d1 it does not) |
| ~~**G17**~~ a project's "one file tree, many containers" (the preview container was addressed wrong, sibling containers missed writes, and the sync wrote back into the chat subdir) | **not F1–F3**: a scope invariant | **decided + fixed (2026-09-18: G+H+A)**: the preview container is addressed by project (G); writes and deletes are broadcast to every live container of the project (H); the **sync write-back is collapsed to the project root** (A, `syncStoreScope`); **per-chat shells are kept**. No migration: copies produced before the change remain in the store |
| ~~**G22**~~ the write-through's mtime stamp silently missed in project sessions (it read the wrong store scope) | **belongs to F1** (the third instance of "one path, one key") | **fixed (2026-09-18)**: the writer hands the store scope down (`sandbox.StoreScope`); measured on real E2B, whole-object reads for that path went **1 → 0**; all three defects at this seam (G21 / G17-A / G22) are now closed |
| ~~**G15**~~ `notifications/initialized` is never sent | **not F1–F3**: protocol compliance | **fixed (2026-09-22, register row 48)**. The live check it was gated on made it more than compliance: the reference SDK server answers `tools/list` with 12 tools without the notification and 13 with it, so a client that skipped it said "these are the server's tools" about a list the server had not finished building. Both transports send it now, and the negotiated revision is read (Streamable-HTTP revisions also get `MCP-Protocol-Version`) |

**Closed the same day (kept here so the index stays comparable; details in [10 §4](./10-harness-state-audit.md))**:
G1–G3 (delivery point / durable carrier), the signal half of G4, G5/G6 (σ telling lies), G7a (the
divergence is named), G8/G9/G10 (outside writers), **both halves of G11** (stdio 2026-09-18, HTTP
2026-09-22, row 45), G12 (drops leave a trace),
G14 (two sources), G16 (the mask written back), **G18** (01 §8 path resolution), **G19** (the unhydrated
declaration lost on an instance hand-off), **G20** (the environment baseline moved into the turn receipt;
`envTracker` deleted). Of these, **G13 was reclassified as "not a defect"**: it is a pull-shaped σ whose
criterion is recomputable (place 1), not an F3 gap.

That table is itself a classification result: **no row left in it is a defect in the formal sense** — the
G11 half that was (a transport with no channel) is closed as of 2026-09-22, and G4's remaining half was a
decision, not a hole; the rest belong to different families (a product decision, a scope invariant,
protocol compliance) and should not be booked as "observability left unfinished". The one thing this pass
moves *into* that class is not a numbered gap: a server that never announces a change is invisible on
every transport (10 §3.4 boundary 3), and only a pull can see it.

**One cell added on 2026-09-19 (F1's family, not F2/F3)**: **G25** — the sandbox lease's fencing token
`epoch` resets to `1` on every takeover (`internal/store/sandbox_leases.go:72`/`:81`), so after the same
`owner` changes generation a delayed release can delete the new generation's live row (measured:
`released=true`). It violates **L4(c)** of [12 §3](./12-lease-formal-design.md) (the token must be unique
per acquisition); the fix is one clause (`epoch = epoch + 1` in the claim branch) — **landed 2026-09-19 in the working
tree**, witnessed by `TestSandboxLeaseEpochNeverResetsAcrossTakeover` (falsification run for real). The
same section states how that obligation lands on `session_turns`, whose **A1 is fully landed
(working tree)**: the four `session_turns` methods in `internal/store`, the port in
`internal/agent/sessionlease.go`, the adapter in `internal/gateway/sessionlease.go`, both admission
points and the IfIdle verdict wired onto it, and the fence (`…Fenced` write statements).

**Two cells added on 2026-09-22 (neither is F1–F3)**: **register row 44** — the *ownership* of a dropped
user space’s MCP clients. It was the piece that made G11’s HTTP half hard to close: both remedies (an SSE
stream, a periodic re-list) create a resource that outlives the agent object, and nothing released one. A
drop now retires the space and a sweep releases its clients once it has been retired for five minutes with
no turn running or waiting (10 §3.4, item 2), so the standing channel can land in a hole that has an owner.
Then **register row 45** used that owner to land the channel itself: HTTP opens the standing GET stream and
reads its frames live, which is why the G11 row above is struck through.

> **What actually remains (updated 2026-09-22)**: the HTTP half of G11 — the last item here that was a
> defect in the formal sense — **is closed** (register row 45). What replaces it is not a transport
> feature: a server that changes its list without announcing it cannot be seen by *any* push channel, so
> only a per-turn re-list would catch it (10 §3.4 boundary 3) — recorded as an open **decision**.
> G15 (`notifications/initialized`) belonged to the same MCP family and was closed on 2026-09-22 (row
> 48): the live check it waited on showed the notification is the difference between reading a server's
> capabilities and reading part of them. **The one non-defect worth recording**: the duplicate copies under the chat subdirs that were
> produced before decision A are still in the store (no longer refreshed, and nobody cleans them) — a
> one-off cleanup script: `fastagent/scripts/workspace_project_chat_duplicate_cleanup.py`
> (`--selftest` needs no deployment; it only lists a copy for deletion when the same bytes survive at the project root — and it should run **after** A ships, or the old sync keeps recreating them).; see the "closed" list
> below and [10 §4](./10-harness-state-audit.md).

## 7. The full inventory: formal systems ↔ mechanism layer ↔ subsystem contracts ↔ models ↔ unformalised

> This section answers "what formalisation actually exists in this system today". Four buckets; the
> criterion is the **question and the shape of the judgement**, not how many symbols there are:
> **A is a formal system, B is a formal system's mechanism layer; C is the same style but an *instance*
> of A inside one subsystem; D is merely a model used as A's domain; E is covered by none of them.**
>
> The A/C line: every row of A answers a **different question** with a different judgement shape
> (declarative / universal / constructive); a row of C asks the *same* question in the *same* shape and
> only changes the key and the obligations. Hence **the bar for a new formal system = a new question +
> a new judgement shape**; otherwise it is a mechanism or an instance of an existing one — a lease is
> exactly F1's mechanism layer ([12](./12-lease-formal-design.md)).

| Bucket | Members | Where | Why this bucket |
|--------|---------|-------|-----------------|
| **A** formal systems | **F1 / F2 / F3** | §1; defined in [06](./06-cordis-review.md) · [07](./07-formal-rootcause-and-fix.md) · [08](./08-state-observability-principle.md) | three different questions + three different judgement shapes; missing one loses one failure mode |
| **B** mechanism layer | the lease contract **L1–L7** + the per-cell classification of the four existing implementations | [12](./12-lease-formal-design.md) | it still answers F1's question; its verdict is F2's δ/σ and its delivery is F3's duty ⇒ **not a fourth system** |

**C. Subsystem contracts** (falsifiable clause tables in the same style, each with its own alphabet — all instances of A)

| Contract | Defined in | Clauses | Where the code cites it | Witness |
|----------|-----------|---------|-------------------------|---------|
| session turn integrity | [../session-turn-integrity.md](../session-turn-integrity.md) §Invariant | **W** single writer · **P** call/reply pairing · **O** ordering · **T** a projection that does not lie | `internal/session/manager.go:86`, `internal/agent/loop.go:2322`/`:2526`/`:3279` | `turn_queue_test.go:97`/`:148`, `tool_recovery_test.go:215`, `TestConcurrentWebAndCronTurnSerialize` |
| sandbox pool lease | [../sandbox-pool-leases.md](../sandbox-pool-leases.md) | **U** naming authority · **A** no rebuild per call · **I** the row names the live instance | `internal/sandbox/lease.go:57-61` | `lease_rebuild_test.go:421`/`:469`, `TestE2BPool*` |
| bounded tool output | [../tool-output-limits.md](../tool-output-limits.md) | a result is bounded as it leaves its producer + the **delivered-frames clause** | `internal/sandbox/e2b_executor.go:1161` | `TestE2BExecClockHints` |
| event delivery | [../chat-event-delivery.md](../chat-event-delivery.md) · [../chat-event-delivery-placement.md](../chat-event-delivery-placement.md) (boundary) | the event log is the transport of record: D1–D5 decisions + R1–R4 falsifications. **Boundary**: live-only events (`content_delta`) are outside that guarantee — **placement** (session-key affinity) carries them, and it cannot reach a cron/goal producer, whose pod is decided by a lock race | the SSE subscription / tail poll | — |
| path and scope identity | [01 §3.5/§8](./01-current-implementation.md) · [02 §5](./02-semantics-and-architecture.md) | one path, one key (the G18/G21/G22 family) | `scopeSessionID`/`wsPath`, `sandbox.StoreScope` | `TestE2BLiveOnePathIsOneKey` |
| project tree invariant | [10 §4](./10-harness-state-audit.md) (G17) | one project, one tree, many containers, writes broadcast | `LiveProjectExecutors`, `syncStoreScope` | — |
| MCP OAuth security clauses | [../mcp-oauth-design.md](../mcp-oauth-design.md) | state is one-shot (`Take` reads-and-deletes) + PKCE S256 + TTL + bound to userID | `usecase/complete`, `pending_store.go` | `internal/mcp/oauth/...` |
| MCP conformance (**protocol-compliance family**, not F1–F3) | [../issues/ext-skills-conformance-checklist.md](../issues/ext-skills-conformance-checklist.md) | the spec's MUSTs ↔ the egress implementation; **declaring is promising** | `server/discover`, `skills/list` | that checklist's "status" column; G15 closed 2026-09-22 (row 48) — the handshake now says `initialized` and reads the negotiated revision (G11’s HTTP half landed 2026-09-22, row 45) |
| **single source / one expression** | [10 §4](./10-harness-state-audit.md) (G14/G16/G20/G23) · [08 §2.2.3](./08-state-observability-principle.md) (landing 2) · [08 §9.1](./08-state-observability-principle.md) | **one fact has exactly one expression**: one source (G14) · one guard (G16) · one rule (G23) · a judgement reuses its existing owner (§2.2.3 landing 2) · one exit (§9.1) | `agent/heartbeat.go` `loadHeartbeatTasks`, `setup/handlers.go` `mergeSkillEntry(s)`/`cloneSkillEntries`, `workspace/scope.go` `ScopeSegments`/`WriteScope` | `TestHeartbeatReadsWhatThePromptShows`, `TestMaskedGlobalSkillSecretKeepsTheStoredValue`, `TestScopeSegmentsIsTheLayoutTable`, `TestAWriterScopeIsTheScopeItsKeysLandIn` |

> **Note (2026-09-20, surfaced when the single-source family entered).** The dividing line stated at the top
> of this section ("a C row asks the **same question in the same form** as A") does **not** cover the last row
> of the table — its judgement form ("how many expressions does this fact have") differs from all three of
> F1–F3. Filing it as a C follows §7's verdict on S1/S2/S3 ("recompute from the authority, do not remember"
> is F2's dual), not that line. So one distinction is **left undecided**: either accept "a different form but
> the same origin (an instance of one discipline)" as a C, or give it a bucket of its own. **Not decided now**:
> there is only one such family, and the house rule is to extract on the third variant that proves the seam.

**D. Models used as domains** (not formal systems; they are the objects A's judgements point at)

| Model | Where |
|-------|-------|
| two registers (docker: one register / e2b: two registers + one-way write-back) | [03 §2/§3](./03-state-machine-and-timing.md) |
| the sandbox's 7-state lifecycle and its per-transition verdicts | [09 §1/§2](./09-sandbox-lifecycle-audit.md) |
| `delegate_task`'s four states → five (client side) | [tokenaissance-cloud › design/09-delegate-task-design.md](https://github.com/tokenaissance/tokenaissance-cloud/blob/develop/docs/fastagent/design/09-delegate-task-design.md) |
| the four-layer mapping (Clean Architecture) | [02 §1](./02-semantics-and-architecture.md) · [../mcp-oauth-design.md](../mcp-oauth-design.md) §1 |
| the MCP OAuth flow states (pending → code → tokens) | [../mcp-oauth-design.md](../mcp-oauth-design.md) |

**E. Covered by none of them yet** (an empty bucket is not the problem; **not knowing it is empty** is)

| Gap | What carries it today | What would absorb it |
|-----|----------------------|----------------------|
| the store's concurrent-write semantics: what does one `Put` guarantee under concurrency | the implicit assumption of last-writer-wins | possibly a **genuinely new system** (a new question: visibility and overwrite semantics under concurrency); G24/A3 is its first instance |
| the `session_key` minting race (the first IM message at two replicas mints a key on each) | nothing | a boundary of F1 ("one key, one migration"); see [12 §6](./12-lease-formal-design.md) |
| retention / GC policy (who deletes an object, when) | a one-off cleanup script + human judgement | undecided; adjacent to F1's "zero migration" |
| **process residency: what may the harness keep in memory, and for how long** | implicit "until the process dies". Audited 2026-09-19 ([10 §10](./10-harness-state-audit.md)): three tables grew with history — `session.Manager.sessions` in the running build (G26), `tools.shellManager.shells` and `tools.sandboxJobs.live` (G27) | **not a new system**: this is F2's dual (S1/S2/S3 — *recompute from the authoritative source; do not remember*), i.e. a C, not a new question. The audit test itself is now part of 10 §10 |

> **Note (the F4 candidate, 2026-09-19: analysed, deliberately not adopted).** The candidate is
> **"what one operation means / concurrency and visibility"**: the question is not "who may act" (that is
> F1) but "when two operations touch one key concurrently, which value is visible, to whom, and when";
> the judgement shape is **membership of an execution history** (linearizable / read-your-writes /
> monotonic reads / snapshot reads), which none of F1/F2/F3 has ⇒ formally it qualifies. The same seam
> already has **four** instances: `LocalFS.Put` is an in-place `O_TRUNC` write (a reader can see half a
> file, `internal/workspace/localfs.go:88`) · `S3.Move` calls itself "Not atomic"
> (`internal/workspace/s3.go:165`) · G24 (`Put` has no precondition; on 09-18 the same deliverable was
> written twice, 15 348 → 11 492 bytes) · A3's three-option ladder.
>
> **Decision: not adopted.** The root cause is not that someone forgot to write the semantics down but
> that **the original design's conditions changed** (see §7.1): it was born single-process with local
> files (sessions as JSONL, `workspace/` merely a template directory), where "what a write means" was
> free and needed no statement; on 2026-04-20 `950070b` (cloud-ready / stateless gateway) introduced the
> S3 backend, multiple writers and `LifecyclePool` in one commit (the very commit
> [04 §6](./04-incident-workspace-2026-09-17.md) names as the defect's birth), turning implicit
> guarantees into obligations nobody had written down. If it is ever adopted, it lands as the
> "per-backend axiom table" in [01 §2](./01-current-implementation.md) (zero code) — **not** as a 13th document.
>
> Conditions that reopen it (any one): ① F1's criterion fails **because a reader did not see a whole
> object** (e.g. hydrate reading half a file on LocalFS, measured); ② a **second** silent overwrite
> (today there is exactly one, 09-18); ③ a **third backend** needing the same axioms.

> Division of labour with §6: §6 lists what is unfinished **inside** the three systems (G4's
> attribution half…); this section's E bucket is what **no system covers yet**. How to use it: before
> reading a piece of code, ask "which system constrains it"; before writing a mechanism, answer §6's
> checklist; and if the answer is "none of them", then either it is a C (a new instance of the same
> shape) or we are **proposing a fourth formal system** — which requires producing both a new question
> and a new judgement shape.

### 7.1 The original design (why these semantics were never written down)

> This subsection answers "was it an oversight at the start?". **It was not an oversight — the conditions
> changed.** The evidence, in order:

| When | Event | The semantic premise at the time |
|------|-------|----------------------------------|
| 2026-03-09 | `d04a132` the MVP; the same day `500b793` moved `DESIGN.md` (184 lines) **out of the repo and into .gitignore** | The original design states **"Minimal / Go-native / Message bus (channels) / Files as memory"**; sessions are **"append-only + JSONL file persistence"**; `workspace/` is merely a **template directory** (AGENTS.md/SOUL.md/USER.md/TOOLS.md). **One process, one host, one writer** ⇒ atomicity, visibility and versioning needed no statement |
| 2026-03-17 | `0cdb64e` "pluggable storage backend (file + database)" | the first **two backends**; the difference was still "where the bytes live", with no concurrent writers |
| 2026-04-20 | `950070b` "cloud-ready architecture — stateless gateway, per-key scoping" | one commit introduced the **S3 backend + the `LocalFS`/`S3` pair + `LifecyclePool` (hydrate-on-create / flush-on-evict)**. Multiple writers, two physical copies and three backends became true **on the same day**, and not one of the original design's implicit guarantees was restated |
| 2026-09-17 | deliverables silently reverted in production | [04](./04-incident-workspace-2026-09-17.md): the defect's birth is `950070b`; the lease was only an amplifier |

Two traces still visible today:

1. Today's README §Architecture still says **"Output files | Application | Your app / S3"** — in the
   original design the produced files **did not belong to the runtime**; `internal/workspace` appeared
   only once the runtime took that over (`950070b`).
2. The only "laws" the original design did write down were **prompt-cache-friendly append and placement
   rules** ("Session messages are append-only", "Variable runtime info placed in user messages") — in the
   same document, **the invariants that were written down survived**; the ones that were not (what a write
   means) silently expired when the conditions changed. That is exactly why this document set (F2/F3 and
   the checklist in [08 §6](./08-state-observability-principle.md)) exists.


> **Patches (2026-09-19, brought back from auditing the cloud side)**: the three systems need four additions
> for the case where **the consumer is a user interface** — **O1′** (a state σ must carry its expiry so the reader
> can degrade to unknown; an **event σ** is immutable and must not be erased by an expiry rule), **D₃** (for a UI
> the delivery point is *a render*: reading its cache/component state; O3/O4 must be restated on D₃), **O6**
> (the **revocation** of a δ must be as visible as its arrival — "absence is invisible" is promoted from corollary
> to obligation), plus two orthogonal C-family contracts: **C1 one fact, one wire shape** (every exit for a fact
> shares one schema; the root fix is a shared type) and **C2 witnesses come from the producer's real payload**
> (contract-test fixtures must not be hand-written). Full statements in [08 §10](./08-state-observability-principle.md).

## 8. In one sentence

> Three formal systems answer three different questions, and missing any one of them produces its own
> failure: **without F1** a mechanism overwrites what someone else wrote; **without F2** the agent
> reasons in a world that no longer exists; **without F3** a signal is produced and never arrives.
> A change is only designed when all three questions can be answered.
