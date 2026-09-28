# Chat event delivery: placement — session-key affinity, not a fan-out relay

> **Status**: decided · 2026-09-24 · in force, **landed in code, live in dev, not in
> production** (§7 lists what shipped, the dev probe that confirmed it, and what is
> left — steps 1–9 are in the tree; the production rollout and the cloud deploy are
> not)
> **Scope**: *which pod* a session's requests land on, and why that is a delivery
> input at all. The mechanism that carries events across pods is
> [chat-event-delivery.md](./chat-event-delivery.md).
> **Authority**: this is the single source for the placement decision (affinity
> vs. a fan-out relay). Where another document disagrees, this one wins.
> **Related**: [chat-event-delivery.md](./chat-event-delivery.md) §4 D4 (why
> `content_delta` is not persisted) · [session-turn-integrity.md](./session-turn-integrity.md)
> (the lease that already serialises one session) · `internal/agent/event_hub.go`.

## 1. Why placement is a delivery input

The hub is in-process, and says so:

> In-memory only — multi-pod deploys need to swap this for redis pub/sub or
> similar. (`internal/agent/event_hub.go:22`)

The sibling doc answers that with the DB tail: every **persisted** event reaches a
subscriber on any pod, ≤500 ms late (`chatEventTailInterval`,
`internal/setup/chat_event_tail.go:35`). One type is deliberately not persisted —
one row per token would dwarf the table (`internal/agent/events.go:70`) — and the
tail is not allowed to carry it (`internal/setup/handlers.go:1615`, `:1626`). So
the split is:

| Event | Persisted (`seq ≥ 0`) | Cross-pod today | Needs placement? |
|---|---|---|---|
| `tool_call` / `tool_result` / `queued` / `turn_active` / `done` | yes | tail replays it, ≤500 ms | no |
| `content_delta` | **no** (`seq = −1`) | **lost** | **yes** |

So "which pod does the subscriber run on" is not a capacity question first: it is
the only thing that decides whether a watching tab sees the answer stream in, or
appear at once at `done`.

Nothing chooses it today. The browser never talks to fastagent directly:

```
browser → cloud proxy → ingress → Service → a pod (round-robin)
```

* the proxy **rebuilds** the request headers, keeping only `Authorization` and
  `Content-Type` (`tokenaissance-cloud`, `src/routes/api/fastagent/$.ts:249`), so
  the ingress's cookie affinity never applies;
* it **rebuilds** the SSE response with three fixed headers (`:286`), so the
  upstream `Set-Cookie` never reaches the browser either.

Net: two browsers signed in as the same user, watching the same session, sit on
two pods — and a turn's `content_delta` reaches only the one that happens to share
the runner's pod.

## 2. Decision

**The standard practice for cross-replica fan-out is a pub/sub broadcast** (Redis,
or any equivalent relay): one publisher, N subscribers, and the subscriber's pod
stops mattering — it receives every event regardless of who produced it. That is
what the hub's own comment names, and it is what we would reach for in a system
that already runs Redis.

**We are not adding Redis.** These deployments have no Redis today — no service, no
`FASTAGENT_REDIS_*` in the environment or the ConfigMap. Buying a new stateful
component (a new deploy dependency, a new failure mode, a new thing to operate and
to page on) to recover one live-only field is the wrong trade while the durable
half already has a transport (§1). Keeping the ops surface small is worth more than
the typewriter effect.

**We affinity-pin the session instead.** Every request belonging to one session —
the POST that runs the turn, and the `subscribe` of every tab watching it — lands
on one pod, so the in-process hub is enough and no cross-process hop exists.

Two notes that belong next to that decision, because they are the parts most
likely to be re-litigated:

* **Why the key is the session, not the user.** A session key is strictly finer:
  more buckets, less skew (§5). It is also the identity the hub fans out on
  (`hubKey(userID, agentID, sessionKey)`, `internal/agent/event_hub.go:78`), and
  the identity every participant in that conversation already shares — a second
  browser and a share-link viewer are on the same session without being the same
  client.
* **Why not the cookie affinity the ingress already has.** It is per-browser, not
  per-session: two browsers are two cookies, therefore two pods — precisely the
  case in §1. And per §1 it is inert on our path anyway.

### Rejected alternatives

| Option | Why not |
|---|---|
| Redis pub/sub relay | the standard answer; deferred **for now** on ops cost (§2), not on capability |
| Persist `content_delta` | one row per token — the wrong shape for the table; already decided in the sibling doc's D4 |
| Cookie affinity (already configured) | per-browser granularity, and inert through the proxy (§1) |
| Affinity keyed on the user | fewer buckets ⇒ worse skew; and a share-link viewer's request carries the *owner's* credentials, so the key would have to be the credential owner, not the caller |
| Route the subscriber to the holder ("read follows the lease") | `holder_id` is `"<pod>/<random>"`, minted per acquisition — a **signature, not an address** (`internal/agent/sessionlease.go:126`); pod names change on every rollout and nothing can address one pod |
| Do nothing | a long-lived `EventSource` never re-runs the one-shot replay, so the gap is permanent until a manual reload (sibling doc §1) |

## 3. The mechanism, when it lands

**The key is the `sessionId` query parameter**, not a header. That choice is
forced: `/api/chat/subscribe` is consumed by `EventSource`, which cannot set
request headers at all — and every chat-scoped call already carries `sessionId`
in its query string (`…/chat/subscribe?agentId=…&sessionId=…`, the history/todo
reads, the session routes). So the ingress hashes on `$arg_sessionId` and the
cloud proxy needs **no change**: it already forwards the query string verbatim
when it builds the upstream URL.

1. **client**: the one call that carries the id in its body instead of the URL —
   `POST /api/chat/stream` — also puts it in the query (`?sessionId=<id>`). One
   line per app (reference webui, cloud), no new header, no proxy edit.
2. **ingress**: `nginx.ingress.kubernetes.io/upstream-hash-by: "$arg_sessionId"`,
   and drop the three cookie-affinity annotations (wrong granularity, and inert).
3. **server**: drop the live-only skip on the **hub** branch of the subscription
   loop so deltas reach a watching tab. The tail branch keeps its guard — a delta
   is never in the table, so that branch is a no-op by construction, and the guard
   is what keeps it a no-op if that ever changes.
4. **client**: render `content_delta` on the subscription path. Both apps already
   have the other half of the rule — the "the foreground POST owns this turn, so
   ignore this connection" early return (cloud
   `use-chat-subscription.ts`, webui `inFlightSendSessionRef`) — so the delta
   branch lands behind that guard and only the *watching* tabs take it.

Steps 1 and 3 are the ones with a red test before them (a subscriber on another
replica is a *different* assertion and stays red: it must still never see a
delta). Step 2 is infrastructure, verified against a live cluster: open the same
session in two browsers and check that both subscriptions report the same holder.

> **Known limit of the key.** Requests with no `sessionId` hash on the empty
> string, i.e. they all land on one pod. That covers the non-chat surface
> (admin, skills, files, agent settings) — low-volume and interactive, so the
> cost is a lopsided but tiny load rather than a stall; if it ever matters, the
> fix is a fallback component in the key (a `map` in the controller's
> http-snippet), not a different key.

## 4. What affinity does not cover

**A second consumer of the same mechanism (2026-09-28): `idempotencyKey` checking.**
`docs/fastagent/design/14-turn-identity.md` §3.1 rules that the de-dupe check must run in the
**acceptance critical section** and be valid across replicas and restarts, and the user chose
option (b) — a process-local check — on the explicit condition that session affinity holds:
the ingress hashes `$arg_sessionId` (§3 of this document), so a client's retry with the same
session lands on the pod that accepted the first attempt and therefore on the same checker.
The guarantee that buys is **"at most once per (account, task, authorized client, key) while
that pod's process lives"** — and it fails exactly on the rows of the table below, in the same
way and for the same reason as the queue: a rolling deploy, a restart, or a caller the ingress
cannot pin (no session id, i.e. an empty-session path) turns a retry into a second
instruction. That boundary is declared in both documents rather than papered over; the upgrade
path, if affinity ever stops holding, is the store (a table keyed the same way), not a bigger
map.

| Producer | Covered? | Why |
|---|---|---|
| Browser `POST /chat/stream` | ✅ | goes through the proxy with a session id |
| Any tab's `chat/subscribe` | ✅ | same session ⇒ same pod |
| `delegate_task` sub-agent events | ✅ | they run inside the lease holder's process |
| Empty-session client paths (IM/channel turns, project-level runtime calls) | ⚠️ partial | no session id to hash on; their deltas stay same-pod-only |
| **cron / goal ticks** | ❌ | the producer is whichever pod wins `LockCronJob` — a DB race (`internal/cron/scheduler.go:228`), unrelated to any client identity |
| Rolling deploy | ❌ | every pinned stream breaks when its pod is replaced; the tail is the recovery path |

## 5. What it costs: the capacity consequence

Measured on prod, 2026-09-24 (`kubectl get deploy fastagent-gateway -n production
-o jsonpath='{.spec.template.spec.containers[0].resources}'`, `kubectl top pods`):

| Fact | Value |
|---|---|
| replicas / HPA | 2 (min 2, max 10), `averageUtilization: 60` |
| CPU requests / limits | `250m` / `2` — an **8× burst ratio** |
| idle usage | 1m per pod |

Read together: the HPA target is 150m per pod, i.e. **1.5 cores of aggregate usage
reaches max=10**. With an 8× burst ratio one pegged pod drags the ten-pod average
to ~98%, so "the average hides the hotspot" is *not* the binding problem here. The
binding problem is that **a new replica only takes new sessions** — a hash never
re-reads load. Capacity therefore is *peak concurrent heavy sessions*, and the knob
is `minReplicas`, not the HPA target.

| Lever | How | Cost | Solves |
|---|---|---|---|
| Finer key | session rather than user (§2) | free | reduces skew (more buckets), never removes it |
| Make scaling see the skew | fix `requests` to a real steady state; or scale on **max in-flight turns per pod** instead of mean CPU | one measurement + tuning | growth by *new sessions*; never a single heavy session |
| Admission + fairness | per-pod in-flight cap (the UI already has the `Queued` state) plus a per-**uid** cap so one heavy user cannot starve another on a shared pod | one middleware at the turn lease — which is already the single-writer gate | turns "this pod is pegged" into "new turns queue" — a bounded, visible degradation |
| Drop affinity | a fan-out relay (Redis pub/sub) or persisted deltas | a new component / a new table shape | the only option that lets routing return to plain round-robin |

The missing measurement is the one that would settle this: **per-pod skew** (max
vs. mean CPU, and in-flight turns per pod) is not exported anywhere today, so the
cost of the choice in §2 is currently unobservable.

## 6. Reopen triggers

Affinity stays the decision until one of these is true:

* per-pod max/mean CPU skew stays above a chosen ratio for a full day (needs §5's
  missing metric to exist first);
* peak concurrent heavy sessions approaches `minReplicas`, i.e. a session waits on
  `queued` for capacity rather than for its own sibling turn;
* Redis arrives for an unrelated reason — then the relay's marginal cost collapses
  and the standard practice (§2) is better than a placement constraint;
* `content_delta` stops being the only live-only type: a second one would be
  evidence that the fan-out gap is structural rather than cosmetic.

## 7. Status of the work

Steps 1–9 of §8 are in the tree, red case first, one file per commit:

| Step | Where it landed |
|---|---|
| 1–3 server | fastagent `06740e6` (red: the subscriber got `body=": ok\n\n"`) → `8849ad5` (hub branch no longer skips live-only) → `9112ecc` (the predicate's comment now names the tail) |
| 4–5 webui | fastagent `05ae99d` (red: URL lacked `sessionId`) → `3884256` → `a0c26bc`/`6700fcb` (delta renders on the subscribe path, behind `inFlightSendSessionRef`) |
| 6–7 cloud | `55553323` (red) → `296968ed` → `0c8e054c`/`b9d6e4a9` (same shape, behind `streamingSessionsRef`) |
| 8 cloud doc | `9aacbf69` (D6 → A′) + `b21e4379` (the C4 clause that still contradicted it) |
| 9 ingress | fastagent `27ea0bb` — cookie affinity out, `upstream-hash-by: "$arg_sessionId"` in |

**Step 10 has not run**: nothing is deployed, so production still routes by
round-robin and the behaviour is exactly what
[chat-event-delivery.md](./chat-event-delivery.md) describes — persisted events
arrive cross-pod, `content_delta` does not. Deployment is the single act that
turns the key on; it is also the only step where a mistake reads as "one browser
stopped streaming" instead of as a failed test.

**Verified in dev, 2026-09-24** (`development`, release 86,
`…/fastagent:20260924105957-fastagent-11152df`): the ingress carries
`upstream-hash-by: "$arg_sessionId"` and no cookie annotations, and the hash
behaves — eight distinct session ids, each sampled twice, were **stable per key**
(2/2) and split 6/2 across the two pods. That split is §5's argument in
miniature: a hash is deterministic, not balanced. Requests with no `sessionId`
(`/healthz`, the `/api/skills` reads the cloud worker makes) all landed on one
pod, the known limit noted in §3. The two-client check is the last thing this
list cannot do for us: it needs two signed-in browsers on one session, and the
ingress probes above prove the routing it would be observing.

## 8. Execution plan (files, order, tests)

Order is "inert first": every code step is a no-op until step 9 (the ingress
annotation) turns affinity on, so nothing here can regress today's behaviour on
its own.

| # | Repo / file | Change | Red case first |
|---|---|---|---|
| 1 | fastagent `internal/setup/chat_event_delivery_e2e_test.go` | the hub branch must forward a live-only event | ✅ `TestChatSubscribeForwardsLiveOnlyEventsFromTheHub` (fails: the skip drops it) |
| 2 | fastagent `internal/setup/handlers.go` | delete the hub-branch skip; the comment becomes "the client owns this rule" | — |
| 3 | fastagent `internal/setup/chat_event_tail.go` | comment: the predicate now guards the **tail** only | — |
| 4 | fastagent `web/src/lib/api.ts` | `?sessionId=` on the stream POST | (covered by the harness's URL assertions) |
| 5 | fastagent `web/src/components/chat-screen.tsx` | `content_delta` case on the subscribe path | ✅ `web/src/__tests__/chat-subscribe-content-delta.test.tsx` (watch-only tab renders tokens; owning tab does not) |
| 6 | cloud `src/shared/lib/fastagent/chat.ts` | `?sessionId=` on the stream POST | (same) |
| 7 | cloud `src/features/chat/use-chat-subscription.ts` | `content_delta` branch behind the existing POST-owns guard; rewrite the D6 comment | ✅ `src/__tests__/fastagent/chat-streaming-parity.test.tsx` — the two D6 cases invert: a watching tab renders, the owning tab still does not |
| 8 | cloud `docs/fastagent/design/10-chat-client-parity.md` | D6 is amended, not deleted: *who* renders the delta changed — and C4, the clause D6 feeds, with it | — |
| 9 | fastagent `deploy/helm/fastagent` | the ingress annotation (§3 step 2), cookie affinity out | — |
| 10 | dev cluster | deploy, then a two-browser check on one session | manual |

Steps 1–8 are inert until 9. Step 9 is the one that changes production
behaviour, and step 10 is where a mistake would show up as "one browser stopped
streaming" rather than as a failed test.
