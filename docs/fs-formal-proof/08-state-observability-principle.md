# 08 · The state observability principle (a harness design constraint)

> Status: principle + field audit · last verified: 2026-09-18
> Origin: the 2026-09-17 deliverable-revert incident ([04](./04-incident-workspace-2026-09-17.md)) and the
> six rounds of repair that followed, during which the same judgement kept reappearing until it was
> distilled into this principle. Filesystem sync is its first application, but what it constrains is
> the whole harness (see the audit scope in §5).
> Prerequisite reading: [07 §3.11](./07-formal-rootcause-and-fix.md) (how the mechanism converged to four).

## 1. The principle

> **A state change inside the harness must be perceivable by the agent.**
> **Otherwise the agent reasons about a world that no longer exists — and has no way to notice.**

This is not a restatement of "write logs". The reader of a log is an operator; **the reader of this
principle is the agent**, so the signal has to appear in a channel the agent actually consumes
(tool results, the next turn's context) — not in slog.

### 1.1 The shape of this principle: two layers, not two rules — and real tension between them

The harness **keeps no representation** — it recomputes from what is there every time (baselines
deleted, fallback copies deleted), because **anything it remembers goes stale**; what was deleted is
not state but a memory that never expires and is taken to be oneself.

The agent **must have a representation** — it has a context, which is a world model by nature;
without one it cannot act.

> **This principle is the interface between the two**: a representation-free harness keeping a
> representation-based agent from drifting.

It is therefore not "one extra safety requirement" but the **reason the asymmetry exists**: one side
deletes its model, the other cannot do without one, and between them there must be a channel that
says "what you remember is no longer true".

**But there is real tension between the two layers — the split is not that clean.** The wording above
is easy to read as "each side minds its own business". In fact two oppositely directed requirements
are welded into one system. Four points, each with a landing place inside this document:

1. **Opposite directions**: the harness **must forget** (anything it remembers goes stale), the agent
   **must remember** (without a representation it cannot act) — inside one system, "remembering" is
   simultaneously a **duty** and a **failure source**.
2. **Different sources of judgement**: the harness judges from the two replicas themselves (the scene,
   §2), the agent judges from its context (a model) — the two judgements **can each be right and still
   contradict each other** (whether one may witness for the other: §2.2.3 landing 2 and
   [12 §3 L7](./12-lease-formal-design.md)).
3. **The interface is one-way only**: P1′ says there is no push (§2.2.4) ⇒ the harness cannot "tell" an
   agent that is mid-inference; it can only **leave it there** for the agent to take at its next
   entry point (the only landings are D₁ and D₂).
4. **Who pays**: the more the agent's memory economises (fewer reads, fewer checks), the heavier A2's
   load — **this principle is not a free interface; it is the channel that pays the agent's memory bill.**

**The tension is not resolved, only declared.** This principle does not promise to abolish the
asymmetry; it promises to make the asymmetry **observable** — to give "what the agent remembers is no
longer true" **a landing place** (D₁ / D₂). The test is one sentence: **the moment someone proposes
"let the harness remember a little too, to save a round trip", that line has already been crossed** —
that is exactly the pod-local baseline deleted after the 09-17 incident
([07 §2.4](./07-formal-rootcause-and-fix.md) / §3.11.1), i.e. "letting the side that keeps no books
start keeping books".

## 2. The formal statement

Let the agent's belief about its own world at time `t` be `Belief(t)` — everything it holds to be
true about "what the files contain, which skills exist, what is remembered, which tools it can
call". Let the world's real state be `World(t)`.

Actions of the harness change `World`. The principle requires:

```
∀ change δ : World(t) → World(t')
    if δ was triggered by the agent's own action
        then Belief is updated through that action's result (the tool result is the receipt) — satisfied by construction
    if δ was triggered by the harness itself (sync write-back, eviction flush, background compaction, memory update, skill refresh, …)
        then there must exist an agent-consumable signal σ(δ) such that
              ¬∃t'' : World and Belief stay inconsistent before σ is delivered and the agent cannot notice
```

**The cost of violating it is measurable**: in the incident the user saw three files reverted while
the agent reported "restored" in two consecutive turns — because nothing told it that the store had
been written back. Its `Belief` stayed on a `World` that no longer existed, so every later inference
("what do I do next") rested on a false premise. Note this is worse than losing a file: **losing a
file is data loss; a belief/reality mismatch is inference corruption**, and the latter spreads into
every subsequent decision.

### 2.1 Formal symbols ↔ code identifiers

The code no longer uses `report` / `notice` as mechanism names: **δ is the fact, σ is the sentence
the agent reads**, and identifiers follow the same symbols so reading the code needs no second
translation.

| Formal symbol | Meaning | Code identifier |
|---------------|---------|-----------------|
| `World(t)` / `Belief(t)` | the world's real state / the agent's belief about it | `envSnapshot` (the per-turn sample, i.e. the `World` the harness believes in); `delta` (the world change one sync observed) |
| `δ` (delta) | **one fact that the world changed**, structured and accumulable | `sandbox.delta{moved, blocked, storeOnly, problem}` (`lifecycle.go:650`), `sandbox.WriteThroughOutcome` (`lifecycle.go:1477`) |
| `σ(δ)` (signal) | δ turned into **one sentence the agent can read** | `signalsFor(delta)` (`lifecycle.go:1114`), `Registry.writeThroughSignal` (`file.go:997`), `renderEnvDelta` / `Agent.signalEnvironmentChanges` (`env_changes.go:96`, `:460`) |
| exit | the single delivery point for σ inside one category | the result of `lazyExecutor.Exec` (appended), `Registry.workspaceSignalExit` (`tools/registry.go:1078`, append/prefix), `ContextBuilder.SetEnvironmentSignal` (`context.go:89`, appended to the end of the system prompt) |
| queue | δ happened but there is no delivery point right now | **no in-process queue any more**: the `sandbox.SignalStore` port with `parkSignal` / `takeSignals`, implemented by `gateway.sandboxSignalStore` (a scope-keyed `configs_kv` row: across processes and replicas, deleted as it is delivered). It carries only the facts that **cannot be recomputed** (paths an eviction sync wrote into the store); refusals and failures are not queued |

> **Anchors re-checked 2026-09-22.** Every `*.go:NNN` anchor in this edition was driven back against the
> tree for existence and range; this table's row is the one that had rotted on content too: `delta` had gained a field
> (`storeOnly`) and moved to `lifecycle.go:650`, `WriteThroughOutcome` to `:1477`, `signalsFor` to
> `:1114`, `writeThroughSignal` to `file.go:997`, `workspaceSignalExit` to `tools/registry.go:1078`, and the
> env signal's name was never `envTracker.signal` — it is `renderEnvDelta` (`env_changes.go:96`) behind
> `Agent.signalEnvironmentChanges` (`:460`). Only `context.go:89` still pointed where it said. The
> lesson is the one the register already states: a name survives a refactor, a line number does not, so
> an anchor table is a claim that has to be re-run, not a citation that stays true.

Discipline: **a mechanism only produces δ (the fact); wording and placement (σ) belong to that
category's single exit**. Nothing outside the exit may build its own string (`addSignal` warns
instead of silently dropping when it cannot find the exit). Test names follow the same vocabulary:
`TestExecObservesSandboxChanges` (a δ was observed), `TestEvictionSignalReachesNextToolResult`
(a σ was delivered once), `TestWriteFileStaysQuietWhenMirrorIsUneventful` (no δ, no σ).

### 2.2 Delivery, stated formally: who produces, who places, who takes

> **This document carries two formal systems**, and they are the two predicates of one sentence:
> "a **state change** inside the harness must be **perceivable** by the agent" — the first half is answered
> by **§2 / §2.1 / §3 (F2, observability)**: which changes must be stated, and that a σ must be true; the
> second half by **§2.2 (F3, delivery)**: who places it, on D₁ or D₂, who takes it, and whether it can be
> lost on the way.
> The document set carries a **third** system — **F1, preconditions / zero migration**
> ([06](./06-cordis-review.md) / [07](./07-formal-rootcause-and-fix.md)) — which answers "may this
> migration happen at all". Their division of labour: **F1 decides whether the mechanism may act, F2
> decides whether it speaks, F3 decides whether the speech arrives**; a design is only complete when all
> three questions have answers. The full index is **[00-formal-systems.md](./00-formal-systems.md)**.

§2 only says "there must exist an agent-consumable σ" — and "exist" is vague: a σ can be **rendered** and
never arrive (§6.1 lost one exactly once). This section tightens "exist" into a decidable method: three
roles, one invariant, five obligations, and a proposition that says only one shape is reachable.

#### 2.2.1 Three roles and invariant I1 (separation of the three powers)

```
produce(δ) → σ   whoever changed the world owns the complete fact, and renders the sentence
place(σ)         put σ where the agent is bound to read
take(σ)          carry it away on the agent's next read
```

> **Invariant I1 (separation of the three powers)**: `produce` and `place` belong to the **change side**;
> `take` happens on the **consumer side**, at its own read boundary. `place` may not be pushed onto the
> consumer ("some subsystem will come and collect it"), and `produce` may not reach into the consumer's
> internals ("tell the model that is currently thinking").

| Role | Owner | Where it lives here |
|------|-------|---------------------|
| `produce(δ) → σ` | the change side | `signalsFor(delta)`, `writeThroughSignal`, `envTracker.signal`, the rendering of the rebuild note |
| `place(σ)` | the change side | `parkSignal` (nobody is reading right now), appending inside a tool result, `SetEnvironmentSignal` (turn entry), the **user-side** outbound note for a dropped cron turn |
| `take(σ)` | the consumer side (its read boundary) | `takeSignals(...) + signalsFor(d)` inside the `lazyExecutor.Exec` result; `BuildSystemPromptAs` folds the environment signal into the prompt |

#### 2.2.2 There are only two delivery points (plus a "waiting area")

**A delivery point is a place the agent is bound to read.** The whole harness has two:

```
D₁  the call receipt (a tool result)   answers "what happened to the thing I just called"  exists only while the agent is calling
D₂  the turn entry (the turn prompt)   answers "what happened while I was away"             exists only when a new turn starts
```

**The waiting area is not a delivery point**: between `place` and the arrival of D₁/D₂, σ needs somewhere
that outlives a process — otherwise O4 fails. Here that is `sandbox.SignalStore` (a scope-keyed
`configs_kv` row). **An in-process queue is not a waiting area**, because it dies with the process
(09 §3, G3).

#### 2.2.3 The five obligations

| # | Obligation | The cost of violating it | Instances here |
|---|------------|--------------------------|----------------|
| **O1 produce** | whoever changed it renders it, and says only what is true | a false σ teaches the model to skip the whole class | the four `CompareResult` states (G5); "an unreadable list says so" (G10) |
| **O2 place** | σ must land on D₁ or D₂ | the signal may as well not exist | the dropped eviction report in §6.1; G7a's `list_dir` divergence line |
| **O3 take** | taking happens on the **consumer's next read**; the consumer may not be required to subscribe | requiring a subscription implies an unimplementable interface (see P1) | `takeSignals` at exec; the environment signal at prompt assembly; `bash_output` (the exit status is recomputed from the world on every read ⇒ a recomputable criterion, place 1) |
| **O4 no loss** | the waiting area between `place` and `take` must span processes and replicas | a restart or a lease hand-off drops it | G3: recomputable facts (refusals/failures) are not queued, the one that cannot be recomputed (moved) goes to the durable carrier |
| **O5 no noise** | no δ, no σ; and never interrupt reasoning in progress | wall-paper noise (C3); interrupting is structurally impossible anyway (see P1) | silence on docker; an unreadable list never reported as a deletion |

**The second form of O4 (added 2026-09-18)**: O4 is not only "σ was lost in the waiting area". **Keeping
the σ's own criterion — its baseline — in process violates it too**, because when the baseline dies with
the instance the σ is not lost in transit, it **cannot be produced at all**. G9 is the cleanest example:
a configuration change takes effect **by rebuilding the Agent**, so the tracker that should report it is
destroyed by the very change it should report — and "first observation is not a change" swallows exactly
that change.

There are three legitimate places for a criterion, in increasing cost — **try them in order**:

| # | Where the criterion lives | The test | Instance |
|---|--------------------------|----------|----------|
| 1 | **nowhere** (recomputable) | it can be derived from the world on the next read | G3: refusals/failures are not queued, they are recomputed |
| 2 | **reuse an existing durable record** | the fact already has an owner and that record is already being written | G9 + G20: before = the conversation's own turn receipt (`provider`/`model` columns + `run_receipt`, carrying the whole world snapshot) — no new storage, no new write path, and five families cross restarts and replica hand-offs together |
| 3 | **a new durable carrier** | neither of the above holds; the fact has no other owner | G3: `moved` goes to `sandbox.SignalStore` (a new row, deleted on take) |

Row 2 only got used on the third pass of 2026-09-18: the first version created a `cfg_seen` row for the
config baseline (place 3). It worked, but it was a second copy of one fact — the shape this document set
keeps running into. **Ask whether the fact already has a home before building one.**

#### 2.2.4 Proposition P1: only the pull shape is reachable (push is not implementable)

> **P1**: there is no implementation that delivers σ *while the agent is thinking*.
>
> **Proof**: the consumer is a **synchronous model call** — it takes (messages, tools) and returns a
> response; the protocol has no "inject mid-generation" slot, and the driver layer (the model provider)
> exposes no receiving end. Delivering mid-reasoning would require the consumer to expose an inbox, i.e.
> the driver to offer an injection channel — which means **writing a driver detail into the policy**
> (violating the dependency direction; 02 §1.3/§1.4). So the push shape is neither implementable nor
> necessary. ∎
>
> **The one near-miss is user steering**: it buffers a user message on the session, and the running loop
> takes it **between two tool iterations** (`appendSteer`). Two qualifications: ① it is still "checked
> between two model calls", not injected into a generation; ② what it carries is **user input**, not a
> harness state change. So it is not a counter-example.
>
> **Corollary P1′**: every harness δ must land on D₁ or D₂. "Later than the fact" is acceptable
> (09 §4, caution A); "no delivery point" is not.

##### P1’s assumption health-check (a watchable trigger condition; added 2026-09-21)

P1’s proof rests on two **factual assumptions about the driver layer**: ① the consumer is a **synchronous model call**
(it takes `(messages, tools)` and returns one response); ② the driver layer **exposes no receiving end**. Those two are
**not theorems** — they can change. So here is the criterion for **when the whole delivery-point table must be redrawn**
(source: *The Mathematical Principles of Cognitive Philosophy*, 19-5 §19.5.8.7, row 1).

**One question decides it: does the injected content change the *same* generation?**

| Channel shape | Where the receiving end is | When it is taken | Does it refute P1? |
|---|---|---|---|
| user steering | application layer (a session buffer) | **between two tool iterations** | **No** — still "between two calls" |
| continuation between tool iterations | application layer | between two model calls | **No** (this row is here to **prevent false positives**: a turn contains several model calls, so do not mistake injection between them for a counter-example) |
| streaming **output** | — | — | **No** (streaming output and input injection are two different things) |
| **injection consumable mid-generation** (the receiving end is *inside* the model call, or the protocol has a "inject mid-generation" slot) | inside the driver layer | **within the same generation** | **Yes** ⇒ assumption ② is broken |

**When to run this table**: ① on every **driver-layer upgrade** (provider SDK / API version);
② whenever someone proposes "giving the agent a live input channel", answer the question above once; and
③ **do not** count "we can slip a message in between two tool calls" as a hit — that is steering, not injection.

**If it does hit (blast radius, stated up front).** It would not mean "P1 was wrong"; it means **the delivery-point table has to be redrawn**:

- a third delivery point appears next to `D₁` / `D₂` (call it **D₀ = mid-reasoning** for now) ⇒ the **three attribution questions**
  in 19.5.1 of *The Mathematical Principles of Cognitive Philosophy* become four, and the answer set of question ② grows;
- this section’s **O3** (taking happens at the consumer’s next read) and **O5** (do not interrupt reasoning in flight) were both written
  against "it cannot be done" — after a hit they **downgrade from obligations to choices**;
- the **③ impossible** cells in 19.4.6.1 that rest on "the interface does not exist" (R4 / P1′) must be re-judged:
  once the interface exists they fall back to **②** (a landing point exists but nothing was done) ⇒ back to the ordinary fix (attach a production point).

> **In one line**: **P1 is a theorem with assumptions, and this health-check is its expiry date.**
> As of 2026-09-21: **the assumptions hold, the table is unchanged.**

#### 2.2.5 The decision procedure (run a new mechanism through it)

```
Given a mechanism M that changes the agent's world:

1 produce  what did M change (δ), and who did it?
     the agent itself → the tool result is the receipt, stop here (C1)
     the harness      → continue
2 σ        render δ as one TRUE sentence; if it cannot be said truly, change the wording (O1)
3 place    which delivery point does the sentence land on?
     a call is in flight → D₁ (the tool result)
     only a turn boundary → D₂ (the turn prompt)
     neither              → do not stop here: design a future delivery point explicitly (§6.1)
4 take     who takes it, and when? The answer must be "the consumer's next read"
5 O4       what does it cross between place and take?
     inside one call       → the call stack ✔ (e.g. the rebuild note)
     across calls, one pod → an in-process queue ✘ (a restart drops it, G3) → recompute or persist
     across pods           → a durable carrier, or recomputable (blocked / problem)
6 O5       silent when there is no δ? (C3)
```

#### 2.2.6 Layer attribution (Clean Architecture)

| Layer | Content | Criterion |
|-------|---------|-----------|
| Entities | the invariant: `Belief` may not drift from the world | §1 |
| Use Cases | **the delivery policy**: I1 + O1–O5 + one exit per category | the policy depends only on ports it defines |
| Interface Adapters | `SignalStore` (durable waiting), `ReplacedWorkspace` (a one-shot fact), `UnhydratedWorkspace` (a state declaration), the two projections | a port must declare **semantics**, not just a capability bit (02 §1.3) |
| Frameworks & Drivers | `configs_kv` rows, the sandbox HTTP API, **the model provider (no receiving end ⇒ P1)** | dependency direction: adapter → port → policy |

```
Frameworks & Drivers ──implements──▶ Interface Adapters ──▶ Use Cases ──▶ Entities
(configs_kv / the provider)          (ports like SignalStore)  (delivery policy)  (the invariant)
```

In one line: **placing is the change side's active obligation, perceiving is the agent's passive
mechanism — "active" means the change side must not stay silent, not that it may interrupt.**

## 3. Three corollaries

| # | Corollary | Design consequence |
|---|-----------|--------------------|
| C1 | **The signal must enter a channel the agent really reads** | tool results / the next turn's context; slog does not count (readable by operators ≠ readable by the agent) |
| C2 | **No signal ≠ no change** | whenever the harness may have moved the world away from the agent's belief, it must say so; silence asserts "the world is as you think" |
| C3 | **A signal must be an exception channel** | it does not appear when nothing changed. A line attached to every call teaches the model to skip it — which is the same as having no signal |

## 4. Which changes need a signal

| Source of change | Naturally perceivable? | Disposition |
|------------------|------------------------|-------------|
| the agent's own tool call | ✅ the tool result is the receipt | no extra mechanism |
| the harness changed the workspace (sync write-back, eviction flush) | ❌ | **must be signalled** (including the actions it refused) |
| the harness changed the agent's identity / memory / skills | ❌ | **must be signalled** (especially removals, see C2) |
| the harness changed the tool set / capabilities (MCP load, skill refresh) | ❌ | **must be signalled** (invisible-by-absence is the hardest kind to notice) |
| the harness changed the context (compaction, clipping) | ⚠️ the result is visible, the event may not be | report "what happened", not just the new state |
| the harness changed the execution environment (sandbox replaced, rebuilt, unhydrated) | ❌ | **must be signalled** (precedents exist: `[sandbox replaced]`, `workspaceUnhydratedSignal`) |

## 5. Audit of the current harness

> This section lists changes (which changes need a signal). The component-by-component checkup —
> both directions, "triggered by the agent" and "triggering the agent", plus gaps G5–G13 (G5 being
> a σ that is **always false**, reproduced locally) — is [10](./10-harness-state-audit.md). The two
> sections complement each other: this one answers "does this kind of change have a signal?",
> 10 answers "did this component slip through?".

| State change | Agent-side signal | Verdict |
|--------------|-------------------|---------|
| a tool writes to the store | the tool result | ✅ |
| sync writes sandbox changes into the store (post-exec) | `exec` result `[workspace] the sandbox changed …` | ✅ 2026-09-18 |
| a path the sync refused | `exec` result `[workspace] NOT synced …` | ✅ 2026-09-18 |
| **the sandbox deleted a file** | **no signal**: the walk's domain is the sandbox snapshot, so a deleted path is not in it, and it cannot be told apart from "a store-only upload" | ❌ **G4** ([09](./09-sandbox-lifecycle-audit.md) §3: restoring detection needs a **durable** store-side manifest, in-process state will not do) |
| **a sync that only happened during idle eviction** | previously the signal was dropped → now queued to the next tool result, delivered exactly once | ✅ 2026-09-18 |
| write-through replaced a different version in the sandbox | write result `[workspace]` (byte count, no content) | ✅ 2026-09-18 |
| write-through found a different version and had no earlier copy to compare it against | write result `[workspace]`: "the sandbox held a different version (N bytes), with no earlier copy to compare it against" | ✅ 2026-09-18 (this branch used to claim "over 2 MiB" for every write that reached it; see [10](./10-harness-state-audit.md) §2.1, G5) |
| the sandbox is unreachable / was replaced | write result `[workspace]`, note on the `exec` error | ✅ |
| **a path the store has and the live sandbox does not** (caused by a user upload/delete) | `list_dir` appends `[workspace] N path(s) … NOT in this sandbox …` after the listing | ✅ 2026-09-18 (no signal at all before; see [10](./10-harness-state-audit.md), G7a) |
| workspace not hydrated (the store listing failed) | `workspaceUnhydratedSignal` | ✅ existing precedent |
| a tool result was clipped | an inline clip marker plus "how to see all of it" (`clipMarker`) | ✅ existing precedent |
| the goal budget is exhausted | `BudgetLimitPrompt` delivered into the session | ✅ existing precedent |
| context compaction | both the summary and the clip placeholder **state what happened** ("earlier turns were compacted…it is lossy", "dropped by context compaction…re-run it") | ✅ 2026-09-18 |
| background memory update (heartbeat) | the unified **environment-change signal**: `long-term memory was rewritten/created/CLEARED` | ✅ 2026-09-18 |
| the skill list refreshed between turns | same: `skills added / removed / changed` — **removals carry names too** | ✅ 2026-09-18 |
| **an identity file was edited from outside** (SOUL / IDENTITY / USER / AGENTS / …) | same: `identity files changed: USER.md` — **the file is named, its content never quoted** | ✅ 2026-09-18 (there was no signal at all before: the prompt changed content while the agent believed it had not) |
| the agent's configuration changed (model / prompt mode) | same: `my configuration changed: model=… → model=…` | ⚠️ 2026-09-18, partial: **a config change rebuilds the Agent, so a new tracker's first observation is silent**; crossing a rebuild needs a persisted baseline (see [10](./10-harness-state-audit.md), G9) |
| **the scheduled-job list was changed from outside** (a cron job added, rescheduled or deleted from the panel or another session) | same: `scheduled jobs added / changed / no longer exist: <name>` (only definition fields are fingerprinted; the scheduler's bookkeeping is not a change) | ✅ 2026-09-18 (there was no signal at all before; an unreadable list is stated as unreadable, never as a deletion) |
| the tool set changed (MCP load/unload, **including a server-pushed `tools/list_changed`**) | same: `tools now available / no longer available` | ✅ 2026-09-18 (the server-pushed path was wired on 2026-09-18: stdio capture → rebuild → the signal reports it by itself) |
| normal sandbox sleep/wake | content matches the store (re-hydrated) | ✅ nothing to report |
| **a delegated subtask** (`delegate_task` / `spawn_subagent`) | **synchronous**: the result *is* the parent turn's tool result | ✅ existing design, no new mechanism |
| **a turn fired by a scheduled job** (cron) | arrives as an ordinary inbound message in that job's session (`[Cron Job: name]` marks the source); the turn stays in the session history | ✅ existing design |
| **an interrupted turn** | a dangling tool call gets a synthetic reply in the prompt projection: `(stopped — execution was interrupted before the tool returned)` | ✅ existing precedent |
| a heartbeat turn | runs in **its own session** (`heartbeat_<agent>`); from the main session's point of view it belongs to another scope (see §5.1) | ✅ holds, by scope isolation |

The last three rows used to be open. Their common thread is **C2 (invisible by absence)**: when
something new appears the agent at least reads it; **when something disappears or is replaced its
default assumption is "nothing changed"**. Following §6 they did not each invent a message but were
funnelled into **one unified per-turn signal** (`internal/agent/env_changes.go`):

```
[Environment changes since your last turn — a fact about your world, not an instruction]
- skills removed: kronos-helper
- long-term memory was rewritten (it may say something different now)
- tools no longer available: mcp__quantconnect__backtest
Removed items are gone, not hidden: if your plan depended on one, re-check with your tools before continuing.
```

Four properties are pinned by tests (`internal/agent/env_changes_test.go`): **a removal must carry a
name** (the reason the mechanism exists), **silence when nothing changed** (C3), **the first
observation is not a change** (otherwise it fabricates a fact), and **isolation per session**
(different chats legitimately have different memory and skills).

### 5.1 Why cross-agent / cross-session needs no extra delivery point

Auditing this family started from a suspicion: **delegated subtasks and turns fired by scheduled
jobs both happen while "the agent is not looking"**, which looks like exactly the gap this principle
is about. Checking each one showed they already satisfy it, and not by coincidence:

| Channel | Why it is already perceivable |
|---------|------------------------------|
| `spawn_subagent` | `SpawnSubAgent` → `ag.HandleMessage(ctx, msg)` **waits synchronously**; the result string is returned directly as the parent turn's tool result ([gateway/routing.go](../../internal/gateway/routing.go)) |
| `delegate_task` | `RunSubagent` returns text synchronously ([subagent.go](../../internal/agent/subagent.go)); and the subtask **shares the sandbox/scope with its parent**, so the files it changed show up in the parent's next exec signal |
| cron | the tick is injected as an ordinary inbound message ([cron/scheduler.go](../../internal/cron/scheduler.go) `fireJob`), which takes the normal turn path → written into that job session's history, with a `[Cron Job: …]` source marker |
| a cancelled turn | unanswered calls get `(stopped — …)` in the projection ([normalize.go](../../internal/agent/normalize.go)) — **the existing precedent for "the result is gone but the agent must know"** |
| heartbeat | runs in its own session; a cross-scope change does not need reporting in another scope (see below) |

**The key criterion (added to the §6 checklist)**: observability holds **per context
(scope/session)**, not globally. A change only has to be perceivable **in the context where it
happened**; broadcasting it to every context would produce "every session knows what every other
session did" noise. A heartbeat turn belongs to the `heartbeat_<agent>` scope, where it has a full
history; the link to the main session is the **memory file** (and memory changes are already
signalled by the environment-change signal). Likewise, a subtask's result belongs to the call that
started it.

So this family **added no mechanism at all**: the audit's conclusion is that they already satisfy
the principle, and another delivery layer would be the same "redundant insurance" that was deleted
once before.

## 6. Review checklist (any new mechanism must pass)

When adding any mechanism that changes harness state, answer each line:

- [ ] Was this change caused by **the agent's action**, or by **the harness itself**?
- [ ] If the latter: through **which channel** does the agent learn it? ("slog" is not an answer)
- [ ] **Who `place`s this change?** (§2.2, O2: the answer must be "the change side put it on D₁ or D₂";
      "some subsystem will collect it" means there is no delivery point)
- [ ] **When** is the signal delivered? Could that be later than the agent's next relevant inference?
- [ ] **Can it be lost between `place` and `take`?** (§2.2, O4: in-process state means yes; it must be
      either recomputable or durable)
- [ ] Does **the absence** of the signal mean exactly "nothing changed"? (C2: silence must not be ambiguous)
- [ ] Does the signal appear only when something is wrong? (C3: the normal case is noise)
- [ ] Is there a test asserting **the signal exists**, and **the silence when it should be silent**?
- [ ] If the change is a removal/replacement: can the agent confirm whether the thing is still there?
- [ ] **Which context does this change belong to?** It only has to be perceivable in the
      scope/session **where it happened**. Before broadcasting to all contexts, ask: does the agent
      really need to know there? (Cross-scope broadcast is the over-design §5.1 rules out.)
- [ ] **If a control acts on this fact, is the control's predicate the fact itself?** (Added
      2026-09-19 from the cloud re-audit.) D₃ says the delivery point for a UI is the render; the same
      rule applies one level in: a button that *acts* on a fact ("stop the turn", "retry", "cancel")
      must be shown by reading that fact, not by reading a local proxy such as "this tab's stream is
      open". A local proxy is right exactly when the fact is absent, and wrong in the case the whole
      mechanism exists for — the fact being true *somewhere else* (another tab, another replica).
      Worked example: the composer's Stop button was driven by local `streaming` while `turnState` was
      already in the same tree, so a turn held elsewhere had no stop affordance at all.
- [ ] **Does the witness reach every delivery point?** ([00 §5.1](./00-formal-systems.md) — a
      verification-side rule: no design work, no runtime behaviour.) A rule witness proves the *rule*;
      it cannot prove the *fact arrived*. For an obligation that sends a fact out, count the fact's
      consumers (`grep` it — each render / store / wire site is a delivery point), require one test per
      point, and require the falsification to redden **that** test. Worked example: the tool row — the
      rule was green and the call site untested, so a peer-held turn rendered "interrupted" for as long
      as nobody expanded the row.
      **And check that the consumers read the same producer** (§10.5 D-5): counting the points is not
      enough if one of them answers the question itself. A surface that reads the *upstream* answer
      instead of this system's one expression of the fact answers a different question — and its own
      tests stay green, because they test that reader. Worked example: the skills panel, which read the
      pod's catalog directly and therefore could not report a refusal only the egress is able to make.
      **And when the delivery just widened, re-read the sentence itself** (§10.5 D-4): a σ that was
      true for the cases the first consumer could carry can be false for the cases the new one
      carries, and the transport test will stay green either way.
- [ ] **If this change adds or moves a switch** (§10.8, O8): does the row have a reader on the
- [ ] **Is the reader inside the harness or outside it?** ([00 §5.2](./00-formal-systems.md) — O9,
      promoted 2026-09-26 from the MCP surface audit.) Every other line here assumes the reader is in
      the loop, where the remedy is "attach it to the next tool result / the next prompt". An
      **external consumer** (an MCP client, a dashboard outside the loop, a webhook subscriber) has no
      forced read at all — it can also simply never ask again. So it needs an explicit delivery point:
      a **pull reply that is authoritative**, and/or a **push notification it subscribed to that
      carries only "changed"** (never the fact itself, or push becomes a second source), plus the rule
      that a re-read reconciles any gap (notifications are best-effort, so correctness must not depend
      on them). Worked example: the MCP task surface's state design was pull-only for a while and was
      green in every test it had — because the tests were its only reader.
      production path — row → the resolved cfg → the option that carries it → the gate that acts on
      it, each hop witnessed, with a falsification that reddens the missing hop — and does the gate do
      what the row's own documentation says the value *means* (nil = inherit, `false` = veto)? Worked
      example: four rows (register 49/51/52/53) whose only reader lived in `NewAgentWithFullCfg`.
      **And if a surface presents the row as a control**: is it writable there for the role that
      surface is for, and — when a surface shows a value derived from the row — does what it shows
      stand for the state, rather than for one input to it?

Reference implementations (inside this directory):
`TestExecObservesSandboxChanges`, `TestExecIsQuietWhenNothingChanged`,
`TestExecObservesRefusedPaths`, `TestEvictionSignalReachesNextToolResult` (delivered once),
`TestWriteFileStaysQuietWhenMirrorIsUneventful`.

### 6.1 Why item 4 deserves its own section: a signal that is *produced* is not a signal that *arrives*

**A real counter-example from this repair.** The sync channel has two triggers; the first one always
carried its report, the second was written like this:

```go
// post-exec: δ becomes σ, attached to the exec tool result — it arrives ✅
d := l.pool.syncSnapshot(ctx, l.scope, ex, "post-exec")
out += l.pool.takeSignals(ctx, l.scope) + signalsFor(d)

// evict (idle eviction): the δ happened, but there is no tool result right now ❌
func (p *LifecyclePool) flushIfSupported(sc sandboxScope) {
    ...
    p.syncSnapshot(context.Background(), sc, ex, "evict")   // nobody takes the return value
}
```

Consequence: **a sync that only happened during idle eviction was never seen by the agent** — and
"the time the agent was idle" is exactly the window in which changes are most likely (background
scripts, heartbeats, another session running). This is not "no signal was produced" but **a signal
produced with no delivery point**: when the second path runs, there is no tool result to attach it to.

The fix is not "write another log line" but **find the signal a future delivery point**: σ is queued
per scope, carried away by that scope's next tool result, and **delivered exactly once**
(`parkSignal` / `takeSignals`, landing in the **durable** `SignalStore`; tests `TestEvictionSignalReachesNextToolResult` and
`TestEvictSignalOutlivesThePoolThatProducedIt` — the latter delivers through a **different pool instance**, the one cell an in-process queue could not cover).

So item 4 really asks:

> **When this change happened, was the premise "the agent is reading something" true?**
> If not (background task, eviction, cross-turn asynchrony) a delivery point must be designed
> explicitly, or the signal may as well not exist.

The same question applies to other subsystems: any state change that happens **between two model
calls** (background memory update, scheduled job, a change triggered by an external webhook) lands on
this item — and their common answer is "queue + a future delivery point", not "report it right now".

## 7. Relation to the other principles

| Principle | Relation |
|-----------|----------|
| **F1** [Cordis preconditions / zero migration](./07-formal-rootcause-and-fix.md) (07 part 2) | complementary: the precondition decides whether the mechanism **may act**; this principle (F2) decides whether the agent **knows** it acted; F3 then decides whether the knowing **arrives**. None is optional — preconditions alone become "refused but unstated", F2 alone becomes "stated but never delivered" (§6.1), F3 alone becomes "delivered but untrue". The index of all three is [00](./00-formal-systems.md) |
| the four mechanisms of [07 §3.11](./07-formal-rootcause-and-fix.md) | this principle applied to filesystem sync: write-through (the two copies agree at the moment of the write) + the memory-free version decision (the criterion for δ) + refuse + signal (the incident's only line of defence) + the exec change signal (the perception channel) |
| the "preserve a copy / choose-a-side tool" dropped from earlier versions | violates the spirit of C3: another layer of insurance for a change that was already announced is a duplicate mechanism (the three-shape comparison in 07 §3.11.1) |

## 8. In one sentence

> **A mechanism's correctness can be guaranteed by preconditions; the agent's correctness can only
> be guaranteed by observability.**
> What the incident destroyed was not just three files but the agent's knowledge of its world — and
> that knowledge is the premise of everything it does next.

## 9. Architecture decision: should there be a "unified state-observation mechanism"?

Reviewed against Clean Architecture's criteria (the question: **should a unified observation exit be
designed?**):

| Criterion | Observation | Verdict |
|-----------|-------------|---------|
| **Is the axis of change proven?** (CCP/SRP: same reason + same rate of change → keep together) | by 2026-09-18 the same seam had changed **6 times**: exec change signal, write-result signal, environment-change signal, unhydrated warning, clip marker, sandbox-replaced warning | **yes**, the boundary is proven by real change; the investment is justified |
| **Is the policy duplicated?** | the policy "when to attach to a tool result / when to queue / when to stay silent" was written once per mechanism — and **was missed once** (the idle-eviction signal was dropped, §6.1) | **yes**; what repeats is the policy, not the rendering |
| **Is an abstraction needed?** (YAGNI: with one implementation, do not invent an interface) | there are only two delivery channels: **the tool result** ("what happened to the thing I just called") and **the per-turn prompt** ("what happened while I was away") | only **one event type + two delivery points**; no bus/subscriber framework |

**The shape that was proposed for a unified mechanism — and REJECTED (§9.1), not pending.** Read the list below as the
record of a design that lost, so nobody picks it up as a TODO: §9.1's decision was the opposite — **one exit per category, no
cross-package framework**, with the criterion stated there (two exits, one question each; a framework would be a fourth
abstraction over two calls). What survived from the analysis is only item 3's durability property, which landed on its own:

1. **Unify δ, not the channel**: subsystems stop writing their own prose and instead produce one
   structured fact (the δ of §2.1: `{kind, paths, bytes, detail}`); **one** use case decides σ's
   delivery and wording;
2. **keep two delivery points** (tool result / per-turn prompt), because they answer different
   questions and merging them loses semantics;
3. **once the policy is centralized, durability must follow** (**landed 2026-09-18**): this item used to say
   "the queue is in-process; a signal must be either recomputable or persisted" — both halves are now done.
   The recomputable ones (refusals, sync failures) are **not queued at all**: the next sync derives them
   again. The ones that cannot be recomputed go through a **durable port** (a path an eviction wrote into
   the store) or ride the **call stack** (a rebuild, in the call that discovered it). The in-process queue
   is gone (see 09 §3, G3).

**Explicitly not doing** (the first two steps of Musk's algorithm):

- no general event bus / pub-sub / observer framework — there is exactly one consumer (the model),
  and an extra layer of indirection only turns "why did this notice not arrive?" into a harder question;
- no new storage: if persisting signals becomes unavoidable, reuse the existing scope-level document
  store rather than adding a table.

### 9.1 The final form of the criterion: **one exit per category**, no cross-package framework

The architectural question is not "is there one global exit?" but:

> **Perceivable by the agent; and for each category (interface/package), the observation exit is unique.**

The two goals live at different levels: the agent can only see its own context (tool results /
prompt / messages), which is the **consumer side**; "unique exit" is a **producer-side** discipline —
several mechanisms in one package must not each decide how to talk to the model, or the policy
duplicates and gets missed (§6.1 lost it exactly once).

The criterion carries one qualification inherited from §2.2: **an exit is a category's single delivery
point; the obligation to place belongs to the change side, the moment of taking to the consumer side**
("active" means the change side must not stay silent — it does not include interrupting reasoning in
progress; P1).

By that criterion this repo has converged to three categories with one exit each:

| Category (package/interface) | Single exit | How it is delivered | Status |
|------------------------------|------------|---------------------|--------|
| `internal/sandbox` (LifecyclePool) | `delta` (moved / blocked / problem) + `signalsFor(delta)`; `SignalStore` (durable) when no tool result can carry it | appended to that `exec` result; the next one across processes and replicas gets it too | ✅ 2026-09-18 (no in-process queue) |
| `internal/agent/tools` (workspace tools) | `Registry.workspaceSignalExit`: tools only `addSignal(ctx, δ)`, the exit decides placement (state marker first, this call's facts after) | appended/prefix to that tool result | ✅ converged 2026-09-18 |
| `internal/agent` (turn level) | `envTracker.signal` → `ContextBuilder.SetEnvironmentSignal`, appended to the end of the system prompt | once per turn | ✅ |

The discipline shared by all three: **tools/mechanisms only produce δ; placement and wording (σ)
belong to the exit**; nothing outside the exit builds its own string (`addSignal` warns instead of
silently dropping when it cannot reach the exit). That keeps "why was this sentence never delivered
to the agent?" a question with **one place to look per package**.


## 10. Patches (2026-09-19, brought back from auditing the cloud side)

> These are **method-level** additions: auditing real changes on the cloud (a different repository, a
> different runtime) forced four refinements. They are not a new system — they are the existing
> obligations made complete for the case where **the consumer is a user interface, not the agent**.

### 10.1 O1′ — freshness, split by the kind of σ

Plain O1 asks only that a σ be true *when placed*. The cloud counterexample: a σ that was true when
placed ("a holder exists") stays true **forever** because its expiry was lost in transit, so a view that
lost its connection keeps showing "running elsewhere".

Adding freshness to *every* σ is equally wrong: `notice` ("your turn was superseded" / "stopped at your
request") is an **event σ** — once it happened it is true forever, and an "expired ⇒ unknown" rule would
**erase** that history (worse than a lie).

| kind of σ | examples | O1′ requirement |
|---|---|---|
| **state σ** ("how things are now") | `turnActive`, `queued`, `subagent_progress` | **must carry its expiry/version**; the reader judges by it; expired ⇒ degrade to `unknown` (never keep claiming) |
| **event σ** ("what happened") | `notice` (superseded / cancelled), `done` | **immutable**: needs no expiry and **must not** be degraded or erased by an expiry rule |

In one line: **a state σ goes false with time; an event σ does not.**

### 10.2 D₃ — for a UI, the delivery point is "the render"

F3 originally had D₁ (tool result) and D₂ (turn prompt) — both **for the agent**. The cloud consumer is a
**user interface**, and its act of taking is a **render** (reading its own cache/state).

> **D₃ has a capability precondition, measured 2026-09-19.** "The delivery point is the render" only
> holds if the renderer can actually draw what arrived. The chat bubble's markdown pipeline had no math
> support at all (`react-markdown` + `remark-gfm` only), so a model that wrote `$$\max E[V]$$` delivered
> backslashes: the fact was produced, placed, and taken — and still not received. Fixed by adding
> `remark-math` + `rehype-katex` to both the bubble and the document renderer, with one delimiter
> policy shared by both: **only `$$…$$` is math**; a single `$` is deliberately not a delimiter,
> because measured on this product's own text (`价格是 $12 与 $30 两档`) the default rule turns ordinary
> prose into a formula. The rule the model needs in order to comply lives in the always-on
> `response_format` prompt module — a *renderer* convention belongs in the base prompt, not in a
> sandbox-specific block (that mistake was made and caught by `TestSandboxPromptStaysUnderItsBudget`).

| delivery point | consumer | act of taking |
|---|---|---|
| D₁ | agent | the next model call reads the tool result |
| D₂ | agent | the next turn's prompt |
| **D₃ (new)** | **UI** | **one render** (Query cache / component state) |

Corollary: O3 (the moment of taking) and O4 (no loss) must be restated on D₃ — "the server sent it" is not
"the UI has it". Both cloud defects (`notice` dropped, `queued` with no landing point) live in this cell.

### 10.3 O6 — an absence must be as visible as an arrival

"Absence is invisible" was a corollary; the cloud audit promotes it to an **obligation**: **the revocation of a δ
(a fact disappearing or expiring) must be as visible as the δ itself.**

Measured counterexample: the server pushes nothing when a turn ends, so a disconnected view keeps showing
"running elsewhere" until expiry or a reload. The fix is to **declare the absence** (`done ⇒ turnActive = null`)
rather than hoping the next read notices.

**Second instance (2026-09-21, found by the same audit one round later):** the absence here has two
consumers, and only one of them could see it. A skill the *egress* refuses — no `SKILL.md`, more than 512
files, over 16 MiB, a digest that is not sha256 — is refused by a rule the pod does not have (it does not
know the extension's limits), so that refusal exists only inside the cloud service. The MCP answer carried
it; the dashboard panel read the pod's catalog through a raw passthrough and therefore could not: a skill no
client can ever load was listed there as published, with nothing anywhere saying why. Both surfaces had
tests, both were green, and the fact still differed between them — because they were not reading the same
producer (D-5). Fixed in cloud by answering the panel's path from `skillCatalogView`, the second projection
of the same partition `listSkills` uses; registered as row 40.

### 10.4 Two C-family contracts (orthogonal to F1–F3; they belong to port vocabulary)

| # | contract | criterion | counterexample (measured) |
|---|---|---|---|
| **C1** | **one fact, one wire shape** | every exit for the same fact uses one schema; consumers parse one shape | one lease expiry had three exits and two spellings (SSE `expires_at`, history `expiresAt`, `queued` snake again) ⇒ a fact arriving over SSE **never expired** |
| **C2** | **witnesses come from the producer's real payload** | contract tests must build fixtures from a live payload (or from a shared type), never by hand | 229 green UTs missed both the spelling bug and the missing `notice` type, because the fixtures were hand-written camel objects |

C1's root fix is a **shared type** (two exits, one struct ⇒ shape drift fails at compile time); C2's is a
**capture-style contract test** (grab one live payload once and keep it as the fixture).

### 10.5 The disciplines the landing process forced (measured on the cloud side)

| # | discipline | what forced it |
|---|---|---|
| **D-1** | **never guess a location; read first, then anchor narrowly** | two consecutive failures in one wiring task: the first regex hit the field inside the **result type** (not the result object), the second missed because `queuedTurn` and `handleQueuedTurnAction` sit on the same line. Both were "guessing structure from shape" — the same root as **C2 (witnesses from real payloads)** |
| **D-2** | **a new field in an implementation must land in its return type too** | added `setQueuedTurn` to `useStreamPipeline`'s result object but not to `UseStreamPipelineResult` ⇒ `TS2339/TS2561`; earlier the same shape happened with `turnState` vs `UseChatSessionResult`. The type face is the machine-checkable half of the contract; the two must move together |
| **D-3** | **close the value domain in the type** | `QueuedTurn.turnId` is a required `string` while the queue σ may omit it ⇒ `TS2322`. If "no id" is a legitimate case, say so in the type (`string \| undefined` or an explicit unknown branch) instead of papering over it with `?? ''` at the call site |
| **D-4** | **when a fact gains a consumer, re-read the sentence — not only the wire** | O7's round moved the pod's `{baseDir}` warning from "the dashboard sees it" to "the dashboard *and* the MCP answer see it". Widening a delivery widens the blast radius of a sentence that was never true for every payload: after that round, a claim about substitution ("replaced when this agent loads the skill") that does not hold for a token sitting in a bundled script was being read on both surfaces. A transport witness cannot catch this: it asks *did the fact arrive*, never *is this sentence true for the cases the new consumer carries* |
| **D-5** | **count the consumers, then check that they read one producer** | the panel and the MCP answer both report which of a user's skills reach a client, and both had witnesses — but the panel read the *pod's* catalog directly, while the MCP answer read this system's partition of it. So the cell that mattered (a refusal only the egress can make × the panel) was empty, and no suite could have caught it on either side: each tested its own reader. Counting "how many surfaces say this fact" counts readers; the invariant is about expressions |

### 10.6 Iteration 2's conclusions (two, one of them a **non-defect**)

| cell | conclusion | evidence |
|---|---|---|
| the `subagent_progress` heartbeat's delivery point | **not a new defect**; it collapses into an already-registered item (A4.1: the heartbeat has a live D₃ but no *reconstructible* one, and it binds by position rather than identity — **that second half was not merely registered, it was a real defect; row 42 closed it on 2026-09-21: every heartbeat names its call, and the client draws it only on the row it names**) | the client subscription does take it (`case 'subagent_progress'` → `setSubagentProgress`) |
| a turn ending in another tab | **not a defect** — the hub publishes per (user, agent, session) and events land in `session_events` first for replay on reconnect | `internal/agent/events.go:72-86` (`AppendSessionEvent` → `hub.Publish(userID, agentID, sessionKey, …)`) |

> Why record a non-defect: **the method's value is not only finding bugs but also refusing to book non-bugs as bugs** — a false positive pulls attention away from the real ones.

### 10.7 O7 — a fact that survives but changes meaning must also speak

O6 says a **disappearing** fact must be as visible as an arriving one. O7 is its sibling, and O6
does not cover it: here nothing disappears. We hand the δ over, the client takes it, and what it
reads is not what the author wrote. "Absence is invisible" is not the gap, so an obligation about
absences never fires on this case — which is why the case sat there, silently, through a round that
was looking straight at it.

The criterion:

> **When a δ is delivered in a form whose meaning differs from the one it was authored in, the
> difference has to be stated on every surface where the difference is visible** — not only on the
> surface that happens to be a dashboard.

Measured instance (the cloud re-audit, 2026-09-21): a skill whose `SKILL.md` carries `{baseDir}`. The
runtime substitutes that token when *this* agent loads the skill (`internal/agent/skills.go`
`loadSkillContent`, and the `load_skill` tool — named by function, not by line: that anchor has
already drifted once), and the MCP egress cannot, because the entry's digest covers the bytes as
they are. So the skill is published — deliberately, since refusing it would break the half that
always worked — and a connected client reads the literal token. The pod reports that as a warning
(`Catalog.Warnings`, code `base_dir_token`, with the files that carry the token) and the dashboard
panel renders it. The MCP surface — the one where a reader actually meets the literal token —
dropped it, together with the refusal's stable code, inside the cloud adapter: two facts lost one
frame after arrival, before any consumer ran.

What this obligation adds to §5.1's rule is one word: **every**. A fact with two consumers needs a
delivery-point witness **per consumer** — the producer's witness and the *other* consumer's witness
are evidence about neither this consumer's wire nor its render. The retired failure shape here is
exactly that: the pod's tests proved it sends `code` and `warnings`, the panel's tests proved it
renders them, `grep warnings` inside the egress returned **zero hits**, and the suite was green.

Witnesses: cloud `skills-list-chain.test.ts` (the delivery point: pod answer → adapter → use case →
the `skills/list` result's `_meta`), plus the rule witnesses in `catalog.test.ts`,
`skills-service.test.ts`, `policy.test.ts` and `tools-service.test.ts`. Falsified for real:
restoring the adapter's `{path, reason}` rebuild reddens 2 tests, returning an empty warnings list
reddens 2, and dropping the `_meta` key reddens 3.

**Narrowed the next day (2026-09-21, same audit)**: the warning's *trigger* was wrong in a way no
transport test could see. It fired on "any file of the skill carries `{baseDir}`", while the
sentence it carries states a difference between two readers — and that difference exists only in
the file the runtime substitutes, `SKILL.md` (`internal/agent/skills.go` `loadSkillContent`, and the
`load_skill` tool: both read exactly that file). A token in a bundled script is substituted by
nobody: the agent reads the literal too. So for that payload the pod was announcing a difference no
reader can observe, on the two surfaces O7 had just connected — a false σ (O1) whose delivery this
round had *widened*. The fix narrows the trigger to the manifest and keeps `Files` as the
measurement (every carrier), which is what makes the remaining sentence — "in every file listed" —
checkable; the sentence itself now names where the substitution happens.

Witnesses: `TestBaseDirTokenInABundledFileIsNotAReaderDifference` (the rule: a script-only token is
not a reader difference, and the scan still reports the carrier),
`TestBaseDirWarningListsEveryCarrierNotOnlyTheManifest` (the list is the measurement, not the
trigger), `TestCatalogHandlerCarriesTheCodesAndTheWarnings` (the wire — the narrowed trigger is
witnessed at the delivery point too), and `TestScanAndCatalogReportTheBaseDirTokenAsAWarning`
(the sentence names `SKILL.md`). Falsified for real: the trigger back to "any carrier" reddens 2
(the rule witness and the wire), and the sentence back to "replaced when this agent loads the
skill" reddens 1. The case that is now *not* reported — a token only a bundled file carries — is
recorded as a decision, not dropped in silence: nobody resolves it, so it is an authoring lint and
not an egress fact, and it would need its own obligation before it gets a code.

### 10.8 O8 — a switch must have a reader on the production path, and its promise must be isomorphic to its effect

O1–O7 are about a fact that is **produced** and has to arrive. O8 is about a **switch**: a settings row
whose entire meaning is "this behaviour is on". Its failure mode is quieter than a lost δ — nothing is
produced at all, so there is nothing to lose, no surface to contradict, and every test of the mechanism
stays green while an operator's click does nothing. Four rows sat in the tree in exactly that state at
once (register 49, 51, 52, 53), and they shared one shape: **the row was writable, readable back through
the panel's mirror, and its only reader lived in `NewAgentWithFullCfg`, a constructor no production path
calls** (`NewAgent`, `NewAgentWithSkillsCfg` and the Manager's `buildAgent` are the callers that exist).

The criterion, in two halves:

> **A switch that is visible to the operator must have a reader on the production path** — row → the
> resolved cfg → the option that carries it → the gate that acts on it, each hop witnessed, with the
> falsification able to redden the hop that is missing (§5.1's rule applied to a switch; the register's
> "belongs to" column now names the chain for rows 49/51/52). **And the switch's promise must be
> isomorphic to its effect**: what the field's own documentation says the value means — nil = inherit,
> `false` = veto, "on ⇒ what leaves for a provider is redacted" — has to be what the gate does. A row
> whose comment describes one behaviour while its gate implements another is a false σ that no test of
> either half can see.

The four instances, and how each ended. **49 `privacy.piiScrubbing`** (`19834b5`): forcing the flag on at
the call site showed the redaction was applied at three of eleven provider call sites, so the row was
not merely unread — its promise ("if the row is on, what leaves is redacted") was false even where it
was read; the fix is one home, on the provider every model call already passes. **51 `skillsLearner`**
(`17f3a3f`): the row was read only by the zero-caller constructor, so turning it on learned nothing; the
Manager now builds the learner and the learned `SKILL.md` lands through the single writer for
`skills/…`. **52 `memory.autoPersist`** (`4ecadc7`): the per-agent field documented "nil = inherit",
while nothing could inherit — the workspace row's only reader was the same dead constructor, so "no
override" meant "off" (the opposite of the promise) and `everyNTurns` / `model` had no path at all.
**53 `memory.fts`** (`86c38b9`): neither end of it was ever wired (the store was constructed only in
that constructor, no caller ever passed a searcher), so the row's promise — a full-text index backing
`memory_search` — was never true; it was deleted rather than wired, because the file scan is a complete
implementation and a per-pod sqlite index would have to be rebuilt for multi-replica correctness.

**The fifth instance is a render, not a row, and it was found by auditing the fix of the fourth**
(2026-09-22). The cloud context panel's auto-remember switch writes the per-agent override and rendered
it as if it were the state (`agent?.autoPersist ?? false`); the agent record carries the override and
nothing else. That was harmless for exactly as long as row 52 had no reader — and the moment row 52 got
one, a workspace row turned on meant the panel said Off while the runtime distilled memory every fifth
chatter turn. The panel now reads the two facts separately (`useWorkspaceMemoryAutoPersist`), renders
the inherited state, and renders **unknown** when the row cannot be read (a non-admin dashboard answers
403 to `GET /api/config`) rather than letting an absent read become "off". Fixed in cloud `80ac0833`.

**The writer half, stated narrowly** — because over-claiming it would be noise: a switch that a surface
*presents as a control* must be writable by the role that surface is for, or else the same false σ
appears from the other side (a control that cannot change what it appears to control). Where the only
writer is the config API, the row is an operator/API switch and the surfaces that show it must say so.
Measured in two passes. On 2026-09-22 morning all three rows were API-only — `POST /api/config` was the
writer, no panel had a form for them, and no surface claimed otherwise — which is why this half was
recorded rather than booked: nothing was misrepresenting a row. The same afternoon the first two got the
control they were missing: the fastagent webui's Runtime page (super_admin ⇒ `scopeForSave` → system
scope, the scope the sandbox block is already saved from) now carries `memory.autoPersist` (enabled /
cadence / model) and `skillsLearner` (enabled / tool-call floor / model) — register rows 52 and 51, whose
readers were witnessed days earlier while their writers were a hand-made HTTP call. `privacy.piiScrubbing`
followed on the same page (`70fa91e`), so the row that was still missing its writer — row 49's reader was
fixed on 2026-09-22 and its writer was a hand-made POST — now has both halves, and its card states the limit the
switch keeps (the model's own earlier reply is replayed byte-for-byte, so a PII echo inside it travels).
The writer half therefore ends with **no row left API-only** among the four instances; the rule stays for
the next switch, which is why it is a checklist item and not a one-off pass.

**The four rows as they stand (2026-09-22).** The table exists because answering "which rows are
switches, who reads them, and who can write them" took three separate greps in two days — the register's
cells say what *changed*, this says what *stands*:

| Row | Resolved at | Reader on the production path | Writer | Witnesses |
|-----|-------------|-------------------------------|--------|-----------|
| 49 `privacy.piiScrubbing.enabled` | system ← user ← agent (`gateway/userspace.go`, `scope.SettingInto(…, NSPrivacy, …)`) | `Agent.setProvider` wraps the provider once per agent, so every model call is redacted | webui Runtime page (super_admin ⇒ system scope), or `POST /api/config` | `TestThePiiScrubbingRowReachesEveryAgentProvider`, `TestRuntimePage_CanSetMemoryAndSkillLearning` |
| 51 `skillsLearner.enabled` / `.minToolCalls` / `.model` | same chain (`NSSkillsLearner`) | `Manager.buildAgent` → `enableSkillsLearner` | same page, or the API | `TestTheSkillsLearnerRowReachesTheLearnerAndWritesThroughTheSingleWriter`, `TestTheSkillsLearnerRowReachesTheSingleWriter` (cloud path) |
| 52 `memory.autoPersist.enabled` / `.everyNTurns` / `.model` | system ← user (`NSMemory`); the per-agent `agents.defaults.autoPersist` stamps over it | `managerOptions` → `WithMemory` → the runPostTurn gate | same page for the row; the agent Context panel (cloud) for the per-agent override | `TestTheMemoryRowIsTheDefaultLayerAndThePerAgentFlagOverridesIt`, `TestTheMemoryRowIsWhatTurnsAutoPersistOn`, cloud `src/__tests__/fastagent/auto-persist-inherited-state.test.tsx` |
| 53 `memory.fts.*` | — (deleted) | — | — | none: the deletion (`86c38b9`) is verified by a repo-wide reference count plus the suites |

Witnesses (one per hop, and the falsification for the hop that matters): `19834b5` —
`TestThePiiScrubbingRowReachesEveryAgentProvider` (the row reaches the provider the gateway builds) and
`TestTheSwitchRedactsEveryModelCallTheTurnMakes` (every call site the turn makes); `17f3a3f` —
`TestTheSkillsLearnerRowReachesTheLearnerAndWritesThroughTheSingleWriter` and
`TestTheSkillsLearnerExtractionCallSitsInsideThePiiScrubbingRule`, plus
`TestTheSkillsLearnerRowReachesTheSingleWriter` on the cloud path; `4ecadc7` —
`TestTheMemoryRowIsTheDefaultLayerAndThePerAgentFlagOverridesIt` (the four precedence cases, including
the veto) and `TestTheMemoryRowIsWhatTurnsAutoPersistOn` (row → real turn → the pass fires); `80ac0833` —
cloud `src/__tests__/fastagent/auto-persist-inherited-state.test.tsx` (the render: inherited on / off /
unknown, and no inherit line once an override exists). Each row's own falsification was run for real and
is recorded in the register cell; `53` is deletion-shaped, so its verification is a repo-wide reference
count plus the suites, not a witness. The writer half landed as `e611f3c`: the page's payload is pinned by
`web/src/__tests__/runtime-settings-memory-learning.test.tsx` (the four cases, including "an emptied box
means 0, the wire's unset"), and the hop from that payload to the reader by
`TestRuntimePage_CanSetMemoryAndSkillLearning` (real handler, real store, system scope, read back through
the typed call the gateway makes). Falsified for real: dropping the two namespaces reddens 3 of the 4 web
cases, and saving at user scope instead of system scope reddens the Go test on every assertion.

### 10.9 No state for a derived fact — stopping a task is an operation over its turns (2026-09-26)

A design rule written down *before* the capability is built, because the shape it would take by default is
the wrong one. The MCP surface stops things **one step at a time** — `stop_task` (the turn that is running) and
`withdraw_task` (the one still queued); cloud `docs/mcp-task-submission.md` §14.2/§14.7 — and the obvious next
ask is "stop the whole task". The obvious implementation — a `cancelled` status on the session/task row — is a **second source
for a fact that is already derived**: a task is stopped exactly when none of its turns is still queued or
running, and that is read off the same two inputs `cancelTurn` already consults
(`internal/setup/turn_cancel.go`: the request's own pending entry, then the session's live lease row). A stored
flag would then have to answer questions the turn facts already answer — what does a turn added *after* the
flag mean? does the flag lie when a queued turn starts anyway? — and each answer is a new way for σ to be false
(O1) or for an absence not to speak (O6).

> **A state may only be added for a fact that has no derivation.** Stopping everything a task still has
> unfinished is *one operation applied to that set* — today two calls (`stop_task` + `withdraw_task`), and if a
> single one is ever added it carries the same shape: whose whole effect is visible in the per-turn facts it
> changes, no task-level state, and idempotent for the same reason `cancelTurn` is (withdrawing twice is one
> withdrawal; stamping twice is one request on the same possession).

Three consequences, stated because they are what make the operation honest rather than merely flag-free:

* **The derivation is only as good as its inputs, so fix the input rather than hiding it.** One input is
  currently weak: the *queued* half lives in the receiving pod's in-process map, so on the MCP surface a
  queued step cannot be found at all (the adapter releases the stream, the handler returns, the entry is
  unregistered — measured on dev 2026-09-26, recorded as the second hole in cloud §14.6). A task-level
  `cancelled` flag would paper over exactly that: it would say "stopped" while a queued turn ran anyway,
  which is the shape of the bug that was just measured, one layer up.
* **Its answer is a count, not a boolean**: "withdrew 1 queued, stamped 1 running, 2 already finished". A
  bare `{canceled:true}` claims more than the server did — the single-turn tool has that shape today, which
  is why the cloud doc measures it instead of trusting the sentence.
* **It cannot be a promise about the future.** A task gains turns; "stopped" is a statement about the turns
  that existed when it was called, and a later turn is a new step. That is a property of the operation, not a
  limitation of the store.

No witness is owed by this section: it adds no obligation, no gap, and no code. It is the design half of
§5.1's rule — the same reason a delivery point is not a rule, a derived fact is not a state.

### 10.10 O9 — an external consumer needs its own delivery point (2026-09-26)

O1–O8 all describe a fact travelling to a reader **inside** the loop. That is why §6's item 4 can ask its
question at all: "when this change happened, was the premise *the agent is reading something* true?" — for
an in-harness reader the remedy is always "attach it to the next tool result / the next prompt". An
**external consumer** (a process outside the harness: an MCP client, an out-of-loop dashboard, a webhook
subscriber) has no such forcing function. It can call, it can subscribe, and it can also simply never ask
again — and nothing in its loop will ever make it.

> **O9 — an external consumer must be given a delivery point, or the contract must name who reads and
> when.** "It can pull whenever it wants" is a delivery point only when the contract says the pulling
> *is* the mechanism; silence is not delivery to a reader whose loop never forces it.

The shape that satisfies it, worked out on the MCP task surface
(`tokenaissance-cloud` `docs/mcp-task-submission.md` §14.2/§14.8):

| Half | Role | Rule |
| :--- | :--- | :--- |
| **pull** — a call the consumer makes (MCP: `read_task`) | **authoritative** | the reply *is* the delivery point, so §5.1's C-family exemption applies: its rule witness is also its delivery-point witness |
| **push** — a native notification the consumer subscribed to (MCP: `resources/subscribe` → `notifications/resources/updated`) | **a hint only** | it carries "something changed", never the fact itself — otherwise push becomes a second source for one fact |
| **reconciliation** — one pull after any gap | required | notifications are best-effort (lost, duplicated, delivered after a reconnect); a design that needs `notifications/progress` to be reliable is a design with no pull point |

**Its first instance** is the audit that promoted it (register row 65): the MCP state design was pull-only
(`status`/`outcome` readable only if the client asked), every test it had was green, and the tests were its
only reader. The user's ruling on 2026-09-26 was to give both halves — pull **and** a native push — with the
roles above.

No obligation is owed by the *harness* until something external consumes it: O9 binds a surface that
advertises itself to an outside reader, and its witness is a test that **the reply or the notification
actually left** — reddening only the rule witness is no evidence about the delivery point (§5.1).
