# Per-chatter files: whose MEMORY.md is that?

> **Status**: fixed. Two halves: the missing-row read path in
> `internal/agent/tools` (a miss is an empty file) and the automatic-turn
> identity in `internal/agent/loop.go` (the turn writes the memory of the
> account it acts for).
> **Trigger**: operator report — `edit_file('MEMORY.md')` answered
> `system file get: store: not found` while the file clearly had content.
> **Second pass (2026-09-27)**: the operator reported that two sessions could
> read one `MEMORY.md` and that a cron job could be deleted across sessions.
> One of those was the design (see "Who counts as one chatter"); the other two
> were real — the Customize page's read fell back to the owner's row for anyone,
> and a scheduled tick acted for the owner's memory whatever chatter had
> created the job. Both are closed below.

## The rule

`USER.md` and `MEMORY.md` are **per-chatter**: every read/write is a strict
lookup of `agent_files(agent_id, user_id, filename)` —
`GetAgentFileExact`, no owner overlay, because a public-link visitor must never
inherit the owner's accumulated memory. Everything else in the identity set
(`SOUL.md`, `IDENTITY.md`, `AGENTS.md`, `BOOTSTRAP.md`, `TOOLS.md`,
`HEARTBEAT.md`) is the agent's shared template and *does* fall back to the
owner's row.

So "which file is this?" is answered by *who the turn is* — and "who" is one
question with a precise answer, which the second pass had to write down because
two different axes were being read as one.

## Who counts as one chatter

The key is `(agent_id, chatter_user_id, filename)`, and `chatter_user_id` is the
`u_xxx` account the turn speaks for. A **session is not a chatter**: sessions are
keyed `(channel, accountID, chatID, projectID)` (`bus.InboundMessage.SessionTriple`).
The two axes differ in both directions — one person's many sessions share one
memory file, and one group session holds one file per participant.

| Inbound | `chatter_user_id` is | Consequence |
|---|---|---|
| web / api | the authenticated account itself (`handlers.go handleChatStream` passes `s.effectiveUserID(r)` straight to the agent — this path never goes through the gateway's chatter minting) | the same person in any browser, on any machine, is **one** file; every chat they open reads it |
| IM channel | a lazily minted `channel_user` for `(channel, platform user id)` — `resolveChatter` builds `extID = channel + ":" + msg.UserID` (no accountID, so a reconnected bot does not split an identity) and `EnsureChatter` mints it | the same person on telegram and on feishu is **two** files |
| channel with `shared_identity` | the channel owner (`routing.go` sets `msg.UserID = ownerID` before the turn starts) | every channel collapses into one file |
| cron / heartbeat / goal / webhook | resolved by `Agent.chatterUserID` from the sentinel: cron uses the job's **creator**, else the owner; heartbeat uses the agent owner | the tick reads and writes the file of the account it acts for, never "whoever happens to own the agent" |

Two readings that have been filed as bugs, and the answer to each:

* **"Two sessions read the same MEMORY.md."** Same `u_xxx`, same agent, several
  chats — that is this design. *(Per-session memory would be a different
  product: the key would gain the session, and a person would stop carrying
  what they told the agent in the previous chat.)*
* **"Another person's session read my MEMORY.md."** That was a bug, twice:
  `handleGetAgentSystemFile` handed the owner's row to any caller with read
  access, and `list_cron_jobs` / `delete_cron_job` / the environment-change
  signal treated the agent — not the chatter — as the owner of a scheduled job.

## Failure 1 — a missing row was a hard error

`read_file` always resolved DB → agent home on disk → empty. `edit_file`
resolved DB only and turned `store.ErrNotFound` into
`fmt.Errorf("system file get: %w", err)`, handing the model a storage-internal
string. A chatter who had never saved MEMORY.md — i.e. everyone, on their first
write — could not edit it, while `read_file` on the same path answered happily.

Both paths (host and sandbox, `edit_file` / `read_file` / `apply_patch`) now go
through `Registry.readSystemFileWithFallback`: **a missing row is an empty
base**, not an error. The edit then either applies or fails with an actionable
`old_string` miss that names the file.

## Failure 2 — automatic turns were not the owner

Cron/heartbeat/goal/webhook turns stamp a sentinel `UserID` (`"cron"`,
`"system"`, `"goal"`, `"webhook"`) instead of an account. The gateway mints an
app_user for any non-`u_` id (`web:cron` → `u_cd824…`), so per-chatter state
keyed on it lands in a row no conversation ever reads — and, before the fix
above, made every `edit_file('MEMORY.md')` fail.

Production repro (2026-09-14, `agt_cda27bb…`, session *Kronos Kline Forecast
Model*): the agent's own `*/5` cron re-entered its session as `UserID="cron"`.
In that one session the operator's turn returned
`Edited MEMORY.md (1 replacement(s))`; the cron turn returned
`system file get: store: not found` four times, and `read_file('MEMORY.md')`
answered with an empty string. The row itself existed all along — under the
operator's `u_396a1f88…`, 18 kB, containing the very `old_string` the model was
patching.

`Agent.chatterUserID` now routes those turns to the account they act for:

| Source | Identity used for per-chatter state |
|---|---|
| real user (web / IM chatter) | the chatter's `u_xxx`, unchanged |
| cron | the job's **creator** (`cron_jobs.creator_user_id`, stamped by `create_cron_job` from the registry's per-turn chatter), else the owner — see below |
| goal continuation | the goal's `owner_user_id`, else the agent owner |
| heartbeat | the agent owner (the tick carries no owner field) |
| webhook | the token/body `userId`, else the agent owner |
| sub-agent | **not** listed — it inherits the parent turn's chatter |

Without this half the write "succeeds" into the synthetic cron user's private
row, which is why both halves ship together.

**The cron row changed on 2026-09-27, and the reason is a second axis.** The
2026-09-14 fix above stopped sentinel turns from stranding writes in a synthetic
row; it answered "cron acts for the owner", which is right for *routing* (the
UserSpace, the channel, the session) and wrong for *per-chatter state*. On a
public agent, a visitor who schedules "5 分钟后提醒我" writes a job whose
`message` is instructions to the model; the tick ran as the owner, so
"把这条记进 MEMORY.md" edited the owner's memory — the model was doing exactly
what it was told, to the wrong person. A job therefore now records the account
that created it, and this table's cron row reads the creator:

* `cron_jobs.creator_user_id` — written by `create_cron_job` from
  `Registry.ChatterUserID()`, back-filled from `user_id` for rows that predate
  the column (those were created in the owner's turn, because that was the only
  identity the tool had). `owner_user_id` / `user_id` keep their routing meaning.
* `InboundMessage.CreatorUserID` carries it from `cronStoreAdapter.GetDueCronJobs`
  → `Scheduler.processDueJobs` → the loop's `autonomousActorUserID`, which prefers
  it over `OwnerUserID`. The static `fastagent.json` job list has no creator and
  keeps acting for the owner.
* The same field decides **visibility**, not just memory: `list_cron_jobs` and
  `delete_cron_job` answer only for the chatter's own jobs (a foreign id reads as
  "no such job" rather than an authorization error, so it does not confirm the id
  exists), and the environment-change signal's `cronFingerprints` samples the
  same way — otherwise one chatter's reminder names arrive in another chatter's
  context as "scheduled jobs added". The agent owner and the channel admin keep
  the agent-wide view, which is what the dashboard (`handlers_cron.go`,
  `requireAgentOwner`) already had.

## Who may read the agent home on disk

`systemRoot/MEMORY.md` is **one un-scoped mirror per agent** — whoever wrote
last owns its bytes, and in practice that is the owner's private memory. The
disk fallback is therefore restricted the same way `ContextBuilder.loadFileForUser`
and `memory_store_adapter.GetMemory` already restrict it:

* shared identity files — readable by any caller allowed to touch them (the
  overlay hands chatters the owner's copy anyway);
* per-chatter files — only the account that owns the agent home. A visitor
  whose row does not exist yet sees an empty profile/memory, never the owner's.

## Tests

Unit (port boundary, fake `SystemFileStore`):

| Test | Locks |
|---|---|
| `TestEditFileMissingMemoryRowDoesNotLeakStoreError` | no raw `store: not found`; the error names the file |
| `TestEditFileOwnerUsesDiskBaseWhenRowIsMissing` | disk-only MEMORY.md edits from the disk base and saves to the row |
| `TestPerChatterFileNeverInheritsTheOwnersDiskCopy` | visitor read/edit cannot see or write the owner's mirror |
| `TestOwnerReadsDiskCopyWhenRowIsMissing` | the owner still reads the legacy/mirror disk copy |
| `TestSharedIdentityFileFallsBackToDiskForNonOwnerCaller` | SOUL.md etc. keep the inherited template path |
| `TestChatterUserID_AutonomousTurnsActAsOwner` | the source→identity table above, including sub-agent pass-through |

Cloud-path e2e (real `DBStore` + `MemoryStoreAdapter`, real file tools):

| Test | Locks |
|---|---|
| `TestAutonomousTurnMemoryRouting_CloudPathE2E` | cron (gateway-minted `u_cd824…` **and** raw sentinel) and heartbeat turns edit the operator's row; no stranded synthetic row |
| `TestVisitorTurnMemoryStaysPrivate_CloudPathE2E` | an IM visitor reads empty, keeps their own row, and the owner's row/mirror stay untouched |

Second pass (2026-09-27), committed on branch `fastagent` — the two bugs the
operator called unacceptable, plus the reader they did not know about:

| Test | Locks |
|---|---|
| `internal/setup` `TestSystemFilesReadANonOwnerGetsAnEmptyMemoryNotTheOwners`, `TestSystemFilesReadANonOwnersOverrideDoesNotCarryTheOwnersBase` (new, `c87e689` red → `3d7e7fd` fix) | a non-owner's Customize read gets their own row or an empty default — never the owner's `content`/`baseContent`; the owner's own read is unchanged |
| `internal/agent/tools` `TestCronJobsAreScopedToTheirCreator` (`a556276` red → `8c213e0` fix) | list and delete are scoped to the creator; the owner still sees the whole agent; a foreign id cannot be cancelled |
| `TestChatterUserID_AutonomousTurnsActAsOwner` / `cron job acts for the chatter who created it` (`71dea7e` red → `f6a58ed` fix) | the source→identity table's cron row, including the creator-over-owner preference |
| `TestCronTurnWritesTheCreatorsMemory_CloudPathE2E` (new, same red) | a real tick's `write_file('MEMORY.md')` leaves the owner's row byte-identical and lands in the visitor's |
| `internal/gateway` `TestCronStoreAdapterProjectsCreator` + `internal/cron` account-route e2e (`8799df8`) | both hops that carry the creator are asserted against real rows; dropping the field reddens the projection test |
| `internal/agent` `TestCronFingerprintsAreScopedToTheChatter` (`3f48c34`) | the environment signal never names another chatter's job; the owner keeps the full sample |
