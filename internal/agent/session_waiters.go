package agent

import "sync"

/**
 * [INPUT]: nothing; the agent's own per-session bookkeeping.
 * [OUTPUT]: noteSessionWaiter (returns the release func) and QueuedSubmissions, which the history
 *           read publishes as `queued` and the task surface reports as `waiting`.
 * [POS]: The count has to be taken where the WAITING happens. A turn contends for its session from
 *        the moment it asks for the cross-replica lease until the moment it owns the session, and a
 *        turn parked at the lease gate never reaches `Session.AcquireTurn` — so counting the
 *        session slot, which is what this used to do, reported 0 for exactly the case the field
 *        exists for. Measured 2026-09-28 on dev: an MCP instruction queued behind a holder,
 *        withdrawable, and `queued` still 0.
 * [PROTOCOL]: Per-pod, in-process, like everything else in this queue — the affinity argument
 *        (docs/chat-event-delivery-placement.md §3) is what makes a pod-local count the right one,
 *        and §4 names the rows where that stops holding.
 */

// noteSessionWaiter marks one turn as waiting for its session and returns the function that marks
// it done. The returned func is idempotent: a caller may release it both on the happy path (the
// moment the session is owned) and from a defer (the paths that give up).
func (a *Agent) noteSessionWaiter(sessionKey string) func() {
	if sessionKey == "" {
		return func() {}
	}
	a.waiterMu.Lock()
	if a.sessionWaiters == nil {
		a.sessionWaiters = make(map[string]int)
	}
	a.sessionWaiters[sessionKey]++
	a.waiterMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			a.waiterMu.Lock()
			defer a.waiterMu.Unlock()
			if n := a.sessionWaiters[sessionKey]; n <= 1 {
				delete(a.sessionWaiters, sessionKey)
			} else {
				a.sessionWaiters[sessionKey] = n - 1
			}
		})
	}
}

// queuedSubmissionsFor is the count behind `queued`/`waiting`: how many turns are waiting for this
// session ON THIS POD. The session id is resolved the same way every other reader resolves it, so
// the count answers for the session a caller names, not for the string it typed.
func (a *Agent) queuedSubmissionsFor(sessionID string) int {
	if sessionID == "" {
		sessionID = "web-ui"
	}
	key := a.sessions.ResolveSessionKey(sessionID)
	a.waiterMu.Lock()
	defer a.waiterMu.Unlock()
	return a.sessionWaiters[key]
}
