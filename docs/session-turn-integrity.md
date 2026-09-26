# Session turn integrity: one writer per session, non-pollutable history

> **Status**: P0–P6 landed, plus Q4 (no persisted synthetic replies). P0–P3 are
> deployed to dev + prod (`20260914015438-deploy-54b07f7`); P4, P5 (grace +
> per-source budgets), P6, Q4 and the timing-margin test fix landed after that
> deploy. Q6 (cross-replica session lease) is **decided-deferred**, with its
> reopen triggers written down in
> [Deferred: cross-replica session lease](#deferred-cross-replica-session-lease-q6).
> **Progress**: P0 wire dedupe + pad scoping + compaction ctx + production data
> repair · P1 session turn gate (`Session.AcquireTurn/ReleaseTurn`, wired into
> `HandleMessage` and `HandleMessageStream`) · P1b `queued` event +
> Codex-style queue block with Edit/Cancel and a withdraw endpoint ·
> P2 `TurnMode`/`RunTurn` + gateway parking of
> automatic turns · P3 `normalizeForPrompt` applied to the prompt in both
> loops · Q4 the loop persists no synthetic "interrupted" reply (the projection
> writes it at prompt-build time) · P5 tool grace · P6 NUL-safe archive +
> `fastagent doctor sessions`.
> Test names live in [Implementation plan](#implementation-plan). Convention:
> every `Test…` name cited in this document exists in the tree as
> `func Test…` **unless it is marked `(planned)`** — those are the
> design-first cases named but not written yet, and they are collected in
> [Integration / e2e](#integration--e2e). Before closing a phase, grep the
> names it claims rather than trusting this page:
> `comm -23 <(grep -oh 'Test[A-Z][A-Za-z0-9_]*' docs/*.md | sort -u) <(grep -rho 'func Test[A-Za-z0-9_]*' --include='*_test.go' . | sed 's/func //' | sort -u)`
> — every name it prints must carry `(planned)`, `(not in the tree)` or
> `（已移除）` at the citation (same table row or bullet).
> **Scope**: how a turn is admitted for a session, and how that session's
> history stays structurally valid for every provider.
> **Storage**: `sessions.messages` (working set the agent loop reads) plus
> `session_messages` (append-only archive the UI reads), Postgres in prod.
> **Last updated**: 2026-09-14
> **Decision owner**: mengmengmengqiang@gmail.com
> **Reviewed by**: pending review (this document)
> **Incident**: 2026-09-13 production, agent `agt_cda27bbfbf4a84e2dfa6`,
> session `hJKMWwtOp3mJOtqN8Uz2mW` (see [Appendix A](#appendix-a--incident-evidence)).
> **Reference design**: Codex ([github.com/openai/codex](https://github.com/openai/codex),
> a local checkout may live at `~/Project/tokenaissance/codex`), cited inline.

## Problem

Production surfaced a provider 400 that made one chat session permanently
unusable:

```text
API error 400: {"error":{"message":"Messages with role 'tool' must be a
response to a preceding message with 'tool_calls'", "type":"invalid_request_error"}}
```

It repeated on **every** turn that session ran on a DeepSeek
(OpenAI-compatible) model — 40 error lines across 10 failed turns (two
retries + the terminal failure + the turn error, four lines per turn)
between 11:35Z and 13:45Z on 2026-09-13, one turn per cron tick — while the
same session ran normally whenever the agent was switched to Claude. The
session history had been written into a shape the OpenAI-compatible
validator rejects: one assistant declaring two tool calls followed by
**four** tool replies (the synthetic "stopped" pad for each call, then the
real result for each call).

Two things made a single bad write permanent:

1. `sessions.messages` **is** the prompt. There is no derived, normalized
   projection, so any structural damage is replayed verbatim on every later
   turn.
2. Two turns can write the same session concurrently, and one of the writers
   (`padOrphanToolResults`, removed in Q4) reasoned about the session globally
   ("the last assistant with tool calls") rather than about its own turn.

## Root cause

### Layer 1 — two writers, no admission

A turn can be started from several entry points, and only some of them
serialize against each other:

| Entry point | Path | Serialized by |
|---|---|---|
| IM / cron / goal / heartbeat / subagent delivery | bus → `processInbound` → `taskQueue.Submit` → `ag.HandleMessage` (`internal/gateway/gateway.go:521`) | `taskqueue.Queue`, per `chatKey(channel, accountID, chatID)` (`internal/taskqueue/queue.go:94`) |
| Dashboard chat POST (streaming) | `handleChatStream` → goroutine → `HandleWebChatStream` → `HandleMessage` (`internal/setup/handlers.go:1254`) | **nothing** |
| Dashboard chat POST (non-streaming), webhook, API-key `/v1/chat/completions` | `HandleWebChat` / `HandleMessage` / `HandleMessageStream` | **nothing** |

`HandleWebChatStream` and the queue worker both end up in
`Agent.HandleMessage` (`internal/agent/loop.go:2239`) or `HandleMessageStream`
(`:3092`), and neither path takes a per-session lock. Two turns on one
session can therefore interleave their appends.

**This layer was not hypothetical.** On 2026-09-04 a `continue` sent while an
`exec` was open started exactly such a second writer, on whatever pod served it
— ten days before the gate existed. The session log is
[Appendix E](#appendix-e--the-same-shape-on-2026-09-04-before-the-gate); it is
the same shape as the 2026-09-18 incident, in the form P1 removed.

### Layer 2 — the pad is a global scan, executed by a *different* turn

`padOrphanToolResults` (`internal/agent/loop.go:2937`) runs from each turn's
`defer` and, before P0, looked for "the last assistant message carrying
tool calls in the whole session" and padded every unresolved id it found
there. In the incident the two overlapping turns were:

* **A** — the cron-fired task `task-1789299000292-1`, started 11:30:00.292,
  killed by the 300 s task-queue timeout at 11:35:00.897
  (`duration_ms=300604`);
* **B** — a dashboard turn started 11:34:32.776.

Because A's tool round was still in flight (`exec` inside the sandbox,
`list_cron_jobs`), B's defer padded **A's** tool_use ids:

```text
11:35:00.439 WARN msg="padding orphan tool_use with stopped result"
             toolCallID=call_00_38dFV2Ml48bAd3FgPCS13513 tool=list_cron_jobs
11:35:00.567 WARN msg="padding orphan tool_use with stopped result"
             toolCallID=call_01_3nApOZmdQE6lQFv22vhH6960 tool=exec
```

A's real results then landed after those pads, leaving two replies for each
`tool_call_id`.

### Layer 3 — the wire sanitizer modelled two shapes, not three

`findOrphanToolCalls` (`internal/provider/openai.go:148`) only knew:

1. an assistant whose declared `tool_calls` are not answered by the
   immediately following run of tool messages (strip the call), and
2. a tool message whose id no earlier assistant declared (drop the reply).

A duplicate answer violates neither: the id *is* answered in the immediate
run, and it *was* declared earlier. So the request shipped a second answer
to an already-closed call. DeepSeek rejects that; the Anthropic conversion
path tolerates it (`internal/provider/anthropic.go:61` coalesces tool
results per assistant), which is why switching models made it look like a
provider or cron problem rather than history corruption.

### Amplifiers

* **300 s turn budget.** `taskTimeoutSec` defaults to 300
  (`internal/gateway/gateway.go:418`) and kills a turn mid-tool — the exact
  condition that produced a pad. Long sandbox work (`setsid nohup …`,
  multi-minute `exec`) exceeds it routinely.
* **Compaction that never compressed.** The summarizer was called with a nil
  context (`internal/agent/compaction.go:190`, now fixed), so every
  compaction fell back to pruning-only and the session stayed at ~130 k
  tokens, re-running compaction on every tick and spending its budget before
  the real work.
* **Archive writes that fail silently for NUL-bearing tool output.**
  `session archive append error: ERROR: invalid byte sequence for encoding
  "UTF8": 0x00 (SQLSTATE 22021)` appears next to the incident: the real
  reply never reached `session_messages`, so the archive and the working set
  disagree (see P6).

### Why "session pollution" is the right name

We use the term for a persisted, propagating, easy-to-miss violation of the
history's structural contract:

| Property | Why it matters here |
|---|---|
| **Persisted** | the bad shape is written to `sessions.messages`, not a transient buffer |
| **Propagating** | every later turn sends the same history, so one bad write breaks all future turns on that session |
| **Hidden** | providers disagree about tolerance (Anthropic ok, DeepSeek 400), so the symptom looks provider-specific |

Note the distinction the design has to preserve: an *incomplete* history
(a tool call whose result never arrived because the turn was interrupted) is
**truth, not pollution**. Codex persists exactly that and repairs it at
prompt-build time. Pollution is when the *derived* prompt is structurally
invalid, or when two turns' intents are interleaved in one history.

## Reference design (Codex)

Codex separates the two concerns we currently conflate: *who may write a
turn* and *what the model is allowed to see*.

**1. One active turn per thread, with explicit admission semantics.**
`session.active_turn` is a single `Option<ActiveTurn>` slot; a turn is
created through `active_turn.get_or_insert_with(ActiveTurn::default)`
(`codex-rs/core/src/tasks/mod.rs`, around the `run_turn` path). Every
producer must pick a submission mode
(`codex-rs/protocol/src/turn_input.rs:133`):

```rust
enum TurnInputMode {
    StartOrSteer,                      // idle → start; busy → steer the running turn
    StartIfIdle,                       // idle → start; busy → NotSubmitted{NotIdle}
    Steer { expected_turn_id: String },// steer only that exact turn
}
```

and must handle the refusal
(`NotSubmittedReason`, same file, `:217`): `NotIdle`, `NoActiveTurn`,
`ExpectedTurnMismatch`, `ActiveTurnNotSteerable{Review|Compact}`, `PlanMode`,
`EmptyInput`, `ActiveTurnOutputSchemaMismatch`. `session/turn_input.rs` is
documented as *"the one place Core decides whether submitted input starts a
turn, steers an active turn, or is rejected"*; `start_if_idle` (`:327`)
returns `NotSubmitted{NotIdle}` instead of starting a second turn, and
queued work is picked up at an idle boundary
(`maybe_start_turn_for_pending_work`). Automatic sources (scheduled work,
mailbox/`trigger_turn` deliveries, memory writebacks) use `StartIfIdle`;
user input uses `StartOrSteer`; an active `Review`/`Compact` turn is
explicitly not steerable.

**2. History invariants are enforced on a derived prompt, not on the log.**
`History::normalize_history`
(`codex-rs/core/src/context_manager/history.rs:466`) states them:

> 1. every call (function/custom) has a corresponding output entry
> 2. every output has a corresponding call entry or names an external tool event
> 3. unsupported image and audio content is stripped

It runs inside `for_prompt()` (`history.rs:218`), i.e. the persisted rollout
may contain an unfinished call, and the prompt never does.

**3. Repair is deterministic and idempotent.**
`ensure_call_outputs_present`
(`codex-rs/core/src/context_manager/normalize.rs:21`) inserts a synthetic
`FunctionCallOutput` **immediately after** the call (`items.insert(idx + 1,
…)`, applied in reverse index order) and only when that `call_id` has no
output **anywhere** in the list — so it cannot double-answer, and it cannot
duplicate on a second pass. Its id is derived from the call id
(`uuidv5(namespace, "fco:<call id>")`) with an explicit comment that
changing the namespace would change model-visible ids and invalidate prompt
caches. `remove_orphan_outputs` drops outputs with no call. Tests:
`context_manager/history_tests.rs:1664+`
(`normalize_adds_missing_output_for_function_call`,
`normalize_removes_orphan_function_call_output`, …).

**4. Removal is pair-aware.** `History::remove_first_item()`
(`history.rs:293`) deletes the counterpart of the item it drops
(`normalize::remove_corresponding_for`), so truncation cannot split a pair
— and compaction, which replaces the item list wholesale, is re-normalized
at the next prompt build rather than needing its own pair logic.

## Decision

| # | Decision | Rationale |
|---|---|---|
| **D1** | One turn at a time per **session** (not per chat key). Every turn-start entry point passes through one admission gate owned by the session. | The session is the unit that has history and memory; `shared_identity` channels and URL-token recovery already map multiple `(channel, accountID, chatID)` triples onto one session, so a chat-key lock is not sufficient. |
| **D2** | Default policy for a turn-start request that finds a turn in flight is **queue and run after it** — not steer, not run concurrently. | Chosen 2026-09-14: the dashboard keeps steering as a deliberate user action. Deterministic, and it is what makes a single writer possible without changing what the model sees mid-turn. |
| **D3** | Steering stays explicit: the web UI keeps its `/api/chat/steer` button; inbound IM messages keep today's best-effort auto-steer (`trySteer`, `internal/gateway/routing.go:258`). Both fold into the running turn instead of starting one. | Steering is not the defect — concurrent *turns* are. IM responsiveness is a product property we are not changing in this doc (see Q1). |
| **D4** | The prompt is a **derived, normalized projection** of history. A pure `normalizeForPrompt` becomes the authoritative guard that every provider sees valid calls/replies. | Provider-specific wire sanitizers are a second line of defence, not the contract; Anthropic already needed an extra sweep the OpenAI path did not have. |
| **D5** | Persisted history stays truthful. We do not rewrite the model's own record except to repair pollution (as in Appendix B); structural fixes are applied on the way *out*. | Keeps audits, prompt-cache stability and the UI's "what actually happened" intact. |
| **D6** | Truncation/compaction never splits a pair. | `safeCompactionCutoff` today only handles the tail starting with a tool message; normalisation makes the whole class unrepresentable. |

### Non-goals

* Not a rewrite of the task queue or of session storage.
* No change to what an IM user sees while a turn is running (typing indicator
  stays; steer behaviour unchanged).
* No cross-session or cross-agent coordination: the invariant is per session.
  Two agents, or two sessions of one agent, may still run in parallel.

## Invariant

Written as four separately falsifiable clauses, each with the mechanism that
enforces it and the test that would catch a violation.

| # | Clause | Violated when | Enforced by | Guarding test |
|---|---|---|---|---|
| **W** · single writer | At most one turn is executing against a session's history at any instant | two `HandleMessage` bodies are past admission for one session | session turn gate (P1, landed): FIFO waiter queue owned by the session | `TestAcquireTurnSerializesCallers`, `TestAcquireTurnHandsOffFIFO`, `TestAcquireTurnContextCancelDoesNotLeakSlot`, `TestHandleMessageWaitsForInFlightTurn`, `TestHandleMessageSerializesQueuedTurns`, `TestQueuedTurnRunsAfterLongTool`, `TestCronTickDoesNotInterleaveWithWebTurn`, `TestConcurrentWebAndCronTurnSerialize`, `TestGoalContinuationDoesNotDeadlock` (each of the last four was verified to fail with the gate neutered) |
| **P** · pair integrity | For the model, every tool call has exactly one reply, and every reply belongs to a call | a request ships N replies for a call id, an unanswered call, or an orphan reply | `normalizeForPrompt` (P3, landed) + wire builder (`internal/provider/openai.go:148`, P0) | `TestNormalizeForPromptShapes` (7 shapes + idempotence + no mutation), `TestNormalizeForPromptReadsRawAssistantCalls`, `TestNormalizeForPromptStripsDuplicateCallDeclaration`, `TestToAPIMessagesDropsDuplicateToolReplies`, `TestToAPIMessagesDropsDanglingToolReplies` |
| **O** · ordering | A turn's own messages append in order and are never interleaved with another turn's | a user message or tool reply from turn B lands between turn A's call and its reply | clause W (there is no other writer) | `TestHandleMessageSerializesQueuedTurns` (asserts the exact role sequence `user,assistant,user,assistant` and the user-message order, not just counts), `TestQueuedTurnRunsAfterLongTool`, `TestCronTickDoesNotInterleaveWithWebTurn`, `TestConcurrentWebAndCronTurnSerialize` (the cross-source pair, through the real chat handler) |
| **T** · truthful pad | Stored history carries **no** synthetic "interrupted" reply: an interrupted turn leaves its call open, and the projection answers each open call with exactly one synthetic reply at field-build time | a synthetic reply is persisted at all, an open call gets two of them in the projection, or a reply is emitted for a call that is already answered | Q4 (pad path removed) + `normalizeForPrompt` (P3) | `TestInterruptedTurnLeavesNoSyntheticReplyInHistory` (loop detection breaks out mid-tool: history has the open call, no pad; the projection is doctor-clean), `TestNormalizeForPromptShapes` (unanswered call indexed in place, duplicate collapses to one, idempotent), `TestTurnBudgetExpiryLetsInFlightToolRecordItsResult` |

Accepted windows / known gaps (to keep the table honest):

* W is per **process**. Two gateway replicas serving the same session can
  still both admit a turn (the sandbox pool solved the same problem with a
  Postgres lease). Accepted deliberately rather than solved: decided
  2026-09-14, with the evidence that would reopen it written down in
  [Deferred: cross-replica session lease](#deferred-cross-replica-session-lease-q6).
  **That evidence arrived on 2026-09-18** — see
  [Trigger fired](#trigger-fired-2026-09-18-two-replicas-one-session) and
  [Appendix D](#appendix-d--cross-replica-repro-2026-09-18). Until the lease
  exists, treat "two replicas, one session" as a live defect, not a gap.
* The projection's synthetic reply says *why* the call is unanswered: it
  always says "interrupted". That is true when the owning turn died and false
  when the owning turn is still running on another replica — the case below.
  A truthful pad needs the same fact the lease would provide (does anyone
  still hold this session?), so both fixes are the same work.
* P is guaranteed for OpenAI-compatible and Anthropic wire builds; other
  providers inherit `normalizeForPrompt` because it runs before the provider
  split.
* A synthetic reply exists only in the projection. Stored history therefore
  shows an *unanswered call* for every interrupted turn, which the doctor
  scanner reports as expected and does **not** gate on (Q4, decided
  2026-09-14 — Codex parity). Sessions written before Q4 keep their pads until
  the next `doctor sessions --fix`; the projection collapses those duplicates
  at request time either way.

## Design

### P1 — Session turn gate (single writer) ✅ landed

Owned by `session.Session` because the session already owns its history,
steer buffer and turn depth, and because `Manager.Get` is the single place
every entry point resolves a session through.

Landed API (`internal/session/manager.go`): `AcquireTurn(ctx) bool`,
`ReleaseTurn()`, plus `TurnActive()` / `TurnWaiters()` for logs and tests.
Handoff keeps `turnActive` set and closes the waiter's channel, so the slot is
never momentarily free and a fresh caller cannot jump the queue; `ReleaseTurn`
without the slot is a no-op rather than a way in.

```go
// AcquireTurn blocks until this caller holds the session's single turn slot,
// or ctx ends. Callers MUST run exactly one turn between AcquireTurn and
// ReleaseTurn, and MUST NOT start another turn for the same session while
// holding it.
func (s *Session) AcquireTurn(ctx context.Context) bool

// ReleaseTurn frees the slot and hands it to the longest-waiting caller.
func (s *Session) ReleaseTurn()
```

* FIFO waiter queue (`[]chan struct{}`), so queueing is fair and the order of
  user messages is preserved.
* `ctx` cancellation while waiting removes the waiter and does **not** leak
  the slot (including the race where the slot is handed over at the same
  moment the ctx ends).
* Acquisition points: `HandleMessage` (after the slash-command and quota
  gates, before the plan-mode branch so plan mode is covered too) and
  `HandleMessageStream`. Everything else — `HandleWebChat`,
  `HandleWebChatStream`, webhook, API — reaches those two.
* `defer sess.ReleaseTurn()` sits **outermost** so it runs after the existing
  defers (`flushLeftoverSteer`) — a parked steer is part of the turn that
  owned the session, not of the next one.
* Steering is unaffected: `PushSteerIfActive` keeps using the existing
  in-flight window; the gate and the steer window are, by construction,
  held by the same turn.
* Re-entrancy is forbidden: a turn must never call back into
  `HandleMessage`/`HandleMessageStream` for the same session synchronously.
  Goal continuations satisfy this by construction — `goal.TryFireContinuation`
  publishes onto the bus (`internal/agent/goal/continue.go:43,54`) instead of
  calling back into the agent, so the follow-up turn runs on the gateway's
  goroutine and simply waits for the slot. The explicit no-deadlock e2e test
  is still on the [Integration / e2e](#integration--e2e) list.

### P1b — Queued-state UX (dashboard), modelled on Codex ✅ landed

Codex renders pending input in a dedicated `PendingInputPreview` widget above
its composer (`codex-rs/tui/src/bottom_pane/pending_input_preview.rs`), not in
the transcript:

```text
• Queued follow-up inputs
  ↳ Hello, world!
  ↳ This is another message
    ⌥ + ↑ edit last queued message
```

(snapshot `render_two_messages`; sections above it read *"Messages to be
submitted after next tool call (press Esc to interrupt and send immediately)"*
and *"Messages to be submitted at end of turn"* — Codex keeps steers and
plain queued follow-ups visually separate, dim/italic per message, 3 lines
max, FIFO, and submits exactly one queued message when the turn goes idle.)

Mapping to fastagent:

| Codex | fastagent dashboard |
|---|---|
| queued message never shown as a turn until it starts | unchanged: the optimistic bubble stays where it is; the queue block lives above the composer (the transcript keeps only the turn that is producing output) |
| `• Queued follow-up inputs` header, `↳ text`, dim + italic, 3-line cap | same header/arrow/italics; the text comes from the `queued` event and is truncated to one line by the composer's width |
| `⌥ + ↑ edit last queued message` | **Edit queued message** link — withdraws the queued turn server-side and restores the text into the composer |
| interrupt / queued-message removal | **Cancel** link — withdraws the queued turn; nothing is written to the session |
| one queued message submitted at a time, FIFO | the session turn gate (P1) + `(n ahead)` in the header |
| submitted when the turn goes idle | automatic: the waiting POST acquires the slot the moment the current turn releases it |

Backend surface for the two actions: `POST /api/chat/cancel` with
`{agentId, sessionId, turnId}` → `200 {"canceled":true}` while the turn is
still queued, `409 {"reason":"already_started"}` once it holds the slot (the
client then falls back to plain Stop semantics — detach this stream, the
server keeps the turn), `404 {"reason":"not_queued"}` when nothing is
registered for that turn id. The "started" bit comes from
`agent.WithAdmissionSignal` (closed by the agent right after it acquires the
session's turn slot), not from the session event hub — the hub is
session-scoped and also carries *other* turns' events (cron ticks stream into
the same chat panel).

**When the block comes down (repaired 2026-09-26 on the cloud client).** The hub's session scope cuts
both ways: while a submission is parked, the events arriving on *its* connection are the holder's, so
"any real event means my turn started" erased the block — and with it the only withdraw control — a
moment after it appeared. Measured on the live two-tab spec
(`tokenaissance-cloud`, `e2e/tests/live/queue-send-behind-a-peer.spec.ts`, gated by
`E2E_LIVE_QUEUE=1`): the parked tab's first event was the holder's `content_delta`. `ChatEvent` is
`{type, data}` — no turn identity — and turns are serialized, so the earliest derivable "this turn
started" is **the holder's end**: the cloud client now clears the block on `done` / `error` only
(`use-stream-pipeline.ts`; witness `queued-turn-holder-events.test.tsx`, falsified by restoring the
old rule). If a third waiter wins the slot first, the lease wait re-emits `queued` on every retry, so
the block comes straight back. The structural alternative — stamping each event with the turn that
produced it — is recorded here as the follow-up to reach for if the client ever needs to attribute
events in general rather than in this one place.

**The queue σ carries whose submission is waiting (2026-09-26).** `turnlease.go` and `loop.go` now put
the submitting turn's id into the `queued` payload (`data["turnId"]`), because this is the one queue
fact whose reader has to *act on a submission*: a tab that learns "something of mine is queued" from a
re-emission — a reload, or any tab that did not POST — can only withdraw it if it knows the id the
submitter minted, and the cloud client was already reading that field (`use-chat-subscription.ts`),
so it was a dead key until the producer sent it. Witness:
`TestSecondReplicaQueuesBehindTheRunningTurnE2E` (falsified by removing the field). Deliberately not
done: identity on the event envelope. Turns are serialized per session, so "the holder ended" answers
every other rule, and a wire field with no second reader is the kind of thing this roster deletes.

**What the receipt does NOT fix, measured while tuning the live spec (2026-09-26): a reload.** The
reloaded tab still renders the block (from the replayed `queued` row) but its Cancel sends nothing —
the replayed payload predates the field — and even a direct `POST /api/chat/cancel` with the right id
answers `{"canceled":false}`: **the pending entry died with the POST that created it**. The root cause
is narrower than "the queue is not in the store": `registerPendingTurn` stores the *turn's own* cancel
func (`defer cancel()` lives in the turn goroutine), yet the register/unregister pair sits in the HTTP
handler — so the entry's **lifetime is the connection's**. Affinity cannot help: it fixes *addressing*
(which pod), not lifetime (whether the entry is still there). `tokenaissance-cloud`
`docs/mcp-task-submission.md` §14.6 now orders the fixes: (1) move the entry's lifetime onto the wait
(the turn goroutine) — about five lines, no new table; (2) narrow the tool's wording until then; (3) the
store-visible queue only as insurance for when affinity stops covering (its trigger, not "frequent
withdrawals"). The live spec keeps its fixture to one live POST and says so.

Not implemented yet: pausing auto-send after an interrupt the way Codex does
(`suppress_queue_autosend`). We have no "interrupt and keep queued" state —
Cancel removes the queued turn outright.

### P2 — Admission result and per-source policy ✅ landed

P1 blocks; P2 makes the refusal explicit so an automatic producer never holds
a queue worker (and its 300 s budget) behind a user turn.

```go
// internal/agent/admission.go
type TurnMode int
const (
    TurnStartOrQueue TurnMode = iota // user-facing: wait for the slot
    TurnStartIfIdle                  // automatic: refuse instead of waiting
)
var ErrTurnNotAdmitted = errors.New("turn not admitted: session is busy")

// RunTurn is the admission-aware entry point; HandleMessage stays the
// waiting behaviour every existing caller already had.
func (a *Agent) RunTurn(ctx context.Context, msg bus.InboundMessage) (string, error)
```

`turnModeForSource` maps `bus.SourceCron`, `bus.SourceGoalContext`,
`bus.SourceHeartbeat` and `bus.SourceSubAgent` to `TurnStartIfIdle`; everything
else (including the empty source = a real user turn) is `TurnStartOrQueue`.

The gateway consumes the refusal: the task handler calls `RunTurn`, and on
`ErrTurnNotAdmitted` it **parks** the message
(`internal/gateway/deferred_turns.go`) and returns the queue slot immediately.
A drain loop re-submits a parked message once its session looks idle
(`Gateway.sessionBusy`), keeping per-chat FIFO and dropping anything that has
waited longer than five minutes with a warning. The agent re-checks on every
attempt, so a race just parks the message again rather than running it into a
busy session.

Policy matrix as landed:

| Source | Mode | Busy behaviour |
|---|---|---|
| dashboard POST (streaming / non-streaming) | `StartOrQueue` | queue (D2) |
| `/api/chat/steer` | `Steer` | steer the running turn (unchanged) |
| IM DM / group inbound | `StartOrSteer` | steer (unchanged, Q1) |
| cron tick | `TurnStartIfIdle` | `ErrTurnNotAdmitted` → parked, retried every second for up to 5 min |
| goal continuation / heartbeat / subagent | `TurnStartIfIdle` | same |
| webhook / API-key completion | `StartOrQueue` | queue |
| plan mode | `StartIfIdle` on a session with an active turn | queue, never preempt |

### Product contract: who queues and who steers (decided 2026-09-14)

The table above is a *product* decision, not an implementation detail, so it is
stated as one here. Users should be able to predict what happens when they
write into a session that is already working:

| Surface | While the session is busy | What the user should expect |
|---|---|---|
| Dashboard chat | **the message queues** behind the running turn; it runs when that turn ends | your message is kept, in order, and answered in its own turn — the running turn is not redirected |
| Dashboard "steer" (explicit action) | folds into the running turn | the mid-flight instruction reaches the model at its next tool boundary; the current turn answers it |
| IM DM / group message | **steers** the running turn | the bot reacts to your newest message inside the work it is already doing (the IM convention: latest message wins) |
| cron tick / goal continuation / heartbeat / subagent | never starts a second turn; parked and retried when the session is idle | scheduled work is deferred, not run in parallel with a human's turn |
| webhook / API-key completion | queues like the dashboard | one turn per session, always |

Rationale for the asymmetry: dashboard users can *see* the running turn and
have an explicit control (the steer button), so queueing is the predictable
default (D2); IM users cannot see it and expect a bot to react to their newest
message, so steering stays automatic there (D3). Steering is not what caused
the 2026-09-13 incident — steering writes into the running turn's own message
list, it does not create a second writer — which is why this contract keeps it
where it is useful.

Changing either half is a product change, not a bug fix: making IM queue would
mean routing IM messages through `submitTask` instead of `trySteer`
(`internal/gateway/routing.go`); making the dashboard auto-steer would mean
calling `PushSteerIfActive` in `handleChatStream` before starting a turn.

### P3 — `normalizeForPrompt` (the authoritative guard) ✅ landed

A pure function applied where the prompt is assembled
(`internal/agent/loop.go:2404` and `:3164`, after compaction, before the
system messages are prepended):

```go
// normalizeForPrompt returns a prompt-safe copy of msgs:
//   - every tool call has exactly one reply, inserted immediately after it
//     when missing (synthetic provider.StoppedToolResult reply — the only
//     synthetic reply left in the system, and it is never persisted)
//   - replies whose call id is unknown are dropped
//   - second and later replies for one call id are dropped (first wins)
//   - a call re-declared after it was answered loses the later declaration
//   - input is never mutated
func normalizeForPrompt(msgs []provider.Message) []provider.Message
```

Rules taken from Codex and adapted:

| Rule | Note |
|---|---|
| insert synthetic reply at `call index + 1`, not at the end | keeps adjacency even when another message was appended after the call |
| only when the id has no reply anywhere in the slice | cannot double-answer |
| first reply wins when an id is answered twice (the one adjacent to the call) | same choice the wire builder makes, so the two layers agree |
| a reply is identified by its `tool_call_id` (= the call's id), so the synthetic reply is stable by construction | repeated runs produce byte-identical prompts (prompt cache); nothing extra to derive |
| a call re-declared after it was answered loses the later declaration (`RawAssistant` cleared so serialisers cannot re-introduce it) | a duplicated assistant append cannot create a second unanswered call |
| drop unknown-id and duplicate replies | the third shape the current sanitizer misses |
| never mutate the input slice | the session keeps the truthful record (D5) |

### Q4 — the synthetic reply is prompt-only ✅ landed

The pad was answering two needs: a well-formed prompt (every call has a
reply) and a terminal UI ("interrupted, not still running" instead of a
forever-spinning tool). P3 took over the first — the projection inserts the
reply at request time — and the chat UI already derives the second from the
call itself (`web/src/components/chat-screen.tsx`, the "any tool_use that
still has no result … mark them stopped" sweep on history rebuild and on
abort). That left only the collision risk: a persisted pad is a second write
on a call a *late real result* can still answer, which is the incident.

Landed: `padOrphanToolResults` and its two defer call sites are gone, along
with the per-turn `turnToolCallIDs` bookkeeping and its test file. An
interrupted turn now ends with the call **open**:

* the session keeps the truth (the tool never returned);
* the next request is valid because `normalizeForPrompt` fills exactly one
  reply per open call, in place, idempotently;
* `provider.StoppedToolResult` remains the shared literal, but its only
  producer is the projection;
* the doctor scanner classifies an unanswered call as *expected* — reported,
  never gated on (`doctor.Finding.Expected`, `doctor.Unexpected`), because it
  is now the normal shape of an interrupted turn rather than history debt.

Guarding tests: `TestInterruptedTurnLeavesNoSyntheticReplyInHistory` (drives
the loop detector into breaking out mid-round on the streaming path, then
asserts no pad in history, a genuinely open call, and a doctor-clean
projection), `TestNormalizeForPromptShapes`, `TestExpectedCoversOpenCallsOnly`,
`TestDoctorSessionsTreatsOpenCallAsExpected`.

### P4 — Truncation, compaction and stable ids

* Any compaction/truncation result is re-normalized before send (P3 makes
  this automatic), so `safeCompactionCutoff`'s special case becomes an
  optimisation rather than correctness.
* Any future "drop oldest item" path must drop the counterpart with it
  (Codex `remove_first_item` → `remove_corresponding_for`).
* Synthetic ids are derived, not random no-increment: `synthetic:<call id>`
  hashed into a stable token so two normalizations of the same history are
  identical, and so the API never sees a synthetic id that collides with a
  real one.

### P5 — Turn budgets and interruption semantics ✅ landed

One number (`taskTimeoutSec`, default 300 s) used to delimit an IM turn *and*
hard-kill any tool execution in flight, which is what manufactured pads. The
split that landed:

* **Grace before the pad**: on budget expiry the turn stops being fed (its own
  ctx is cancelled, so no further model round starts) while an in-flight tool
  keeps running for `toolGraceDefault` (60 s) and lands its real result. The
  call is left open when the grace also expires — the projection answers it
  with one synthetic reply at request time (Q4), and nothing is written to
  history that a late result could collide with.
* **Per-source budget** via the system `taskqueue` namespace
  (`TaskQueueCfg`): `maxConcurrent` (global), `taskTimeoutSec` (every queued
  turn — IM, cron, goal, webhook), `cronTimeoutSec` (cron ticks only; 0 = same
  as `taskTimeoutSec`). Web turns never reach the queue: the dashboard handler
  carries its own 45-minute budget.
* **Hot reload**: saving that namespace (`POST /api/config` →
  `{"taskQueue":{…}}`, system scope) re-reads it into the running queue —
  `handleUpdateConfig` → `reloadSystemTaskQueue()` → `Gateway.ReloadTaskQueue()`
  → `Queue.SetMaxConcurrent` / `SetDefaultTimeout` + the cron budget. No pod
  roll needed. Semantics: a new default applies to the next task (in-flight
  tasks keep the budget they started with), and a resize cannot strand a slot
  because each task releases into the semaphore it acquired from.
* **One policy site**: `Gateway.taskTimeoutFor` decides which budget an inbound
  gets, and every routing path queues through `Gateway.submitTask` (including
  the deferred-turn drain, so a parked tick keeps its budget when admitted).
* **The budget only bounds *waiting*.** Work that legitimately outlives a turn
  does not need a bigger budget: `exec({"run_in_background": true})` detaches
  the job inside the sandbox and `bash_output` polls it from later turns — the
  r39–r42 incident was exactly a long job measured against a turn budget. See
  `docs/sandbox-background-exec.md`.
  Since 2026-09-14 the tool enforces the shape rather than trusting the model to
  remember it: a sandbox/host `exec` whose command waits in the foreground
  (`sleep` ≥ 30 s at a command position) is refused before it runs, with the
  `run_in_background` recipe as the only guidance. The bet the refusal removes is
  "my turn outlives my wait" — five times it did not, and each loss cost the
  output, not just the time.
  Two limits are accepted, not hidden: the predicate is lexical, so a wait hidden
  inside a script the command invokes still slips through; and a genuinely
  foreground wait has a door — `"allow_long_wait": true`, present in the tool
  schema but deliberately absent from the refusal text, so the default answer
  stays `run_in_background` and a bypass is visible in the log
  (`long foreground wait allowed by allow_long_wait override`).
* **A web turn outlives its client, and where `cancel()` is deferred decides
  that.** `handleChatStream` detaches the agent ctx from the request on purpose
  (`context.WithoutCancel(r.Context())`, 45 m ceiling) and returns as soon as the
  client's connection drops (`clientGone`). Since d471fa2 (2026-05-14) —
  which needed the SSE to stay open past `HandleMessage` returning on the
  slash-continuation path — `cancel()` was deferred on the *handler*, so that
  return ran it: "the browser hung up" silently became "the turn is over", with
  no budget involved.
  Production, 2026-09-16T15:26:29Z, pod `fastagent-gateway-568cc96dcb-6pzbl`: a
  web turn 26 minutes into a babysitting loop logged `turn ctx ended with a tool
  in flight … cause="context canceled"` — the only event was the client's
  connection ending, and the same agent's genuine budget expiry that hour logged
  `cause="context deadline exceeded"` instead. 60 s later
  `tool still running after its grace window; cancelling it`, and the in-flight
  `sleep 200` exec died as `e2b exec body read: context canceled (got 68 bytes)
  … first={"event":{"start":{"pid":1928}}}` while its sandbox and the job stayed
  healthy.
  Fix (2026-09-17): `cancel()` is deferred on the agent goroutine again — where
  `git show d471fa2^` had it — and the SSE's `turnPending` safety net watches the
  turn's *deadline* (`time.NewTimer` on `agentCtx.Deadline()`) instead of
  `agentCtx.Done()`, which now closes when `HandleMessage` returns. The
  continuation path keeps its open stream with no re-coupling of the turn to its
  client. Pinned by `internal/setup/client_disconnect_turn_test.go`
  (`TestClientDisconnectDoesNotKillTheTurnE2E`; it fails with "client disconnect
  cancelled the turn" if the defer moves back to the handler).
* **The other budget is rounds, not seconds.** `maxToolIterations` (default 20)
  caps how many model rounds one turn may run; hitting it ends the turn with a
  forced synthesis and the chat panel's "Iteration limit reached" badge
  (`capReachedNudge` / `iterationCapMetadata`). Running out of *seconds* and
  running out of *rounds* need different answers, so the round budget no longer
  ends a turn that is still working: a segment that produced at least one real
  tool result earns an extension — `maxToolIterationContinues` (default 1,
  `0` = never, chatbot mode 0 unless set explicitly) — with a nudge telling the
  model to build on what it already has and stop repeating calls. A segment
  whose rounds *all* failed is not extended: it keeps the old bounded ending,
  because another budget would burn on the same wall. The badge then reports
  the budget the turn actually had (segments × rounds), and sub-agents are
  excluded — their own cap plus the wall-clock budget already bound them.

* **A sub-agent's wall budget is clamped to the turn it runs in.**
  (Product-facing design record with the historical comparison:
  [tokenaissance-cloud › docs/fastagent/design/09-delegate-task-design.md](https://github.com/tokenaissance/tokenaissance-cloud/blob/develop/docs/fastagent/design/09-delegate-task-design.md).
  This section stays the implementation record — test names and line references
  live here, the intent and the before/after table live there.)
  `delegate_task` is registered serial, so N calls in one round cost N × the
  single-run wall time — against a web turn whose only clock is
  `agentTurnTimeout` (45 m). Two 25-minute requests (`wall_timeout_sec: 1500`)
  therefore cannot both finish, and nothing checked: the model asked, the tool
  passed the number straight through, and the second sub-agent never got to
  start. On 2026-09-14 that surfaced as a turn that ended with the tool row
  still reading *"Queued (waiting on prior sub-agent)…"* — the result had
  nowhere to go, and the row is what a reader sees for the whole wait.
  **Scope of "serial":** one *Agent object*, not one pod and not one agent ID —
  a pod holds an Agent object per user space (plus rebuilds), each with its own
  slot, so cross-tenant calls do not serialize while same-tenant cross-session
  ones do. The table and its evidence live in
  [09 §5.3](https://github.com/tokenaissance/tokenaissance-cloud/blob/develop/docs/fastagent/design/09-delegate-task-design.md#53-并发上限与串行关系按对象而不是pod--agent).
  `subagentWallBudget(ctx, explicit)` (subagent.go) is now the one owner of that
  number — resolve the caller's request / the configured default / the built-in,
  then hold it inside the turn. It reserves `subagentTurnMargin`, derived as
  `subagentSalvageTimeout + 30 s` rather than chosen, so the sub-agent's *own*
  budget expires first: that is what keeps it on the salvage path instead of
  being cut down mid-flight by the parent, which is the difference between
  "partial result with a reason" and "nothing" (`TestClampedSubagentStillSalvagesWhenItsOwnBudgetEnds`
  fails with *cancelled with its parent* when the reservation is dropped). The
  clamp bit is named in the tool result and in a warning log. When the turn has
  less than the margin left, delegating is refused with the remaining time and
  the instruction to re-issue in a fresh turn — starting a doomed sub-agent is
  worse than saying so, because the parent still has to answer within the same
  clock.
  The ceiling has to cross the grace boundary to get there: `toolGraceContext`
  deliberately drops the deadline (an in-flight tool may outlive its turn), so
  the turn's deadline travels as a **value** (`withTurnDeadline` /
  `turnRemaining`, both stamped onto every tool context in the loop). Reading
  `ctx.Deadline()` inside a tool answers "no ceiling" for every call in
  production — the first version of this clamp did exactly that and was dead
  code with green tests, until a turn-level fan-out test refused to lie about it.
  The wait itself is bounded now too: `RegisterSerialFrom` serialises through a
  one-slot channel instead of a `sync.Mutex`, so a queued call returns
  `ctx.Err()` the moment its turn ends rather than staying blocked until the
  running sibling finishes (and then entering the tool body with a dead ctx).
  A deadline-less caller (cron tick, CLI, tests) has nothing to clamp against
  and keeps its configured budget.

### P6 — Archive integrity and operations ✅ landed

* NUL bytes are stripped at the persistence boundary (`sanitizeNUL` on
  `AppendSessionMessage` and `AppendSessionEvent`), so a tool result that
  carries `\x00` — sandbox exec frames its stream with four of them — can no
  longer vanish from the archive while the JSON-escaped working set keeps it.
* The incident's manual analysis is a command now:

```bash
fastagent doctor sessions                      # whole deployment, exits 1 on findings
fastagent doctor sessions --agent <id> --json  # narrow + machine-readable
fastagent doctor sessions --session-key <key> --fix   # withdraw duplicates, backing the row up
```

It reads `sessions.messages` through `ListSessionSnapshots`, reports the three
pairing shapes (`duplicate_tool_reply`, `orphan_tool_reply`,
`unanswered_tool_call`), and with `--fix` removes duplicate replies — the same
repair applied by hand during the incident — after writing
`<backup-dir>/<sessionKey>.json`. Orphan and unanswered findings are reported
only: the prompt projection (`normalizeForPrompt`) handles those without
rewriting history.

## Alternatives considered

* **Route every entry point through `taskqueue.Queue` (per chat key).**
  Rejected as the primary mechanism: the queue keys on
  `channel:accountID:chatID`, and the session — the thing that owns history —
  can be shared across channels (`shared_identity`) or reached through
  several URL tokens (`recoverWebTriple`). Keying on the chat tuple would
  leave exactly the hole we are closing. It also forces one timeout for all
  sources (P5) and gives no place to express "queued" vs "rejected" (P2).
  The queue stays as the IM/cron transport; the gate becomes the authority
  everyone passes.
* **Auto-steer everything while busy (Codex's `StartOrSteer` for user input).**
  Rejected by decision D2 for the dashboard: a mid-turn user message changing
  the running turn's direction is a product choice, and the UI already offers
  it explicitly. Not rejected for IM (Q1).
* **Advisory lock in the store (Postgres) instead of in-process.**
  Deferred by decision (2026-09-14): it is the only way to make W hold across
  gateway replicas, but it adds a round-trip to every turn and couples the
  agent loop to the store for a property that has only ever been violated
  inside one process. Q6 records the reopen triggers and the fix shape
  ([Deferred: cross-replica session lease](#deferred-cross-replica-session-lease-q6));
  the sandbox-lease design is the precedent to copy when that trigger fires.
* **Make the provider layer (wire sanitizer) the only defence.**
  Rejected: it is per-provider (Anthropic needed its own sweep), it runs after
  compaction and truncation have already shaped the prompt, and it silently
  rewrites the request without telling anyone — the incident survived a
  release precisely because the OpenAI builder looked "defensive enough".
* **Rewrite history on every anomaly (self-healing store).**
  Rejected: destroys the audit trail and prompt cache stability, and hides
  the concurrency defect instead of removing it.

## Implementation plan

Each phase is independently shippable and test-first. P0 is already in the
working tree.

| Phase | Change | Files | Tests first |
|---|---|---|---|
| **P0** ✅ | Drop duplicate tool replies at wire build; scope pads to the turn's own ids; thread a real ctx into the compaction summarizer; repair the incident session's stored history | `internal/provider/openai.go`, `internal/provider/provider.go`, `internal/agent/loop.go`, `internal/agent/compaction.go`, `internal/agent/slash.go` | `openai_dangling_tool_test.go` (+2), `pad_orphan_tool_test.go` (new, +2), `compaction_test.go` (+1) |
| **P0.5** | Build and deploy P0 (`./build-image.sh dev` → verify → `prod`). Prod still runs `6345e2b`, i.e. the old pad path. | — | Appendix C checklist |
| **P1** ✅ | `Session.AcquireTurn/ReleaseTurn` (FIFO, ctx-aware, no leak); acquired in `HandleMessage` + `HandleMessageStream` before the plan-mode branch, released outermost so the leftover-steer writer stays inside the turn (the pad writer it originally also covered is gone — Q4); admission waits >1 s logged | `internal/session/manager.go`, `internal/agent/loop.go` | `internal/session/turn_gate_test.go` (serialize, FIFO, cancel-while-queued, cancel-at-handoff race), `internal/agent/turn_gate_test.go` (waits for in-flight turn; queued turns serialized, roles `user,assistant,user,assistant`), plus the four e2e-level cases in [Integration / e2e](#integration--e2e): long tool, cron-vs-web, real POST vs cron tick, goal continuation |
| **P1b** ✅ | Queued-state UX modelled on Codex's `PendingInputPreview`: `queued` event, queue block above the composer with `↳ text`, `(n ahead)`, and Edit/Cancel actions backed by a new withdraw endpoint (`/api/chat/cancel`, `agent.WithAdmissionSignal` marks the point of no return) | `internal/agent/loop.go`, `internal/agent/admission_signal.go` (new), `internal/setup/handlers.go`, `internal/setup/handlers_chat_cancel.go` (new), `internal/setup/server.go`, `web/src/components/chat-screen.tsx`, `web/src/lib/api.ts` | `TestRunTurnQueuesUserSourceAndEmitsQueuedEvent`, `TestWithAdmissionSignalClosesWhenTurnStarts`, `TestPendingTurnRegistryWithdrawContract`, `TestPendingTurnKeyIsolatesTabsAndSessions`, `TestQueuedChatTurnIsAnnouncedAndWithdrawableE2E`, `TestStartedChatTurnCannotBeWithdrawnE2E`; `tsc --noEmit` clean |
| **P2** ✅ | `TurnMode` + `ErrTurnNotAdmitted` + `RunTurn`; gateway parks refused automatic turns and retries them at the next idle point instead of blocking a queue worker | `internal/agent/admission.go` (new), `internal/gateway/deferred_turns.go` (new), `internal/gateway/gateway.go` | `admission_test.go` (refusal, queued event + position, source policy), `deferred_turns_test.go` (FIFO drain, busy skip, budget expiry) |
| **P3** ✅ | `normalizeForPrompt` applied to the prompt in both loops; `provider.Message.EffectiveToolCalls()` added so a call declared only inside `RawAssistant` is still recognised, and the OpenAI wire scanner reuses it instead of parsing raw a second time | `internal/agent/normalize.go` (new), `internal/agent/loop.go`, `internal/provider/provider.go`, `internal/provider/openai.go` | `internal/agent/normalize_test.go` (7 shapes incl. the incident's legacy duplicate, raw-assistant declaration, duplicate declaration; each case also asserts idempotence and input immutability) |
| **Q4** ✅ | The loop stops persisting synthetic "interrupted" replies: `padOrphanToolResults`, its defer sites and the per-turn `turnToolCallIDs` bookkeeping are deleted, so an interrupted turn leaves its call open and `normalizeForPrompt` answers it at request time. The doctor scanner keeps reporting an unanswered call but stops gating on it (`Finding.Expected`/`Unexpected`) | `internal/agent/loop.go`, `internal/provider/provider.go` (doc), `internal/provider/anthropic.go`, `internal/doctor/scan.go`, `cmd/fastclaw/cmd_doctor.go`, `web/src/components/chat-screen.tsx` (comments; the UI sweep was already the renderer) | `internal/agent/interrupted_turn_test.go` (new), `internal/doctor/scan_test.go` (+1), `cmd/fastclaw/cmd_doctor_test.go` (+1); `pad_orphan_tool_test.go` deleted with the code it pinned |
| **P4** ✅ | Truncation cannot split a pair *and* the claim is now tested: compacted history, after `normalizeForPrompt`, carries no pairing findings — verified with the doctor scanner as the oracle, for a cutoff landing inside a pair, for a retained tail that itself holds a duplicate, and for a retained tail that holds an open call (Q4's shape). `safeCompactionCutoff` is documented as an optimisation (it keeps the prompt byte-identical to last turn's) rather than the correctness guarantee | `internal/agent/compaction.go` (comment), `internal/agent/compaction_pairs_test.go` (new) | `TestCompactionOutputNormalisesToAPairingCleanHistory` (three shapes, through prune + compress) |
| **P5** ✅ | **Grace** (bounded, in-flight tools land their real result — on the main loop **and** inside a sub-agent), **per-source budgets** (`TaskQueueCfg.CronTimeoutSec`; one policy site `Gateway.taskTimeoutFor` behind `submitTask`), and **hot reload** of the whole `taskqueue` namespace (`ReloadTaskQueue` + the `taskQueueReloader` hook, no pod roll; resize-safe semaphore swap) | `internal/agent/tool_grace.go` (new), `internal/agent/loop.go`, `internal/agent/subagent.go`, `internal/taskqueue/queue.go`, `internal/config/config.go`, `internal/gateway/gateway.go`, `internal/gateway/routing.go`, `internal/gateway/taskqueue_reload.go` (new), `internal/setup/handlers.go`, `cmd/fastclaw/main.go` | `TestToolGraceContextSurvivesCancellationForGrace`, `TestToolGraceContextStopEndsImmediately`, `TestToolGraceContextDisabled`, `TestToolGraceContextLogsWhyItCancelled`, `TestTurnBudgetExpiryLetsInFlightToolRecordItsResult`, `TestSubagentBudgetExpiryLetsInFlightToolFinish`, `TestSubmitWithTimeoutOverridesQueueDefault`, `TestTaskTimeoutForSourcePolicy`, `TestSetDefaultTimeoutAppliesToTheNextTask`, `TestSetMaxConcurrentResizesWithoutStrandingInFlight`, `TestReloadTaskQueueAppliesSystemConfig`, `TestReloadTaskQueueDegradesQuietly`, `TestTaskQueue_HotReloadCloudPathE2E` |
| **P6** ✅ | `sanitizeNUL` at the persistence boundary (session_messages + session_events) so a NUL-bearing tool result can no longer vanish from the archive; `fastagent doctor sessions` reports duplicate/orphan/unanswered pairings, exits non-zero on the *actionable* ones (duplicates and orphans — an unanswered call is expected and only reported, see Q4), and `--fix` removes duplicate replies after backing the row up | `internal/store/database.go`, `internal/doctor/scan.go` (new), `internal/store` `ListSessionSnapshots`, `internal/session/store_adapter.go` (`ProviderMessages`), `cmd/fastclaw/cmd_doctor.go` (new) | `TestAppendSessionMessageStripsNUL`, `internal/doctor` shape table + RawAssistant declarations + `TestExpectedCoversOpenCallsOnly`, `TestListSessionSnapshotsOrderingAndFilter`, `TestDoctorSessionsFindsAndFixesDuplicateReplies` (CLI end to end: seed → scan fails → fix → backup → clean), `TestDoctorSessionsTreatsOpenCallAsExpected` |

## Test plan

### Unit

* **Session gate** ✅ — `TestAcquireTurnSerializesCallers`,
  `TestAcquireTurnHandsOffFIFO`, `TestAcquireTurnContextCancelDoesNotLeakSlot`,
  `TestAcquireTurnCancelAtHandoffDoesNotStrandSlot` (200-iteration race).
* **Agent admission** ✅ — `TestHandleMessageWaitsForInFlightTurn` (no
  provider call, no history write, no return while the slot is held),
  `TestHandleMessageSerializesQueuedTurns` (history roles are exactly
  `user,assistant,user,assistant`, contents in arrival order).
* **normalizeForPrompt** ✅ — `TestNormalizeForPromptShapes`: well-formed
  history unchanged, legacy synthetic reply + real result collapsed, the
  incident's 2-calls-4-replies shape, orphan reply dropped, unanswered call
  (Q4's shape: an interrupted turn leaves the call open) answered in place,
  late reply pulled next to its call, empty input; every case also asserts
  idempotence and that the input slice was not mutated.
  `TestNormalizeForPromptReadsRawAssistantCalls` (call declared only in
  `RawAssistant`), `TestNormalizeForPromptStripsDuplicateCallDeclaration`.
* **Interrupted turn (Q4)** ✅ — `TestInterruptedTurnLeavesNoSyntheticReplyInHistory`:
  the loop detector breaks out mid-round, history keeps the open call and no
  synthetic reply, and `doctor.Scan(normalizeForPrompt(history))` is empty —
  the same oracle the provider contract is written against.
* **Admission** ✅ — `TestRunTurnDefersAutomaticSourceWhenSessionIsBusy`
  (refused, nothing written), `TestRunTurnQueuesUserSourceAndEmitsQueuedEvent`
  (queues, emits `queued` with position 1, runs after release),
  `TestTurnModeForSourcePolicy`.
* **Gateway parking** ✅ — `TestDeferredTurnsDrainPolicy` (busy sessions are
  skipped, per-chat FIFO head only, one submission per idle observation),
  `TestDeferredTurnsDropsMessagesPastBudget`.
* **Sub-agent wall budget** ✅ — `TestSubagentBudgetIsClampedToTheTurnDeadline`
  (a 25 m request in a 10 m turn runs with ~8 m), 
  `TestSubagentBudgetUnderTheTurnCeilingIsUntouched` (5 m in a 45 m turn stays
  5 m), `TestSubagentBudgetWithoutATurnDeadlineIsUntouched` (cron/CLI keeps the
  configured budget), `TestSubagentRefusesWhenTheTurnHasNoRoomLeft` (no model
  round is ever spent),
  `TestClampedSubagentStillSalvagesWhenItsOwnBudgetEnds` (the clamped expiry is
  the sub-agent's own, so the salvage round runs — dropping the margin makes it
  *cancelled with its parent*, which salvages nothing), and the serial wait
  itself:
  `TestRegisterSerialQueuedCallIsReleasedByItsContext`,
  `TestRegisterSerialAlreadyCancelledCallNeverEnters`.
* **Fan-out turn** ✅ — `TestTurnFanOutOfDelegateTasksIsScheduledAndAlwaysReturns`:
  one turn emitting 1, 2 or 3 `delegate_task` calls either runs them all (one at
  a time, in emission order — which is what the dashboard's "the first
  unresolved one is live" reads) or refuses them all with the remaining time
  named; either way every call has a result and the turn returns. A call left
  pending is the failure this exists to make impossible, and it is the one the
  first version of the clamp produced under a short turn. With one call's worth
  of clock left, exactly one runs (and salvages) while the rest report.
* **Slot across turns** ✅ —
  `TestDelegateTaskSlotSpansTurnsAndReleasesOnTheWaiterClock` (`internal/agent`):
  two sessions of one agent both delegate. The second waits for the first's
  sub-agent instead of starting a sibling (peak concurrency 1) and then runs when
  the slot frees; when the *waiting* turn's clock ends first, the call comes back
  saying it never started — the wait belongs to the turn, not to the tool's grace
  window, which `TestRegisterSerialWaitEndsWithTheTurnDeadline` pins directly in
  `internal/agent/tools`.
* **Grace boundary** ✅ — `TestToolContextCarriesTheTurnsDeadlineAsAValue`: the
  tool context outlives its turn (no deadline of its own) yet still reports the
  turn's remaining time once stamped, and a caller with no turn clock reports
  nothing rather than zero.
* **Wire builder** ✅ — the P0 tests stay as defence-in-depth
  (`TestToAPIMessagesDropsDuplicateToolReplies`,
  `TestToAPIMessagesDropsDanglingToolReplies`,
  `TestToAPIMessagesKeepsAnsweredPairAndDropsStrayReply`).
* **Still to write** — a `toolu_*`/`call_*` mixed-provider snapshot fixture
  (the incident's real session mixed Anthropic and DeepSeek ids) and a
  multi-turn transcript with compaction in the middle.
* **Doctor / archive** ✅ — `internal/doctor` shape table +
  `TestScanReadsDeclarationsFromRawAssistant`,
  `TestAppendSessionMessageStripsNUL`,
  `TestListSessionSnapshotsOrderingAndFilter`,
  `TestDoctorSessionsFindsAndFixesDuplicateReplies`.
* **Tool grace** ✅ — `TestToolGraceContextSurvivesCancellationForGrace`,
  `TestToolGraceContextStopEndsImmediately`, `TestToolGraceContextDisabled`
  and `TestTurnBudgetExpiryLetsInFlightToolRecordItsResult`.
* **Budgets** ✅ — `TestSubmitWithTimeoutOverridesQueueDefault` (a cron budget
  outlives the queue default; a default task is still cut on time),
  `TestTaskTimeoutForSourcePolicy` (cron with/without the knob, everything
  else falls back), `TestSetDefaultTimeoutAppliesToTheNextTask`,
  `TestSetMaxConcurrentResizesWithoutStrandingInFlight` (reload safe under a
  running task), `TestReloadTaskQueueAppliesSystemConfig` /
  `TestReloadTaskQueueDegradesQuietly` (gateway), and
  `TestTaskQueue_HotReloadCloudPathE2E` (system-scope save fires the hook once;
  a user-scope save and a resolver without the capability both stay silent).
* **Compaction/pairing** ✅ — `TestCompactionOutputNormalisesToAPairingCleanHistory`.

### Integration / e2e

* **Delegate fan-out over the real endpoint** ✅ —
  `TestDelegateTaskFanOutTurnE2E` (`internal/setup`): the real chat handler, agent
  runtime, tool registry and sub-agent loop, with only the model faked. One turn
  asks for 1, 2 and 3 `delegate_task` calls in separate cases; each is answered in
  the session, in emission order, the stream carries one heartbeat per sub-agent
  *and* one `phase:"done"` per call (so the dashboard's indicator clears), and the
  turn delivers its answer. `seq:-1` on those events is this harness's missing
  store, not a production shape.
* **Web POST vs cron tick** ✅ — `TestConcurrentWebAndCronTurnSerialize`
  (`internal/setup/concurrent_turn_e2e_test.go`): a cron tick parked inside a
  long tool while a real dashboard POST arrives for the same session. Asserts
  the POST is announced as `queued` on its own SSE stream, writes nothing while
  the tick holds the slot, and that the history the two turns leave behind is
  `user,assistant,tool,assistant,user,assistant` with one reply per call and a
  clean `doctor.Scan`.
* **Queued chat POST** ✅ — `TestQueuedChatTurnIsAnnouncedAndWithdrawableE2E`
  (real handler + real agent + fake provider: the POST is announced with a
  `queued` event, `/api/chat/cancel` returns 200, the turn never reaches the
  model and the session stays empty) and `TestStartedChatTurnCannotBeWithdrawnE2E`
  (once started, cancel returns 409 and the turn completes).
* **Queued turn behind a long tool** ✅ — `TestQueuedTurnRunsAfterLongTool`
  (`internal/agent/turn_queue_test.go`): turn A holds the slot through a slow
  tool; turn B must not append anything — not even its own user message —
  until A released, and the two turns land as whole turns in arrival order.
* **Goal continuation** ✅ — `TestGoalContinuationDoesNotDeadlock`
  (`internal/agent/goal_continuation_gate_test.go`): the PostTurn hook fires
  while the turn still holds the slot, so it publishes the continuation onto
  the bus; the test asserts the turn returns, the continuation carries
  `Source=goal_context` and the goal's chat id, and that the continuation can
  then take the slot (which is what an inline call would have deadlocked on).
* **The incident's timing, agent level** ✅ — `TestCronTickDoesNotInterleaveWithWebTurn`
  (`internal/agent/turn_queue_test.go`): cron source in a long tool + a user
  message during it; asserts the same serialized shape and that no
  `tool_call_id` is answered twice — the second answer is what 400s a session.
* Replay harness `(not in the tree)` `TestZZReplay`: run as
  `FA_DIAG_HISTORY=<jsonl> go test ./internal/provider/ -run TestZZReplay`
  (offline, never part of CI) ad hoc against the incident's snapshots — they
  replay clean under both the rules deployed that day and the P3 rules. The
  harness itself was not kept; re-create it from this line if a replay is
  needed again.

### Verification against production

* `fastagent doctor sessions --session hJKMWwtOp3mJOtqN8Uz2mW` returns clean
  after the P0 repair (it did: 517 → 512 messages, 0 duplicate ids).
* After deploy: grep the gateway logs for
  `must be a response to a preceding message` over a 24 h window and for
  `padding orphan tool_use` paired with a later real result for the same id
  (pre-Q4 images only — that log line is gone now, and its absence is the
  point: nothing writes a synthetic reply into a session any more).
* `sessions.messages` scan for duplicate `toolCallId` across all sessions.
  Since Q4, `doctor sessions` exits 0 when the only findings are unanswered
  calls, so this scan is now a clean gate again.

## Rollout, verification, rollback

1. **P0.5** — ship the landed fixes to `development`, replay the incident
   session through a live conversation, then `production`.
2. **P1** — ship behind a config flag (`turn_gate_enabled`, default on in
   dev) so it can be disabled without a rollback; watch
   `turn admission wait` logs for p99 and for waits that exceed a turn
   budget (P5 will make those a first-class outcome).
3. **P2–P4** — no flag needed; each is covered by unit + integration tests
   and by the doctor checker before/after.
4. **Rollback** — the gate holds no persisted state, so rolling the image
   back is complete; the data repair is independent and reversible from the
   backup taken during the incident (`/tmp/fa-diag/backup_session_*.json`).

## Open questions for review

| # | Question | Current default in this doc |
|---|---|---|
| **Q1** | IM inbound while a turn is running: keep auto-steer, or queue like the dashboard? | keep auto-steer (D3) |
| **Q2** | Does the dashboard need a visible "排队中" state, or is the typing indicator enough? | yes, add one (P1b) |
| **Q3** | Cron/goal on a busy session: block the queue worker (P1) or `NotAdmitted` + re-queue (P2)? | P1 blocks (bounded by the queue timeout), P2 re-queues |
| **Q4** | Keep synthetic "interrupted" replies persisted, or move them to prompt-only (Codex parity) and render them in the UI from the call? | **prompt-only, decided 2026-09-14** (Codex parity): the projection writes the reply, stored history keeps the call open, the UI renders `(stopped)` from the call, and the doctor scanner treats the open call as expected. The persisted pad's only remaining effect was the incident's collision risk |
| **Q5** | Per-source turn budgets and the graceful-interrupt semantics (P5) | web 15 min, IM 300 s, grace 60 s |
| **Q6** | Do we need cross-replica session locking (store lease) now, or is in-process enough? | **not now, decided 2026-09-14**: the in-process gate is enough until one of the measurable triggers appears — a post-Q4 `duplicate_tool_reply`, a cross-turn interleave in stored history, or two pods waiting on one session. Triggers, rationale and the fix's shape: [Deferred: cross-replica session lease](#deferred-cross-replica-session-lease-q6) |
| **Q7** | Should the checker ship as a CLI subcommand or a test-only harness? | CLI subcommand (`doctor sessions`), P6 |

## Deferred: cross-replica session lease (Q6)

**Decision (2026-09-14): not now.** Clause W holds per *process*. The gap is
real but unobserved, and closing it costs availability — so the thing that
reopens it is evidence, not a date. This section is the record of that
decision and of what to build when the evidence shows up.

### Why not now

* The incident's symptom — a session that 400s forever — is already gone
  without the lease. A cross-pod interleave today costs a duplicated or
  misordered message inside one turn; the wire builder drops a duplicate
  reply, the prompt projection repairs the request (P3), the loop no longer
  persists a synthetic reply that a late result can collide with (Q4), and
  the doctor reports whatever is left. Nothing writes a permanent hole any
  more.
* Every turn would pay a store round trip for a property no measurement has
  asked for. The only admission contention in production so far is
  same-process: the dashboard POST queued behind the cron tick that started
  the incident, which is exactly what the in-process gate fixed.
* A lease adds a new way to be unavailable: the agent loop would need the
  store to *start* a turn (the sandbox pool accepts that coupling for
  long-lived executors, but a turn is a much hotter path), plus TTL renewal,
  a fencing token so a stale holder's appends cannot land, and a policy for
  "lease store unreachable". Availability is worth more than an unobserved
  concurrency property.

### What reopens it (measurable triggers)

In rough order of how cheap they are to see:

1. **A post-Q4 duplicate.** `doctor sessions` reports a
   `duplicate_tool_reply` on a session whose whole history was written after
   Q4 shipped. No persisted pad exists to explain that shape any more, so
   some other writer produced it.
2. **Cross-turn interleaving.** Stored history shows turn B's user message or
   reply between turn A's call and its reply (the shape
   `TestHandleMessageSerializesQueuedTurns` rejects). One process cannot do
   that.
3. **Two pods on one session.** Two replicas log
   `turn admission: waited for the in-flight turn` for the same session
   within one turn budget. Prep needed before this trigger is usable: that
   log line carries `chat_id` but not the session key or the pod identity —
   add both, or the trigger is invisible.

### Where the overlap would come from

The gateway runs two replicas (`deploy/helm/fastagent/values.yaml`,
`deploy/k8s/fastagent.yaml`). The ingress does pin a *browser* to a pod —
cookie affinity (`fastagent-affinity` in
`deploy/helm/fastagent/templates/ingress.yaml`) with a `ClientIP` Service
fallback — but the server-originated sources (cron tick, goal continuation,
webhook) fire on whichever replica owns the queue. There is no leader
election and no session→pod routing for them, so affinity covers the web half
of the traffic while the combination that actually produced the incident
(dashboard turn + cron tick) is only serialized when both land on one pod.

### Trigger fired: 2026-09-18 (two replicas, one session)

**What happened.** One user message and two follow-up `continue` messages
reached the same session over 34 minutes. Two of the three turns ran **at the
same time on two replicas**, and the concurrent turn — reading a history whose
tool calls were still in flight — concluded those calls had been interrupted
and re-issued the same two research sub-tasks. Nothing crashed; the damage was
duplicated work, duplicated writes and a UI that read "Interrupted".

| time (UTC) | turn | landed on | admission |
|---|---|---|---|
| 16:18:40 | `深度调研一下 quantconnect` (the real ask) | pod A `…-6mswd` | ran (long turn: 16:18:40 → `done` 16:52:35) |
| 16:31:21 | follow-up `continue` | pod A | **correct**: emitted `queued{position:1}`, waited 1 274 666 ms, started 16:52:35 → `done` 17:01:58 |
| 16:34:22 | follow-up `continue` | pod B `…-wxtbn` | **wrong**: pod B's gate knew nothing of pod A's turn, so it ran immediately → `done` 16:45:33 |

**Which triggers fired.** #3 in its sharpest form — not "both replicas logged
`turn admission: waited`" but "both replicas *ran* a turn for one session at
once". #2's shape (cross-turn interleaving) followed from it: pod B appended an
assistant message and duplicate tool calls inside pod A's batch window.

**Not the first time — and not this deploy's doing.** The precondition (a user
message arriving while a tool call was open) occurs on exactly two days in the
whole `session_events` history: 2026-09-04 and 2026-09-18. The 09-04 case is the
same defect in the form P1 removed, with no cross-replica coincidence needed;
its log and the day-by-day census are in
[Appendix E](#appendix-e--the-same-shape-on-2026-09-04-before-the-gate).

The week's changes moved the *likelihood* and the *visibility*, not the rule:
the deployed range `a24c0a8..8984c99` contains no change touching `AcquireTurn`,
`SteerWeb` or the `queued` path, and `gateway.replicas` has been 2 since at least
helm revision 72 (2026-09-14). Longer turns came from the sub-agent wall budgets
and salvage (09-13) plus the turn budgets and tool grace (09-14/15) — the open
window in this incident was 605 s, five times the 120 s window the 09-04 case had
— and the symptoms became visible in the cloud chat rewrite (`f7d106e3` 09-14
streaming parity + sub-agent progress + resend, `9004bccf` 09-15 the queued
block, `28ef00a5` 09-16 "the tool group follows the row — interrupted, not
running forever").

**The amplifier (worth fixing even with the lease).** Pod A's batch
(`apply_patch` + two `delegate_task`) writes its tool results only when the
whole batch returns — 10 minutes later, seq 358–360 — and `delegate_task` is
sequential, so those calls stay open for that entire window. Pod B read the
open calls and `normalizeForPrompt` answered them with
`provider.StoppedToolResult` = *"(stopped — execution was interrupted before
the tool returned)"*. The model acted on that sentence: *"补查的两条线刚才被打断了，重新发起"*.
The sentence was false — the calls were running, not interrupted — so a σ that
should have said "still running" caused exactly the duplication clause W exists
to prevent (O1 of `docs/文件系统形式化证明/08-state-observability-principle.md`).

**Measured cost.** Two research sub-tasks ran twice (11 467 / 5 330 chars from
pod A, 13 339 / 9 057 from pod B), `apply_patch(todo.md)` ran twice, and one
turn spent 21 minutes queued. The client rendered pod A's three calls as
`Interrupted — this call never returned a result` for those 10 minutes even
though the results were written (10 minutes late). A full join over
`session_messages` — every call id inside an assistant row against every
`role='tool'` row — comes back **57 calls, 57 replies, 0 unanswered, 0 orphan
replies**: the stored history is intact, and "Interrupted" was a rendering
verdict, not a fact.

**What the verification pass added (2026-09-19 re-read of the whole session).**

* **FIFO is broken across replicas too, not just mutual exclusion.** Of the two
  follow-ups, the earlier one (16:31:21) was served *after* the later one
  (16:34:22): pod A queued it behind the long turn and started it at 16:52:36,
  while pod B ran the later one immediately. Same-pod FIFO held (pod A's own
  queue was ordered); the pair across pods did not.
* **The two turns then fought over the sandbox lease.** Pod B adopted pod A's
  instance at 16:34:49 (`e2b sandbox adopted from shared lease`), hit
  `adopted after lease race` at 16:36:33, and then logged *three* consecutive
  `e2b rebuilt sandbox superseded by another pod; adopting current lease` /
  `adopted (local cache stale)` at 16:44:06, 16:44:48 and 16:45:06. The scope
  was hydrated **12 times in ~40 minutes** (7 on pod A, 5 on pod B; the normal
  number is one or two), and the session carries the resulting
  `[workspace] the sandbox was REPLACED — the previous instance had …` signals
  at 16:52:12, 17:01:03 and 17:01:41. A cross-replica turn costs more than a
  duplicated prompt: it thrashes the executor lease both turns depend on.
* **Both turns wrote the same deliverable, and one version was lost.** Pod B
  wrote `quantconnect-deep-research.md` at 16:44:07 (`Written 15348 bytes`,
  its iteration 6); pod A wrote the same path at 16:50:16
  (`Written 11492 bytes`, its iteration 8) and then patched it repeatedly. A
  store `Put` is last-writer-wins, so the 15 KB version was replaced — the
  workspace has no admission policy at all, because until now nothing could
  produce two writers for one session.

**Not decided here.** The 2026-09-14 "not now" rested on "the gap is real but
unobserved". It has been observed, so the trade-off — one store round trip per
turn against duplicated multi-minute sub-agent work — now has a number
attached. The fix shape below is unchanged (the same lease the sandbox pool
already uses) and it now has two consumers: admission, and the truthful pad.

### Shape of the fix when it is triggered

Reuse the sandbox lease design rather than inventing one
(`docs/sandbox-pool-leases.md`, upstream PR #124; the store slice is measured
in [upstream-pr-split.md](upstream-pr-split.md)):

* scope key `(user_id, agent_id, session_key)` instead of a sandbox pool id;
* owner = pod identity, TTL ≥ turn budget + tool grace, renewed while the turn
  runs; CAS adoption and the `state`/`paused_at` columns already exist in that
  design to copy from;
* the in-process FIFO gate stays the fast path — only a caller that finds a
  live lease held by *another* pod pays the round trip, and the `queued` event
  (P1b) is already the user-facing half of "you are waiting for the other
  writer";
* a fencing token per acquisition, checked where the session appends, so a
  holder that lost its lease (crash, partition, TTL expiry) cannot keep
  writing when its goroutine resumes.

If the trigger fires before that work is ready, the cheaper intermediates are
all partial:

* **Stickiness** — route a session's turns to one pod by hashing the session
  key. It closes the web half only, unless the server-originated sources hash
  the same way.
* **Write fence without queueing** — a monotonic generation per session,
  compared on append so a stale writer is rejected rather than interleaved.
  This still needs the store to arbitrate a compare-and-append, i.e. most of
  the lease's cost without its ordering semantics.
* **Park automatic sources by policy** — P2 already parks them per process; a
  cross-replica version needs the same shared state, so it is not cheaper.

## Appendix A — incident evidence

Session: agent `agt_cda27bbfbf4a84e2dfa6`, `session_key` /
`chat_id` `hJKMWwtOp3mJOtqN8Uz2mW`, owner
`u_396a1f8812880c67e6ff`, model `deepseek/deepseek-flash` at failure time
(`claude-sonnet-4-6` earlier and later); cron job `kronos-crypto-resume`;
gateway image `20260913111345-fastagent-6345e2b`.

```text
11:30:00.011 firing store-backed cron job  id=5498f0ea-… name=kronos-crypto-resume
11:30:00.292 task submitted  task-1789299000292-1 chat_key=web::hJKMWwtOp3mJOtqN8Uz2mW
11:34:32.776 turn: refreshing skills   chat_id=hJKMWwtOp3mJOtqN8Uz2mW   ← second turn (dashboard)
11:34:41.381 hook: before tool call    tool=list_cron_jobs / tool=exec
11:35:00.294 tool execution error      tool=exec  (e2b snapshot failure, tool still in flight)
11:35:00.439 WARN padding orphan tool_use with stopped result toolCallID=call_00_38dF…S13513
11:35:00.567 WARN padding orphan tool_use with stopped result toolCallID=call_01_3nA…hH6960
11:35:00.897 task completed            duration_ms=300604      ← 300 s timeout killed turn A
11:35:32.146 openai request            api.deepseek.com/v1/chat/completions
11:35:32.957 API error 400: Messages with role 'tool' must be a response to a
             preceding message with 'tool_calls'
…same 400 on every cron tick through 13:45:02 (40 error lines on the pod that
served the ticks; the second replica logged the 19:50 repeat below)…
19:50:06.124 same 400 after the agent was switched back to deepseek-flash
```

History snapshot analysis (13 `history_*.jsonl` snapshots written by
compaction, replayed through the repo's own wire builder):

| Snapshot | Duplicate replies | Provider | Outcome |
|---|---|---|---|
| 11:30:01, 11:34:33 | 0 | deepseek | ok |
| 11:35:32 … 13:45:02 (11 snapshots) | 1 run with 4 replies for 2 calls | deepseek | 400 every time |
| same snapshots | 1 | anthropic | ok |
| 19:50:05 | 1 | deepseek | 400 |

Stored history after the incident: 5 `tool_call_id`s answered twice
(`…OfGixT0207`, `…GCBlDA3546`, `…FgPCS13513`, `…v22vhH6960`, `…kP8wt7ABwG`);
in every one of the five the first reply is the
`(stopped — execution was interrupted before the tool returned)` pad and the
second is the real result.

## Appendix B — the P0 repair (already applied)

1. `internal/provider/openai.go` — a `tool_call_id` may be answered at most
   once on the wire; later replies are dropped (first reply wins, i.e. the one
   adjacent to the declaring assistant).
2. `internal/agent/loop.go` — `padOrphanToolResults(sess, turnToolCallIDs)`
   pads only ids the current turn declared, resolved against the whole
   session; the pad literal is shared as `provider.StoppedToolResult`.
   **Superseded by Q4**: the function is gone; the literal now belongs to the
   projection. This entry is kept because it describes the binary that was
   deployed on 2026-09-14 and the repair that ran against production data.
3. `internal/agent/compaction.go` — summariser receives the live `ctx`
   (`API error`-free compaction; previously `net/http: nil Context`).
4. Production data — the incident session's `sessions.messages` went from 517
   to 512 entries (5 duplicate pads removed, real results kept) and
   `session_messages` from 573 to 572 rows; the row was backed up first to
   `/tmp/fa-diag/backup_session_20260913T202427Z.json`. Verification: the
   repaired history replays through the wire builder with 0 rejected replies
   and 0 runs with extra replies under **both** the deployed and the new
   rules — i.e. the repair alone unblocked the session on the old binary.

## Appendix C — deployment checklist for P0.5

```bash
cd /Users/reina/Project/tokenaissance/fastagent
go test ./internal/provider/ ./internal/agent/ ./internal/session/ -count=1
./build-image.sh dev                       # tag: <ts>-fastagent-<sha>, namespace development
# smoke: run one turn in the incident session, check logs for the 400 string
./build-image.sh prod                      # requires typing 'production'
```

Post-deploy verification:

```bash
kubectl logs -n production -l app=fastagent --since=24h | grep -c "must be a response to a preceding"
# expect 0

# History debt behind the request-time repairs (duplicates are what --fix removes):
FASTAGENT_STORAGE_DSN=... fastagent doctor sessions --json
```

### Build-time gotcha (2026-09-14)

`docker build` needs the **amd64** base images (`node:22-alpine`,
`golang:1.25-alpine`, `alpine:3.21`) because the helm images are built with
`--platform linux/amd64`, while a developer Mac may only hold arm64 copies.
When `registry-1.docker.io` is unreachable (it was, twice: `Bad Gateway`, then
`context deadline exceeded`), the build dies in "load metadata" before any
layer runs. Working around it without touching the Docker daemon:

```bash
for img in node:22-alpine golang:1.25-alpine alpine:3.21; do
  docker pull --platform linux/amd64 "docker.m.daocloud.io/library/$img"
  docker tag "docker.m.daocloud.io/library/$img" "$img"     # digest-identical upstream layers
done
```

That replaces the local arm64 tags — re-pull them (`docker pull <img>`) if an
arm64 build is needed later. The in-container `pnpm install` uses npmjs
(reachable) and `go mod download` falls back to `direct` → github.com
(reachable), so only the base images need the mirror.

## Appendix D — cross-replica repro, 2026-09-18

Everything the finding rests on is in `session_events` / `session_messages`
plus the two gateway pods' logs. Session: agent
`agt_e5867879c33d9e98662b`, session `qCxrNG3tgTl4C10xvaOYBL` (dev).
Read-only recipe; run the SQL from a throwaway pod that borrows the DSN
(`envFrom: secretRef: fastagent-secrets`, then strip the pgx-only parameter:
`sed -E 's/[?&]default_query_exec_mode=[^&]*//'`).

```sql
-- 1. the user messages: three, not one — the extra two are the trigger
SELECT to_char(created_at,'HH24:MI:SS'), role, content
FROM session_messages
WHERE agent_id='agt_e5867879c33d9e98662b' AND session_key='qCxrNG3tgTl4C10xvaOYBL'
  AND role='user' ORDER BY created_at;
--   16:18:40 | user | 深度调研一下 quantconnect
--   16:34:22 | user | continue          <- ran on pod B, concurrently
--   16:52:36 | user | continue          <- queued on pod A for 21 minutes

-- 2. the queue, and the proof that only ONE of the two was queued
SELECT seq, to_char(created_at,'HH24:MI:SS'), type, data FROM session_events
WHERE agent_id='agt_e5867879c33d9e98662b' AND session_key='qCxrNG3tgTl4C10xvaOYBL'
  AND type IN ('queued','done') ORDER BY seq;
--   186 | 16:31:21 | queued | {"position":1}
--   438 | 16:45:33 | done            <- pod B's turn
--   517 | 16:52:35 | done            <- pod A's turn
--   595 | 17:01:58 | done            <- the queued turn

-- 3. every call has a reply (so "Interrupted" was a rendering verdict, not a fact)
--    LEFT JOIN tool_call -> tool_result on data::json->>'id'; expect 0 NO-RESULT.
--    The three calls of the 16:32:27 batch land as seq 200-202, their results as
--    seq 358-360 at 16:42:32 — 10 minutes later, all three at once.

-- 4. what the concurrent turn's model was told, and what it did
SELECT seq, to_char(created_at,'HH24:MI:SS'), left(content,60) FROM session_messages
WHERE agent_id='agt_e5867879c33d9e98662b' AND session_key='qCxrNG3tgTl4C10xvaOYBL'
  AND role='assistant' ORDER BY created_at LIMIT 5 OFFSET 1;
--   ~16:34:29 | 补查的两条线刚才被打断了，重新发起（范围收紧一点）。

-- 5. every call has exactly one reply (the "Interrupted" claim, settled)
WITH calls AS (
  SELECT (e->>'id') AS id
  FROM session_messages m, jsonb_array_elements(m.tool_calls::jsonb) e
  WHERE m.agent_id='agt_e5867879c33d9e98662b' AND m.session_key='qCxrNG3tgTl4C10xvaOYBL'
    AND m.role='assistant' AND coalesce(m.tool_calls,'') NOT IN ('','null','[]')
), reps AS (
  SELECT tool_call_id AS id FROM session_messages
  WHERE agent_id='agt_e5867879c33d9e98662b' AND session_key='qCxrNG3tgTl4C10xvaOYBL'
    AND role='tool' AND coalesce(tool_call_id,'')<>''
)
SELECT (SELECT count(*) FROM calls), (SELECT count(*) FROM reps),
       (SELECT count(*) FROM calls c LEFT JOIN reps r ON r.id=c.id WHERE r.id IS NULL),
       (SELECT count(*) FROM reps r LEFT JOIN calls c ON c.id=r.id WHERE c.id IS NULL);
--   57 | 57 | 0 | 0

-- 6. the two turns wrote the same deliverable
SELECT to_char(created_at,'HH24:MI:SS'), left(content,60) FROM session_messages
WHERE agent_id='agt_e5867879c33d9e98662b' AND session_key='qCxrNG3tgTl4C10xvaOYBL'
  AND role='tool' AND content LIKE '%quantconnect-deep-research.md%' ORDER BY created_at;
--   16:44:07 | Written 15348 bytes to quantconnect-deep-research.md   <- pod B
--   16:50:16 | Written 11492 bytes to quantconnect-deep-research.md   <- pod A
```

```bash
# the two replicas, one session (the trigger itself)
kubectl -n development logs <pod-A> | grep -E 'turn: refreshing skills|turn admission'
#   pod A: 16:18:40 refreshing skills      + 16:52:35 "waited for the in-flight turn … 1274666 ms"
#   pod B: 16:34:22 refreshing skills      <- same chat_id, no wait, no queue

# two independent loops, same chat (iteration counters restart per turn)
for p in <pod-A> <pod-B>; do
  kubectl -n development logs "$p" | grep 'agent loop iteration' | grep qCxrNG3tgTl4C10xvaOYBL
done
#   pod A: iteration 1..18  16:18:40 → 16:52:28   (then 1 again at 16:52:36: the queued turn)
#   pod B: iteration 1..13  16:34:22 → 16:45:26   (then 1 again at 17:12:52: the next question)

# the lease thrash the pair caused (12 hydrates for one scope in ~40 min)
kubectl -n development logs <pod> | grep 'scopeKey=agt_e5867879c33d9e98662b:s:qCxrNG3tgTl4C10xvaOYBL' \
  | grep -E 'adopted|superseded|hydrated|rebuilt'
#   pod B: 16:34:49 adopted from shared lease (pod A's instance)
#           16:36:33 adopted after lease race · 16:44:06/16:44:48/16:45:06
#            "rebuilt sandbox superseded by another pod; adopting current lease"
```

The reproduction needs no fault injection: send a follow-up while a long turn
runs, and let the two requests land on different replicas (the ingress's
ClientIP affinity does not cover server-originated traffic, and a browser whose
second request goes through the cloud app's server-side fetch is not pinned
either).

## Appendix E — the same shape on 2026-09-04, before the gate

Session: agent `agt_e5867879c33d9e98662b` (the same agent as the 2026-09-18
incident), `session_key` `oMdMoz8imLGDtFrH21fMuA`, owner
`u_4ab6b430b5ee1cc12a20`, model `deepseek-v4-flash`. The pod logs and the
gateway image that served it are gone (replicas are recreated on every
rollout); the event log survives and is enough to identify the shape. **No
`queued` event exists in this session** — the gate that emits it (P1) landed ten
days later, on 2026-09-14.

```text
17:36:50 content   "仍在跑（~3 分钟）。再等 2 分钟：" + tool_call exec call_00_iieZuVk…
                                                    ↑ open until 17:38:50 (120 s)
17:36:55 user      "continue"                       ← arrives INSIDE the open call
17:37:06 content   "继续。等 2 分钟后查变体回测状态：" + tool_call exec call_00_AOGpXgCI…
                                                    ← a SECOND writer, while the first
                                                      writer's call is still open
17:38:50 tool_result exec call_00_iieZuVk…          (no output — response stream truncated…)
17:38:53 first writer continues (content + mcp call)
17:39:07 tool_result exec call_00_AOGpXgCI…         (no output — response stream truncated…)
17:39:10 second writer continues
17:39:25 done    ← a turn ends
17:41:02 done
17:41:58 done    ← three turns ended within 2 m 33 s
```

The loop is strictly sequential — it never issues a new assistant message while
its own tool call is unanswered — so line 17:37:06 cannot belong to the writer
that opened `call_00_iieZuVk…` at 17:36:50. It is a second writer, and its
wording ("继续") matches the user message that arrived 11 seconds earlier.

Census of the whole session: 750 tool calls, 749 replies (one call left
unanswered), 340 `done`, 399 `content`, 67 `error`, 1 `steer`, and **0
duplicated replies**. So unlike Appendix A, the interleaving did not poison the
history: each writer answered its own calls, and no pad collided with a late
result (`normalizeForPrompt` and Q4 came later). The damage was the duplication
itself — two writers deciding what the same session does next, one of them blind
to the other's open call.

**Why it belongs next to the 09-18 record.** Across the whole `session_events`
history these are the only two days on which a user message arrived while a tool
call was open:

| day | occurrences | sessions | admission in force | form that resulted |
|---|---|---|---|---|
| 2026-09-04 | 1 | `oMdMoz8imLGDtFrH21fMuA` | none (P1 landed 09-14) | two writers, interleaved on whatever pod(s) served them |
| 2026-09-18 | 3 | `qCxrNG3tgTl4C10xvaOYBL` | per-process (P1) | the same-pod copy queued for 21 min; the cross-replica copy ran |

Reproduce the precondition for any session, any day:

```sql
WITH tc AS (SELECT agent_id, session_key, data::json->>'id' AS cid, created_at AS ctime
            FROM session_events WHERE type='tool_call'),
     tr AS (SELECT agent_id, session_key, data::json->>'id' AS cid, created_at AS rtime
            FROM session_events WHERE type='tool_result'),
     um AS (SELECT agent_id, session_key, created_at AS utime
            FROM session_messages WHERE role='user')
SELECT date_trunc('day', tc.ctime) AS day, count(*) AS occurrences, count(DISTINCT tc.session_key) AS sessions
FROM tc
JOIN tr ON tr.cid=tc.cid AND tr.agent_id=tc.agent_id AND tr.session_key=tc.session_key
JOIN um ON um.agent_id=tc.agent_id AND um.session_key=tc.session_key
WHERE um.utime > tc.ctime AND um.utime < tr.rtime
GROUP BY 1 ORDER BY 1;
-- 2026-09-04 | 1 | 1
-- 2026-09-18 | 3 | 1
```

**What this changes about the plan.** The 09-04 record removes the last reading
in which the 09-18 incident could be "a new feature misbehaving": the defect
predates the gate, the gate removed its *common* form (a same-pod duplicate now
queues), and what is left is the cross-replica form Q6 describes. It also dates
the second half of the fix: a truthful pad is not a new requirement invented on
09-18 — the 09-04 history already contains a writer that could not tell "my peer
is still running" from "the call died".

## Adopted (2026-09-19): four fixes, in order

Decided after the 09-18 incident and the 09-04 precedent. Each item states the
cause, the layer that owns it, the formal home, the shape of the fix, and the
"cheaper alternative" the Musk gate rejects (Question → Delete → Simplify →
Accelerate → Automate, order invariant). Interfaces are named, not built here.

> **Status (2026-09-19): design complete, implementation pending — and it is one
> unit.** A1–A4 each carry their port/table/wording/anchor-level design, the tests
> they must pass, and the falsification that proves the test bites. No code is
> written until all four designs are agreed (the sequencing table at the end of
> this section is the implementation order). Anything the design leaves open is
> marked *not built now* with the condition that would reopen it.
>
> **Landing log (working tree, 2026-09-19).** Landed and green: **G25** (sandbox
> lease's claim branch `epoch = epoch + 1`, `TestSandboxLeaseEpochNeverResetsAcrossTakeover`,
> falsification run for real); **A2 step 1** (the three-sentence vocabulary in
> `internal/provider/provider.go`, `normalizeForPrompt` using the no-fact sentence,
> `TestProjectionDoesNotClaimInterruptedWithoutEvidence`); **A1-a/b/e** — the
> `session_turns` table and its four CAS methods + two store tests, the port
> `internal/agent/sessionlease.go`, the adapter `internal/gateway/sessionlease.go`, and
> the fence, **by explicit signature as reviewed**: `session.SessionStore`'s two write
> methods take a `*session.TurnFence` (the port names its own type), the adapter
> translates it into `store.SessionFence`, and the store exposes
> `SaveSessionFenced` / `AppendSessionMessageFenced` whose statements carry the
> `EXISTS (session_turns …)` predicate (the same shape A3 plans for
> `PutIfVersion`); `Session.SetTurnFence` is set at admission, refreshed on renew,
> cleared on loss, and a refused write records `Session.FenceLost()`.
> **A1-c/d landed too (same day)**: both entry points (`loop.go` `HandleMessage` /
> `HandleMessageStream`) take the lease first and the local FIFO slot second, release the
> lease last, and emit `queued{position, holder, expires_at}` while waiting; `RunTurn`'s
> `TurnStartIfIdle` verdict now reads the lease (`Live`) so an automatic turn defers instead
> of queueing behind a peer; both ReAct loops stop at the iteration boundary once the lease
> is lost. The real `SessionLease` is wired at the composition root
> (`agent.WithSessionLease(storeSessionLease{st: st})` in `internal/gateway/userspace.go`).
> Agent-level witness: `internal/agent/turnlease_test.go` (3 tests; two falsifications run
> for real — dropping the lease from admission and from the IfIdle verdict both go red).
> **A2 step 2 landed too**: the projection's sentence for an open call now follows the
> lease (`Agent.openCallAnswer`: an unreadable lease ⇒ *not known*; a live holder that is
> not us ⇒ *still running, do not re-issue*; nobody else holds the session ⇒ *interrupted*
> is provable again). Both projection sites use `normalizeForPromptWith(...)`.
> **A4's server half landed (same day)**: `/api/chat/subscribe` announces
> `event: turn_active` (`{holder, epoch, expires_at}`) when a live holder exists, and
> `/api/chat/history` carries the same fact as `turnActive`; silence means "no live
> holder", which the client must render as its *unknown* branch rather than as a
> death claim. The `queued` event already carries holder + ETA (A1-c).
> **Known limit of the server half (2026-09-21)**: the fact is announced **once**,
> when the subscription opens, and is **not re-announced** on lease renew or release
> — `internal/agent/turnlease.go` emits only `queued` (while a waiter is parked) and
> the superseded `notice` (on loss). So a view that outlives the lease TTL keeps the
> last fact it saw and must self-correct; the client does this with its own O1′
> expiry trigger (cloud `use-chat-session.ts` `useTurnFactNow`), which degrades an
> expired fact to *unknown* rather than leaving it claiming a holder. A server-side
> re-announce on renew/release would remove the client's dependence on the TTL, but
> is not implemented; the client must not assume it.
> **A4's client half — the *view* is landed, the *composer* is not (2026-09-21)**:
> the panel derives its four states once, in cloud `src/features/chat/turn-state.ts`
> (`selectTurnState` + `toolPhase` + `toolRowLabelKey`), and the tool row, the group
> header and the rounds bundle all judge from it. That day's re-read found the row had
> **never received the fact** — the `turnState` prop was not passed at the one call
> site — so a peer-held turn still read *Interrupted*, and the row-level spec meant to
> catch it read only the *header* (a different code path), so it stayed green. Fixed:
> the row now calls `toolRowLabelKey({ turnState, turnOver })` and the spec expands the
> group **and** the row. **Still not landed**: the composer reading `turnActive`
> (C4/C5) and Stop/detach; and A3.
>
> **Added the same day, while checking every existing lease against A1's
> requirements (12):** A1.2 is now a designed port (types, the fact-carrying
> error, layer placement), A1.3 answers the epoch question with a measured
> counterexample in the *existing* sandbox lease (**G25**: its token resets to 1
> per generation, and a delayed same-owner release deletes a live row), and A1.4
> pins the fence inside the store's write statement rather than the caller. The
> induced contract (L1–L7) and the as-built classification of all four lease
> mechanisms are in [12](./文件系统形式化证明/12-lease-formal-design.md).

### A1 — One turn per session, across replicas (the root cause)

**Cause.** The invariant "at most one writer per session" (clause W) is real at
the *entity* level but its enforcement lives in a framework detail — the
in-process `a.sessions` map (`internal/agent/loop.go:2318-2340`). Its scope
(the fleet) is wider than its storage (one process), so the assertion holds only
until two requests land on two pods. Formal home: **F1** — "start a turn" is a
migration whose preconditions must be evaluated by the system, not by each
process.

**Fix.** A session turn lease in the store, keyed `(user_id, agent_id,
session_key)`: owner = pod identity, `acquired_at`/`expires_at`, a monotonic
`fencing_token`, renewed while the turn runs (TTL ≥ turn budget + tool grace).
Acquire is CAS; a loser gets the existing `queued{position}` event and waits
FIFO. The in-process gate stays as the fast path. **The fencing token is checked
where the session appends** (`session.Append` — the single write point), so a
holder that lost its lease cannot keep writing when its goroutine resumes.
Lease-store failure is fail-closed for *starting* a turn: refusing to start is
recoverable, two writers are not.

**Gate.** Cheaper alternatives rejected: *stickiness* (hash the session to one
pod) covers browser traffic only — cron/goal/heartbeat/webhook ignore it, and it
puts the policy in the routing detail, i.e. outside-in; *fencing without
admission* still lets the second turn spend a full model+tool budget before its
appends are refused. The lease is the smallest mechanism that yields admission,
FIFO **and** fencing together.

**Evidence / verification.** UT: two pools over one store racing `Acquire` ⇒ one
winner, one `queued`; expiry hands off; a stale holder's append is refused.
E2E: two gateway instances, one session, two `chat/stream` POSTs ⇒ the second
queues (assert one `iteration=1` at a time). Falsify: drop the store check ⇒ two
concurrent `iteration=1` loops, the 09-18 shape.

#### A1.1 Where the coordination lives (and why not Redis)

Redis in this system, verified 2026-09-19:

| Redis is used for | Code | Default when Redis is off |
|---|---|---|
| per-`(channel, account)` singleton lease | `internal/rediscoord/lease.go:28-30` (`SetNX` + TTL), `:77` (Lua release that checks the holder) | `storeLeaser` → `AcquireChannelLease` (`internal/gateway/channels.go:16-30`; `internal/store/database.go:4975-5055`) |
| cross-replica inbound/outbound | `internal/bus/bus.go:177` `NewRedis` (Streams + consumer group + `XAck`) | in-process channels |
| user-space cache invalidation | `internal/rediscoord/invalidator.go` (pub/sub) | nothing (one process) |
| OAuth refresh-rotation mutex | `internal/mcp/oauth/adapter/redis_locker.go` | nothing |

`FASTAGENT_REDIS_ENABLED` defaults **off** (`internal/config/env.go:45-51`), and
**neither dev nor prod sets it**: no `FASTAGENT_REDIS_*` in either deployment's
env, no Redis keys in `fastagent-secrets`, and no Redis/Valkey pod in the cluster
(checked 2026-09-19). The gateway's default leaser is the **store** one —
`internal/gateway/gateway.go:424` — and the store's naming is deliberately
kind-specific so it "can grow other lease kinds without renaming"
(`internal/gateway/channels.go:18-20`).

**Conclusion.** The turn lease is a *second lease kind in the same store*, not a
new Redis mechanism. Redis stays what it already is (an optional accelerator for
IM delivery and channel singletons); correctness must not depend on a component
that the incident's own environment does not run. A Redis-backed implementation
would be a legitimate fast path later — the port below would not change — but
per the gate it is not built now.

The shape is not new to the codebase: `channels.Leaser` exists for the *same*
failure ("two replicas sharing the same bot token would both long-poll the
upstream and the user would receive every reply twice",
`internal/channels/lease.go:9-16`). A1 applies that pattern to the key nobody has
covered yet.

#### A1.2 Port

```go
// internal/agent/sessionlease.go — the admission port. The consumer is the turn,
// not IM: do NOT reuse channels.Leaser, whose key has no session dimension.
type SessionKey struct{ UserID, AgentID, SessionKey string } // = the sessions primary key

// Turn is what a successful Acquire yields: everything the caller (and the
// signals it emits) needs to name this possession.
type Turn struct {
    Holder    string    // "<pod>/<uuid>" — unique per acquisition, not per process
    Epoch     int64     // fencing token: strictly increasing inside the row, never reset
    ExpiresAt time.Time // what the dashboard's ETA and this turn's timer read
}

// ErrSessionTurnBusy carries the *facts* of the other holder, because the only
// consumers are the signals: the queued event needs a holder and an ETA, and a
// caller that retries needs to know when retrying could work. A bare bool would
// force the caller to invent those numbers (A4.1).
type SessionTurnBusyError struct{ Holder string; ExpiresAt time.Time }
func (e *SessionTurnBusyError) Error() string
func (e *SessionTurnBusyError) Is(target error) bool // errors.Is(err, ErrSessionTurnBusy)

type SessionLease interface {
    // Acquire returns the Turn when this holder now owns the session, and
    // *SessionTurnBusyError when a live holder already does. Every other error
    // (store unreachable, …) is a refusal to start: fail closed.
    Acquire(ctx context.Context, s SessionKey, ttl time.Duration) (*Turn, error)
    // Renew extends the lease and returns the new Turn. A lost lease is
    // *SessionTurnBusyError again: the caller MUST stop writing and say so.
    Renew(ctx context.Context, s SessionKey, t Turn, ttl time.Duration) (*Turn, error)
    // Release is guarded on the same pair (12 §3, L5).
    Release(ctx context.Context, s SessionKey, t Turn) error
}
```

Implementations: `storeSessionLease` (**default** — the store is always present),
`NopSessionLease` (single-instance / not wired, mirroring `NopLeaser`), and — see
the gate — no Redis one.

Three design choices worth naming, because each replaces a cheaper shape that
cannot express a fact the signals need:

* **`Turn` instead of `(epoch int64, ok bool)`.** The loser of a race must be able
  to say *who* holds the session and *until when* (`queued{holder, ETA}`, A4.1)
  without a second read that can race the hand-off. The error carries the fact,
  so the fact has one source.
* **`Acquire` generates the holder, not the caller.** `SessionLease` owns the
  nonce because the nonce's uniqueness is the fence's premise (12 §3, L4(c)); a
  caller that passes `host:pid` would silently reintroduce the sandbox lease's
  weakness (12 §5). The store adapter is the only place that knows how a holder
  is minted.
* **`Renew` takes the whole `Turn`, not `(holder, epoch)`.** A turn that lost its
  lease and renews again must not be able to "renew itself back": the token it
  presents is the one it was given, and the store's CAS is on the pair. Two
  loose arguments invite a caller to update one of them.

**Layer placement (Clean Architecture; details in [12 §7](./文件系统形式化证明/12-lease-formal-design.md)).**

| Layer | What lives here | Anchor |
|---|---|---|
| Entities | the invariant (one writer per session) + `SessionKey` — the session's identity, never the transport's | `internal/session` |
| Use case | the admission policy (`TurnStartOrQueue` / `TurnStartIfIdle`) and the turn that holds the lease; **owns the port** | `internal/agent/admission.go`, `internal/agent/sessionlease.go` |
| Interface adapter | the port's implementation and the wiring into each agent | `internal/gateway` (`storeLeaser`'s sibling), `agent.WithSessionLease` (`internal/agent/manager.go:86-150`) |
| Frameworks & drivers | the DDL + CAS SQL, Redis (later, optional), `NopSessionLease` | `internal/store` |

The dependency rule decides one thing here that is easy to get backwards: the
**fence SQL lives in the outermost layer** and the port only carries the token.
That is not an inversion problem — it is what L4(a) requires: the resource
validates the token in the same atomic step as the effect (A1.4).

#### A1.3 Storage

```sql
CREATE TABLE IF NOT EXISTS session_turns (
  user_id     TEXT NOT NULL,
  agent_id    TEXT NOT NULL,
  session_key TEXT NOT NULL,
  holder_id   TEXT NOT NULL,
  epoch       INTEGER NOT NULL DEFAULT 0,   -- fencing token; both acquire paths supply it
                                            -- explicitly (insert → 1, claim → epoch + 1),
                                            -- so this default is never observed
  acquired_at TIMESTAMP NOT NULL,
  expires_at  TIMESTAMP NOT NULL,
  PRIMARY KEY (user_id, agent_id, session_key)
);
```

Acquire is the `channel_leases` CAS verbatim (`internal/store/database.go:4975-5055`)
plus one clause — `epoch = epoch + 1` on the successful path, returned to the
holder; `Renew`/`Release` both carry `AND holder_id = ? AND epoch = ?`. Both
dialect branches (Postgres `ON CONFLICT … WHERE`, SQLite `excluded.…`) are copied
as-is. Expiry is lazy, like the sandbox lease: the next `Acquire` claims a row
whose `expires_at <= now` (`internal/store/sandbox_leases.go:68-73`).

**Does the epoch start at 0 and grow per row?** No — and it must not, which the
sandbox lease demonstrates the hard way. In `sandbox_leases` the token is *not*
per-row: both write paths of `AcquireSandboxLease` hard-code `epoch = 1`
(`internal/store/sandbox_leases.go:72` and `:81`) and only renew/replace
increment, so it means "renewals inside this possession", not "which generation
of this row". Measured consequence (probe, deleted after the run): the same
owner (`host:pid` — repeatable) re-acquires the scope after expiry and gets
`epoch=1` again; a delayed `ReleaseSandboxLease(scope, "pod-a", 1)` from the
first generation then returns **`released=true`** and deletes the *new*
generation's live row. The claim in `docs/sandbox-pool-leases.md:237-240` ("any
stale destroy fails closed") is false; `:87-89` of the same file already admits
why. It is registered as **G25** and generalised as obligation **L4(c)** in
[12 §3](./文件系统形式化证明/12-lease-formal-design.md) — *the `(holder, epoch)`
pair must be unique per acquisition*.

For `session_turns` the rule is therefore explicit, and it is two-part because
either half alone is not enough:

* `epoch` starts at 1 on the **insert** and is `epoch + 1` on **every**
  successful acquisition, including the claim of an expired row. It never
  returns to 1 while the row exists.
* `holder` is `<pod>/<uuid>`, minted per acquisition, so even a row that is
  deleted (release) and re-created cannot hand out a `(holder, epoch)` pair that
  a stale caller still holds.

The second clause is the one that covers the deletion case (the epoch restarts
at 1 by construction when a row has no predecessor); the first is the one that
makes the epoch usable as an *ordering* fact, which the signals need to say "your
turn was superseded by a newer one" rather than just "refused".

#### A1.4 Renewal, fencing, waiting, failure

* **TTL = turn budget + tool grace** (today 45 m + 60 s), renewed **on a timer at
  TTL/3 by the turn's own goroutine**. Deliberately *not* the sandbox lease's
  rule ("renew when an operation has been running > `defaultLongOpRenew`",
  `internal/sandbox/lifecycle.go:145-149`): a turn spends minutes inside one
  delegate call without appending anything, so activity-driven renewal would let
  the lease lapse mid-turn — the 605-second window of the 09-18 incident is
  exactly that shape.
* **Fencing**: the epoch is checked before each append. `session.Append` is the
  only writer of `session_messages`, so that is the one place to check. A lost
  lease stops the turn and emits a σ; it never keeps writing. **The notice is
  emitted from the renewal goroutine, so `Stop` joins it** — emitting a σ is
  writing too, and "stops" has to be true of it as well. `Stop` is the turn's
  last statement (`defer lease.Stop()`); it closes the stop channel and then
  waits for the renewer to return before clearing the fence or releasing the
  lease. Without the wait a caller that closes its event channel once the turn
  returns is racing a send, which is the one `-race` failure this package
  shipped (`TestSupersededTurnStopsAndSignals`). The wait is bounded (2 s, one
  WARN): the emit respects the turn's context, so a reader that stopped reading
  is ended by that context rather than by patience. (A store failover
  or a TTL race can produce two *holders*; only the fence stops two *successful
  writers*, which is why it is part of A1 rather than a later nicety.)
  **Where the check executes matters, and it is not the caller.** A guard of the
  shape "read the lease, then append" leaves a TOCTOU window of one round trip —
  exactly the window a TTL race needs. The token therefore travels *with the
  write*: `Session.append` carries `(holder, epoch)` into the store, and the
  store's `INSERT`/`UPDATE` for `session_messages` and `sessions` carries
  `AND EXISTS (SELECT 1 FROM session_turns WHERE user_id = ? AND agent_id = ?
  AND session_key = ? AND holder_id = ? AND epoch = ? AND expires_at > now)`, so
  a refused append reports 0 rows affected. One statement, one snapshot: the
  fence and the effect cannot be separated. The same shape is what makes the
  sandbox lease's `DELETE … WHERE owner = ? AND epoch = ?` sound *within* a
  generation (12 §4).

#### A1.4a Why "with the write" means *in the signature*, and what else converged
(decided 2026-09-19)

The sentence above says the token travels with the write. What it did not say is
**how** — and until 2026-09-19 this package carried that kind of fact three
different ways:

| fact | route | owner of the type |
|---|---|---|
| `fence` | method argument | this package (`TurnFence`) |
| `channel` / `accountID` / `chatID` / `projectID` | method arguments | this package |
| `chatterUserID` | a **context value** | the `store` package |

Two mechanisms for one concept, and the second one cost the Dependency Rule:
`internal/session/manager.go` imported `internal/store` for a single context
tagging helper. The rule that decided the survivor is not taste, it is the
**obligation each fact discharges**:

* a **precondition** (the fence: "a write without the possession must not land")
  must be *distinguishable when absent*. A context value cannot do that — "no
  fence" and "the caller forgot the fence" are the same thing at the type level,
  so `L4a` would be unenforceable;
* a **record** (the chatter: a column that may legitimately be empty) needs only
  to arrive with the call.

So the survivor is the explicit one, and everything of that family rides one
value: `session.WriteScope{Channel, AccountID, ChatID, ProjectID, ChatterUserID,
Fence}`, assembled in exactly one place (`Session.writeScope()`). The judgement,
stated for reuse: **within one family of facts, the strongest obligation decides
the transport** (refusal semantics > record semantics), and age is only a
tie-breaker — the context value here was older precisely because the obligation
it served was weaker.

The same derivation applies to the *refusal* itself, which is a value too:
`ErrSessionFenceLost` is now owned by this package, and `store_adapter.go`
translates the store's sentinel into it. The store **produces** the refusal; the
use case **names** it; only the adapter knows both. `manager.go` no longer
imports `internal/store` at all.

Witnesses: `TestWriteScopeCarriesTheChatterAndTheFence` (the chatter, the fence
and the triple arrive by that one route), `TestWriteRefusalTranslatesTheStoresSentinel`
(the store's sentinel becomes this package's). Falsifications run for real:
dropping `ChatterUserID` from `writeScope()` fails the first; removing the
mapping fails the second.
* **Where the fence type lives, and why the store has a second pair of write
  methods.** The precondition belongs to the write, so it travels as an
  argument, never as a context value. It is *this* layer's type
  (`session.TurnFence`) that the port names: an inner interface must not name an
  outer package's type, so the adapter (`internal/session/store_adapter.go`) is
  the single place that translates `session.TurnFence` into `store.SessionFence`.
  The store keeps the plain `SaveSession` / `AppendSessionMessage` untouched and
  adds the fenced pair beside them, because the fenced call has a different
  contract (it may refuse) and a different caller set (turns vs. the doctor,
  migrations and tests) — the same "capability beside the capability-free form"
  shape A3's `PutIfVersion` will take.
* **A loss leaves the fence in place (stale), and the turn stops.** `Stop()` clears
  the fence only when the turn still owns the session; after a takeover the stale
  pair stays on the cached `Session` so every later write — including whatever the
  loop emits after it notices — is refused by the store rather than landing in the
  peer's history. The next turn overwrites it at admission.
* **Waiting**: `Acquire` failure ⇒ the existing `queued{position}` event
  (`internal/agent/loop.go:2330-2336`). `position` counts this pod's waiters only
  — best effort, stated as such — and there is no cross-pod waiter registry
  (deleted by the gate). The payload also gets the holder and the ETA (A4.1),
  because "position 1" cannot say *whose* turn you are waiting for or whether
  waiting is even the right move.
* **Failure**: store unreachable ⇒ refuse to start the turn (fail-closed) with an
  explicit message; single-instance or not wired ⇒ `NopSessionLease`. Refusing to
  start is recoverable; two writers are not.
* **Holder identity**: `<pod>/<uuid>`. The sandbox lease uses `host:pid`
  (`internal/gateway/userspace.go:91-97`); including the pod name makes rows and
  logs attributable, which the 09-18 investigation needed and did not have.
* **The `TurnStartIfIdle` path must consult the lease too (found in the 09-19
  review).** `RunTurn` (`internal/agent/admission.go:60-70`, called by the
  gateway's task queue at `internal/gateway/gateway.go:593`) decides "is the
  session busy?" from the **in-process** gate (`sess.TurnActive()`). Across
  replicas that check is blind: an automatic turn (cron tick, goal
  continuation, heartbeat, sub-agent delivery) arriving at pod B sees "idle"
  while pod A holds the session, and then falls into `HandleMessage`, where the
  lease makes it **queue** instead of being refused. That inverts P2's decision:
  `TurnStartIfIdle` exists precisely so automatic work does not hold a queue
  worker and its turn budget hostage ("must not hold a queue worker and its turn
  budget hostage", `internal/agent/admission.go:26-33`). So A1's implementation
  must move the IfIdle verdict onto the lease: try the lease first, and map "a
  live holder exists" to `ErrTurnNotAdmitted` (no queue entry, no budget spent)
  rather than letting the caller fall through to the waiting path. **Landed (2026-09-19, working tree)**: `admission.go` reads the lease
  (`Live`) before the in-process gate; witness `TestAutomaticTurnDefersWhenAPeerHoldsTheLease`. Without this,
  A1 closes the user-turn race and leaves the automatic-turn race in place —
  the same duplicate-work shape, just sourced from cron instead of a human.

#### A1.5 Why not one table with the sandbox lease

The mechanisms are the same (a CAS row + TTL + monotonic epoch); what they guard
is not.

| Dimension | `sandbox_leases` (as built) | `session_turns` (this design) |
|---|---|---|
| key | `scope_key` = `agent[:p:<proj>][:s:<sid>]`, **no `user_id`** (`internal/sandbox/docker_executor.go:219-227`) | `(user_id, agent_id, session_key)` — the identity `session_messages` uses (`internal/store/database.go:1735-1739`) |
| rows per turn | **one turn can touch N scopes** (its chat container `agent:s:<sid>` *and* the project-addressed preview container `agent:p:<pid>`; `LiveProjectExecutors` matches both prefixes, `internal/sandbox/e2b_executor.go:2058-2065`) | exactly one |
| row contents | the resource's facts: `sandbox_id / envd_token / template / state / paused_at / unhydrated` (`internal/store/database.go:1679-1691`) | only the exclusion: `holder / epoch / expires_at` |
| acquire means | "is this row my instance?" — compared on `sandbox_id` + `envd_token` (`internal/store/sandbox_leases.go:96-100`) | "is anyone else holding it?" |
| transferable | **yes, by design** — "Ownership moves to the renewing pod only when the row still points at sandboxID and has not expired" (`internal/store/sandbox_leases.go:105-113`), because the sandbox outlives the pod | **no** — a dead pod's turn is dead; a peer waits for expiry and starts a *new* turn |
| renewal trigger | activity: an operation running longer than `defaultLongOpRenew` (2 m) renews (`internal/sandbox/lifecycle.go:145-149`; `internal/sandbox/e2b_executor.go:2264/2402/2471`) | a timer in the turn's goroutine |
| TTL tuned for | **cost** — `DefaultSandboxLeaseTTL = 15 m` (`internal/sandbox/lease.go:134`); too short costs one extra provision (measured 32 208 ms) and no correctness | **correctness** — must exceed the longest turn, or a live writer is evicted |
| epoch fences | destroys/replacements — but **only within one generation**: the token resets to 1 on a takeover, so a delayed same-owner release can still match a newer row (**G25**, measured; [12 §5](./文件系统形式化证明/12-lease-formal-design.md)) | appends to the transcript, with a token that never resets |
| holder | `host:pid` (`internal/gateway/userspace.go:91-97`) | `<pod>/<uuid>` |
| when the store is down | may degrade (wait / skip a rebuild) — it guards cost and instance identity | must fail closed — it guards a data invariant |

What is shared is exactly three things, and they are enough: the CAS statement
shape, an `epoch` column whose *purpose* is fencing (the sandbox's resets per
generation — G25 — which is why the turn lease states the invariant instead of
copying the implementation), and the fixture pattern (`lease_pool_test.go`,
`sandbox_pool_lease_test.go` already assert "two pools, one store", "expiry hands
off", "a stale epoch is refused").

### A2 — The pad must not assert what it does not know

**Cause.** `normalizeForPrompt` answers every open tool call with
`provider.StoppedToolResult` = *"(stopped — execution was interrupted…)"*. When
the owning turn is alive on another replica that sentence is false, and it is
not inert: the 09-18 model read it and re-issued the same two sub-tasks
(16:34:29, "补查的两条线刚才被打断了，重新发起"). Formal home: **F2 / O1** — σ must be
true; this σ also *produced* the duplication clause W exists to prevent.

**Fix, in two steps (Delete before Simplify, per the gate).** **Both steps have landed
(working tree, 2026-09-19).** the vocabulary is `provider.{StoppedToolResult,
NoReplyTurnAliveResult, NoReplyUnknownResult}` + `provider.SyntheticToolPads`, and
`normalizeForPrompt` answers every open call with the *no-fact* sentence until A1's
`turnActive` supplies the holder fact (A2.1's third row). Step 1 reads (and is what the no-argument `normalizeForPrompt` still does, so a
caller with no fact cannot accidentally claim more); step 2 is `openCallAnswer` in
`turnlease.go`, whose witness is `TestOpenCallAnswerFollowsTheLeaseFacts` (peer-held
and unreadable-lease cases go red when the unconditional claim is restored). replace the claim with the fact the projector
actually has — "no reply was recorded for this call; the owning turn may still
be running" — and never use the word *interrupted* without evidence. Step 2,
once A1 exists: split into the two sentences that are now provable — holder
dead ⇒ interrupted; holder alive ⇒ *still running, do not re-issue; wait or read
what landed*. (An optional `tool_call` execution heartbeat would give the
projector the same distinction without the lease, but it is a second mechanism
for one fact; do it only if a third consumer appears.)

**Evidence / verification.** UT: three history shapes ⇒ the wording for each;
no "interrupted" when no lease/heartbeat fact says so. Falsify: restore the old
string ⇒ the "must not assert" test goes red.

#### A2.1 The sentence, decided

The substitution lives in one place: `internal/agent/normalize.go:81` writes
`provider.StoppedToolResult` (`internal/provider/provider.go:59`) into the prompt
projection for every open call (`normalizeForPrompt`, `internal/agent/normalize.go:21`;
its own comment at `:11` states the rule). Replace that single write with a
choice over three shapes:

| shape | what the projector knows | the sentence |
|---|---|---|
| open call, **holder dead** (A1's lease expired or was released, no heartbeat) | the owning turn ended without replying | keep today's `"(stopped — execution was interrupted before the tool returned)"` — now a provable claim |
| open call, **holder alive elsewhere** (A1's lease is live and is not ours) | a peer owns the session; this call has no reply *yet* | `"(no reply recorded yet — the owning turn is still running; do not re-issue this call; wait, or read what has already landed)"` |
| open call, **no fact available** (no lease wired, pre-A1 history, single instance) | nothing | `"(no reply recorded yet; whether the owning turn is still running is not known to this projection)"` |

Keep the three strings as named constants next to `StoppedToolResult` so the
vocabulary has one home, and make "the word *interrupted* may only appear with
evidence" a test rather than a convention.

#### A2.2 Tests and falsification

* extend `TestNormalizeForPromptShapes` (`internal/agent/normalize_test.go`) and
  the dangling-reply cases in `internal/provider/openai_dangling_tool_test.go`
  with the three shapes above;
* new assertion: with no lease fact present, the projection contains **no**
  occurrence of "interrupted" (today it always does);
* falsify: restore `provider.StoppedToolResult` for the alive-holder case ⇒ the
  new assertion goes red, and the 09-18 transcript shows why it matters (the
  model re-issued the two sub-tasks word-for-word).

### A3 — Writer uniqueness for the store and the sandbox

**Cause.** A tool's `Put` is last-writer-wins and a scope is assumed to have one
sandbox instance. Both assumptions are single-writer assumptions; A1's absence
breaks them. Measured on 09-18: the same deliverable written twice (pod B
15348 bytes at 16:44:07, pod A 11492 bytes at 16:50:16 — the first version lost),
and the two turns fought over the sandbox lease (adopt → lease race → three
"rebuilt sandbox superseded by another pod"; 12 hydrates in ~40 minutes for one
scope; three `[workspace] the sandbox was REPLACED` signals). Formal home:
**F1 / R1–R2** — the "refuse, don't migrate" rule currently covers the
sandbox→store direction only; the tool→store direction has no precondition at
all.

#### A3.0 In plain words: what this item is, and what it is not

Every file the agent produces ends up in one place — a **key** in the workspace
store (`<agent>/<project>/<session>/<path>`). "One key" is the whole design: the
dashboard, the sandbox, the download endpoint, the preview container all read
that one object. What varies is **who writes it, and when**.

A single-writer world looks like this:

```
turn starts → tool writes <path> → (write-through) sandbox copy updated → turn ends
```

The 09-18 world had two of these in flight at once:

```
16:44:07  pod B  → Put(<path>, 15348 bytes)  → the agent sees "written"
16:50:16  pod A  → Put(<path>, 11492 bytes)  → the same key, silently replaced
```

Nothing refused, nothing was reported, nothing in either transcript says the
other turn existed. That is what "no precondition" means concretely: the store
accepts a `Put` on a key that another live writer may already own. Compare the
**sandbox→store** direction, where T1 already refuses and reports
(`BLOCKED`) — same shape of hazard, one direction protected and one not.

So A3 is *not* "we need a merge algorithm", and *not* "make the store
transactional". It is the narrow question: **when two writers can touch one key,
does the second one find out?** There are exactly three answers, and they differ
in cost by an order of magnitude:

| Answer | What the second writer does | What changes for the user/agent | Cost |
|---|---|---|---|
| do nothing | overwrite | the first version is gone, silently | 0 |
| notice and say it | overwrite, then report *"another writer replaced `<path>` while this turn was working; re-read before editing further"* | the loss is still a loss, but it is **on the record**, so the agent can re-derive it | one extra `Stat` per write |
| refuse | refuse + report | the loss becomes a **refusal**, one policy for all divergence (same as T1's `BLOCKED`) | `Version` on `ObjectInfo`, `PutIfVersion`, 3 implementations, 11 call sites |

**Why the "delete" option comes before both**: with A1 in force, the 09-18
sequence cannot happen, because pod B never runs a turn while pod A holds the
session. A3 is therefore a belt, and the gate's order (question → delete →
simplify → …) says a belt is only bought once the braces are on. The one thing
worth checking before deleting it entirely: are there writers that A1 does **not**
serialise? Today there are — a panel upload (`internal/setup/handlers_agents.go:1445`),
an attachment write (`internal/agent/attachments.go:118`), a skill file publish
(`internal/skills/objectstore.go:105`), and the sandbox's own write-back
(`internal/sandbox/lifecycle.go:767`, `:1247`) all write the store without
holding a turn lease. Those are the paths that decide between option 1 and
option 2.

**Decision (2026-09-19, this review): family B — versioned conditional writes.**
Every tool→store write path gets a precondition the *store* evaluates, rather
than a client-side "read the metadata and compare" (family A). Why B:

* S3's `LastModified` has **one-second** resolution, so an A-family `size+mtime`
  judge silently misses a same-second, same-size overwrite — the exact class this
  item exists for — while an ETag is exact.
* ETag is already trusted in this repository as a content identity (the cleanup
  script keys on "the same non-multipart ETag"), so this promotes something
  already used for *equality* into a *write precondition*, rather than
  introducing a new concept.
* It works for the blind-overwrite path too: `write_file` has no "version I last
  saw", so no honest A-family check exists for it — and that path is what lost
  the 09-18 deliverable.
* The mechanism is already proven here: A1's fence is the same shape
  (`…Fenced` statements that refuse), so B makes the whole store speak one
  language: **a write may carry a precondition, and the resource enforces it**.

Boundary (unchanged, and deliberately so): the *sandbox→store reconcile* (T1)
keeps its A-family judge and stays "observe once, then say what was replaced". A
version-based refusal was tried there on 2026-09-18 and withdrawn because
refusing the write-back deadlocks the turn (docs 07 §3.11: "不做 CAS"). A3 is
about the *tool→store* direction, where refusing is safe: nothing is waiting on
that write, and the model can re-read and re-issue.

The concrete change list (B1–B11) is in the register, §A3.1 below.

**Fix (as originally framed).** First choice is *delete*: with A1 in force there is no second writer to
defend against, so this item is a belt, not the braces — build it only if A1
cannot cover a path (e.g. a panel upload racing a turn). If it is built, make it
optimistic concurrency on the object (`Put(if-match: etag)`), refusing and
reporting rather than overwriting — the same "refuse + say so" shape as T1's
`BLOCKED`, which keeps one policy for divergence instead of two. Explicitly
rejected: auto-merge (no semantics) and per-turn directories (changes the
product's "one project, one tree").

**Evidence / verification.** UT: two clients with the same etag ⇒ second is
refused and reported. E2E: two instances writing one path ⇒ one wins, the loser
is told. Falsify: drop the etag check ⇒ the overwrite reappears.

#### A3.1 The change list (family B; each item is reviewed before it lands)

> **Landing log (2026-09-19, working tree).** **B1–B5 landed**: `ObjectInfo.Version` + `workspace.Version`
> (+ `VersionAbsent`, `ErrVersionConflict`) on the port; LocalFS `Version = size:mtime_ns` and a
> best-effort `PutIfVersion` **declared as such**; S3 `Version = ETag` and a real conditional PUT
> (`SetMatchETag` / `SetMatchETagExcept("*")` for create-only, 412 → `ErrVersionConflict`); `Metered`
> passes both through. Witness: `TestLocalFSPutIfVersionRefusesAStaleExpectation` (create-only on a live
> key, a stale expectation, and the stale writer's bytes proven not to have landed), with the
> falsification run for real. The port's doc states the per-backend strength (S3 exact, LocalFS
> best-effort) — B6's code half; its table in `01 §2` is still to write. **B7–B9 landed**: `write_file`, `edit_file` and `apply_patch` now write through
> `Registry.putGuarded` (Stat → `PutIfVersion`); a conflict returns "another writer changed <path> while
> this turn was working; nothing was overwritten — read it again and re-apply your change", and the
> bytes provably do not land. **B6 landed** as the per-backend strength table in `01 §2.x`.
> **B10 landed**: attachments are create-only (`VersionAbsent`); a skills publish reads the
> current version and conditions on it (an intentional re-publish, so read-modify-write).
> **B11-a landed**: the panel's upload is create-only, and a name collision answers **409 with the
> current version / size / modified-at**, so the panel can offer the industry's three answers
> (keep both = server-side auto-rename, replace = re-send with that version, or cancel).
> **B11-b's server half landed**: the upload accepts an optional `expectedVersion` form field — with
> it the write means "replace the version I saw" (`PutIfVersion(expected)`; exactly one file per
> request), without it the write stays create-only. Witness
> `TestFileUpload_NameCollisionOffersTheCurrentVersionThenReplaces` (create → 409 with the current
> version → replace with it → content is the new one), falsification run for real. The cloud panel's
> three answers (keep both / replace / cancel) and the auto-rename half ride the cloud work.
>
> **Landing log, second pass (2026-09-21, working tree).** The first pass proved the *rule*; this one
> proves the *delivery point* — the call site that actually carries the fact to the store. Rule-level
> green was the state that hid the tool-row defect (A4 §A4.2), so the same standard is applied here.
> **B7–B9 now have delivery-point witnesses**: `internal/agent/tools/write_guarded_peer_test.go` drives
> `write_file` (overwrite and create), `edit_file` and `apply_patch` through a real `workspace.LocalFS`
> whose `Stat` answers the read-time version and *then* lets a peer's bytes land (longer body, so
> `size:mtime_ns` cannot match by accident). Each asserts the refusal text (`another writer changed`,
> `nothing was overwritten`, `read it again`) *and* that the peer's bytes survived. **Falsifications run
> for real, each verified by `grep` before trusting the red, each reverted**: a plain `Put` at
> `file.go:592` ⇒ 2 red, at `file.go:702` ⇒ 1 red, at `apply_patch.go:549` ⇒ 1 red.
> **B3's fake was faithful and therefore blind**: it emulates `If-Match` the way AWS does, so nothing
> in it could show that the bucket this deployment runs on (Ceph RGW / DO Spaces) answers 412 to
> every `If-Match` — which made every overwrite of an existing path report "another writer changed
> it" (09-22 dev session `MImz6pYfMoZLJabEHJRI4p`). `s3_live_test.go`
> (`TestS3LiveOverwriteOfAnExistingPath`, gated by `FASTAGENT_S3_LIVE=1`) is the witness that closes
> that gap against a real bucket.
> **B3 now has the witness its backend was missing** — `internal/workspace/s3_version_test.go` runs a
> hand-rolled minimal S3 over `httptest` (location, HEAD → ETag, PUT honouring `If-None-Match: *` /
> `If-Match` → 412). It asserts that `Stat`'s version *is* the ETag, that the conditional PUT carries
> exactly that `If-Match`, that a stale write is refused with the object untouched, and that a
> create-only PUT sends `If-None-Match: *` (falsification: delete the `SetMatchETag*` branch ⇒ both
> red). Sequential ETags (`etag-1`, `etag-2`) mean the test reads the version rather than re-deriving
> it — the point of B3 is that the *store* owns the token.
> **B10 changed posture after its witness found a silent wrong answer.** `attachments.go` used to
> `Put` first and, on `ErrVersionConflict`, log a warning *while still appending the old name to
> `paths`* — so the `[Attached: …]` breadcrumb named a file whose bytes were somebody else's. The
> store write now happens **first** (it is the store that decides the name): create-only
> `PutIfVersion(VersionAbsent)`, and on conflict it **keeps both** under `<stem> (n)<ext>`, bumping an
> existing counter instead of nesting, returning the name that actually landed — and `""` when nothing
> could be had, in which case the caller claims nothing rather than naming an unwritten file.
> Witness `internal/agent/attachments_store_posture_test.go` (a fake store whose `Put` is a genuine
> blind overwrite, so a regression to `Put` is visible) covers create-only, counter bump, and
> all-spellings-taken. Falsifications run for real: `PutIfVersion` → `Put` ⇒ 3 red; drop the keep-both
> loop ⇒ 2 red. **A skills publish is the opposite posture and stays read-modify-write** — it is an
> intentional re-publish — witness `internal/skills/objectstore_posture_test.go` (expectation recorded
> per write: `v0` then `v1`; falsification: force `VersionAbsent` ⇒ the re-publish case red).
> That contrast is the reason B10's two halves are documented together: a shared helper between them
> would erase exactly the difference that matters.
> **B11's cloud panel half landed** (cloud `develop`, working tree): the panel uploads one file per
> request through `uploadAttachments` (`src/features/chat/upload-attachments.ts`), which turns a 409
> into a `NameConflict` and asks the user; the three answers live in
> `src/features/chat/attachment-conflicts.ts` (pure) and `src/features/chat/components/owner/upload-conflict-dialog.tsx`
> (per-file rows, replace offered only when the store named a version). The delivery point is asserted
> end-to-end in `src/__tests__/fastagent/attachment-conflict-flow.test.tsx` (attach → dialog →
> Continue/Replace ⇒ the nth `uploadAgentFile` call carries `{name, expectedVersion}` and the turn
> still leaves). Falsifications run for real: no asker ⇒ 2 red; drop `expectedVersion` ⇒ 1 red; drop
> the pending re-merge ⇒ 4 red; short-circuit `autoRename` ⇒ 7 red.

| # | change | anchor | cost | verification |
|---|--------|--------|------|--------------|
| **B1** | `ObjectInfo` gains `Version string` — an **opaque** token owned by `internal/workspace` (never `minio.ETag` / `syscall.Stat_t`); supplied by `Stat`/`List` | `internal/workspace/workspace.go:76` | 0 | DTO test: every implementation fills it |
| **B2** | the port gains `PutIfVersion(ctx, …, expected Version) error` + `ErrVersionConflict`; `Put` keeps its meaning (blind overwrite) | `workspace.go:48-72` | 0 | interface doc + the two error strings |
| **B3** | S3: conditional PUT via `PutObjectOptions.SetMatchETag` (minio-go v7.3.0 has it); `Stat` returns the ETag as `Version`. **Where the bucket answers 412 to every `If-Match`** (Ceph RGW / DO Spaces; measured 2026-09-22), a refused precondition is re-checked against the object and landed unconditionally when it still carries `expected` — compare-then-write, the strength LocalFS has | `internal/workspace/s3.go:130-191` | 0 extra requests where the bucket is exact; on Spaces one HEAD + one retry PUT | ✅ `s3_version_test.go` (minimal S3 over `httptest`, incl. the `ifMatchUnsupported` backend): `Stat`'s version *is* the ETag, the PUT carries that `If-Match`, stale ⇒ 412/conflict with the object untouched, create-only sends `If-None-Match: *`, and on a bucket that refuses every `If-Match` the overwrite still lands while a superseded expectation is still refused; ✅ live `s3_live_test.go::TestS3LiveOverwriteOfAnExistingPath`; falsification (make a refused precondition an immediate conflict) ⇒ the overwrite case red |
| **B4** | LocalFS: `Version = size:mtime_ns`; `PutIfVersion` compares before writing. **Declared as best-effort** (read-then-write, no kernel CAS) — honest, because multi-replica installs must use S3/PG anyway | `internal/workspace/localfs.go:88/119` | 0 extra requests | test: stale version ⇒ conflict; doc states the strength |
| **B5** | `Metered` passes both through (no cost, no behavior) | `internal/workspace/metering.go:48` | 0 | existing decorator test |
| **B6** | per-backend **strength table** written down (exact: S3/PG; best-effort: LocalFS) and the port doc states "callers may rely on the declared strength only" | `01 §2` + the port's L3 header | 0 | doc |
| **B7** | `write_file` writes with `PutIfVersion` (version read right before; `VersionAbsent` when the object must not exist) — in **both** registrations: `registerFile` and the `RouteWorkspaceStore` branch of `registerSandboxedFile` (`SetExecutor`) | call sites `internal/agent/tools/file.go:592` (host) and `:1207` (sandbox); the guarded write itself `file.go:900-923` | +1 HEAD (0 bytes) | ✅ delivery point: `write_guarded_peer_test.go` (overwrite + create, **per registration** — the sandbox half added 2026-09-22, register row 47); falsification (plain `Put` at either call site) ⇒ 2 red |
| **B8** | `edit_file` uses the version from its own read (it already `Get`s the object) | call sites `internal/agent/tools/file.go:702` (host), `:1404` (sandbox — the only branch in that registration that was already guarded) | 0 extra | ✅ delivery point: same file (peer lands after the read), in both registrations; falsification ⇒ 1 red |
| **B9** | `apply_patch` uses the version captured by its own write path — in both registrations (`writeForPatch` and `writeForPatchSandbox`) | call sites `internal/agent/tools/apply_patch.go:549`, `:648` | 0 extra | ✅ delivery point: same file (peer lands between the plan and the write); falsification ⇒ 1 red. The sandbox half was blind until 2026-09-22 (register row 47) |
| **B10** | the two postures, declared separately and **kept** separate: attachments (`agent/attachments.go`) = must-not-exist, and on a taken name **keep both** under `<stem> (n)<ext>` (the store names the file); skills publish (`skills/objectstore.go:105-115`) = read-modify-write, because it is an intentional re-publish | as listed | 0 | ✅ `attachments_store_posture_test.go` (create-only / counter bump / all taken ⇒ claim nothing) + `skills/objectstore_posture_test.go` (expectation follows `v0` → `v1`); falsifications ⇒ 3 red, 2 red, and 1 red respectively |
| **B11** | panel upload / delete (`setup/handlers_agents.go:1445/1512`): condition on the version the panel last listed (owner-visible UI), refuse + report on conflict | server: as listed; cloud panel: `src/features/chat/{upload-attachments,attachment-conflicts}.ts` + `components/owner/upload-conflict-dialog.tsx` | +1 HEAD per upload | ✅ server (`TestFileUpload_NameCollisionOffersTheCurrentVersionThenReplaces`) + ✅ panel delivery point (`attachment-conflict-flow.test.tsx`: attach → dialog → Continue/Replace ⇒ the request carries `{name, expectedVersion}` and the turn still leaves); 4 falsifications run for real |

**Not in this list, on purpose**: the sandbox→store reconcile (T1) and the
sandbox mirror's "observe once, then report" — see the boundary note above.

#### A3.2 Formal home and cross-reference

T1 turned the *sandbox→store* direction into a reconciler with preconditions
(`BLOCKED`: refuse, don't migrate). The *tool→store* direction has **no
precondition at all** — a plain `Put`. That is an F1 gap, recorded as **G24** in
`docs/文件系统形式化证明/10-harness-state-audit.md` §4 (and its English mirror),
pointing back here. A1 is the primary closure (there is no second writer);
A3.1's options are the belt.

### A4 — The client must not infer "the server is still running" from its own socket

**Cause.** The dashboard treats its local `streaming` flag as the server's turn
state: Stop aborts the fetch while the server keeps working
(`handleChatCancel` answers `already_started` for a running turn), the composer
immediately offers Send again, and unresolved rows are rendered as
`Interrupted — this call never returned a result` from `streaming !== true`.
What the client knows is "my connection ended"; what it claims is "the call
never returned" (all 57 calls in the 09-18 session had replies). Formal home:
**F3's take-side fact used as F2's world fact** — the consumer answering a
question only the producer can answer.

**Fix, in the gate's order.** Delete the assertion first: state what the view
knows ("no reply in this view yet; the turn may still be running"), which is the
same wording A2 adopts on the server. Then make Stop mean something: either
cancel server-side, or relabel it *detach* and say the turn continues. Then
replace inference with fact: carry the turn-active/lease state (A1) into the
subscription/history payload so the composer and the tool rows read the server's
state instead of guessing. Progress binding by `tool_call_id` (the earlier note)
is part of this item, not a separate one.

**Evidence / verification.** Component tests over three event sequences ⇒ three
labels, and "connection lost, turn alive" must not render *Interrupted*. E2E:
drop the SSE, send a second message, assert the UI says the turn is still
running / queued.

#### A4.1 What the wire must carry (server side, one field)

The client cannot be fixed by wording alone: it needs the fact. A1's lease row is
already that fact, so expose it read-only:

* `chat/subscribe` (and the session-history payload) gains `turnActive:
  {holder, epoch, expiresAt}` — present while a live lease exists, absent
  otherwise. One field, one reader (`internal/setup/handlers.go` subscription
  writer + the history handler).
* `queued`'s payload (`internal/agent/loop.go:2330-2336`) gains the holder and an
  ETA for honest wording ("waiting for the turn that holds this session" rather
  than a bare position).
* `subagent_progress` carries `id` (the `tool_call_id`) so progress is bound by
  identity instead of "the first unresolved `delegate_task`" —— the binding that
  made two concurrent turns look like one restarting. **Landed 2026-09-21** (register
  row 42): the call's id rides that call's own arguments out of the SDK bridge
  (`internal/agent/tools/toolcall.go`, `internal/agent/sdkbridge.go`), and the client
  draws a heartbeat only on the row it names (`message-list.tsx`, `heartbeatOwnerId`).
  Witnesses: `TestSubagentHeartbeatsNameTheirOwnCallE2E`, `TestToolCallIDReachesTheToolAndNotItsArguments`,
  and two per-row cases in `message-list-tool-status.test.tsx` — each falsified for real.

#### A4.2 The client edits (anchors are in `tokenaissance-cloud`)

> Paths in this table are relative to the cloud repository
> ([tokenaissance/tokenaissance-cloud](https://github.com/tokenaissance/tokenaissance-cloud), branch `develop`);
> prefix a path with `https://github.com/tokenaissance/tokenaissance-cloud/blob/develop/` to open the file.

| what | where | change |
|---|---|---|
| "Interrupted" assertion | **landed 2026-09-21** — the rule is `src/features/chat/turn-state.ts` (`toolPhase`, `toolRowLabelKey`), read by the row (`message-list.tsx:61`, `:120`, call site `:256-265`), the group header (`:217-219`) and the bundle (`:335-337`) | three states: **interrupted** only when the fact says nobody holds the session (or no fact + the bubble stopped); **unknown** when the fact is unusable; a peer-held turn says *running elsewhere*. The row previously re-derived this from `msg.streaming` alone and was never passed the fact — so the requirement was unmet while the old spec (which read only the *header*) stayed green; the anchors once cited here (`:182`/`:96`) are superseded |
| progress binding | **landed 2026-09-21** — `message-list.tsx` (`heartbeatOwnerId` in `ToolCallGroup`; the row keeps `tool_queued` for non-owners that have not exited) | the heartbeat names its call (`id`, `tools.ToolCallID` → register row 42) and the row it names owns it; with no id the old rule stands (the first unresolved call). A name matching no live row in the group draws nothing rather than a counter on another bubble's row  **Extended 2026-09-22**: a non-owner whose run has *exited* reads “finished”, not “queued” — the exit fact (`phase:"done"` + that call’s id) is kept in `subagentFinishedIds` (cloud `use-stream-pipeline.ts`: one applier, both connections; cleared at the turn’s end), and the positional fallback skips exited calls so it cannot hand the live label back. Witnesses: cloud `subagent-finished-ids.test.tsx` (4), `message-list-tool-status.test.tsx` (+3), `chat-streaming-parity.test.tsx` (+1); four falsifications run for real. |
| Send while a turn runs | **landed 2026-09-21 as a sentence, repaired 2026-09-26 as a control** — `chat-composer.tsx` (`showStop` / `showSend`), `turn-state.ts` (`composerHintKey`) | Send stays on screen and enabled while a **peer** holds the turn, so the sentence it prints there ("Enter interjects into it, Send queues behind it") names a control that exists: Send POSTs `chat/stream` and the server parks the turn (`queued`, `queued_chat_e2e_test.go`), which is then what the queued block's Edit/Cancel withdraw. **The control had been missing for five days**: the render was `showStop ? Stop : Send`, so a peer-held turn showed Stop alone and "queue a follow-up" had no entry point at all — measured in a real browser 2026-09-26 (peer-held → `["Stop generating"]`, `send=0`; idle → `["Send message"]`). Deliberately unchanged: while *this* tab streams, Stop keeps the slot alone — that state's sentence says what Enter does and nothing about Send. Witnesses (cloud): `src/features/chat/__tests__/owner/chat-composer-send-while-running.test.tsx` (4 cases, including the asymmetry) and `e2e/tests/fastagent/app-chat-turn-fact-composer.spec.ts` (Send → `chat/stream` with the steer route untouched; Enter → `chat/steer`). Falsification run: putting `showStop ? Stop : Send` back reddens 2 unit cases and the Send browser case, and leaves the Enter one green |
| Stop | **landed 2026-09-21** — `use-stream-pipeline.ts` (`handleStopTurn` = `handleStop` + `cancelRunningTurn(agentId, sessionId)`, no `turnId`), wired as `onStop` in `owner-view.tsx` | the first option shipped: the control really stops the turn server-side. The two entry points stay separate on purpose — `handleStop` alone is also used when switching sessions, and cancelling there would stop the **new** session turn. `already_started` is retired; the cancel contract reads its body (`{canceled, wasRunning, isRunning}`) |
| strings | **no reword, on purpose** — `src/config/locale/messages/en/fastagent/chat.json` (`tool_interrupted`, `tool_queued`) | this cell planned to reword `tool_interrupted`; the fix that landed makes the **rule** right instead (`toolRowLabelKey` says *interrupted* only when the fact proves it), so the sentence is true wherever it prints now. `tool_queued` keeps its words and sharpens its meaning: "not the row the heartbeat named" (row 42) |

The reference webui (`fastagent/web/src/components/chat-screen.tsx`) still makes
the same inference from its own `streaming` flag and has no hint of this kind
(checked 2026-09-21: `composerHintKey` has no counterpart there). The cloud client
is the one that landed the change; the webui did not follow, so this divergence is
recorded as **open**, not as deliberate — it talks to the same backend, so it
could take the same facts, and whether it does is a product call.

#### A4.3 The documentation that landed with it (cloud design 09)

[tokenaissance-cloud › docs/fastagent/design/09-delegate-task-design.md](https://github.com/tokenaissance/tokenaissance-cloud/blob/develop/docs/fastagent/design/09-delegate-task-design.md) defines
the panel's five states (queued / running / running-without-heartbeat /
interrupted / finished-awaiting-the-round). Its **turn-state** half is still **four values plus one orthogonal
property** (reviewed 2026-09-19: three independent axes — does a live holder
exist / is something of mine pending / is the fact usable — whose reachable
combinations are exactly `running` / `interrupted` / `unknown` / `idle`; the
panel's "running-without-heartbeat" is a label variant of `running`, and
"queued" is a property of the pending message, not a turn state). **Unknown
(stale view)** is the value that was missing — the
view cannot tell "the call died" from "I lost the connection" and must say so.

**Why four grew to five, stated so the implementer cannot collapse them back.**
The four as-built states are decided by facts the *view* happens to have
(`§5.5`, commits `8eb8ff93` and `b566bbe5`):

| as-built state | what decides it | where |
|---|---|---|
| finished (awaiting its own result) | the call’s **exit fact** has arrived (`subagent_progress` with `phase:"done"` and its id) but not its result — the exit and the result are two separate statements: the exit is emitted when the run returns, the result only when the `delegate_task` call itself returns | `message-list.tsx` (`tool_delegate_finished`; the ids come from cloud `use-stream-pipeline.ts` `subagentFinishedIds`) — landed 2026-09-22 |
| queued | not the owner **and no exit fact** — the heartbeat names the live `delegate_task`, and with no id the first unresolved one is it | `message-list.tsx` (`tool_queued` in `toolProgressLabel`; the owner rule lives in `ToolCallGroup`) — quoted by name, since the line numbers this table used had drifted |
| running | `active` + a heartbeat arrived | `message-list.tsx:51-61` |
| running (no heartbeat yet) | `active`, no heartbeat yet | the fallback at `message-list.tsx:63` |
| interrupted | `msg.streaming !== true` — **the bubble stopped streaming** | `message-list.tsx:49` (`turnOver`), passed as `:238`; message-level copies at `:96`, `:182`, `:289`; string `chat.json:137` |

> **Anchor note (2026-09-21).** The `message-list.tsx` line numbers in this table are
> the *pre-landing* revision; the four states are no longer re-derived in the
> component. They now come from `turn-state.ts` (`toolPhase` + `toolRowLabelKey`), and
> the header/bundle/row all read that one judgement — see the landed row in §A4.2.

The fourth row is a **transport inference, not a world fact**: the client knows
"my stream ended", and claims "this call never returned a result". On 09-18 the
stream ended because the view lost its connection while a *different replica*
was running the turn, and **every one of the 57 calls in that session had a
reply** in `session_messages`. So the fourth state is really two:

| the world (server's fact) | the view's knowledge | the state |
|---|---|---|
| a live holder exists | `turnActive` present | running / running-without-heartbeat / queued |
| no live holder, the call has no reply | `turnActive` absent *and* the turn is provably over | **interrupted** (now a provable claim) |
| — | no fact at all (stale view, subscription gap, pre-A1 history) | **unknown** — say "no reply in this view yet; the turn may still be running" |

The fifth state is therefore **epistemic, not a world state**: it exists because
the consumer was answering a question only the producer can answer (F3's
take-side fact used as F2's world fact). It is the same partition A2.1 applies
on the server (holder dead / holder alive / no fact), seen from the client; and
it is why A2 and A4 are one fix in two places rather than two features — the
09-18 model read the mirrored claim and re-issued the two sub-tasks.

Rule for the implementer: **a state may only be entered from a delivered fact,
never from the transport**; and each state must lead to a different action
(wait / wait-with-hint / re-issue / refresh), which is what keeps the count at
five instead of "always unknown".

> **Extended 2026-09-26 ([08 §10.9](./fs-formal-proof/08-state-observability-principle.md)).** The same rule
> governs *adding* a state, not only entering one: this vocabulary does not grow a task-level `cancelled`
> value. "Stop the whole task" is `stop_task(task)` — cancel every turn of it that has not completed — an
> operation over the turn set whose effect is already readable from the per-turn facts, so a stored
> task-level status would be a second source for a derived fact (O6), and it would paper over the input that
> is genuinely missing (the queued half is not store-visible yet: cloud `docs/mcp-task-submission.md` §14.6's
> second hole). Fix the input, not the vocabulary.

#### A4.4 Tests

Component tests over the three event sequences that decide the partition (turn
alive + connection dropped; turn gone; no fact) ⇒ the five labels of §A4.3;
"connection lost, turn alive" must not render *Interrupted*, and the no-fact
case must render *unknown* rather than either claim. E2E: drop the SSE mid-turn,
send a second message, assert the UI reads "still running / queued" and the row
does not claim "never returned".

**The composer half (landed 2026-09-21).** Same lesson, one layer out: the rule
was right and its delivery point was not. "While a turn runs, Enter interjects"
was delivered by a sentence chosen from the coarse `canStop` flag and rendered
only in the empty/hero composer — absent in the one variant where a turn can be
running — and the handler that performs it (`handleSteer`) bailed on this tab's
own stream flag. So a peer-held turn printed "Enter interjects" while Enter did
nothing at all, and the only control that worked was Send, which queues. Both
halves now read the fact through one rule each (`turnIsRunning`,
`composerHintKey`); steering itself is session-scoped on the server
(`POST /api/chat/steer` → `SteerWeb` → `PushSteerIfActive`, 200 for any caller
while a turn runs, 409 otherwise), which is why the client-side gate was the
defect rather than the transport. Witnesses: cloud
`chat-composer-fact-hint.test.tsx`, `steer-from-another-view.test.tsx`,
`turn-state.test.ts`, and the browser spec
`e2e/tests/fastagent/app-chat-turn-fact-composer.spec.ts` (peer-held fact ⇒ Stop
shown, the "elsewhere" sentence rendered, Enter reaching `/chat/steer`).

**Sequencing (all four, in order).** A1 is the root and the long pole. A2 and A4
are wording-first changes that can ship independently and immediately, and they
shrink the blast radius while A1 is being built. A3 lands last, and only if a
path exists that A1 does not already serialise.

| step | item | files | tests | falsification |
|---|---|---|---|---|
| 0 (done) | A1–A4 design | this document; G24 row in `docs/文件系统形式化证明/10-harness-state-audit.md` §4 | — | — |
| 1 | A2 step 1 — the pad stops asserting | `internal/provider/provider.go:48-59`, `internal/agent/normalize.go:21-90` | three shapes; "no fact ⇒ no *interrupted*" | restore `StoppedToolResult` for the alive-holder case |
| 2 | A4 steps 1–2 — the client stops asserting, Stop means something | cloud: `message-list.tsx`, `chat-composer.tsx`, `use-stream-pipeline.ts`, `chat.json`; cloud `docs/.../09-delegate-task-design.md` | three-sequences component tests | revert the wording; the stale-view case renders *Interrupted* again |
| 3 | **A1** — store lease + port + admission + fence | `internal/store` (DDL + `Acquire/Renew/ReleaseSessionLease`), `internal/agent/sessionlease.go` (port + `Turn`), `internal/agent/loop.go:2337` and `:3190` (the two `AcquireTurn` points), `internal/agent/admission.go:60-70` (**the IfIdle verdict moves onto the lease**, A1.4), the store's session write statements (`session_messages` + `sessions`) carrying the `EXISTS(session_turns …)` fence handed down by `internal/session/store_adapter.go:175`, `internal/agent/manager.go` (`WithSessionLease`), `internal/gateway` wiring | two-pool race; expiry hand-off; stale-token append refused (0 rows); `NopSessionLease` path; an automatic source on a peer ⇒ `ErrTurnNotAdmitted` **without** queueing; two-instance E2E | drop the store check ⇒ two concurrent `iteration=1` loops; drop the `EXISTS` ⇒ a taken-over turn keeps appending; leave `RunTurn` on the in-process gate ⇒ a cron turn queues behind a peer's turn instead of deferring |
| 4 | A2 step 2 + A4 steps 3–4 — the fact reaches the projection and the view | `internal/agent/normalize.go` (holder alive ⇒ *still running*), `internal/setup/handlers.go` (subscription/history `turnActive`), `internal/agent/loop.go` (`queued` payload, `subagent_progress.id`); cloud binding | UT + component + E2E | force a stale `turnActive` ⇒ the client must say *unknown*, not *interrupted* |
| 5 | A3 option 1 — detect and report the overwrite | file tools' write paths (`internal/agent/tools/file.go`, `apply_patch.go`), `internal/workspace` (no interface change) | UT: version moved between read and write ⇒ σ | drop the re-`Stat` ⇒ the loss is silent again |
| 6 | **G25** — the sandbox lease's token stops resetting (one clause, independent of A1) | `internal/store/sandbox_leases.go:70-76` (claim branch → `epoch = epoch + 1`) | UT: the token strictly increases across two takeovers; the existing insert-path assertions stay valid | restore `epoch = 1` ⇒ the new test goes red |

**No code before step 0 is complete**: design first, then implement in this
order — the order is the Musk gate's output (delete the second writer, simplify
the vocabulary, and only then add surface).
