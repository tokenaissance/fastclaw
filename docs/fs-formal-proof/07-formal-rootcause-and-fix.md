# 07 · The root cause in formal terms: statement, proof, and fix

> Status: root cause closed (including evidence read directly out of a live sandbox) · last verified: 2026-09-17
> The notation follows the Cordis formal language of [mcp-oauth-design.md §13](../mcp-oauth-design.md)
> (revertible effect, left inverse, preconditions, keyed diff, system boundary). This document only
> states things in that language; it introduces no new framework.
> Prerequisites: [01](./01-current-implementation.md) · [the incident chain in 04](./04-incident-workspace-2026-09-17.md) · [06, the first re-review against six criteria](./06-cordis-review.md)
>
> **Position**: this document is the authoritative definition of **F1 (preconditions / zero migration)**
> among the document set's **three formal systems** — it answers exactly one question: **may this
> migration happen at all** (if not ⇒ error + zero migration). The other two are **F2, observability**
> (which changes must be stated) and **F3, delivery** (how a statement actually arrives), both defined in
> [08 §2 / §2.2](./08-state-observability-principle.md); the division of labour and the index are in
> [00-formal-systems.md](./00-formal-systems.md).

---

## Part 1 · Confirming the root cause (empirical, not inferred)

### 1.1 Closing the loop with evidence

For the incident session `OvDLzEPKcEK84Hpfq0U6LK` (agent `agt_cda27bbfbf4a84e2dfa6`), we located the
live sandbox `iydyr4nz1rt1kxq57a1mh` through its row in `sandbox_leases`, obtained a token via E2B's
`POST /sandboxes/{id}/connect`, and read `/workspace` directly:

```
GET /workspace/byo-account-design.html : status=200 bytes=41262
GET /workspace/qc-email-draft.md       : status=200 bytes=5705
GET /workspace/todo.md                 : status=200 bytes=580
GET /workspace/sessions/OvDLzEPKcEK84Hpfq0U6LK/byo-account-design.html : status=404
```

The three files inside the sandbox are **still the old versions**, and their byte counts match the
store's exactly (S3: 41,262 / 5,705 / 580, `LastModified` 12:15:16–17); the fourth request's 404 also
rules out "there is another copy under `sessions/<sid>/…`". (The sandbox was paused again afterwards.)

**Byte-level re-check (done in the second self-audit on 2026-09-17)**: inside the sandbox,
`md5sum /workspace/byo-account-design.html = 305a43137bf54d18235ef50647579413`, which **matches** the
S3 object's ETag `"305a43137bf54d18235ef50647579413"` exactly — the object in the store *is* the
sandbox's copy, not "another version of the same length".

The `/workspace` metadata read during the same pass (`stat`):

```
birth  2026-09-17 07:56:53.453830420 +0000   byo-account-design.html   41262
birth  2026-09-17 07:56:53.453830420 +0000   qc-email-draft.md          5705
birth  2026-09-17 07:56:53.457830476 +0000   todo.md                     580
mtime  2026-09-17 07:41:04                   byo-account-design.html
mtime  2026-09-17 07:39:44                   qc-email-draft.md
mtime  2026-09-17 07:41:29                   todo.md
```

Every `birth` equals sandbox B's hydrate moment, 07:56:53 ([04](./04-incident-workspace-2026-09-17.md) §2.2),
while `mtime` preserves the store's `LastModified` from that time (compare `qc-lean-mcp/README.md`:
`05:30:21` in the sandbox and `05:30:21` in S3). This metadata yields two conclusions at once:

1. **B never received a host-tool write after it was born** — if a mirror had taken effect these files
   would show a new mtime or a new copy; in fact the sandbox contains **no** copy of 55,527 / 6,893 /
   462 bytes (and no leftover mirror paths in that directory either).
2. **mtime cannot be used as a cross-copy criterion**: hydrate writes the store's `LastModified` into
   the sandbox file's mtime, so on both sides "new content" and "old content" look like the same write.
   This independently rules out the "arbitrate by file timestamp" design (consistent with the ADR in
   [05](./05-remediation-plan.md) §8).
   > **Added 2026-09-18 (G22)**: that is exactly why the **write-through must stamp too** (write the
   > store's mtime onto the copy it mirrors); otherwise the two copies' mtimes differ by construction and
   > every sync has to read the whole object to prove "same". In project sessions that stamp silently
   > stopped landing (it read the store with the sandbox scope while the tools write the project root) —
   > fixed, see [10 §4 G22](./10-harness-state-audit.md); measured on real E2B: whole-object reads for
   > that path **1 → 0**.

### 1.2 The event chain (every line backed by a log or an object timestamp)

| Time (UTC) | Event | Source |
|-----------|-------|--------|
| 05:25:33 | sandbox **A** `iknoadnp4ho8un3yhmjr2` is created, `workspaceFiles=1` | `e2b sandbox hydrated` |
| 05:25:50 – 07:30 | all of A's snapshot syncs **fail** (`over the 32.0 MB cap`) | `sandbox sync: snapshot failed` ×42 |
| 06:26:14 / 07:38:05 / 07:52:58 | A is already dead (`extend timeout … 404`) | `could not extend the sandbox timeout` |
| 07:56:44 | A expires, the rebuild starts | `e2b sandbox expired, recreating` |
| 07:56:53 | sandbox **B** `iydyr4nz1rt1kxq57a1mh` hydrates, **`workspaceFiles=10`** | `e2b sandbox hydrated` |
| 07:59:21 | B is adopted by another pod (its local cache was stale) | `e2b sandbox adopted (local cache stale)` |
| 08:04–08:19 | the agent rewrites the three files with `edit_file`/`apply_patch` (**store only**) | `session_messages` seq 256–297 |
| 08:09:47 | **A's evict sync** pushes 2 files → first revert | `synced … cause=evict files=2` |
| 08:15:26 | the agent notices the revert for the first time | seq 280 |
| 08:15:44 / 12:07:00 | the agent repairs twice (writes back 6,893 / 55,527 / 462) | seq 283 / 318–320 |
| 08:24:18 | **B's evict sync** pushes 3 files → second revert | `synced … cause=evict files=3` |
| 12:05:26 | B's post-exec sync pushes 1 file | `synced … cause=post-exec files=1` |
| 12:15:17 | **B's evict sync** pushes 3 files → final state = B's 07:56 content | `synced … cause=evict files=3` |

The critical extra fact: B was created at 07:56:53, and the three files were first written by the host
at 08:04 — **B held the wrong version from the moment it was born**, and never received an update
during the remaining 4.5 hours of the session.

### 1.3 Conclusion (nothing changed by it)

> The root cause is `syncSnapshot`'s criterion: **it treats the sandbox copy as authoritative and
> infers "the sandbox has newer content" from `|X[p]| ≠ |S[p]|`, while the only premise that could
> support that inference — "content at this path in the store can only have come from the sandbox" —
> does not hold once host file tools exist.**

Two amplifiers (not introducers): during A's lifetime the snapshot kept failing on the size cap
(which hid this path), and B lived long because a lease can be adopted across pods (so the stale copy
survived 4.5 hours and 5 syncs).

---

## Part 2 · Formalisation

### 2.1 Domain

Take one scope `σ` (here `σ = (agt_cda27bbfbf4a84e2dfa6, "", OvDLzEPKcEK84Hpfq0U6LK)`, i.e. a loose
session with no project).

```
K          the set of keys: every path in the workspace; here we care about
             P = {byo-account-design.html, qc-email-draft.md, todo.md}
Γ          the digest domain: a content digest that can be tested for equality (size, or size+hash)
Δ          the full digest set, including ⊥ (does not exist)

S : K ⇀ Γ  the store's current content (the authoritative copy)
X : K ⇀ Γ  the sandbox /workspace's current content (the cached copy)
B : K ⇀ Γ  the baseline: the content the last time the two sides were observed to be equivalent
           (does not exist in the current implementation)
```

Actions and their effects:

```
HostWrite(p,v)     S[p] := v            — a host file tool; in this implementation it does not touch X
SandboxWrite(p,v)  X[p] := v            — exec writing inside the sandbox
Hydrate            X := S|dom(S)        — the one-off copy at sandbox birth
Sync(cause)        — to be defined: this is exactly where the current implementation and the fix differ
```

System boundary (§13.1 principle 6):

```
inside   S, X, Hydrate, Sync — the system may modify these exclusively and restore them
outside  Read(t): the snapshot of (S,X) an external observer (user, the agent's later inference,
         another pod) reads at time t
         Emission: any action triggered by Read(t) that the system cannot take back
```

### 2.2 The proposition to prove

> **Proposition D (Defect)**: the current `Sync_cur` does not satisfy "a false precondition ⇒ zero
> migration", so whenever `dom(S) ∩ dom(X) ≠ ∅` and the two sides have been updated by different
> writers, there exists an execution in which `S` loses content that **has already been observed**.

### 2.3 `Sync_cur`: definition and defect

The current implementation ([lifecycle.go](../../internal/sandbox/lifecycle.go), line 461)
does this per key:

```
Sync_cur(p):
    if S[p] = ⊥            then  S[p] := X[p]        (T1)
    elif |S[p]| = |X[p]|   then  skip                (T2)
    else                        S[p] := X[p]         (T3)
```

Analysis:

| Branch | Semantics | Correct? |
|--------|-----------|----------|
| T1 | a sandbox artefact is persisted | ✅ correct: `p ∉ dom(S)` is sufficient for "the sandbox produced this" |
| T2 | equal size means unchanged | ⚠️ holds only with a single writer; different content of equal length is missed |
| T3 | sizes differ ⇒ overwrite S with X | ❌ **no precondition**. Either side may be the newer one, and the implementation picks X unconditionally |

**The defect, stated formally**: `Sync_cur` performs no precondition check at all — it infers the
direction of intent from a difference in state. By §13.1 principle 5, `set(k,v)` requires `k ∉ dom`;
when violated it must fail and migrate nothing. T3 is equivalent to "silently overwrite an existing
key", i.e. **performing a migration with no precondition satisfied whatsoever**.

### 2.4 Proof (constructed on this incident)

The timeline below maps line by line onto the evidence in §1.2. Let `p₀ = byo-account-design.html`.

```
t₀ = 07:56:53   Hydrate:      X := S|dom(S)               ⇒ X[p₀] = 41,262
t₁ = 08:04–08:19 HostWrite:   S[p₀] := 55,527 (via intermediates) ⇒ X[p₀] = 41,262 (not updated)
t₂ = 08:24:18   Sync_cur(evict):
                  |S[p₀]| = 55,527 ≠ |X[p₀]| = 41,262
                  ⇒ takes T3 ⇒ S[p₀] := 41,262            ⇒ X = S, and S's 55,527 is gone
```

Therefore:

1. **Whenever `X[p₀] ≠ S[p₀]`, T3 cannot tell two worlds apart**: `W₁` (the sandbox edited, the store
   did not) and `W₂` (the store was edited, the sandbox did not) produce **the same observation**
   `(|S[p₀]|, |X[p₀]|)`. Choosing X is right in `W₁` and wrong in `W₂`; this incident was `W₂`
   (witnessed by the host writes at seq 259–264, 297, 318). ∎(D)

2. **That execution contains an outside violation**: `Read(t)` after `t₂` sees 41,262, while the agent
   had already been reasoning on 55,527 / 6,893 / 462 after `t₁` (its report at seq 323, the user's
   decision at seq 324). Once content has been observed it cannot be taken back ⇒ this is an emission,
   outside the reversible range of `inside`.

3. **`Sync_cur` is not a reconcile (violates §13.1 principle 7)**: `Sync_cur² ≠ Sync_cur` in general —
   every run overwrites `S` with the then-current `X`, so if a third write lands in between, running
   it again changes `S` again. It is neither idempotent nor convergent, so it cannot be called
   declarative convergence.

### 2.5 Why D is unreachable on docker (a corollary of the same theorem)

On the docker backend, `X` is a "bind view of the host directory", i.e. `X ≡ S` always holds
(the docker row of [01](./01-current-implementation.md) §2; `SnapshotWorkspace` walks the host
directory directly). So for any `p` at any time: `|S[p]| = |X[p]|` is vacuously true ⇒ `Sync_cur` always
takes T2 and T3 is unreachable. **D needs a domain in which X and S can be separated, and docker does
not provide one.**

This also gives the precise version of "why did earlier versions not expose it": the defect (T3 with no
precondition) has existed since 2026-04-20, but E2B's `SnapshotWorkspace` only made `X ≠ S` possible on
2026-05-01 (see [04](./04-incident-workspace-2026-09-17.md) §6).

---

## Part 3 · The fix

### 3.1 In kind: Sync is not an effect, it is a reconciler

By §13.1, `effect = Γ → Γ×(Γ→Γ)` applies to **intentional** actions (add/remove/login…). `Sync` has no
intended direction; it is a **declarative convergence** (a keyed diff). The right requirements for a
reconciler are:

> **R1 (precondition)** every key's migration happens only when an explicit precondition holds;
> **R2 (zero migration)** when a precondition fails, no key changes, and an observable failure is produced;
> **R3 (convergence)** `Reconcile` is idempotent: `Reconcile ∘ Reconcile = Reconcile`;
> **R4 (locality)** one reconcile does not change `dom` (it neither adds nor removes neighbouring keys).

### 3.2 Introducing the baseline B: turning "who changed it" into a decidable fact (**retired**, see §3.11.3)

> This section, the decision table in §3.3, the digest conclusion in §3.3.1, §3.8 and §3.10 all
> describe **the same intermediate shape**: a scope-level (later in-process) baseline `B` recording
> "what the sandbox was last handed". On 2026-09-18 that shape was **retired wholesale** (cross-replica
> arbitration plus in-process memory is not a guarantee, see §3.11.3). It is kept here as a record of
> the evolution: it explains why it was needed at first, and why it was not enough.

`B` is a scope-level durable document, one row per key (reusing `configs_kv`, `kind = ws_baseline`,
isomorphic to `mcp_undo`; see [mcp-oauth-design.md §13.2/§13.7](../mcp-oauth-design.md)):

```
Hydrate:      ∀p∈dom(S):  B[p] := S[p]        (written once, produced on the spot by hydrate)
Reconcile(p):  B[p] is updated only when a migration happens
Release(X):   B survives (scope-level, does not die with the instance)
```

`B` separates the "two indistinguishable worlds" of §2.4:

```
W₁ the sandbox edited, the store did not  ⟺  X[p] ≠ B[p]  ∧  S[p] = B[p]
W₂ the store edited, the sandbox did not  ⟺  S[p] ≠ B[p]  ∧  X[p] = B[p]
W₃ both sides moved (a real conflict)     ⟺  S[p] ≠ B[p]  ∧  X[p] ≠ B[p]
```

### 3.3 Write-through + preconditions (landed 2026-09-17; the decision-table part is superseded by §3.11.3)

Before the criteria of §3.2/§3.3, add **one action** that removes most conflicts at their source:

> **When a host tool writes a file, write the content into that scope's sandbox at the same time**
> (`LifecyclePool.WriteThrough`, called **per path** by `write_file` / `edit_file` / `apply_patch`).

The reason is that sentence from the formalisation: the incident's third necessary condition ("the host
wrote and the two copies differ") can **never** hold on docker — the bind mount makes `X ≡ S`.
Write-through transplants that property onto remote sandboxes: at the moment of the write the two
copies agree, instead of arbitrating afterwards.

Write-through proceeds **per path** (one file per call), so failures are per path too: when a path's
mirror fails, only that path is affected and every other path proceeds. **There is no "all or nothing"** —
when `apply_patch` touches several files, one failure must not strand the other files' exec artefacts.

On top of write-through the sync's decision table became (`decideReconcile`, five branches):

> **Retired (2026-09-18)**: two of this table's three criteria columns (the baseline digest and the
> stale bookkeeping) no longer exist; the current table is §3.11.3. It is kept here to show what the
> trade bought at the time.

| store | sandbox vs baseline digest | Verdict | Action |
|-------|---------------------------|---------|--------|
| no such path | — | a sandbox artefact | **push** |
| exists | this path's mirror failed (stale) | the sandbox copy is older | **refuse** + warn |
| exists | equal to the baseline digest | the sandbox did not move | skip |
| exists | different, and the store still equals the baseline | **the sandbox's own edit** | **push** |
| exists | different, and the store has moved off the baseline | both sides moved = the incident shape | **refuse** + warn |

### 3.3.1 Why a digest was still needed after write-through (the conclusion at the time; the digest was retired on 2026-09-18)

> **Postscript (2026-09-18)**: the conclusion below holds for the "in-process / durable baseline"
> design, but it depends on a **remembered** fingerprint. The final shape replaces the fingerprint
> with a **self-describing timestamp**: right after a write-through, `touch -d @<store mtime>` aligns
> the sandbox file's timestamp with the store object's (hydrate does the same, E2B's tar entries carry
> `obj.ModTime`), so "does the sandbox still hold what it was handed?" needs no memory at all — at the
> cost that **a sandbox edit of an existing path is no longer written back**, see §3.11.3.

**Note the timing**: the second line below is not the final state — the sandbox's content reaches the
store in the **write-back after the next `exec`**, provided the store object still equals the baseline
at that moment (row 4 of §3.3). The full loop is in §3.10.

My original inference was that "write-through lets the digest retire"; implementing it **was refuted
by a test**, recorded here as it happened:

```
the host writes notes.md (write-through succeeds) → sandbox = store = "HOST"
the sandbox then edits it                          → sandbox = "HOST+EDIT", store is still "HOST"
```

At this point the store object **equals the baseline** (no further host write), while the sandbox
content has changed — which looks **exactly the same** in metadata as "the sandbox did not move".
To separate "the sandbox edited it" from "nothing happened", the only available fact is the
fingerprint of "the content last handed to the sandbox". So the digest is **load-bearing** and
write-through cannot remove it; what write-through really removes is the **over-broad rule "the host
wrote ⇒ always refuse"** (which had made sandbox-side changes impossible to write back).

The division of labour is therefore clear:

| Mechanism | What it eliminates |
|-----------|--------------------|
| write-through | the fork caused by a host write (the incident's shape) |
| the digest baseline | the ambiguity after write-through between "the sandbox edited it" and "nothing moved" |
| ~~`staleWrites`~~ | **retired** (removed with the baseline on 2026-09-18): nothing remembers "which path's mirror failed" any more — the two copies disagreeing *is* that state, and the criterion reads it directly |

---

### 3.3.2 Earlier versions (kept as a record of the evolution)

```
Reconcile(p):
  (1) S[p] = ⊥  ∧  X[p] ≠ ⊥
        precondition: p ∉ dom(S)                     [a sandbox artefact]
        migration: S[p] := X[p] ; B[p] := X[p]

  (2) X[p] = B[p]  ∧  S[p] = B[p]
        migration: none (the two sides agree)

  (3) X[p] ≠ B[p]  ∧  S[p] = B[p]
        precondition: X moved off the baseline ∧ S did not   [a sandbox edit]
        migration: S[p] := X[p] ; B[p] := X[p]

  (4) S[p] ≠ B[p]  ∧  X[p] = B[p]
        precondition: S moved off the baseline ∧ X did not   [a host edit]
        migration: X[p] := S[p] ; B[p] := S[p]        ← pull, rather than skip

  (5) S[p] ≠ B[p]  ∧  X[p] ≠ B[p]  ∧  S[p] ≠ X[p]
        precondition: does not hold (a real conflict)          [see 3.4]
        migration: none ; set B[p] := ⟨CONFLICT, X[p], S[p]⟩ ; raise an error

  (6) X[p] = ⊥  ∧  S[p] ≠ B[p]
        precondition: does not hold (a sandbox-side deletion; the inverse needs a snapshot of the deleted content)
        migration: none ; raise an error

  (7) B has no such key (a legacy scope)
        migration: perform (1) only; leave everything else alone (conservative degradation)
```

Compared with the original implementation: **T2 was split into (2)(3)(4)**, where (4) is a new
direction (sync a host edit into the sandbox instead of skipping); **T3 was replaced by (5)(6):
error + zero migration.**

### 3.4 A convergent exit for conflicts (keeping R3/R4)

A violated precondition must migrate nothing, but **zero migration means the same conflict triggers
again on the next Reconcile**. By R3/R4 the only exit is to change the declarative document `B`, not to
add files:

```
first time:  B[p] := ⟨CONFLICT, X[p], S[p]⟩ ; error (fail loud, into tool_result / session trace)
second time: B[p] = ⟨CONFLICT, …⟩ is read ⇒ converge by the declared policy (default: S is authoritative)
             X[p] := S[p] ; B[p] := S[p]
```

Compare the `shadow` approach I proposed earlier in [06](./06-cordis-review.md): it added a key
(violating R4) and produced different content on each run (violating R3), so this document formally
retires it.

### 3.5 Boundary statement (principle 6)

| Position | Class | Formal reason |
|----------|-------|---------------|
| writes to `S` / writes to `X` / Hydrate / Reconcile | inside | the system modifies these exclusively and can restore the last observed-equivalent state |
| an `S[p]` the user has already read in the file panel or a download | outside | `Read(t)` has happened and cannot be taken back |
| any later write the agent made based on what it read | outside | as above; only the agent can redo it (compensation) |
| another pod's writes while a lease is being adopted | outside | the adopter cannot roll back the other pod's actions |
| a sandbox-side deletion (rule 6) | inside but currently irreversible | the inverse needs a snapshot of the deleted content; without one, raise an error |

**Corollary**: to avoid the incident's class of outside violation ("the agent has already reasoned on
it"), the protection has to happen *before* `Read` — i.e. converge immediately after the write (the
"pull" of rule 4), rather than waiting for eviction to reconcile. That is the necessity argument for
"post-exec sync + timely reconcile".

### 3.6 At the level of file actions (principles 1–3, currently entirely missing)

By §13.1 principle 1, a file write should also return `(new state, inverse)`, with the inverse produced
at the application site. This repo already has an isomorphic example: the `<mcp-undo>` marker
([mcp_undo.go](../../internal/agent/mcp_undo.go)).

| action | precondition | inverse (produced on the spot) | witness |
|--------|--------------|-------------------------------|---------|
| `edit_file` | `old_string` matches uniquely (**already**) | the reverse replacement | `digest` returns to its previous value |
| `write_file` / `apply_patch` overwriting an existing key | none today | the old content (or a reference to it) returned with the tool_result | `S[p] = B[p]` after the write-back |
| `write_file` creating a key | `p ∉ dom(S)` (guaranteed by T1) | delete the key | the key does not exist |

Among these, `edit_file`'s `old_string` match is the only place in this system that already satisfies
principle 5, and it can serve as the template for "a precondition *is* the error semantics".

### 3.7 Acceptance (each line is a provable proposition)

| Proposition | How to check |
|-------------|--------------|
| R2 zero migration | construct states (5)(6), assert `S`, `X` and `dom` are unchanged and an error is returned |
| R3 convergence | call `Reconcile` twice on the same state, assert the second is the identity |
| R4 locality | assert `dom(S) ∪ dom(X)` is unchanged across a reconcile |
| correct direction | construct the `W₁`/`W₂`/`W₃` states, assert `W₁→push`, `W₂→pull`, `W₃→error` |
| boundary statement | assert that after `Read` the "pull" of (4) has completed (there is no "the user read the new version while the sandbox still has the old") |
| left-inverse witness | for each file action assert `g(δ) = γ` (undo after the write returns to the original digest) |

**Landing status (2026-09-17)**: [internal/sandbox/lifecycle_sync_contract_test.go](../../internal/sandbox/lifecycle_sync_contract_test.go)
holds 10 executable tests, **all passing**, each mapping onto one proposition above:

| Test | Proposition | Assertion |
|------|-------------|-----------|
| `StoreEditIsNotOverwritten` | correct direction (`W₂`) | a key the host wrote **must not** be overwritten by the sandbox, with zero migration |
| `StoreEditSurvivesPostExecTrigger` | the same × post-exec | even two consecutive reconciles must not revert it |
| `NewPathIsFlushed` | correct direction (`W₁`) | a sandbox artefact must be written back |
| `WriteThroughKeepsSandboxEditsPushable` | write-through + digest | after write-through, a further sandbox edit of the same file → must be written back (the digest's only purpose) |
| `StaleMirrorBlocksOnlyThatPath` | per-path failure semantics | one path's mirror failing locks only that path; unrelated artefacts are written back as usual |
| `FailedMirrorIsRecordedNotFatal` | failure is not fatal | a mirror failure is reported to the caller and recorded, the store is unaffected |
| `SecondReconcileWritesNothing` | R3 convergence | a second reconcile writes nothing |
| `DomainUnchanged` | R4 locality | `dom` is unchanged |
| `StoreOnlyKeySurvives` | §5.1 | a store-only key is never touched |
| `UnknownBaselineBlocksOverwrite` | conservative degradation | with no baseline, overwriting may not be authorized |

### 3.8 The criterion for rule (3): a content digest taken at hydrate time (phase 2; **retired**, see §3.11.3)

The first version of the fix carried only "store-side metadata", so it blocked `W₂` (a host edit) and
**also blocked rule (3)** (a sandbox edit of a hydrated path) — with store metadata alone the two
traces are indistinguishable:

```
W₂  a host edit    ⟺ the store object no longer equals the baseline (the host wrote it)
W₁' a sandbox edit ⟺ the store object still equals the baseline, but the sandbox bytes differ from it
```

Adding the **third column of fact** separates them: `baselineEntry.digest` records the sha256 of
**the content that was copied over** at hydrate time (`refreshBaseline` / `digestOf` in
[lifecycle.go](../../internal/sandbox/lifecycle.go)). Thus:

| store object vs baseline | sandbox bytes vs baseline digest | Verdict | Action |
|---|---|---|---|
| timestamps differ (the host wrote) | — | `W₂` | **BLOCK** + warn |
| timestamps equal | equal | unchanged | skip |
| timestamps equal | different | `W₁'` a sandbox edit | **push** |
| digest missing (over the cap / unreadable) | — | undecidable | **BLOCK** + warn |

Costs and boundaries:

- the digest only covers objects `≤ baselineDigestMax` (2 MiB); larger objects are left empty → the
  conservative branch. The reason: "documents that need editing inside the sandbox" and
  "deliverables/datasets" are of different orders of magnitude, and reading every large file in full at
  hydrate time does not pay for itself;
- the digest is computed at hydrate time and lives with the instance, rebuilt after a process restart —
  the same lifetime as the sandbox instance, so an old instance's data is never used to judge a new one;
- the decision was extracted into a pure function `decideReconcile(statErr, info, base, data)`, its six
  branches mapping one-to-one onto that table, with tests aimed directly at it.

This version **tightened no existing capability**: rule (1) (sandbox artefacts) and rule (3) (sandbox
edits) both work, and rule (4) (host edits) continues to be refused.

### 3.9 End-to-end verification (a real E2B)

[internal/sandbox/e2b_live_repro_test.go](../../internal/sandbox/e2b_live_repro_test.go)
holds acceptance tests that run against a real sandbox (`FASTAGENT_E2B_LIVE=1` gated, not in regular CI):

| Test | What it verifies | Result |
|------|------------------|--------|
| `TestE2BLiveRepro` | the incident's timeline (Hydrate→host write→exec→sync) **no longer reproduces** on a real backend, while `exec` artefacts still reach the store | ✅ the store keeps the host's version; one `BLOCKED … storeBytes=54 snapshotBytes=40` log line; `from_exec.txt` is written back normally |
| `TestE2BLiveHydrateKeepsStoreStamp` | hydrate writes the store's `LastModified` into the sandbox file's mtime (±1s, tar truncates to whole seconds), and the sandbox and store are **two separate** copies | ✅ |
| `TestE2BLiveLooseScopeLayout` | the sandbox layout for a loose session = `/workspace/<key>` (the comparison table in 01 §3.5) | ✅ |
| `TestE2BLiveSandboxEditMigrates` | rule (3) holds on a real backend: an in-place sandbox edit (with no host write) must reach the store | ✅ |

### 3.10 The sandbox → store write-back path (**the baseline-era loop, retired**, see §3.11.3)

> The current implementation writes back **new paths only**; the decision table is §3.11.3. The loop
> below is the shape when the digest baseline still existed.

Sandbox changes **do** flow back into the store. The loop is:

```
an exec inside the sandbox changes /workspace/notes.md
        │
        ├─ trigger 1: after every exec returns normally (RemoteWorkspace backends only)
        │             lazyExecutor.Exec → syncSnapshot(cause=post-exec)
        └─ trigger 2: before idle eviction / sleep
                      flushIfSupported → syncSnapshot(cause=evict)
        │
        ▼
   SnapshotWorkspace()            e2b: tar -czf - -C /workspace . | base64
   · /skills is not inside /workspace, so it is not in the snapshot (it is a read-only mount)
   · the whole /workspace becomes one tar; the cap is 32 MiB (after base64), over which the whole sync fails
        │
        ▼
   per-path decideReconcile (the five-row table of §3.3)
   · no such path in the store        → write
   · this path's mirror failed (stale) → do not write
   · the sandbox content == the baseline digest       → do not write (the sandbox did not move)
   · digest differs + store == baseline → **write** (the sandbox's edit)
   · digest differs + store ≠ baseline  → do not write (both sides moved = the incident shape)
        │
        ▼
   workspaceStore.Put (S3/local) → after a successful write, re-anchor that path's baseline to what was just written
```

So the **full** timeline of the example in §3.3.1 is:

```
t0  host writes notes.md + write-through → store = sandbox = "HOST", baseline digest = H("HOST")
t1  an exec in the sandbox edits notes.md → sandbox = "HOST+EDIT" (the store is still "HOST")
t2  the post-exec sync after that exec
      digest H("HOST+EDIT") ≠ baseline H("HOST"), and the store still equals the baseline
      ⇒ row 4 ⇒ Put("HOST+EDIT"), baseline digest := H("HOST+EDIT")
t3  the store and the sandbox agree, steady state
```

In other words, "the store still holds HOST" exists only between `t1` and `t2`, a window as wide as
**one exec**. That is why the criterion has to distinguish "the sandbox moved" from "it did not" —
otherwise `t2` would take the "skip" branch and the sandbox's edit would never reach the store (the old
implementation got this right by luck via "different byte counts ⇒ overwrite", at the price of
overwriting host writes, i.e. the incident).

Two known boundaries:

| Boundary | Consequence | Mitigation |
|----------|-------------|-----------|
| `/workspace` over 32 MiB (the snapshot cap) | **the whole sync fails**, nothing is written back; the log says `workspace snapshot is over the 32.0 MB cap` | keep large files/logs/caches in `/tmp` (the prompt already asks for this); it also explains the "no write-back" gap between 05:25 and 07:52 on the day of the incident |
| the sandbox never runs another exec and is evicted directly | trigger 2 still syncs once | nothing to do |

### 3.11 Make state changes visible instead of pairing every change with a fallback (landed 2026-09-18)

Earlier generations of the mechanism were built on "refuse + warn". Warnings only reach the log; the
agent cannot see them — and the incident's lesson is precisely that **nobody knew an inconsistency had
happened**. The final conclusion is to move the mechanism's centre of gravity from "prevent every
overwrite" to **"make every change visible"** — once visible, the fallbacks are duplicate insurance
and should be deleted.

#### 3.11.1 Final shape: **perception first, decisions second, no fallback copies**

This section went through three shapes; the first two were overturned and the reasons are worth keeping:

| Shape | Approach | Why it was overturned |
|-------|----------|-----------------------|
| ① CAS | compare digests before overwriting and **refuse to write** on a mismatch | it creates a dead end: a `write_file` rewrite is refused again by the same check, and an exec write inside the sandbox is refused by the reconcile — both paths are blocked; and choosing a side is not the code's decision anyway |
| ② preserve | before overwriting, **copy the sandbox's version to `<path>.sandbox-version`** for the agent to decide later | copying large files is expensive (hundreds of MB to keep a version nobody may want); it pollutes the file panel with copies; **and it duplicates the perception channel** — the agent already knew from the exec signal what the sandbox had changed |
| ③ **perception** (current) | **overwrite directly + state the fact**: observe once before overwriting, put "replaced a different version (N bytes)" into the tool result | — |

The third overturn rests on the principle in §3.12: **once a state change has made the agent aware,
a fallback copy is duplicate insurance**. The agent had already seen, in the exec result, that a script
changed the file, so its write is an **informed decision**; keeping a copy for it and giving it a
side-picking tool mistakes "prevent a mistake" for "a necessary mechanism".

Four mechanisms therefore remain:

| Mechanism | Responsibility | Formal role |
|-----------|----------------|-------------|
| **write-through** | the two copies agree at the moment of the host's write (including timestamp alignment) — the only mechanism that keeps the store from lagging | removes future δ |
| **memory-free version decision** | `size + mtime` equality (`statsFor`/`sameVersion`), falling back to comparing bytes (`equalToStore`) when metadata disagrees; the criteria live on the two copies themselves, so they hold across replicas | the criterion for δ |
| **refuse + signal** | when both copies moved and neither can be shown newest, the sandbox may not overwrite the store, and the fact is handed to the agent | δ → σ |
| **exec change signal** | tells the agent about sandbox-side change (new paths synced / existing paths refused / snapshot failed) (§3.11.2) | σ |

The baseline `B` and `staleWrites` of §3.2/§3.8/§3.10 are gone: the decision no longer needs to
remember "what was last handed to the sandbox", because **the sandbox file's own mtime is that
record** (§3.11.3).

**A methodological note (added 2026-09-20): why those three decision rows could be written before
anything went wrong.** That elimination was not waited out of an incident — it was read off the
specification, because the unit of analysis in this system is **stipulated**, not observed: who
`produce`s, who `place`s, who `take`s (invariant I1), which two delivery points a signal must land on
(`D₁` / `D₂`), and the "∀δ ⇒ ∃σ" duty of the channel are all written down and can simply be read out.
Hence: **an omission is a claim about a complement, and a complement can only be stated against a
specification** — observation always tells you what happened; to say that something is *missing* you
must first have the specification, and only whoever wrote it is entitled to say "missing".
**The price (which must be stated too)**: the verdict holds only inside **this system's own** scope —
where the specification is silent, this method is blind as well.
(The full form of this criterion is in *The Mathematical Principles of Cognitive Philosophy*, 19.4.9,
"is the unit stipulated or observed".)

#### 3.11.3 The current implementation: the baseline is retired, the criteria come from the two copies themselves (settled 2026-09-18)

The reason for retiring the baseline is directly tied to cross-pod sandbox leases: a lease can be
**adopted by another replica**, and an in-process baseline table (empty after a pod restart, never seen
by the other replica) would make different replicas judge the same sandbox by different rules —
"the same sync, with the verdict depending on who handled it". So today there is **no memory across
calls or replicas at all**; the criteria come entirely from the metadata the two copies carry:

| Fact | Source | Cost |
|------|--------|------|
| the store object's size + LastModified | `workspace.Store.Stat` | one round trip per path |
| the sandbox file's size + mtime | one `find /workspace -type f -printf '%P\t%s\t%T@\n'` (`statsFor`) | one exec per sync |
| "does the sandbox still hold what it was handed?" | `sameVersion`: equal sizes and mtimes within ≤1s (tar truncates to whole seconds) | metadata equality decides it, **no digest is computed** |
| when metadata disagrees | `equalToStore`: read the store's copy back and compare bytes | one read, only on this branch |

Write-through is the **precondition** of this criterion: right after the mirror writes, `touch -d
@<store mtime>` aligns the sandbox file's timestamp with the store object's; hydrate does the same (E2B's
tar entries carry `obj.ModTime`). "Same version" is therefore **self-describing** in both copies, with
nothing recorded anywhere else.

> **Amended 2026-09-28 (change register row 87).** The third row below — "different ⇒ refuse" — was the
> absorbing state §13.4 measured (446 refusals, the oldest for 10+ hours): a refusal writes nothing, so
> the same pair came back every sync. The two copies are now **ordered** rather than merely compared,
> using two instruments that keep this section's memory-free posture — the file's mtime against the
> store object's write time with the guest-clock offset **measured in the same exec** as the listing
> (`statsFor`), and strict-prefix containment of the bytes, which needs no clock at all. A verdict
> requires every instrument that can speak to agree, and a verdict must survive both readings of the
> mtime (a sandbox write, or the stamp this section describes). The table becomes:

| store | the two copies' metadata | Verdict | Action |
|-------|--------------------------|---------|--------|
| no such path | — | a sandbox artefact | **push** |
| exists | equal (size + mtime) | the sandbox did not move | skip |
| exists | different, and every instrument that can speak agrees | one copy is the **successor**: the clock orders the two writes, or the bytes nest (a strict prefix is ancestry) | **push** when the sandbox's copy is the successor; **deliver the store's copy into the sandbox** (a new direction, stamped like the mirror's) when the store's is |
| exists | different, and no instrument can speak, or they disagree | cannot tell which is newest | **refuse + signal** (silently skipped when the bytes are in fact equal) |

Two costs that must be recorded honestly (both traded away by this simplification, not accidents):

1. ~~**a sandbox edit of an existing path is no longer written back**~~ — **retired by row 87**: when
   nothing else has written the store, §3.2's W₁ says the sandbox's copy is the successor and it is
   collected (and §3.2's W₂ — the store moved, the sandbox did not — is now *delivered into the
   sandbox* instead of leaving it stale, which is what `KeyError: 'current_pnl'` was). What row 87
   keeps refusing is the pair no instrument can order: two writes closer than ≈3 s, or two instruments
   whose blind spots disagree.
2. **deletions cannot be detected** (G4): the criterion's domain is the sandbox snapshot, and a deleted
   path is not in it. Restoring detection needs a **store-side persistent manifest**, not in-process
   state ([09](./09-sandbox-lifecycle-audit.md) §3).

**Deleted**: the preserved copy (`.sandbox-version`), `resolve_workspace_conflict`, the
`ConflictResolver` interface, and the `ErrTooLargeToCompare` refusal path that came with them. With
them gone there is no "locked state" and therefore no need for an unlock tool — **the mechanism count
dropped from 6 to 4, with no deadlock**.

Write-through has three outcomes, matching three (mutually exclusive) statements in the tool result:

| Case | What write-through does | Tool result |
|------|------------------------|-------------|
| the sandbox copy is the one it was handed (same size+mtime) | write directly, without even reading | none (silent) |
| the sandbox copy is **different** (when comparable) | **overwrite**, and state the fact | `[workspace]`: replaced a different version (N bytes), no copy kept — re-run the producing command if you need it |
| the sandbox's copy differs from what is being written and the caller had no expectation to check it against | **overwrite**, and state that what it was cannot be reconstructed | `[workspace]`: the sandbox held a different version (N bytes), with no earlier copy to compare it against |
| the sandbox is unreachable | the write fails (the judgement stays on the two copies) | `[workspace]`: your write is safe, the sandbox may read the older content (**no "choice" is offered**, because there is nothing to choose) |

The three guarantees about "can a large file blow up the context?" are unchanged: the marker carries
only a path and a byte count; every tool result is clipped by `ClipAndLog` to 64 KiB head + 64 KiB
tail; large files were never copied in the first place.

The risk that overwriting brings is replaced by **one piece of discipline** (a prompt-level suggestion,
not code): after a script edits a large file, the agent should confirm with `exec ls -l` before the
next write, because in that case write-through cannot check on its behalf.

#### 3.11.2 The perception channel (the exec signal)

```
exec("python3 build.py")
  ...the command's own output...
  [workspace] the sandbox changed generated.csv, report.html — synced to the workspace store.
  [workspace] NOT synced (the store's copy differs and neither side may be chosen automatically): notes.md — both versions are intact.
```

- **it does not appear when nothing changed** (an exception channel, or it gets ignored);
- **paths use the sandbox's own spelling** (the same as `exec ls` and `read_file`);
- **refused paths are listed too** — that is the class the agent most needs to know and has no other signal for;
- **a sync during idle eviction is delivered too**: that path has no tool result to attach to, so the part
  that must be carried goes into a scope-level **durable** carrier (`SignalStore`; an in-process queue before
  2026-09-18) and is collected by the scope's next tool result, exactly once
  (`TestEvictionSignalReachesNextToolResult`, `TestEvictSignalOutlivesThePoolThatProducedIt`).

**The fourth class is deletion — it has no signal, and currently cannot have one** (corrected in the
2026-09-18 re-check): the sync's walk domain is the **sandbox snapshot**, and a deleted path is simply
not in it, so the loop cannot reach it. It used to be decided from the baseline ("it was in the
baseline, it is not in the snapshot" ⇒ the sandbox deleted it); when the baseline was retired in
§3.11.3 that criterion went with it: "the sandbox deleted it" and "this file only ever existed in the
store (an upload/attachment)" are now **indistinguishable**. Restoring detection needs a **store-side
persistent manifest** (in-process state will not do), recorded as G4
([09](./09-sandbox-lifecycle-audit.md) §3). So the other three signals in this section are as-built,
while deletion is a **known gap**, not a solved item.

---

## Part 4 · Completeness self-check (three rebuttals against this document)

### 3.12 The state observability principle

Every mechanism in this directory ultimately converges on one harness-level principle:

> **A state change inside the harness must be perceivable by the agent; otherwise the agent reasons
> about a world that no longer exists.**

It is a general constraint independent of filesystem sync, so it stands as its own document:
**[08-state-observability-principle.md](./08-state-observability-principle.md)** (formal statement,
three corollaries, a per-item audit of the current harness, a review checklist for new mechanisms).

This section keeps its shortest form here, as the basis for choosing among §3.11's four mechanisms:

- a change the agent itself caused → the tool result is the receipt, no extra mechanism;
- **a change the harness caused → must be stated explicitly, in a channel the agent really reads**;
- no signal ≠ no change (silence asserts "the world is as you think");
- the statement is an exception channel (the normal case is noise, and the model learns to skip it).

**Rebuttal 1: "B can also expire or be lost — isn't it wrong then too?"**
Rule (7) covers that case: with no baseline only (1) is allowed (write only when the store does not have
the key), i.e. it degrades to "never overwrite an existing key". That is strictly more conservative
than the status quo and does not re-introduce D.

**Rebuttal 2: "Does rule (4)'s `X := S` overwrite unsynced sandbox edits?"**
No — (4)'s precondition is `X[p] = B[p]`, i.e. the sandbox has not touched that key since hydrate. When
the sandbox really has an edit it lands in (3) or (5): the former pushes, the latter errors.

**Rebuttal 3: "Then why did the agent's two repairs in the incident still fail?"**
Because a repair executed only `S[p] := v` (half of `inside`), leaving `X[p]` untouched, so the next
reconcile's precondition was still judged as `W₂` (S moved off B, X did not). Under the **fixed** rules
that triggers (4)'s "pull", syncing the repair into the sandbox and updating B, which terminates the
revert chain. **In other words: fixing D is not only "do not overwrite" but also "push the
authoritative content back into the cache" — neither half alone is enough.**

---

## Part 5 · New problems found by the self-audit (recorded honestly)

### 5.1 `Sync` is "add-only"

During the second self-audit I compared the sandbox's file set with the store's object set and found a
fact that had never been written down:

```
the sandbox's /workspace top level: byo-account-design.html  qc-email-draft.md  todo.md
                                   qc-lean-mcp/  quantconnect-gonogo.html  quantconnect.html
the store (same scope):             all of the above + mcp-oauth-authorization-flow.md + mcp-oauth-design.md
```

The last two are attachments the user uploaded at 12:04 (`LastModified 12:04:32`); they exist in the
store and **not in the sandbox**. `read_file` can read them (it reads the store), but `exec` cannot see
them.

The cause is the direction of `Sync`'s walk: its **domain is the sandbox snapshot**, it only ever does
`S[p] := X[p]`, and it never deletes a key that the sandbox does not have:

| Symptom | Cause | Impact |
|---------|-------|--------|
| an attachment is in the store but not in the sandbox | it was uploaded after hydrate, and there is no "re-hydrate" mechanism | the agent's `exec` and `read_file` see inconsistent worlds |
| `Sync` never deletes an attachment by mistake | the walk is "sandbox → store", add-only | safe, but it also means **a sandbox-side deletion never propagates** |

### 5.2 A gap in the fix rules that this exposes

Rules (1)–(7) in §3.3 assume "keys present on both sides" and "keys the sandbox added", but they do not
cover **`p ∈ dom(B)` with `X[p] = ⊥` (a sandbox-side deletion) when `S[p] = B[p]`** — that is, "the
sandbox deliberately deleted it". The current implementation silently keeps the key alive (the walk
cannot reach it), so it neither loses data nor honours the deletion: **an undeclared behaviour**.

One more rule is needed, and a decision is required before implementing it:

```
 (8) X[p] = ⊥  ∧  S[p] = B[p]  ∧  p ∈ dom(B)
       semantics: a sandbox-side deletion (the user/agent ran rm inside exec)
       option A (keep alive, conservative): no migration, B[p] is kept — i.e. today's behaviour,
                but it must be written into the contract
       option B (propagate the deletion): precondition = evidence of a delete action (needs the undo
                marker of §3.6)
                migration: S[p] := ⊥ ; B[p] := ⊥
       forbidden: deleting S[p] with no evidence (that is the same family of error as "silently
                destroying the user's data")
```

Whichever is chosen **must be declared explicitly** — exactly what §3.5's boundary table demands for
irreversible parts. My inclination is option A plus "declare the existence of store-only files to the
agent" (so `list_dir` can distinguish the two origins), which is conservative without creating a new
irreversible action; option B waits until §3.6's deletion snapshot (the inverse) exists.

### 5.2.1 A controlled reproduction (ran successfully, 2026-09-17)

The full timeline of §2.4 was run against a real E2B backend by adding
[internal/sandbox/e2b_live_repro_test.go](../../internal/sandbox/e2b_live_repro_test.go)
(`FASTAGENT_E2B_LIVE=1` gated, not in regular CI). Steps and assertions:

```
1. write v1 (40 bytes) to the store → create the sandbox (Hydrate brings v1 into /workspace)
2. change only the store, from the host: v2 (54 bytes)   ← simulating write_file/edit_file/apply_patch
3. run one exec inside the sandbox (ls /workspace)
4. evict-sync: syncSnapshot(cause=evict)
```

Result (identical over two runs):

```
step 1 ok: sandbox born holding v1
step 2 ok: store holds v2, sandbox still holds v1
store after sync: "OLD SNAPSHOT VERSION (1111111111111111)\n"     ← v1; v2 was destroyed
per-key 'path differs' log lines: 1
  level=INFO msg="sandbox sync: path differs, taking sandbox copy"
    cause=post-exec path=report.txt storeBytes=54 snapshotBytes=40
REPRODUCED the incident
```

Three facts worth quoting directly:

1. **`cause=post-exec`** — what triggered it was **the write-back attached to step 3's exec, not the
   eviction**. That explains why the incident's `12:05:26 cause=post-exec files=1` could sit alongside
   the `12:15` evict: on this backend "just run any command" is enough to open an overwrite window.
2. **`storeBytes=54 snapshotBytes=40`** — the newly added per-key log line prints both sides' byte counts
   directly; the next unresolved contradiction in §5.3 was located precisely with this field.
3. the sandbox is **hot-reused** (the second run did not rebuild the instance and finished in 2.9s),
   matching the incident's shape of "B lived 4.5 hours across 5 syncs".

The correspondence with the incident therefore lines up cell by cell:

| The incident | This controlled reproduction |
|--------------|------------------------------|
| A/B hold the old version from the moment of hydrate | the sandbox is born holding v1 |
| 08:04–08:19 host tools write only the store | step 2 writes only the store |
| 12:05:26 `cause=post-exec files=1` | step 3's exec triggers the same path |
| 12:15:17 `cause=evict files=3` | step 4's explicit evict |
| the S3 object returns to the old byte count | the store returns to v1 |

(The E2B sandbox used for the reproduction is unrelated to the incident session; it was released
afterwards and left no row in the production `sandbox_leases`.)

### 5.3 An observation once recorded as a "contradiction" (now characterised; only the log is missing)

In the third of five re-checks, a data point appeared that the available evidence could not explain; by
the discipline of "do not force a conclusion before the investigation is complete" it is recorded here
verbatim and not folded into §2's event chain.

`list_dir` reads the store, and the byte counts it reported for `byo-account-design.html` were:

| Time (UTC) | Bytes | Situation |
|-----------|-------|-----------|
| 08:19:32 | 50,282 | after the two repairs (08:15) |
| 12:06:17 | **41,262** | **after** this round's `write_file` (12:05:45) + `edit_file` (12:06:13) |
| 12:07:06 | 55,527 | after this round's repair (12:07:00) |
| 12:15:17 | 41,262 (final) | after the evict (there is a log line) |

**Conclusion (corrected in the 2026-09-17 re-check)**: there is no arithmetic contradiction here; I had
initially conflated two tools.

- `write_file` is a **whole-file replacement**: the byte count it reports is exactly what it wrote to the
  store. The `Written 6893 bytes` at 12:07:00 matches `list_dir`'s 6,893, so the store write is
  self-consistent;
- the incident's `byo-account-design.html` went through `edit_file` (read-modify-write) at 12:06:13, yet
  `list_dir` reported **41,262** at 12:06:17 — and that is exactly the byte count **inside sandbox B**
  (§2.5's direct read: 41,262, md5 matching the store object).

So at 12:06:17 all three files had returned to **their respective sandbox copies' byte counts**
(41,262 / 5,705 / 580), meaning that at the start of this round they **had already** been reverted
(`list_dir` at 08:19:32 still showed the healthy 50,282 / 6,893 / 462). The revert happened **between
08:19:32 and 12:04:33**, not as a result of this round's three execs.

**The only remaining gap is the log**: there is no `synced to workspace store` record in that window
(the log is empty from `08:24:18` to `12:05:26`), but the only writer that can produce those byte
counts is the sandbox-snapshot channel — the byte sequence itself (41,262 / 5,705 / 580) *is* the
sandbox copy's content. So the gap is **"this sync left no log line"**, not "a third writer exists";
the newly added per-key `BLOCKED` log line exists precisely to close it.

Hypotheses ruled out:

| Hypothesis | Test | Conclusion |
|-----------|------|-----------|
| a healthy version exists inside the sandbox (50,282 / 55,527) | md5 the whole `/workspace` with a direct read | ❌ it does not; only the 41,262 version |
| a third (non-lifecycle) write-back path exists | repo-wide, `SnapshotWorkspace` has only the two lifecycle call sites | ❌ no such path in the code |
| the 12:05:26 post-exec sync is the culprit | that run had `files=1`, and all three files were already old before it | ❌ it is not |
| another pod holds a second sandbox | `sandbox_leases` has a single row, and querying the scope directly returns one sandbox id | ❌ does not hold |

**Conclusion**: this does not affect §2 (D depends only on "T3 has no precondition" and "the two sides
had diverged", corroborated three ways by md5, birth time and four sync log lines), nor does it change
the fix. The only gap is that this particular revert **left no log line**; the per-key `BLOCKED` logging
added since exists to close exactly that.

---

## Appendix: the one-off probes used to confirm this (reproducible)

```bash
# 1) fetch the scope's live sandbox and its state
select sandbox_id, state, envd_token, epoch from sandbox_leases
 where scope_key = 'agt_…:s:OvDLzEPKcEK84Hpfq0U6LK';

# 2) unlock the envd token (AES-256-GCM, key = sha256(FASTAGENT_OAUTH_SECRET))
#    see internal/mcp/oauth/adapter/cryptor.go

# 3) connect (paused → running, returns a new token) and read files directly
curl -X POST "https://api.e2b.dev/sandboxes/$SBID/connect" \
     -H "X-API-Key: $E2B_API_KEY" -H 'Content-Type: application/json' -d '{"timeout":120}'
curl -H "X-Access-Token: $TOKEN" \
     "https://49983-$SBID.e2b.app/files?path=/workspace/byo-account-design.html&username=user"

# 4) restore after reading: POST /sandboxes/$SBID/pause
```

These probes are read-only plus restore; every conclusion in this document can be reproduced
independently from the table in §1.2 and these commands.
