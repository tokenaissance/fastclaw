# 01 · The implementation as built

> Status: as-built (checked point by point against `fastagent`'s code at the time) · last verified: 2026-09-17
> Purpose: to make clear where workspace files actually live, who writes them, and when they sync. Every
> later analysis in this directory takes this as its premise.

> **Postscript (added 2026-09-18; it amends §3–§8).** This section is a snapshot of the code on
> 2026-09-17. The fix landed the next day and changed exactly the parts this document calls broken:
> `write_file` / `edit_file` / `apply_patch` now **all** call `writeThroughSignal` →
> `LifecyclePool.WriteThrough` on a remote backend (the `codingSubdir == ""` gate is gone, and
> `mirrorCodingWriteToSandbox` no longer exists — it was generalised into that function), so §3.1's
> "conditional mirroring" and §3.5's path asymmetry no longer describe the current code. §3.4's criterion
> (`Stat().Size == len(data)` → skip) was replaced by the memory-free `size + mtime` comparison of
> [07 §3.11.3](./07-formal-rootcause-and-fix.md); §6's logging gap was closed by the per-key `BLOCKED`
> line; §7's missing tests now exist as `lifecycle_sync_contract_test.go`.
>
> **§8 was fixed too (same day, second pass):** `apply_patch`'s store path resolution now uses the same
> function as `write_file` / `edit_file` (`scopeSessionID()` + `wsPath()`, all 6 touchpoints), pinned by
> unit tests plus a live E2E; the falsification record is in §8.1. Fixing it surfaced a second scope
> inconsistency in the **same family** (§8.2 / [10](./10-harness-state-audit.md) G17): the rule "a
> project session collapses the session segment" is applied by hydrate but not by sync — live-verified,
> a product decision, and still open.

## 1. Two copies

One logical path (say `byo-account-design.html`) can exist in two places at once at runtime:

| Copy | Location | Who reads it | Who writes it |
|------|----------|--------------|---------------|
| **durable store** ("the store" below) | production: S3 (DO Spaces, `prod/<agent>/sessions/<sid>/<path>`); local: `~/.fastagent/workspaces/<agent>/…` | the UI file panel, downloads, signed URLs, `read_file`, `list_dir`, `/api/agents/{id}/files/...` | host file tools, the sandbox write-back path |
| **execution copy** | `/workspace/<path>` inside the sandbox | `exec` inside the sandbox (scripts, builds, git) | `exec`, and the host tools' mirror (conditional, see §3.3) |

The store port is defined in [internal/workspace/workspace.go](../../internal/workspace/workspace.go):

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

The scope-to-path mapping (identical on both backends; implemented in `scopeDir` in
[localfs.go](../../internal/workspace/localfs.go) and `key` in [s3.go](../../internal/workspace/s3.go)):

| projectID | sessionID | Location under the store |
|-----------|-----------|--------------------------|
| `""` | `""` | `<agent>/<path>` (shared by the agent) |
| `""` | `s` | `<agent>/sessions/<s>/<path>` |
| `p` | `""` | `<agent>/projects/<p>/<path>` |
| `p` | `s` | `<agent>/projects/<p>/<s>/<path>` |

> Note that `ObjectInfo` carries `Size`, `ModTime` and `Path` — **no version, no content hash, no
> author**. That is the material basis of every arbitration difficulty later: the store itself cannot
> tell whether an object was "just written by the host" or "written back by the sandbox".

## 2. Sandbox backends and their physical facts

The `Executor` port has only 7 methods ([internal/sandbox/executor.go](../../internal/sandbox/executor.go))
and says nothing about what `/workspace` is. That is expressed by three marker interfaces and the three
backend implementations:

| Backend | Physical form of `/workspace` | `RemoteWorkspace` (`/workspace` is not shared with the host) | `WorkspaceSnapshotter` (can produce snapshot bytes) | `PortExposer` (can expose ports) |
|---------|------------------------------|------------------------------------------------------------|----------------------------------------------------|----------------------------------|
| **docker** | a bind mount of a host directory: `-v <host dir>:/workspace:rw` ([docker.go](../../internal/sandbox/docker.go) line 255) | not declared → **shared** | implemented: `filepath.WalkDir(host dir)`, skipping build dirs like `node_modules` ([docker_executor.go](../../internal/sandbox/docker_executor.go) line 75) | implemented |
| **e2b** | an independent filesystem inside the sandbox | declared ([e2b_executor.go](../../internal/sandbox/e2b_executor.go) line 1565) | implemented: `tar -czf - -C /workspace . \| base64` pulls the remote copy back | implemented |
| **boxlite** | an independent filesystem inside the sandbox | declared ([boxlite_executor.go](../../internal/sandbox/boxlite_executor.go) line 741) | implemented: downloads a tar through the Files API | not declared |

The key asymmetry:

- docker's `SnapshotWorkspace` **returns the host's own copy** (it walks the source directory of the bind
  mount), so "the snapshot" and the store agree by construction;
- e2b / boxlite's `SnapshotWorkspace` returns **another copy**, and that copy is frozen at the moment of
  the last hydrate (or the last write inside the sandbox).

### 2.x What each backend promises for one write (strength table, 2026-09-19)

> The port gained a conditional write, `PutIfVersion(expected Version)` (family B, obligation L7). **Strength
> differs per backend and must be declared** — callers may rely only on the declared part.

| Backend | What `Version` is | Strength of `PutIfVersion` | Consequence for callers |
|---|---|---|---|
| **S3 / Spaces (multi-replica production)** | the object's ETag | **Exact where the bucket evaluates `If-Match`** (AWS S3, MinIO): the conditional PUT shares the request with the write, 412 ⇒ `ErrVersionConflict`. Ceph RGW — the store behind DigitalOcean Spaces, which this deployment runs on — implements only the create-only form and answers 412 to *every* `If-Match`, even with the ETag the client just read (measured 2026-09-22 against nyc3 Spaces), so an overwrite there is a **compare-then-write**: `Stat` the object and land the write unconditionally only while it still carries `expected` | may be used to **refuse** an overwrite where the bucket is exact; on Spaces it can only detect an already-stale expectation, and create-only (`If-None-Match: *`) stays exact either way |
| **LocalFS (single host / dev)** | `size:mtime_ns` | **Best-effort**: stat → compare → write, with no kernel CAS | can only "detect and refuse an already-stale expectation"; multi-replica installs must use S3/PG |
| **Metered** | pass-through | same as its inner store | — |

Writer postures (who uses which precondition):

| Writer | Precondition | Status |
|---|---|---|
| `write_file` / `edit_file` / `apply_patch` | `Stat` right before the write ⇒ `PutIfVersion` | ✅ landed (B7–B9) |
| attachments | `VersionAbsent` (must not exist); a taken name ⇒ **keep both under a new name** | ✅ landed (B10) — `internal/agent/attachments.go:178` passes `workspace.VersionAbsent` to `PutIfVersion`; on S3 that is `If-None-Match: *`, on LocalFS a stat-compare (best effort, §2 above). **Posture changed 09-21**: the old path did `Put` first and, on a conflict, only `slog.Warn`ed while *still* appending the **old** name to the `[Attached: …]` breadcrumb — a silent wrong answer (the breadcrumb named somebody else's bytes). Now the **store write happens first** (the store is what decides the name); a conflict keeps both as `<stem> (n)<ext>` (bumping an existing counter, never nesting) and returns the name that actually landed; when no spelling is free it returns `""` and the caller claims **nothing**. Witness `internal/agent/attachments_store_posture_test.go`, falsifications run for real (`PutIfVersion`→`Put` ⇒ 3 red; drop the keep-both loop ⇒ 2 red) |
| skills publish | read the current version, then conditional write | ✅ landed (B10) — `internal/skills/objectstore.go:113` reads the version, then `PutIfVersion`, and maps `ErrVersionConflict` |
| panel upload/delete | the version the panel last listed | ✅ landed on **both** sides (B11) — server: the upload reads the optional `expectedVersion` form field and answers 409 with `current{version,size,modified_at}` (`internal/setup/handlers_agents.go:1492-1531`); cloud panel: now uploads **one file per request**, turns that 409 into a named conflict, and offers the **three answers** (keep both = auto-rename to the name the server returns / replace = re-send carrying `expectedVersion` / cancel), with the delivery point asserted in cloud `src/__tests__/fastagent/attachment-conflict-flow.test.tsx`. A **delete** has no content to overwrite, so its second half is the d1 mirror removal instead (§: `sandbox.LiveWorkspaceFileRemover` — panel 2026-09-18, tool path 2026-09-20, [10 §4](./10-harness-state-audit.md) G7b) |
| sandbox↔store (write-through / write-back) | **no conditional write**: the witness exists only in-band, in the copy's mtime stamp (L7 §3.1) | stays family A |

> **A checkable reading (added 2026-09-21; the structural component of "the cost of refusal", 19-5 §19.5.8.8)**:
> the backend strength table has **3 cells** = exact (buys **prevention**) **1** · best-effort (buys only **detection**) **1** · pass-through **1**;
> the writer postures have **5 rows** = buys prevention **4** (**on S3**; the same four degrade to detection on LocalFS) · **buys nothing 1** (sandbox↔store stays family A).
> **Two conclusions**: ① **the only place in this system where refusal is actually exercised is 1 row**
> (the family-A write-back where the sandbox would overwrite the store) **plus 1 conditional cell** (lease store unavailable ⇒ [12 §3.2](./12-lease-formal-design.md));
> ② 19.4.6.2’s self-check passes on the spot: **the count of "prevention" cells (1) does not exceed the count of "co-located check" cells (1)** — this table is not lying yet.

## 3. Writers and write paths

### 3.1 Host file tools → the store

Registered in [internal/agent/tools/file.go](../../internal/agent/tools/file.go); routing is
decided by `routeFor` ([route.go](../../internal/agent/tools/route.go)):

| Tool | Function | Store write point | Mirrored into the sandbox at the same time? |
|------|----------|-------------------|---------------------------------------------|
| `write_file` | `makeWriteFile` | line 589 / line 1035 (the `RouteWorkspaceStore` branch) | **conditionally**: `mirrorCodingWriteToSandbox` at line 1042 |
| `edit_file` | `makeEditFile` | line 703 / line 1224 | **conditionally**: line 1231 |
| `apply_patch` | `writeForPatchSandbox` ([apply_patch.go](../../internal/agent/tools/apply_patch.go)) | `workspaceStore.Put` inside that function | **never** |

"Conditionally" refers to the two gates in
[mirrorCodingWriteToSandbox](../../internal/agent/tools/file.go):

```go
if r.codingSubdir == "" || r.executor == nil { return }        // must be a coding-runtime session
if _, ok := r.executor.(sandbox.RemoteWorkspace); !ok { return } // must be a separated backend
```

So:

- **ordinary research / deliverable sessions** (no coding runtime, `codingSubdir == ""`, i.e. **the
  default shape**): the mirror does not run → the store has the new version and the sandbox still has
  the old one;
- **coding session + e2b**: the mirror runs → the two copies agree, which is why those sessions rarely
  show a revert;
- **`apply_patch` in any session**: never mirrored → as soon as the same path exists in the sandbox, a
  fork is planted.

`SetCodingSubdir("app")` is only set by two paths in the preview runtime ([loop.go](../../internal/agent/loop.go)
line 224, [runtime_tools.go](../../internal/agent/runtime_tools.go) line 87). The mirror
therefore **serves "dev-server hot reload in a coding-runtime session", not "keep the store and the
sandbox consistent"** — the commits that introduced it (`418ce82` / `ede73d3`, 2026-06-14,
"backend-agnostic project runtime + multi-template + preview UX") were about preview, not sync. That
origin determined its path choices, see §3.5.

### 3.5 The coding mirror's path asymmetry (important)

The mirror writes to `"/workspace/" + path`, i.e. it **hard-codes a mapping from the store's logical path
to the root of `/workspace`**:

```go
// file.go, mirrorCodingWriteToSandbox
dest := "/workspace/" + strings.TrimPrefix(filepath.ToSlash(filepath.Clean(path)), "/")
```

Hydrate, by contrast, expands strictly by scope prefix: E2B's hydrate packs store objects into a tar using
the relative paths returned by "list" and extracts them at `/`
([e2b_executor.go](../../internal/sandbox/e2b_executor.go) lines 633, 783), and S3's `List`
returns relative paths that, **for a loose session, include the `sessions/<sid>/` prefix**
([s3.go](../../internal/workspace/s3.go), `scopePrefix`/`List`).

The correspondence therefore drifts with the scope:

| Session shape | Store key (logical path) | Location in the sandbox after hydrate | Where the mirror writes | Aligned? |
|---------------|-------------------------|---------------------------------------|-------------------------|----------|
| loose session (no project) | `sessions/<sid>/report.html` | `/workspace/sessions/<sid>/report.html` | `/workspace/report.html` | ❌ **misaligned** |
| project session | `report.html` (the project root) | `/workspace/report.html` | `/workspace/report.html` | ✅ |

Misalignment has two consequences, neither of which has ever been measured:

1. **the mirror does not modify the copy hydrate made**: after `write_file` fixes the store,
   `/workspace/sessions/<sid>/…` is still the old version (the sync skips it because the store key's size
   is unchanged), while `/workspace/report.html` produced by the mirror is a **copy**. So what the agent
   sees through `exec` and through `read_file` is not the same content.
2. **duplicate objects appear in the store**: once the mirror writes to `/workspace/<path>`,
   `lazyExecutor.WriteFile`'s `mirrorSandboxWrite` writes it back to the store as a **root key** (not
   `sessions/<sid>/…`) ([lifecycle.go](../../internal/sandbox/lifecycle.go) line 784), so
   one logical file ends up under two keys in the store.

Conclusion: **"mirror the host write into the sandbox", which looks like the path that prevents reverts,
actually lands somewhere else in the default (loose) session shape.** It cannot be used as the
correctness basis of any plan; see [05-remediation-plan.md](./05-remediation-plan.md) §7.

### 3.1.1 Every writer of the store (added in the 2026-09-18 re-inventory)

The table above only covers the **tool** path. A fresh inventory finds six writers, with different scopes
and different mirroring behaviour:

| Writer | Where | Which keys | Writes the sandbox too? |
|--------|-------|-----------|-------------------------|
| host file tools (`write_file` / `edit_file` / `apply_patch`) | `tools/file.go`, `tools/apply_patch.go` | `scopeSessionID()` + `wsPath()` (one resolution for all three tools since 2026-09-18; see §8) | ✅ write-through on remote backends (`WriteThrough`) |
| sandbox write-back (`syncSnapshot` in bulk / `mirrorSandboxWrite` per file) | `sandbox/lifecycle.go:742`, `:1164` | sandbox paths mapped back to store keys | — (the other direction) |
| **attachments** (`WriteSessionAttachments`) | `agent/attachments.go:118` | session scope | ✅ all three: the host dir, the store, and the **live sandbox** |
| **panel upload/delete** (`POST/DELETE /api/n`) | `setup/handlers_agents.go`, `handleAgentFileUpload` / `handleAgentFileDelete` | session/project scope | **upload: no write-through** (decision a, 2026-09-18: "the panel is the file library"); **delete: writes through** (decision d1, same day: `Gateway.RemoveWorkspaceFile` → `LiveWorkspaceFileRemover`, live instances only, never creates one) — see 10 §3.2 / §4 G21 |
| **skill mirror** | `skills/objectstore.go:105`, `:192` | `<owner>/skills/<skill>/<file>` (a **second logical namespace** inside the workspace bucket) | carried into the sandbox by hydrate |
| ~~`WorkspaceSync`~~ (dead code, **deleted 2026-09-18**) | ~~`sandbox/workspace_sync.go`~~ | the old userID-only keys | — |

The last row is this re-inventory's finding: `WorkspaceSync` / `NewWorkspaceSync` have **zero references
repo-wide**, and the `WorkspaceStore` interface it carries (`List(ctx, userID)` /
`Put(ctx, userID, path, r)`) is **signature-incompatible** with today's `workspace.Store`
(`(agentID, projectID, sessionID, path, size, contentType)`) — today's `LocalFS`/S3 do not satisfy it.
It was born in `87c50ee` (the 2026-04-12 multi-user refactor), never modified since, and no commit ever
referenced `NewWorkspaceSync`: **it was never wired up**, and the whole "hydrate everything, flush on
demand" job was later taken over by `LifecyclePool` + `hydrateWorkspace` + `syncSnapshot` +
`WriteThrough` (see 10 §9).

### 3.2 exec → the sandbox copy

`exec` inside the sandbox can only write to `/workspace`. Those writes have exactly one route back to the
store: `syncSnapshot` (see §3.4).

### 3.3 The sandbox → store direction (two paths)

**a) single-file mirror**: `mirrorSandboxWrite` in [lifecycle.go](../../internal/sandbox/lifecycle.go)
(line 784), called by `lazyExecutor.WriteFile` (line 752) on a `RemoteWorkspace` backend when the path
starts with `/workspace/`. It serves the "a tool writes an absolute sandbox path" route
(`RouteSandbox`, e.g. `ex.WriteFile`), not `write_file`'s normal route (which goes to the store).

**b) full snapshot write-back**: `syncSnapshot` (line 442), with two triggers:

```go
// 1) after every exec (RemoteWorkspace backends only)
if _, remote := ex.(RemoteWorkspace); remote {
    l.pool.syncSnapshot(ctx, l.scope, ex, "post-exec")
}
// 2) before idle eviction / sleep
p.flushIfSupported(sc)   // internally syncSnapshot(..., "evict")
```

### 3.4 `syncSnapshot`'s criterion

> **Amended 2026-09-28 (change register row 87) — read this before the sketch.** The code below is the
> 2026-09-17 shape ("the sizes differ ⇒ overwrite"): it is three repairs old. The criterion as it
> stands is [07 §3.11.3](./07-formal-rootcause-and-fix.md) with row 87's amendment — the store has no
> such path ⇒ push; size+mtime equal ⇒ skip; otherwise the two copies are **ordered**, not merely
> compared (the file's mtime against the store's, with the guest-clock offset measured in the same exec
> as the listing, plus strict-prefix containment of the bytes), and the verdict is push / **deliver the
> store's copy into the sandbox** / refuse-and-signal.

```go
for path, data := range files {                    // files = the sandbox snapshot
    if info, err := p.workspace.Stat(...); err == nil && info.Size == int64(len(data)) {
        continue                                   // same size → skip
    }
    p.workspace.Put(..., path, bytesReader(data), int64(len(data)), "")
}
```

That is: **the store does not have the path → write; the sizes differ → overwrite with the sandbox's
content.** No timestamp comparison, no provenance marker, no conflict handling.

## 4. Hydrate: the channel in the other direction

When a sandbox is created (or rebuilt), `/workspace` is filled from the store, one way:

| Backend | Hydrate implementation |
|---------|-----------------------|
| docker | not needed: the bind mount *is* the host directory |
| e2b | the inner pool hydrates itself (one tar.gz covering `/skills` and `/workspace`); around [lifecycle.go](../../internal/sandbox/lifecycle.go) line 575 it detects `workspaceAware` and skips the per-file fallback |
| others | per-file `hydrateWorkspace` ([workspace_hydrate.go](../../internal/sandbox/workspace_hydrate.go)) |

The timing of hydrate decides the sandbox copy's "birth version". When the sandbox then lives a long time
(across turns, across sleep/wake, across pod adoption), that birth version can drift arbitrarily far from
the store — and it is the source of later conflicts.

## 5. Lifecycle facts (relevant to this incident)

- the scope key is `(agentID, projectID, sessionID)`; the `sandbox_leases` table
  ([internal/store/sandbox_leases.go](../../internal/store/sandbox_leases.go)) stores
  `sandbox_id`, `state`, `expires_at`, `paused_at`, `epoch` per `scope_key`.
- idle eviction defaults to `idleTTL=10m`: backends that can sleep go through `ScopeSleeper` (keeping the
  filesystem and the `hydrated` flag), those that cannot go through `Release` (destroyed, re-hydrated at
  the next rebuild). See [lifecycle.go](../../internal/sandbox/lifecycle.go) around line 320.
- `hydrate` and `syncSnapshot` together express the intent "keep as much of the sandbox in the store as
  possible when it closes", but both directions are **last write wins**.

## 6. Observability today

| Event | Log | Enough to locate a conflict? |
|-------|-----|------------------------------|
| sandbox hydrate | `e2b sandbox hydrated … workspaceFiles=N tarBytes=…` | yes (it shows the birth version's time) |
| a successful snapshot write-back | `sandbox synced to workspace store … cause=post-exec\|evict files=N` | **no**: only a file count — no paths, no byte counts, no pre-overwrite size |
| the snapshot exceeds the size cap | `sandbox sync: snapshot failed … over the 32.0 MB cap` | partly (it explains "no write-back for a while") |
| a single-file mirror fails | `sandbox sync: write_file mirror failed … path=…` | yes (it has the path) |

Conclusion: **today's logs cannot answer "how many bytes did this write-back overwrite, and what did it
overwrite"**. That is also why this incident could only be reconstructed from S3 objects' `LastModified`
and individual session messages, rather than seen at a glance.

## 7. Test coverage today

In [internal/sandbox/lifecycle_test.go](../../internal/sandbox/lifecycle_test.go) the only
write-back-related tests are:

- `TestLifecycle_FlushOnEvict` (line 842): asserts that a sandbox file is pushed to the store (one
  direction, covering only "the sandbox has a new file");
- `TestLifecycle_HydrateOnCreate` (line 742): asserts the store → sandbox direction.

**No test covers "the store has the new version and the sandbox the old one"**, and none distinguishes
the docker (shared) and e2b (separated) backend semantics. The fixture `fakeWorkspace.Stat` returns an
`ObjectInfo` whose `ModTime` is always the zero value, and `snapshottingExecutor` offers only a
`map[string][]byte` (no notion of time), so "the two copies are different versions" **cannot even be
expressed** in the existing tests.

## 8. Appendix: path resolution — one path, one key (§8.1 fixed), and the rest of the family (§8.2 open)

### 8.1 Fixed (2026-09-18): `apply_patch` resolves paths like the other two file tools

Before the fix, `apply_patch`'s store path resolution **differed** from `write_file`'s / `edit_file`'s:

| Tool | Session segment passed to the store | Path mapping |
|------|-------------------------------------|--------------|
| `write_file` / `edit_file` | `r.scopeSessionID()` (collapsed to `""` in coding-root-scope mode) | `r.wsPath(path)` (appends `codingSubdir`) |
| `apply_patch` (before) | `r.sessionID` (the raw value) | as-is |
| `apply_patch` (now) | `r.scopeSessionID()` | `r.wsPath(path)` |

At the time the collapse was driven by a flag: `a.registry.SetCodingRootScope(a.projectRuntime != nil
&& projectID != "")` ([internal/agent/loop.go](../../internal/agent/loop.go) line 214).
**2026-09-18 ([10 §4 G23](./10-harness-state-audit.md)) deleted that flag**: the collapse now lives in
[`workspace.WriteScope`](../../internal/workspace/scope.go) (keyed on "is there a
project", no longer on "is a runtime wired") and is shared by the file tools, the sandbox's write-back
and the panel's parser. Before the fix:

| Session shape | Did the two tools diverge? | Note |
|---------------|---------------------------|------|
| loose session (no project) | no | `projectID==""` ⇒ the session segment stays, and `codingSubdir==""`, so both resolve identically |
| project session (with a runtime) | **yes** | `write_file` landed at `<codingSubdir>/foo.md`, `apply_patch` at `foo.md` |

The fix covers **6 touchpoints** (`apply_patch.go`): `readForPatch` / `writeForPatch` / `deleteForPatch`
on the host path, and `readForPatchSandbox` / `writeForPatchSandbox` / `deleteForPatchSandbox` on the
sandbox path. Two things from the same source went in with it:

1. **The mirror's path end and the store end are one mapping.** `writeThroughSignal` derives both the
   store key and `/workspace/<key>` from the same `wsPath(path)`
   ([file.go](../../internal/agent/tools/file.go) around line 977). It is the **second
   consumer** of that mapping: if the writing end resolves differently, a single write contradicts
   itself — the store lands on key A, the mirror on path B, and the agent inside the sandbox keeps
   reading B. So "is the mirror right?" is not a question about the mirror's own line: it is a question
   about **both ends coming from the same function**. `TestWriteThroughMirrorsOneKeyAndOnePath` pins
   exactly that pair, `("app/notes.md", "/workspace/app/notes.md")`, for both tools.
2. **The sandboxed `apply_patch` never called the mirror at all** (an implementation fact, not an
   inference). `registerSandboxedApplyPatch` only wrote the store. It now calls `writeThroughSignal`
   per file and attaches the result to that file's tool result (`runApplyPatch` already hands the
   closure one path at a time, so a refusal or a signal lands on the right file).

**The tests that pin it** (each one was falsified: reverting the fix line to `r.sessionID, path` turns
it red):

| Layer | Test | Output under falsification |
|-------|------|----------------------------|
| host-mode keys | `apply_patch_path_scope_test.go` (3 cases) | `keys = [app/notes.md sessions/sess-1/notes.md]` |
| sandbox-mode keys + the mirror pair | `write_through_signal_test.go::TestWriteThroughMirrorsOneKeyAndOnePath` | `keys = [app/notes.md sessions/sess_1/notes.md]` |
| live E2B | `apply_patch_live_scope_e2e_test.go::TestE2BLiveOnePathIsOneKey` | — (live; the run command is in the file header) |

The live test walks the incident's shape — seed → hydrate → `write_file` → `apply_patch` — and asserts
that the store holds **exactly one** key, `app/notes.md`; that the sandbox can read the new content; and
that `/workspace/notes.md` (the sandbox-side shadow of the root key the pre-fix build produced) does not
exist. The point of the falsification is that these three assertions go red under either degradation:
split resolution, or a missing mirror.

One leftover from the earlier pass was removed along the way: `Registry.sandboxSessionID` had zero
references across the repo and its comment pointed at a non-existent `sandboxScopeSession`
([10](./10-harness-state-audit.md) §9).

One thing §8.1 did **not** claim, and the 2026-09-22 pass had to add to: `apply_patch` is the only
file tool that does not dispatch through `routeFor` — it keeps its own backend ladder
(`readForPatch` / `writeForPatch` / `deleteForPatch` and their `*ForPatchSandbox` twins). The store-key
resolution §8.1 fixed is shared, but `routeFor`'s **policy** half (the skill rules) was not: the ladder
gated no `SKILL.md` and routed no `skills/<name>/…`. So `Update File /skills/<name>/SKILL.md` +
`*** Move to:` could carry the operator's manifest text out of the read-only mount to a path the
chatter could then `read_file`, and a `skills/<name>/…` patch landed in the sandbox `/workspace` (or the
agent home) instead of the skills bucket. Both are now refused at the tool's entry, before any op is
planned (`applyPatchRefusal`), with the refusal naming `write_file` for the namespace case — register
row 50. The report is **not** that the ladder should be replaced by `routeFor` (that is a larger
refactor and not obviously right: apply_patch needs the same host/sandbox split per path); it is that a
tool which owns its own ladder also owns the duty to carry every rule the shared one carries, and this
one had not.

### 8.2 Fixed (G17: visibility via G+H, the sync write-back via A) — the as-built record follows

> **Final update, 2026-09-18**: the mismatch described in this section has been fixed in all three
> parts: **G** the preview container is addressed by project (one project, one preview container, both
> entry points on it); **H** writes and deletes are broadcast to every live container of the project
> (per-chat shells kept); **A** `syncSnapshot`'s write-back is **collapsed to the project root**
> (`syncStoreScope`) — the same key hydrate and the file tools use. So neither "a sandbox-born file the
> tools cannot see" nor "every project file duplicated under the chat subdir" happens any more; the live
> test `TestE2BLiveProjectSessionKeepsOneTree` pins the new shape (no duplicates, the exec artefact at
> the project root and visible to the tools, and a sandbox edit of an existing path still refused).
> **No migration**: copies produced before the change remain in the store (no longer refreshed, and
> nobody cleans them). The original text is kept below as the "why it was like that" record.

While fixing §8.1 the next question was "who else consumes this path mapping?", and it surfaced the
second instance in the same family — one layer down. Inside a single coding project session, three
places disagree about the scope:

| Who | Scope | Where |
|-----|-------|-------|
| file tools (`scopeSessionID()` → `workspace.WriteScope`) | `(agent, projectID, "")` — the session is collapsed | `scopeSessionID()` |
| the sandbox pool's instance key | `(agent, projectID, chatSession)` — one instance per chat | `internal/agent/loop.go` line 244, `pool.Get(a.name, projectID, sessionID)` |
| **hydrate** | when `projectID != ""`, lists with **session=""** (the whole project) | `e2b_executor.go` lines 739–746 (comment: *"so the chat sees sibling chats' files"*) |
| **syncSnapshot** | uses the sandbox scope's `sc.sessionID` throughout | `lifecycle.go` from line 671 (`Stat` / `Put` / the `syncSnapshot` List) |

The hydrate half is **right** (it delivers the project root's objects into the sandbox, so a tool-written
file really is at `/workspace/<path>`). The mismatch is in sync: it writes the sandbox's content back
under `<agent>/projects/<pid>/<chat>/…`, while the tools read `<agent>/projects/<pid>/…`.

Live verification (**recorded at the time** with `TestE2BLiveProjectScopeIsNotTheSandboxScope`, 2026-09-18,
line by line; after decision A that test was renamed `TestE2BLiveProjectSessionKeepsOneTree` and now
asserts the new shape):

| Step | Observation |
|------|-------------|
| 1 | seed one `app/notes.md` at the project root → hydrate `workspaceFiles=1` → sandbox `/workspace/app/notes.md` = `one\n` ✅ agrees with the tools |
| 2 | the first `exec`'s post-exec sync: the path "does not exist" in the **chat** scope → writes a second copy at `projects/<pid>/<chat>/app/notes.md` (log: `new path from sandbox … session=chat-scope path=app/notes.md`) |
| 3 | edit the same path inside the sandbox (4 → 17 bytes) → the sync returns **BLOCKED** (`storeBytes=4 snapshotBytes=17`), and the signal `[workspace] NOT synced …` is delivered to the agent as usual |
| 4 | `read_file` (the project-root copy) reads `one\n`: the sandbox's edit **never reaches the copy the tools read** |

The difference from the refusal in [07 §3.3](./07-formal-rootcause-and-fix.md) has to be stated: that one
is "the two copies genuinely differ and attribution cannot be decided, so refuse" — a *valuable*
conservatism. This one is **systematic** — what gets refused is the second copy sync itself just
created, it repeats on every edit, and the two objects coexist in the store indefinitely (exactly the
"duplicate objects" §3.5 predicted, now with live evidence).

The fix direction is one line: have sync apply the **same collapse** as hydrate (`session=""` when
`projectID != ""`). But that changes "which key a sandbox-born file lands on", and therefore how visible
chats are to each other inside a project (docker's behaviour is to cwd into the chat subdir), so it is a
**product decision**, not an observability gap — recorded as [10 §4](./10-harness-state-audit.md) G17
and not changed unilaterally. The live test above pins the as-built fact: when the decision changes, the
test changes **with** it, so the change is always an explicit act.
