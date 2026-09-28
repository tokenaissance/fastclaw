package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/workspace"
)

// E2B API: https://e2b.dev/docs
// Sandbox creation: POST https://api.e2b.dev/sandboxes
// Command execution: Connect protocol via envd on the sandbox

const e2bBaseURL = "https://api.e2b.dev"
const e2bEnvdPort = "49983"

// sandboxIdent is the pair every envd request authenticates against: the
// instance id (it selects the endpoint) and that instance's access token.
//
// The two travel as one value because a rebuild swaps them together. Reading
// them separately lets a request be assembled from a new id and the old token
// (or the reverse), which envd answers as an opaque 401/502 — indistinguishable
// from the sandbox being gone, so it would trigger yet another rebuild.
//
// rebuilt rides along in the same value: it means "this identity has not
// reached the shared lease yet". Keeping it in the snapshot means the flag is
// always read with the identity it describes, and the pool's clear step can be
// conditional on that identity (see clearRebuild).
type sandboxIdent struct {
	id      string
	token   string
	rebuilt bool
}

// E2BExecutor implements Executor using E2B hosted sandboxes.
type E2BExecutor struct {
	apiKey string
	// stateMu guards ident. Critical sections are a few field reads — never
	// I/O — so a request building its URL cannot be held up by a rebuild, and
	// a rebuild cannot be observed half-applied.
	stateMu  sync.Mutex
	ident    sandboxIdent
	client   *http.Client
	template string        // remembered for recreate() so the new sandbox uses the same template; immutable once handed out by the pool
	timeout  time.Duration // remembered for recreate()
	// workspaceUnhydrated records that this executor's /workspace could not be
	// hydrated from the store (the listing failed, not "the scope is empty").
	// Policy C: the sandbox is still handed out — a store hiccup must not cost
	// the scope its warm instance — but the caller retries the hydrate on the
	// next use and the tools declare the state to the turn.
	workspaceUnhydrated atomic.Bool
	// workspaceReplaced records that this scope's /workspace was rebuilt from
	// the store after the previous instance died (expiry, or a killed
	// instance). It exists for one reason: the rebuild is otherwise SILENT —
	// the next exec succeeds normally — while anything that lived only inside
	// the old sandbox (a script's changes that were never synced) is gone for
	// good. Consumed once by the lifecycle pool, which turns it into a signal on
	// the next tool result (docs 09, G1/G2).
	workspaceReplaced atomic.Bool
	// readyTimeout / readyInterval bound the post-create readiness wait. Zero
	// means the defaults; tests shrink them so a persistent routing gap does
	// not cost a minute of wall clock.
	readyTimeout  time.Duration
	readyInterval time.Duration
	// rebuildMu serialises recreate(). Parallel tool calls share one executor
	// (the agent loop fans tool calls out concurrently and only exec itself is
	// not serialised), so several goroutines can observe the same dead sandbox
	// and all decide to replace it. Without this lock each one mints its own
	// instance and every instance but the last is stranded: no lease row names
	// it and nothing ever closes it.
	rebuildMu sync.Mutex
	// closeSandboxFn overrides the HTTP DELETE used to destroy a sandbox.
	// Test-only seam so pool unit tests can assert an evicted/adopted-away
	// sandbox was closed without calling the real e2b API. Nil keeps the
	// production behavior.
	closeSandboxFn func(sandboxID string) error
	// createFn mints the replacement sandbox inside recreate(). Defaults to
	// newE2BExecutor; a field (rather than a direct call) so the whole rebuild
	// path — create → hydrate → verify → mark rebuilt — is exercisable offline,
	// and so a pool that injected its own create seam gets it honored on
	// rebuild too instead of silently falling back to the package function.
	createFn func(ctx context.Context, apiKey, template string, timeout time.Duration) (*E2BExecutor, error)
	// hydrate sources — set by the pool after creation so recreate()
	// can rebuild /skills + /workspace without reaching back into the
	// pool. Workspace store is optional; skill dirs may be empty.
	skillDirs []string
	workspace workspace.Store
	agentID   string
	projectID string
	sessionID string
}

// identSnapshot returns a consistent view of the identity a request must use.
func (e *E2BExecutor) identSnapshot() sandboxIdent {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	return e.ident
}

// setIdent publishes a new identity. rebuilt must stay false while the
// replacement is still being hydrated: the pool may route sibling pods to
// whatever the row names, and a half-built sandbox is worse than a dead one.
func (e *E2BExecutor) setIdent(id, token string, rebuilt bool) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	e.ident = sandboxIdent{id: id, token: token, rebuilt: rebuilt}
}

// pendingPublish reports the identity that still has to reach the shared
// lease, if any. See E2BExecutorPool.reconcileLocalLeaseLocked.
func (e *E2BExecutor) pendingPublish() (sandboxIdent, bool) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	return e.ident, e.ident.rebuilt
}

// clearRebuild drops the pending bit — but only when the executor still holds
// the identity that was just published. If a newer rebuild landed while the
// write was in flight, its own bit survives, so the next reconcile moves the
// row again instead of letting it drift from the executor.
func (e *E2BExecutor) clearRebuild(seen sandboxIdent) bool {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	if e.ident.id != seen.id || e.ident.token != seen.token || !e.ident.rebuilt {
		return false
	}
	e.ident.rebuilt = false
	return true
}

func newE2BExecutor(ctx context.Context, apiKey, template string, timeout time.Duration) (*E2BExecutor, error) {
	if template == "" {
		template = "base"
	}
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}

	// No global Client.Timeout: it covers the entire round-trip
	// including streaming the body, which silently cut long execs
	// (image generation, etc.) at 60s and made tools return empty
	// output with no error. We rely on per-request context.WithTimeout
	// at the call site instead — execOnce derives ctx from the user-
	// supplied tool timeout, and create-sandbox below uses an
	// explicit short ctx.
	client := &http.Client{}

	// Field name is `templateID` (camelCase) — verified by server's
	// validation error: `Error at "/templateID": property "templateID"
	// is missing` when the field was renamed to snake_case. The
	// snake_case form shows up in some SDK source code but the
	// production REST API rejects it.
	body := e2bCreateBody(template, timeout)
	// Bound the create-sandbox call to 60s — the call itself usually
	// completes in 1–2s; if it's hanging past that there's a control-
	// plane problem and we'd rather surface a clear timeout than wait
	// indefinitely on a request that inherits no deadline from ctx.
	createCtx, cancelCreate := context.WithTimeout(ctx, 60*time.Second)
	defer cancelCreate()
	req, err := http.NewRequestWithContext(createCtx, "POST", e2bBaseURL+"/sandboxes", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("e2b create sandbox: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("e2b create sandbox: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		SandboxID       string `json:"sandboxID"`
		EnvdAccessToken string `json:"envdAccessToken"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("e2b parse response: %w", err)
	}

	slog.Info("e2b sandbox created", "sandboxID", result.SandboxID, "template", template)

	ex := &E2BExecutor{
		apiKey:   apiKey,
		ident:    sandboxIdent{id: result.SandboxID, token: result.EnvdAccessToken},
		client:   client,
		template: template,
		timeout:  timeout,
		createFn: newE2BExecutor,
	}
	// POST /sandboxes returns an id before e2b's edge can route it. Waiting for
	// the first answer here keeps that gap inside creation, where it is a retry,
	// instead of leaking it into hydrate, where it is indistinguishable from a
	// dead sandbox (see waitUntilRoutable).
	if err := ex.waitUntilRoutable(ctx); err != nil {
		// Do not leak an instance we cannot talk to.
		_ = ex.closeSandboxByID(result.SandboxID)
		return nil, err
	}
	return ex, nil
}

// Defaults for waitUntilRoutable. The 60s ceiling matches the create call's own
// bound; the interval is short enough that a normal sandbox pays it once.
const (
	defaultReadyTimeout  = 60 * time.Second
	defaultReadyInterval = 1500 * time.Millisecond
)

// waitUntilRoutable blocks until a freshly created sandbox answers envd.
//
// Why this exists: POST /sandboxes returns an id before the edge can route it.
// The first call after create — hydrate's /files upload — is what pays for the
// gap, and it gets the edge's
//
//	{"sandboxId":...,"message":"The sandbox was not found","code":502}
//
// which is byte-identical to the answer for a sandbox that died long ago. That
// made a transient routing gap look like a dead instance, and the caller's
// recovery — rebuild — could never converge, because every rebuild created
// another sandbox and hit the same window.
//
// Only "not routable yet" is retried: a provider status that is not 502/404
// (a 401 from a stale token, a 500 inside the sandbox) is returned at once,
// because rebuilding cannot fix it and the caller should see it immediately.
// Transport errors are retried too — a brand-new hostname may not resolve yet.
func (e *E2BExecutor) waitUntilRoutable(ctx context.Context) error {
	timeout := e.readyTimeout
	if timeout <= 0 {
		timeout = defaultReadyTimeout
	}
	interval := e.readyInterval
	if interval <= 0 {
		interval = defaultReadyInterval
	}
	deadline := time.Now().Add(timeout)
	started := time.Now()

	for attempt := 1; ; attempt++ {
		_, err := e.execOnce(ctx, "true", 15*time.Second)
		if err == nil {
			// Logged even when the first attempt works: "how long does e2b take
			// to route a fresh id" is the question this wait exists to answer,
			// and a silent success would leave nothing to measure.
			slog.Info("e2b sandbox routable",
				"sandboxID", e.identSnapshot().id,
				"attempts", attempt,
				"elapsedMs", time.Since(started).Milliseconds())
			return nil
		}
		if _, isProviderVerdict := statusCodeOf(err); isProviderVerdict && !sandboxGone(err) {
			return fmt.Errorf("sandbox %s is not usable after create: %w", e.identSnapshot().id, err)
		}
		if time.Now().After(deadline) {
			slog.Warn("e2b sandbox never became routable",
				"sandboxID", e.identSnapshot().id,
				"attempts", attempt,
				"elapsedMs", time.Since(started).Milliseconds(),
				"error", err)
			return fmt.Errorf("sandbox %s never became routable within %s (%d attempts): %w",
				e.identSnapshot().id, timeout, attempt, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (e *E2BExecutor) envdURLFor(sandboxID string) string {
	return fmt.Sprintf("https://%s-%s.e2b.app", e2bEnvdPort, sandboxID)
}

// Pause puts the instance to sleep: e2b snapshots filesystem and memory (running
// processes included) and keeps it, unbilled and outside the concurrency limit,
// until something resumes it. This is the control-plane counterpart of the
// autoPause option on create — the same transition, requested early by the idle
// sweep instead of at the instance's own timeout.
//
// https://docs.e2b.dev/api-reference/sandboxes/pause-sandbox.md
func (e *E2BExecutor) Pause(ctx context.Context, sandboxID string) error {
	if sandboxID == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/sandboxes/%s/pause", e2bBaseURL, sandboxID), nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", e.apiKey)
	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("e2b pause sandbox %s: %w", sandboxID, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return &sandboxHTTPError{op: "e2b pause " + sandboxID, status: resp.StatusCode, body: string(body)}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &sandboxHTTPError{op: "e2b pause " + sandboxID, status: resp.StatusCode, body: string(body)}
	}
	slog.Info("e2b sandbox paused", "sandboxID", sandboxID)
	return nil
}

// ExtendTimeout pushes the instance's expiry out to now+d. Called before an
// operation long enough to straddle that expiry: with autoPause on, an expiry
// mid-exec pauses the sandbox and cuts our stream, which the caller experiences
// as a truncated response even though the process itself survives in the
// snapshot.
//
// e2b rewrites the TTL from the time of the request, so the caller must pass
// the whole budget it needs (see LifecyclePool.extendBudget), not an increment.
//
// https://docs.e2b.dev/api-reference/sandboxes/set-sandbox-timeout.md
func (e *E2BExecutor) ExtendTimeout(ctx context.Context, sandboxID string, d time.Duration) error {
	if sandboxID == "" || d <= 0 {
		return nil
	}
	body, _ := json.Marshal(map[string]any{"timeout": int(d.Seconds())})
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/sandboxes/%s/timeout", e2bBaseURL, sandboxID), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", e.apiKey)
	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("e2b extend timeout %s: %w", sandboxID, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	// 204: the API's documented success for this call.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &sandboxHTTPError{op: "e2b extend timeout " + sandboxID, status: resp.StatusCode, body: string(respBody)}
	}
	// Logged because it is the only evidence that a long operation protected
	// itself: without it an operator cannot tell an extended expiry from one
	// that was never needed.
	slog.Info("e2b sandbox timeout extended", "sandboxID", sandboxID, "timeoutSec", int(d.Seconds()))
	return nil
}

// Connect resumes the sandbox if it is paused and returns the envd access token
// from the response. e2b returns the full `Sandbox` schema here, so this is also
// how a token is refreshed: a secure sandbox's token can change across a pause,
// and the row must be brought up to date before a sibling adopts it.
//
// The timeout in the request extends the TTL ("TTL is only extended"), which is
// why this doubles as the "keep it alive" call.
//
// https://docs.e2b.dev/api-reference/sandboxes/connect-to-sandbox.md
func (e *E2BExecutor) Connect(ctx context.Context, sandboxID string, ttl time.Duration) (string, error) {
	if sandboxID == "" {
		return "", nil
	}
	if ttl <= 0 {
		ttl = e.timeout
	}
	body, _ := json.Marshal(map[string]any{"timeout": int(ttl.Seconds())})
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/sandboxes/%s/connect", e2bBaseURL, sandboxID), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", e.apiKey)
	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("e2b connect sandbox %s: %w", sandboxID, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	// 200 = already running, 201 = resumed.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", &sandboxHTTPError{op: "e2b connect " + sandboxID, status: resp.StatusCode, body: string(raw)}
	}
	if resp.StatusCode == http.StatusCreated {
		// 201 = the sandbox was paused and is now running again; 200 = it was
		// already running. The distinction is what makes a pause→resume cycle
		// visible in the log.
		slog.Info("e2b sandbox resumed", "sandboxID", sandboxID)
	}
	var out struct {
		EnvdAccessToken string `json:"envdAccessToken"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("e2b connect %s: parse response: %w", sandboxID, err)
	}
	return out.EnvdAccessToken, nil
}

// staleEnvdToken reports whether an envd error looks like a token that has been
// superseded rather than a sandbox that is gone. e2b answers 401 for a token it
// no longer accepts; the repair is to reconnect and publish the new one, not to
// rebuild the instance.
func staleEnvdToken(err error) bool {
	status, ok := statusCodeOf(err)
	return ok && status == http.StatusUnauthorized
}

// refreshEnvdToken reconnects to obtain the current token and records it as
// pending publication: the row is updated by the next reconcile, through the
// same path a rebuild uses, so no caller has to know the token changed.
func (e *E2BExecutor) refreshEnvdToken(ctx context.Context, observed sandboxIdent) error {
	token, err := e.Connect(ctx, observed.id, e.timeout)
	if err != nil {
		return err
	}
	if token == "" {
		return fmt.Errorf("e2b connect %s returned no envd token", observed.id)
	}
	e.setIdent(observed.id, token, true)
	slog.Info("e2b envd token refreshed", "sandboxID", observed.id)
	return nil
}

// e2bCreateBody is the POST /sandboxes payload. It lives in its own function
// because these fields are a design decision rather than incidental JSON, and
// the wire shape should be assertable without a network seam.
//
// The three lifecycle fields change what a routine timeout MEANS. Left at the
// provider defaults the sandbox is killed when its 30 minutes are up, so an
// ordinary expiry arrives at our code as `502 sandbox not found` and the
// recovery is the whole rebuild path: create a new instance, re-upload the
// hydrate bundle, move the lease row onto the new id. With autoPause the same
// expiry produces a paused sandbox — full memory snapshot, running processes
// included — and with autoResume the next request wakes it. Paused sandboxes
// are not billed, do not count toward the concurrency limit, and are kept
// indefinitely, so a scope stops minting a new instance every half hour.
//
// https://docs.e2b.dev/sandbox/auto-resume.md
// https://docs.e2b.dev/faq/paused-sandboxes-concurrency.md
func e2bCreateBody(template string, timeout time.Duration) []byte {
	body, _ := json.Marshal(map[string]interface{}{
		"templateID": template,
		"timeout":    int(timeout.Seconds()),
		"autoPause":  true,
		// Provider default; sent explicitly so the intent survives a change of
		// default. false would persist the filesystem only, which cannot be
		// woken by traffic and must be resumed explicitly.
		"autoPauseMemory": true,
		"autoResume":      map[string]interface{}{"enabled": true},
		// envd authenticates with a per-sandbox access token, which e2b only
		// issues for secure sandboxes ("Null for non-secure sandboxes (envd
		// endpoints work without auth)"). Without it anyone holding the sandbox
		// id can reach envd — and we hand the id out: preview URLs are built
		// from it, the lease row stores it, the logs print it. The token is
		// carried like every other credential here: encrypted in the row,
		// refreshed from the connect/resume response when it goes stale.
		"secure": true,
	})
	return body
}

// recreateIfCurrent replaces the sandbox the caller observed failing. The same
// template / timeout the executor was originally built with are reused —
// hardcoding "base" here would silently demote a custom-template sandbox once
// it idled out. The full hydrate (skills + workspace) is replayed so
// /skills/<name>/ and /workspace/ stay populated across recreations.
//
// observed is the identity the failed request actually used, which makes this
// idempotent under concurrency: parallel calls that all watched the same
// sandbox die queue on rebuildMu, and every waiter but the first finds the
// executor already pointing somewhere else and returns without creating
// anything.
func (e *E2BExecutor) recreateIfCurrent(ctx context.Context, observed sandboxIdent, cause error) error {
	e.rebuildMu.Lock()
	defer e.rebuildMu.Unlock()

	if cur := e.identSnapshot(); cur.id != observed.id {
		slog.Info("e2b rebuild already performed by a parallel call",
			"observedSandboxID", observed.id, "currentSandboxID", cur.id)
		return nil
	}
	started := time.Now()
	create := e.createFn
	if create == nil {
		create = newE2BExecutor
	}
	// cause is the whole forensic value of this line: it is the only record of
	// WHAT the instance answered. The body is what separates a dead instance from
	// a live one answering about a path, and when the 09-22 rebuilds were being
	// diagnosed this field did not exist — the first attempt's answer had to be
	// inferred from the timestamps around it.
	slog.Info("e2b sandbox expired, recreating", "oldSandboxID", observed.id, "error", cause)
	newEx, err := create(ctx, e.apiKey, e.template, e.timeout)
	if err != nil {
		return err
	}
	// Swap the identity before hydrating (the hydrate calls must land on the
	// replacement) but leave the pending bit clear: the pool may only publish
	// this identity once the sandbox below is proven usable.
	replacement := newEx.identSnapshot()
	e.setIdent(replacement.id, replacement.token, false)

	// Hydrate is mandatory for the same reason it is in Get() — it's
	// the only step that makes /workspace writable to the exec user.
	// If we just warn-log a failure here, the recreated sandbox is
	// silently broken: agent calls succeed but every /workspace write
	// fails with Permission denied, and the bytes get stranded in
	// /tmp where they evaporate at the next eviction.
	if err := e.Hydrate(ctx); err != nil {
		return e.abandonRebuild(observed, replacement.id,
			fmt.Errorf("hydrate after recreate (sandboxID=%s): %w", replacement.id, err))
	}
	if err := verifyWorkspaceWritable(ctx, e); err != nil {
		return e.abandonRebuild(observed, replacement.id,
			fmt.Errorf("recreated sandbox unusable (sandboxID=%s): %w", replacement.id, err))
	}
	// The shared lease still names the sandbox that just died. Mark the
	// executor so the pool moves the row onto this replacement at the next Get
	// (reconcileLocalLeaseLocked) — the pool owns the lease, so the executor
	// only records the fact and stays free of SQL and scope bookkeeping.
	e.setIdent(replacement.id, replacement.token, true)
	// One line per rebuild, with both ids and how long it took. Counting
	// instances in the provider dashboard is otherwise guesswork: cold creates
	// and rebuilds both log "e2b sandbox created", and only this line says which
	// dead instance each new one replaced.
	slog.Info("e2b sandbox rebuilt",
		"oldSandboxID", observed.id,
		"newSandboxID", replacement.id,
		"elapsedMs", time.Since(started).Milliseconds())
	// The replacement is live and hydrated; the old instance (and anything that
	// existed only inside it) is gone. Hand that fact to the caller.
	e.workspaceReplaced.Store(true)
	return nil
}

// TakeWorkspaceReplaced states — once — whether this scope's workspace was
// rebuilt since the last call, so the caller can tell the agent that its sandbox
// was swapped and unsynced sandbox-side work is gone. It is the delta source
// behind the replacement signal, not the signal itself.
func (e *E2BExecutor) TakeWorkspaceReplaced() bool { return e.workspaceReplaced.Swap(false) }

// abandonRebuild undoes a replacement that was created but never became
// usable. Without it the executor keeps a sandbox that cannot serve while the
// lease row still names the dead one, so the next reconcile reads the mismatch
// as "another replica took over", adopts the corpse back and closes the
// replacement — a create + close cycle on every call.
//
// Restoring the previous identity keeps memory and row agreeing, and
// destroying the replacement keeps a failed rebuild from leaking an instance
// nothing references. The cause is returned unchanged so callers still see
// why the rebuild failed; the next call retries from a consistent state.
func (e *E2BExecutor) abandonRebuild(prev sandboxIdent, failedSandboxID string, cause error) error {
	e.setIdent(prev.id, prev.token, false)
	slog.Warn("e2b rebuild abandoned",
		"failedSandboxID", failedSandboxID,
		"restoredSandboxID", prev.id,
		"error", cause)
	if cerr := e.closeSandboxByID(failedSandboxID); cerr != nil {
		slog.Warn("e2b could not destroy the unusable replacement sandbox",
			"sandboxID", failedSandboxID, "error", cerr)
	}
	return cause
}

// SetHydrationSources records the inputs Hydrate() should pull from on
// the next call. Called by the pool right after sandbox creation; the
// executor then carries them so recreate() can replay everything without
// asking the pool. Pass nil/empty for any source you don't have.
func (e *E2BExecutor) SetHydrationSources(skillDirs []string, ws workspace.Store, agentID, projectID, sessionID string) {
	e.skillDirs = append(e.skillDirs[:0], skillDirs...)
	e.workspace = ws
	e.agentID = agentID
	e.projectID = projectID
	e.sessionID = sessionID
}

// Hydrate populates the sandbox with everything the agent's tools expect
// to find on disk:
//   - /skills/<name>/...   from each configured skill dir (per-agent +
//     global, first-wins precedence to match the docker bind-mount layer)
//   - /workspace/...       from the agent's workspace.Store (so files
//     written via write_file in past sessions survive sandbox restarts,
//     same contract as the existing per-file hydrateWorkspace)
//
// Implementation: pack everything into one tar.gz, upload it via envd's
// /files multipart endpoint (same path writeFileOnce uses — known good),
// then run a tiny `bash -c` to extract+chown. Earlier versions inlined
// the base64'd tar inside the `bash -c` arg; that worked for trivially
// small bundles but empirically truncated the Connect-protocol response
// once the encoded payload climbed past ~80KB-of-base64 (8 bundled
// skills was already 103KB and tripped it). Truncation surfaced as an
// envd End-frame with `exited=false` and no exit-status — the old code
// treated that as success because exitCode was still 0, so Hydrate
// looked like it ran while the chown step had actually been cut off
// mid-flight. The fix moves the bulk transfer off the exec channel
// entirely so the script stays small and constant-sized regardless of
// bundle size.
// listWorkspaceWithRetry bounds the cost of a flaky store listing. The prod
// failure of 2026-09-16 was an intermittent `context deadline exceeded`, which
// a retry is exactly the right answer for; the attempts and interval mirror the
// hydrate exec loop's, so a slow store is treated the same in both phases.
// hydrateWorkspaceEntries copies the listed workspace objects into the bundle
// and returns how many made it.
//
// Per-file failures stay best-effort — one unreadable object must not sink the
// whole hydrate — but the SHORTFALL is not allowed to be silent. "Listed 1228"
// and "bundled 1228" are two different claims, and until now nothing sat
// between them: a failed Get/Read/tar was a warn and a `continue`, so a partial
// archive went out looking exactly like a complete one
// (docs/sandbox-scope-leak.md §5 P1, §9.3). A shortfall marks the executor
// unhydrated, the same signal a failed listing raises, so the scope lands on
// the existing rebuild path.
//
// Extracted from Hydrate so the invariant is testable without a live sandbox.
func (e *E2BExecutor) hydrateWorkspaceEntries(
	ctx context.Context, objs []workspace.ObjectInfo, bundle *tarBundle,
	projectID, sessionID string,
) int {
	bundled := 0
	for _, obj := range objs {
		rc, err := e.workspace.Get(ctx, e.agentID, projectID, sessionID, obj.Path)
		if err != nil {
			slog.Warn("e2b hydrate: workspace get", "path", obj.Path, "error", err)
			continue
		}
		data, rerr := io.ReadAll(rc)
		rc.Close()
		if rerr != nil {
			slog.Warn("e2b hydrate: workspace read", "path", obj.Path, "error", rerr)
			continue
		}
		rel := strings.TrimPrefix(obj.Path, "/")
		if err := bundle.addBytes("workspace/"+rel, data, 0o644, obj.ModTime); err != nil {
			slog.Warn("e2b hydrate: workspace tar", "path", obj.Path, "error", err)
			continue
		}
		bundled++
	}
	if bundled != len(objs) {
		slog.Warn("e2b hydrate: workspace bundle is INCOMPLETE — some files could not be read into the archive",
			"agent", e.agentID, "project", projectID, "session", sessionID,
			"listed", len(objs), "bundled", bundled, "skipped", len(objs)-bundled)
		e.workspaceUnhydrated.Store(true)
	}
	return bundled
}

// listWorkspaceWithRetry bounds the cost of a flaky store listing. The prod
// failure of 2026-09-16 was an intermittent `context deadline exceeded`, which
// a retry is exactly the right answer for; the attempts and interval mirror the
// hydrate exec loop's, so a slow store is treated the same in both phases.
func (e *E2BExecutor) listWorkspaceWithRetry(ctx context.Context, listProject, listSession string) ([]workspace.ObjectInfo, error) {
	for attempt := 1; ; attempt++ {
		objs, err := e.workspace.List(ctx, e.agentID, listProject, listSession)
		if err == nil {
			return objs, nil
		}
		if attempt >= hydrateAttempts {
			return nil, err
		}
		slog.Warn("e2b hydrate: workspace list failed, retrying",
			"agent", e.agentID, "project", e.projectID, "session", e.sessionID,
			"attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(hydrateRetryInterval):
		}
	}
}

// WorkspaceUnhydrated reports whether this executor's /workspace could not be
// hydrated from the store — "the listing failed", never "the scope is empty".
// Policy C reads it so the scope's hydrated flag stays false and the next use
// retries (docs/sandbox-scope-leak.md §9).
func (e *E2BExecutor) WorkspaceUnhydrated() bool { return e.workspaceUnhydrated.Load() }

// setWorkspaceUnhydrated carries the "never filled from the store" fact onto
// this executor. The one caller that must set it is adoption: hydration is not
// replayed for an instance another pod created, so the only place that fact can
// come from is the lease row it was published with (docs 10 §4, G19).
func (e *E2BExecutor) setWorkspaceUnhydrated(v bool) { e.workspaceUnhydrated.Store(v) }

// UnhydratedWorkspace is the read side of Policy C: a sandbox whose /workspace
// never got filled from the store because the listing failed, so it may hold no
// files at all — not "old files". Implemented twice on the way out to the
// tools: by the executor itself (what this instance's hydrate achieved) and by
// the lifecycle pool's lazy executor (what the scope's current instance came up
// with, which the tools read to declare the state into the turn). See
// docs/sandbox-scope-leak.md §9.2 and §9.5.
type UnhydratedWorkspace interface {
	WorkspaceUnhydrated() bool
}

func (e *E2BExecutor) Hydrate(ctx context.Context) error {
	bundle := newTarBundle()

	skillCount := 0
	skillFileCount := 0
	seen := make(map[string]bool) // per-skill: first dir wins
	for _, dir := range e.skillDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := entry.Name()
			if seen[name] {
				continue
			}
			seen[name] = true
			n, err := bundle.addLocalDir(filepath.Join(dir, name), "skills/"+name)
			if err != nil {
				slog.Warn("e2b hydrate: skill tar", "skill", name, "error", err)
				continue
			}
			skillCount++
			skillFileCount += n
		}
	}

	workspaceCount := 0
	if e.workspace != nil {
		// For project chats, hydrate the whole project (List with
		// session=""), so the chat sees sibling chats' files at
		// /workspace/<other-sid>/... — same visibility docker gets
		// from mounting projects/<pid>/ as the bind root. Loose chats
		// stay scoped to their own session subtree.
		listProject := e.projectID
		listSession := e.sessionID
		if e.projectID != "" {
			listSession = ""
		}
		// The listing is the one step whose failure must not be swallowed: a
		// hydrate that lists nothing uploads an EMPTY workspace, which is
		// indistinguishable from "this scope has no files" — and that is how a
		// run ends up acting on an empty /workspace (see
		// docs/sandbox-scope-leak.md §8, prod 2026-09-16). Retry it, and if it
		// still fails say so loudly and mark the executor UNHYDRATED —
		// "never filled", not "holding old files" (see §9.6 on the naming) —
		// instead of pretending the workspace is empty.
		objs, err := e.listWorkspaceWithRetry(ctx, listProject, listSession)
		if err != nil {
			slog.Warn("e2b hydrate: workspace list failed after retries — handing out an EMPTY workspace",
				"agent", e.agentID, "project", e.projectID, "session", e.sessionID,
				"attempts", hydrateAttempts, "error", err)
			e.workspaceUnhydrated.Store(true)
		} else {
			workspaceCount += e.hydrateWorkspaceEntries(ctx, objs, bundle, listProject, listSession)
		}
	}

	if err := bundle.close(); err != nil {
		return fmt.Errorf("close tar: %w", err)
	}

	// Why every word here matters:
	// - We ALWAYS run the mkdir+chown step, even when there are no
	//   files to push. /workspace and /skills must exist and be
	//   writable by `user` regardless — image-tool / write_file /
	//   anything that writes there fails with ENOENT or EACCES if the
	//   dirs are missing. This was the failure mode on a fresh session
	//   with empty workspace: no files → previous code returned early
	//   → /workspace never created → "mkdir: Permission denied" when
	//   the LLM tried to make it itself as the non-root `user`.
	// - `sudo`: E2B's "base" template runs as `user`, who has no
	//   write access to /. The default user has passwordless sudo per
	//   e2b's published Dockerfile; custom templates that strip sudo
	//   either need to keep it or pre-create /skills + /workspace
	//   chowned to user.
	// - The bundle (when any files exist) is now uploaded via the
	//   /files endpoint BEFORE this exec runs — see uploadBytes above
	//   and the size-related comment on Hydrate itself. The script
	//   below only ever references /tmp/fc-hydrate.tar.gz as a path,
	//   so the bash -c arg stays small regardless of how large the
	//   bundle gets.
	// - chown after extract: tar-as-root lands files root-owned, so
	//   re-chown after extract; agent's subsequent writes run as user.
	cmdParts := []string{
		"set -e",
		"sudo mkdir -p /skills /workspace",
		"sudo chown user:user /skills /workspace",
	}
	if bundle.fileCount > 0 {
		cmdParts = append(cmdParts,
			"sudo tar -xzf /tmp/fc-hydrate.tar.gz -C /",
			"sudo chown -R user:user /skills /workspace",
			"rm -f /tmp/fc-hydrate.tar.gz",
		)
	}
	cmd := strings.Join(cmdParts, "; ")
	// Ship it, with a bounded retry. A sandbox created moments ago can cut a
	// stream once while it finishes booting — seen in production as "did not
	// exit cleanly … response stream truncated" from the mkdir/chown step, and
	// as a 502 from the upload before it. Both network steps are idempotent
	// (same tar to the same path, same mkdir/chown), and anything a retry
	// cannot fix — a 401, a permission error, an instance that is simply gone —
	// is not retried. The bundle is built once, above: assembling it walks the
	// workspace store.
	hydrateStarted := time.Now()
	var err error
	for attempt := 1; attempt <= hydrateAttempts; attempt++ {
		err = e.shipBundleOnce(ctx, cmd, bundle)
		if err == nil {
			break
		}
		if attempt == hydrateAttempts || !sandboxUnusable(err) {
			break
		}
		slog.Warn("retrying hydrate against a freshly created sandbox",
			"sandboxID", e.identSnapshot().id, "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(hydrateRetryInterval):
		}
	}
	if err != nil {
		slog.Warn("e2b hydrate failed",
			"sandboxID", e.identSnapshot().id, "error", err,
			"elapsedMs", time.Since(hydrateStarted).Milliseconds())
		return err
	}
	slog.Info("e2b sandbox hydrated",
		"sandboxID", e.identSnapshot().id,
		// The scope, not the listing scope: project chats hydrate with
		// session="" (they pull the whole project) while the scope key keeps the
		// session, and it is the scope an operator correlates against.
		"scopeKey", poolKey(e.agentID, e.projectID, e.sessionID),
		"skills", skillCount,
		"skillFiles", skillFileCount,
		"workspaceFiles", workspaceCount,
		"tarBytes", bundle.gz.Len(),
		"elapsedMs", time.Since(hydrateStarted).Milliseconds())
	return nil
}

// Hydrate's bounded retry. See shipBundleOnce for why a retry is safe and
// sandboxUnusable for what is allowed to use it.
const (
	hydrateAttempts      = 3
	hydrateRetryInterval = 1500 * time.Millisecond
)

// shipBundleOnce uploads the tar (when there is one) and runs the
// mkdir/chown/extract script. Idempotent by construction, which is what makes
// the retry in Hydrate safe.
func (e *E2BExecutor) shipBundleOnce(ctx context.Context, cmd string, bundle *tarBundle) error {
	if bundle.fileCount > 0 {
		// Upload as `user`-owned to /tmp; tar still runs under sudo so
		// it can land /skills/* and /workspace/* at the filesystem root.
		if err := e.uploadBytes(ctx, "/tmp/fc-hydrate.tar.gz", bundle.gz.Bytes()); err != nil {
			return fmt.Errorf("hydrate upload tar: %w", err)
		}
	}
	out, err := e.execOnce(ctx, cmd, 60*time.Second)
	if err != nil {
		return fmt.Errorf("hydrate sandbox dirs: %w (output: %s)", err, out)
	}
	return nil
}

// tarBundle is a small helper around archive/tar + gzip so the Hydrate
// path doesn't have to repeat the writer-close dance. All paths in the
// bundle are sandbox-relative (no leading slash); callers pick the
// extraction root.
type tarBundle struct {
	gz        bytes.Buffer
	gw        *gzip.Writer
	tw        *tar.Writer
	fileCount int
}

func newTarBundle() *tarBundle {
	b := &tarBundle{}
	b.gw = gzip.NewWriter(&b.gz)
	b.tw = tar.NewWriter(b.gw)
	return b
}

// addBytes adds an in-memory file to the bundle. tar -xz auto-creates
// parent dirs from the entry path, so we don't need explicit dir
// entries — verified by a roundtrip test on the host.
func (b *tarBundle) addBytes(name string, data []byte, mode int64, modTime time.Time) error {
	if modTime.IsZero() {
		modTime = time.Now()
	}
	if err := b.tw.WriteHeader(&tar.Header{
		Name:     strings.TrimPrefix(name, "/"),
		Mode:     mode,
		Size:     int64(len(data)),
		Typeflag: tar.TypeReg,
		ModTime:  modTime,
	}); err != nil {
		return err
	}
	if _, err := b.tw.Write(data); err != nil {
		return err
	}
	b.fileCount++
	return nil
}

// addLocalDir walks a host directory and adds every regular file under
// it to the bundle, rooted at sandboxPrefix. Symlinks / sockets / etc.
// are skipped — a skill bundle should be plain files.
func (b *tarBundle) addLocalDir(localRoot, sandboxPrefix string) (int, error) {
	count := 0
	err := filepath.Walk(localRoot, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(localRoot, p)
		if err != nil {
			return err
		}
		// Never ship a skill's top-level SKILL.md into the sandbox — it's
		// the agent's IP, the model already has it via load_skill, and the
		// sandbox only needs the skill's scripts/resources to run. Keeping
		// it out closes the `cat /skills/<name>/SKILL.md` exfil path.
		if rel == "SKILL.md" {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		name := sandboxPrefix + "/" + filepath.ToSlash(rel)
		if err := b.addBytes(name, data, int64(info.Mode().Perm()), info.ModTime()); err != nil {
			return err
		}
		count++
		return nil
	})
	return count, err
}

func (b *tarBundle) close() error {
	if err := b.tw.Close(); err != nil {
		return err
	}
	return b.gw.Close()
}

// isSandboxGone checks if a status code means the sandbox itself is gone:
// e2b answers 502 for an instance it has already reaped and 404 for one it
// never had.
func isSandboxGone(statusCode int) bool {
	return statusCode == http.StatusBadGateway || statusCode == http.StatusNotFound
}

// sandboxGone reports whether err says the instance no longer exists — the
// only failure a rebuild can cure. Anything else (a 401 from a stale token, a
// 500 inside the sandbox) must surface to the caller instead of costing a
// sandbox.
func sandboxGone(err error) bool {
	status, ok := statusCodeOf(err)
	return ok && isSandboxGone(status)
}

// sandboxGoneOnFileAPI is sandboxGone for envd's file endpoints, which answer a
// missing PATH with a 404 of their own:
//
//	{"code":404,"message":"path '/home/user/CURRENT.md' does not exist"}
//
// Believing that as "the instance is gone" replaced a live sandbox over a file
// that simply was not there — production 09-22, three rebuilds in 21 minutes,
// the first triggered by two read_file calls for missing paths. Each rebuild
// hydrated a replacement whose workspace held only the persisted files, so the
// retry answered from an instance that really did not have them and the model
// was told the file it had just written was gone.
//
// A 502 is still the edge saying the instance is gone, and so is a 404 that does
// not name a path: an answer this classifier does not recognise stays on the
// safe side, which is one rebuild.
func sandboxGoneOnFileAPI(err error) bool {
	status, ok := statusCodeOf(err)
	if !ok {
		return false
	}
	if status != http.StatusNotFound {
		return isSandboxGone(status)
	}
	var httpErr *sandboxHTTPError
	if errors.As(err, &httpErr) && bodyNamesAPath(httpErr.body) {
		return false
	}
	return true
}

// bodyNamesAPath reports whether an envd error body is a verdict about a path
// rather than about the instance. Only the shape measured against the provider is
// recognised; anything else falls back to the instance-level reading.
func bodyNamesAPath(body string) bool {
	var payload struct {
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(body), &payload) != nil {
		return false
	}
	return strings.Contains(payload.Message, "does not exist")
}

// sandboxUnusable reports whether err indicts the INSTANCE rather than the
// command that ran on it: the sandbox is gone (502/404 from the edge), or the
// exec stream ended without its exit-status trailer.
//
// Two callers ask this one question for two different reasons. Hydrate retries
// it, because a sandbox created moments ago can cut one stream while it finishes
// booting and every step of a hydrate is idempotent. The lifecycle layer
// replaces the instance over it, because a sandbox that cuts a stream for an
// ordinary tool call is broken for every subsequent call
// (docs/sandbox-scope-leak.md §7.4).
//
// Deliberately narrow — NOT "the command exited non-zero", and not a 401 or a
// permission error. Those are verdicts: retrying them delays the report, and
// destroying a healthy sandbox over one would re-run side effects the command
// already had.
func sandboxUnusable(err error) bool {
	if err == nil {
		return false
	}
	if sandboxGone(err) {
		return true
	}
	var truncated *execStreamTruncatedError
	return errors.As(err, &truncated)
}

// Unusable implements the lifecycle-side UnusableClassifier, so the policy layer
// can act on "this instance is bad" without knowing e2b's error shapes. Only the
// question crosses the boundary; the vocabulary stays here.
func (p *E2BExecutorPool) Unusable(err error) bool { return sandboxUnusable(err) }

// connectEnvelope wraps JSON payload in Connect protocol envelope framing.
// Format: [1 byte flags][4 bytes big-endian length][payload]
func connectEnvelope(payload []byte) []byte {
	buf := make([]byte, 5+len(payload))
	buf[0] = 0 // flags: no compression, not end of stream
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)
	return buf
}

// connectFrameReader walks the Connect streaming frame layout —
// [1 byte flags][4 bytes length][payload] — straight off the response body, one
// frame at a time.
//
// It used to be a function over a []byte: the caller read the whole body (101 MB
// in the 2026-09-14 incident, base64 of a 76 MB tar) and then carved frames out
// of it, so the peak was the body plus the decoded output plus its copies. Here
// a frame is the unit: the payload buffer is reused, and the only bytes that
// survive are the ones the sink decided to keep.
type connectFrameReader struct {
	r     io.Reader
	frame []byte // reused payload buffer
	// bytes is how much arrived on the wire. It is what the diagnostics report
	// as bodyBytes / "got N bytes", and it is deliberately not the size of
	// anything we kept.
	bytes int
	// sniff is the first few wire bytes, the only fallback left for a stream
	// that died before a single complete frame arrived.
	sniff []byte
}

// connectMaxFrame bounds a length prefix before it is trusted. Real frames are
// tens of KB; this is only here so a corrupt header cannot ask for a 4 GB
// allocation.
const connectMaxFrame = 32 << 20

const connectSniffBytes = 300

func newConnectFrameReader(r io.Reader) *connectFrameReader { return &connectFrameReader{r: r} }

// next returns the next message frame, or a trailer when the frame carried the
// end-stream flag. io.EOF means the stream ended on a frame boundary;
// io.ErrUnexpectedEOF means it ended inside one. Both are how envd says "no
// exit status", so the caller treats them as an end, not as a transport fault.
func (c *connectFrameReader) next() (payload []byte, trailer bool, err error) {
	var hdr [5]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return nil, false, err
	}
	c.note(hdr[:])

	length := int(binary.BigEndian.Uint32(hdr[1:5]))
	if length < 0 || length > connectMaxFrame {
		return nil, false, fmt.Errorf("connect frame of %d bytes", length)
	}
	if cap(c.frame) < length {
		c.frame = make([]byte, length)
	}
	c.frame = c.frame[:length]
	if _, err := io.ReadFull(c.r, c.frame); err != nil {
		return nil, false, err
	}
	c.note(c.frame)
	return c.frame, hdr[0]&0x02 != 0, nil
}

func (c *connectFrameReader) note(b []byte) {
	c.bytes += len(b)
	if n := min(connectSniffBytes-len(c.sniff), len(b)); n > 0 {
		c.sniff = append(c.sniff, b[:n]...)
	}
}

// connectErrorFrom returns the server's own error text from an end-stream
// frame, if it sent one. Trailers are not skipped: they are the one place the
// protocol carries a server-side error, and throwing them away left callers
// with "the stream was truncated" and no idea why.
func connectErrorFrom(raw []byte) string {
	var envelope struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Error == nil {
		return ""
	}
	if envelope.Error.Code != "" && envelope.Error.Message != "" {
		return envelope.Error.Code + ": " + envelope.Error.Message
	}
	if envelope.Error.Message != "" {
		return envelope.Error.Message
	}
	return string(raw)
}

// execOutput is where a command's output goes as the stream delivers it. The
// two implementations are the two contracts on this transport: a tool result is
// bounded (head+tail, middle dropped), a machine payload is capped and refused.
// A write error stops the read at once.
type execOutput interface {
	writeStdout(b []byte) error
	writeStderr(b []byte) error
	// produced is the pre-bound size, for the completion log and the marker.
	produced() int
	// text renders what the caller gets. Safe to call only after the read.
	text() string
}

// execStreamTruncatedError is an exec response that ended without its
// exit-status trailer. It is a distinct type so callers can decide whether the
// failure is worth retrying: a fresh sandbox can cut a stream once while it
// finishes booting, and a genuinely broken one cuts every stream.
type execStreamTruncatedError struct {
	detail string
}

func (e *execStreamTruncatedError) Error() string { return e.detail }

// Two clocks can end a long exec, and e2b reports them differently. Both read
// as plain failures, which is what made the kronos batch look broken while it
// was in fact running:
//
//	r39/r41/r45  "context canceled", nothing but the start frame  → the turn's
//	             clock (budget expired, turn superseded, caller gone)
//	r42 / 553    "deadline_exceeded" WITH the command's output     → envd's
//	             clock, with a process still holding the stream open
//
// The error text is the only thing the model sees, so each clock's message
// carries the next step instead of leaving a bare provider string.
const execCancelledHintText = " [hint: the exec request was cancelled by the runtime, not by the sandbox — the turn's budget expired, the turn was superseded, or the caller disconnected. A process this command started may still be running inside the sandbox: check it (ps, plus whatever log file it was redirected to) and adopt that result before re-running anything. To make that check possible next time, start it with exec({\"run_in_background\": true}) and read it with bash_output.]"

// execCancelledHint answers for the errors that mean "the runtime stopped
// waiting", as opposed to "the sandbox answered with a problem".
func execCancelledHint(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return execCancelledHintText
	}
	return ""
}

// execStalledHint explains the other clock: envd cut the stream at its Connect
// deadline because a process the command started kept the stream open. The
// clause about delivered output matters — in the r42/553 case the number the
// model needed was already in the output it was about to discard.
func execStalledHint(connectErr, output string) string {
	if !strings.Contains(connectErr, "deadline_exceeded") {
		return ""
	}
	var b strings.Builder
	b.WriteString(" [hint: ")
	if strings.TrimSpace(output) != "" {
		b.WriteString("the output above was delivered, but ")
	}
	b.WriteString("a process this command started is still holding the exec stream open — envd ended the request at its own deadline. Run it with exec({\"run_in_background\": true}) instead: that hands back a bash_id immediately and bash_output reads it later, so the waiting happens in the sandbox while the turn stays free. Don't read this as a failed run: check the sandbox before re-running.]")
	return b.String()
}

// snippet bounds a body for a log line or an error message.
func snippet(body []byte, limit int) string {
	s := strings.TrimSpace(string(body))
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

func (e *E2BExecutor) Exec(ctx context.Context, command string, timeout time.Duration) (string, error) {
	// Match Docker's behavior: default cwd is /workspace, not the
	// invoking user's $HOME. Without this, relative-path writes from
	// agent commands (e.g. `camoufox-cli screenshot out.png`) land in
	// /home/user/ and never make it to the host-visible workspace.
	// Hydrate creates /workspace before any agent Exec runs, and
	// recreate() re-hydrates after a sandbox replacement, so `cd` is
	// guaranteed to succeed here.
	wrapped := "cd /workspace && " + command
	observed := e.identSnapshot()
	// The tool-result port: whatever the command prints comes back bounded, and
	// the bound is applied on every return path including the retries below.
	result, err := e.execOn(ctx, observed, wrapped, timeout, newClipOutput("exec/e2b"))
	if sandboxGone(err) {
		if rerr := e.recreateIfCurrent(ctx, observed, err); rerr != nil {
			return "", fmt.Errorf("sandbox recreate failed: %w (original: %v)", rerr, err)
		}
		return e.execOnce(ctx, wrapped, timeout)
	}
	if staleEnvdToken(err) {
		if rerr := e.refreshEnvdToken(ctx, observed); rerr != nil {
			return "", fmt.Errorf("envd token refresh failed: %w (original: %v)", rerr, err)
		}
		return e.execOnce(ctx, wrapped, timeout)
	}
	return result, err
}

func (e *E2BExecutor) execOnce(ctx context.Context, command string, timeout time.Duration) (string, error) {
	return e.execOn(ctx, e.identSnapshot(), command, timeout, newClipOutput("exec/e2b"))
}

// execOn runs one command against an explicit identity, streaming its output
// into out. Callers that may want to rebuild afterwards use this form so the
// identity they report as "the one that just died" is exactly the one the
// request went to.
//
// out decides the budget and owns the truncation log; what this function
// guarantees is that the response body is never a variable in the peak — one
// frame is held at a time, however large the command's output turns out to be.
func (e *E2BExecutor) execOn(ctx context.Context, id sandboxIdent, command string, timeout time.Duration, out execOutput) (string, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"process": map[string]interface{}{
			"cmd":  "/bin/bash",
			"args": []string{"-c", command},
		},
	})

	enveloped := connectEnvelope(payload)

	// Per-request deadline: timeout (the user-supplied tool budget) plus
	// a 30s slack so the server has room to flush trailing frames before
	// our side gives up. Critical because the underlying http.Client now
	// has no global timeout — without this the request would hang
	// forever if envd died mid-stream.
	execCtx, cancelExec := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancelExec()

	reqURL := e.envdURLFor(id.id) + "/process.Process/Start"
	req, err := http.NewRequestWithContext(execCtx, "POST", reqURL, bytes.NewReader(enveloped))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/connect+json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Connect-Timeout-Ms", fmt.Sprintf("%d", int(timeout.Milliseconds())))
	if id.token != "" {
		req.Header.Set("X-Access-Token", id.token)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("e2b exec: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// The body of a non-200 is an error page, not output: show it, bounded.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return "", &sandboxHTTPError{op: "e2b exec", status: resp.StatusCode, body: string(detail)}
	}

	rd := newConnectFrameReader(resp.Body)
	exitCode := 0
	exited := false
	frames, trailers := 0, 0
	connectErr := ""
	var firstFrame []byte
	// Don't drop a mid-stream read error — when the connection is severed (our
	// deadline hit before the process finished streaming its output), it is the
	// only signal that what we got is incomplete. Previously we silently kept the
	// partial body, the parser saw zero complete frames, and the tool returned
	// empty stdout — exactly the symptom of the long-running exec calls.
	var readErr error

	for {
		frame, isTrailer, ferr := rd.next()
		if ferr != nil {
			// A stream that ends on a frame boundary (io.EOF) and one that ends
			// inside a frame (ErrUnexpectedEOF) are both "envd stopped talking":
			// the frames already delivered stay, and the missing exit trailer is
			// the verdict. Neither is a transport failure — that split is what
			// the read-everything version got by ignoring ReadAll's nil error on
			// a partial body, and it is what hydrate's retry depends on.
			if !errors.Is(ferr, io.EOF) && !errors.Is(ferr, io.ErrUnexpectedEOF) {
				readErr = ferr
			}
			break
		}
		if isTrailer {
			trailers++
			if connectErr == "" {
				connectErr = connectErrorFrom(frame)
			}
			continue
		}
		frames++
		if firstFrame == nil {
			firstFrame = append([]byte(nil), frame...)
		}

		// E2B response format: {"event":{"data":{"stdout":"base64..."}}}
		var msg struct {
			Event struct {
				Start *struct {
					Pid int `json:"pid"`
				} `json:"start,omitempty"`
				Data *struct {
					Stdout string `json:"stdout,omitempty"` // base64 encoded
					Stderr string `json:"stderr,omitempty"` // base64 encoded
				} `json:"data,omitempty"`
				End *struct {
					Exited bool   `json:"exited"`
					Status string `json:"status"` // "exit status 0"
				} `json:"end,omitempty"`
			} `json:"event"`
		}
		if json.Unmarshal(frame, &msg) != nil {
			continue
		}
		if msg.Event.Data != nil {
			if msg.Event.Data.Stdout != "" {
				if decoded, err := base64.StdEncoding.DecodeString(msg.Event.Data.Stdout); err == nil {
					if err := out.writeStdout(decoded); err != nil {
						return out.text(), err
					}
				}
			}
			if msg.Event.Data.Stderr != "" {
				if decoded, err := base64.StdEncoding.DecodeString(msg.Event.Data.Stderr); err == nil {
					if err := out.writeStderr(decoded); err != nil {
						return out.text(), err
					}
				}
			}
		}
		if msg.Event.End != nil {
			exited = msg.Event.End.Exited
			// Parse "exit status N" to get exit code
			if strings.HasPrefix(msg.Event.End.Status, "exit status ") {
				fmt.Sscanf(msg.Event.End.Status, "exit status %d", &exitCode)
			}
		}
	}

	if readErr != nil {
		hint := execCancelledHint(readErr)
		if out.produced() == 0 && firstFrame != nil {
			// Nothing decoded arrived, so the only evidence the run ever started
			// is its first frame — the pid an operator can go look for. (The
			// read-everything version showed the raw framed body here, which the
			// model cannot read.)
			hint += "; first=" + snippet(firstFrame, 300)
		}
		return out.text(), fmt.Errorf("e2b exec body read: %w (got %d bytes)%s",
			readErr, rd.bytes, hint)
	}

	output := out.text()
	// outputLen stays the pre-bound size — that is the number operators grep for
	// to spot a runaway command; resultLen is what actually leaves here.
	slog.Info("e2b exec completed", "sandboxID", id.id, "exitCode", exitCode, "exited", exited, "outputLen", out.produced(), "resultLen", len(output), "frames", frames, "trailers", trailers, "bodyBytes", rd.bytes, "connectError", connectErr)

	// Reject a stream that didn't deliver a proper "End/exited=true" trailer.
	// Why this matters: when the request payload pushes envd past some
	// internal buffer (empirically >~80KB-of-base64 in a single bash -c arg
	// triggers it), the response comes back with frames but no final exit
	// status. exitCode stays at its zero default and we'd otherwise return
	// nil error — which is exactly the silent-failure mode that left Hydrate
	// looking successful while the chown step hadn't actually run, then
	// verifyWorkspaceWritable reported "/workspace probe: Permission denied"
	// with no clue why the chown didn't take.
	if !exited {
		// Read the hint before `output` is replaced by the placeholder — it
		// says something different depending on whether the command got to
		// print anything before the clock ran out.
		clockHint := execStalledHint(connectErr, output)
		if output == "" {
			output = "(no output — response stream truncated before exit-status trailer)"
		}
		// Say what the provider said. Without this the caller could not tell a
		// sandbox that vanished mid-exec from an envd error we failed to parse;
		// the trailer usually names it ("not found", "internal", …), and the raw
		// body is the only clue when the stream simply died.
		detail := fmt.Sprintf("e2b exec did not exit cleanly (frames=%d, trailers=%d, bodyBytes=%d): %s",
			frames, trailers, rd.bytes, output)
		if connectErr != "" {
			detail += "; server error: " + connectErr
		}
		detail += clockHint
		// One real frame reads far better than the framed bytes, and the stream
		// that died before its first complete frame is itself the diagnosis: that
		// is when the raw wire bytes get shown.
		if firstFrame != nil {
			detail += "; first=" + snippet(firstFrame, 300)
		} else if raw := snippet(rd.sniff, 300); raw != "" {
			detail += "; raw=" + raw
		}
		return output, &execStreamTruncatedError{detail: detail}
	}

	if exitCode != 0 {
		if output == "" {
			output = fmt.Sprintf("Process exited with code %d", exitCode)
		}
		return output, fmt.Errorf("exit code %d", exitCode)
	}
	return output, nil
}

func (e *E2BExecutor) ReadFile(ctx context.Context, path string) (string, error) {
	observed := e.identSnapshot()
	result, err := e.readFileOn(ctx, observed, path)
	// The file API's own 404 is a verdict about the path, not the instance
	// (sandboxGoneOnFileAPI): a file that is not there must not cost a sandbox.
	if sandboxGoneOnFileAPI(err) {
		if rerr := e.recreateIfCurrent(ctx, observed, err); rerr != nil {
			return "", rerr
		}
		return e.readFileOnce(ctx, path)
	}
	if staleEnvdToken(err) {
		if rerr := e.refreshEnvdToken(ctx, observed); rerr != nil {
			return "", rerr
		}
		return e.readFileOnce(ctx, path)
	}
	return result, err
}

func (e *E2BExecutor) readFileOnce(ctx context.Context, path string) (string, error) {
	return e.readFileOn(ctx, e.identSnapshot(), path)
}

func (e *E2BExecutor) readFileOn(ctx context.Context, id sandboxIdent, path string) (string, error) {
	reqURL := fmt.Sprintf("%s/files?path=%s&username=user", e.envdURLFor(id.id), url.QueryEscape(path))
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return "", err
	}
	if id.token != "" {
		req.Header.Set("X-Access-Token", id.token)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("e2b read: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return "", &sandboxHTTPError{op: "e2b read", status: resp.StatusCode, body: string(body)}
	}
	return string(body), nil
}

func (e *E2BExecutor) WriteFile(ctx context.Context, path, content string) (string, error) {
	observed := e.identSnapshot()
	result, err := e.writeFileOn(ctx, observed, path, content)
	if sandboxGoneOnFileAPI(err) {
		if rerr := e.recreateIfCurrent(ctx, observed, err); rerr != nil {
			return "", rerr
		}
		return e.writeFileOnce(ctx, path, content)
	}
	if staleEnvdToken(err) {
		if rerr := e.refreshEnvdToken(ctx, observed); rerr != nil {
			return "", rerr
		}
		return e.writeFileOnce(ctx, path, content)
	}
	return result, err
}

func (e *E2BExecutor) writeFileOnce(ctx context.Context, filePath, content string) (string, error) {
	return e.writeFileOn(ctx, e.identSnapshot(), filePath, content)
}

func (e *E2BExecutor) writeFileOn(ctx context.Context, id sandboxIdent, filePath, content string) (string, error) {
	if err := e.uploadBytesOn(ctx, id, filePath, []byte(content)); err != nil {
		return "", err
	}
	return fmt.Sprintf("Wrote %d bytes to %s", len(content), filePath), nil
}

// uploadBytes POSTs `data` to envd's /files multipart endpoint at
// `sandboxPath`, owned by the `user` account exec runs as. Pulled out of
// writeFileOnce so Hydrate can ship its tar bundle through the same
// large-payload-safe path instead of inlining a base64 blob inside a
// `bash -c` arg (the latter empirically truncates the Connect response
// stream at ~80KB and leaves the sandbox half-hydrated; see the comment
// on the Hydrate caller).
func (e *E2BExecutor) uploadBytes(ctx context.Context, sandboxPath string, data []byte) error {
	return e.uploadBytesOn(ctx, e.identSnapshot(), sandboxPath, data)
}

func (e *E2BExecutor) uploadBytesOn(ctx context.Context, id sandboxIdent, sandboxPath string, data []byte) error {
	// E2B envd's POST /files expects multipart/form-data with a `file`
	// field, NOT a raw octet-stream body. The earlier raw-body version
	// returned 200 OK but silently dropped the upload, leaving the file
	// non-existent inside the sandbox — caught when uploaded skills
	// failed with "No such file or directory" at exec time.
	reqURL := fmt.Sprintf("%s/files?path=%s&username=user",
		e.envdURLFor(id.id), url.QueryEscape(sandboxPath))

	// envd: the destination path comes from the `path` query param;
	// the multipart `filename` is just metadata, so basename is fine.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", path.Base(sandboxPath))
	if err != nil {
		return err
	}
	if _, err := fw.Write(data); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", reqURL, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if id.token != "" {
		req.Header.Set("X-Access-Token", id.token)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("e2b upload: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return uploadHTTPError(sandboxPath, resp.StatusCode, string(body))
	}
	return nil
}

// uploadHTTPError gives a write refusal one thing to act on.
//
// envd writes every file as `user`, so a path owned by another account (a file
// some earlier root/sudo command created; /tmp is not covered by Hydrate's
// chown) is readable but not writable, and the provider's whole answer is
// "open …: permission denied". Production 2026-09-17 05:36:47Z: apply_patch
// failed that way and edit_file, two seconds later, returned the identical
// bytes — write_file / edit_file / apply_patch share this one entry point.
//
// Only that verdict is translated, and only into a hint: any other 500 (a full
// disk, a missing directory) and every other status keep the provider's words.
// The result still wraps sandboxHTTPError, so the status stays readable and
// §7.4's classifier stays narrow — a write refusal is not an instance failure.
func uploadHTTPError(sandboxPath string, status int, body string) error {
	raw := &sandboxHTTPError{op: "e2b upload", status: status, body: body}
	if status != http.StatusInternalServerError || !mentionsPermissionDenied(body) {
		return raw
	}
	return fmt.Errorf("%s: 写入权限不足；/workspace 是合法写入路径，或换一个新文件名。原始错误：%w", sandboxPath, raw)
}

// mentionsPermissionDenied reads one verdict out of the body. Matching on text
// is acceptable here, and only here, because the decision it drives is a hint
// rather than a sandbox's fate — the gone/unusable classifiers read the status
// code instead.
func mentionsPermissionDenied(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, "permission denied") || strings.Contains(lower, "eacces")
}

func (e *E2BExecutor) ListDir(ctx context.Context, path string) (string, error) {
	// Use exec to list directory since the files API doesn't have a list endpoint
	return e.Exec(ctx, fmt.Sprintf("ls -la %s", path), 10*time.Second)
}

// IsRemoteWorkspace marks this executor as cloud-hosted so the
// LifecyclePool runs syncSnapshot after every exec instead of only on
// idle eviction. See sandbox.RemoteWorkspace.
func (e *E2BExecutor) IsRemoteWorkspace() {}

// ExposePort implements PortExposer. E2B serves every sandbox port at
// https://<port>-<sandboxID>.e2b.app with no publish step (same scheme
// envdURL uses for the control port), so the dev server bound to 0.0.0.0
// is reachable the moment it listens.
func (e *E2BExecutor) ExposePort(_ context.Context, port int) (string, error) {
	sandboxID := e.identSnapshot().id
	if sandboxID == "" {
		return "", fmt.Errorf("e2b: sandbox not created")
	}
	return fmt.Sprintf("https://%d-%s.e2b.app", port, sandboxID), nil
}

// ProvisionDir implements TemplateProvisioner: tar localDir (skipping the
// heavy, host-specific trees the scaffold reinstalls anyway), upload it
// over the same /files channel Hydrate uses, and extract it into destDir
// inside the sandbox. This seeds a coding template into an E2B sandbox
// that has no host bind mount to share it through.
func (e *E2BExecutor) ProvisionDir(ctx context.Context, localDir, destDir string) error {
	root := filepath.Clean(localDir)
	skip := map[string]bool{"node_modules": true, ".git": true, ".output": true, "dist": true, ".next": true}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	walkErr := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		top := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
		if skip[top] {
			if fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// Symlinks (and other irregular files) can't be faithfully tarred
		// without resolving targets; skip them — templates are plain trees.
		if !fi.IsDir() && !fi.Mode().IsRegular() {
			return nil
		}
		hdr, herr := tar.FileInfoHeader(fi, "")
		if herr != nil {
			return herr
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			f, oerr := os.Open(p)
			if oerr != nil {
				return oerr
			}
			_, cerr := io.Copy(tw, f)
			f.Close()
			if cerr != nil {
				return cerr
			}
		}
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	const tmp = "/tmp/fc-template.tar.gz"
	if err := e.uploadBytes(ctx, tmp, buf.Bytes()); err != nil {
		return fmt.Errorf("upload template: %w", err)
	}
	cmd := fmt.Sprintf("mkdir -p %q && tar -C %q -xzf %s && rm -f %s", destDir, destDir, tmp, tmp)
	if out, err := e.Exec(ctx, cmd, 3*time.Minute); err != nil {
		return fmt.Errorf("extract template: %w: %s", err, out)
	}
	return nil
}

// errSnapshotOverCap is this boundary's own name for "the snapshot was refused
// for size". The cap decision is `newPayloadOutput`'s (`errPayloadOverCap`), but
// the string that carries it to the caller is prose — and prose is not a
// classifier: without a sentinel the lifecycle layer cannot tell this refusal
// from a cut turn's `context.Canceled`, which is why its message named the cap
// for every failure (row 84 of docs/fs-formal-proof/11-change-register.md).
var errSnapshotOverCap = errors.New("sandbox: workspace snapshot over the cap")

// SnapshotWorkspace tars /workspace and ships the bytes back as base64 over
// stdout. This is the inverse of Hydrate's tar+base64 push — used by the
// LifecyclePool to flush sandbox-side files back to the durable
// workspace.Store after every successful exec, so files that the skill
// wrote inside the sandbox (image-tool's /workspace/gen_xxx.webp etc.)
// end up reachable from the host's UI / signed URL paths.
//
// Returns map of /workspace-relative path → contents. Skips silently
// when /workspace is empty or doesn't exist.
//
// This is a machine payload, so it has a cap that FAILS rather than a clip
// that truncates: a clipped base64 tar decodes to nothing. The cap exists
// because a snapshot runs after every exec — on 2026-09-14 the agent had left a
// growing run log under /workspace, so every command re-uploaded ~76 MB of
// base64 (~101 MB of framed body) and two pods were OOMKilled inside an hour.
const snapshotBase64Cap = 32 << 20

func (e *E2BExecutor) SnapshotWorkspace(ctx context.Context) (map[string][]byte, error) {
	// `2>/dev/null` swallows the "tar: ./: directory not found" noise
	// when /workspace doesn't exist yet; we still want to proceed with
	// an empty result. base64 -w0 keeps output on a single line so
	// envd's frame parser doesn't fight whitespace folding. Falls back
	// to the empty tar if /workspace is missing entirely.
	cmd := "if [ -d /workspace ]; then " +
		"tar -czf - -C /workspace . 2>/dev/null | base64 -w0; " +
		"fi"
	// payloadOutput, not the tool-result sink: this string is decoded and
	// untarred by the caller, so it must be refused past the cap rather than
	// shortened — and refused while reading, not after buffering the rest.
	sink := newPayloadOutput(snapshotBase64Cap)
	_, err := e.execOn(ctx, e.identSnapshot(), cmd, 60*time.Second, sink)
	if err != nil {
		if errors.Is(err, errPayloadOverCap) {
			// The tag at the end is this error's class, not decoration: it is what lets the caller
			// tell "over the cap" from "the turn died while this ran" without matching text.
			return nil, fmt.Errorf("workspace snapshot is over the %s cap — refusing to flush /workspace after every exec; move large or growing files out of /workspace (use /tmp for run logs) and retry. Largest entries: %s [%w]",
				humanBytes(snapshotBase64Cap), e.largestWorkspaceEntries(ctx), errSnapshotOverCap)
		}
		return nil, fmt.Errorf("snapshot workspace exec: %w (output: %s)", err, snippet([]byte(sink.text()), 200))
	}
	out := strings.TrimSpace(sink.text())
	if out == "" {
		return nil, nil
	}
	gz, err := base64.StdEncoding.DecodeString(out)
	if err != nil {
		return nil, fmt.Errorf("snapshot workspace decode: %w", err)
	}
	gr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, fmt.Errorf("snapshot workspace gunzip: %w", err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	out2 := make(map[string][]byte)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out2, fmt.Errorf("snapshot workspace tar read: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// Tar names start with "./" because we tarred `.` from inside
		// /workspace; strip it so callers see the same agent-relative
		// path layout the workspace.Store uses.
		name := strings.TrimPrefix(hdr.Name, "./")
		name = strings.TrimPrefix(name, "/")
		if name == "" {
			continue
		}
		// Skip macOS resource forks (`._foo`) in case anyone ever runs
		// this against a BSD-tar template — Linux/E2B's GNU tar
		// doesn't emit these, but they'd otherwise pollute the store.
		base := name
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		if strings.HasPrefix(base, "._") {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return out2, fmt.Errorf("snapshot workspace read entry %s: %w", name, err)
		}
		out2[name] = data
	}
	return out2, nil
}

// largestWorkspaceEntries is the diagnosis attached to an over-cap snapshot:
// naming the biggest paths is what turns "the sync stopped" into "this file is
// 400 MB and it is under /workspace". Best effort — the cap has already refused
// the payload, and failing to explain it must not change the outcome.
func (e *E2BExecutor) largestWorkspaceEntries(ctx context.Context) string {
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := e.execOnce(probeCtx, "du -ak /workspace 2>/dev/null | sort -rn | head -3", 15*time.Second)
	out = strings.TrimSpace(out)
	if err != nil || out == "" {
		return "(could not list)"
	}
	return strings.Join(strings.Split(out, "\n"), ", ")
}

// verifyWorkspaceWritable runs a one-shot probe against /workspace as
// the same `user` account agent exec runs under. It catches the
// silent-failure mode where Hydrate appeared to succeed but the
// underlying chown didn't actually take — empirically observed when
// the e2b template ships /workspace owned by root and the chown step
// is skipped or sudoless. The probe writes a single byte and removes
// it; the touch+rm round-trip is the same operation pattern the agent
// uses on its first /workspace write, so anything that would fail
// there fails here too.
func verifyWorkspaceWritable(ctx context.Context, ex *E2BExecutor) error {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	const probeCmd = `touch /workspace/.fc-health && rm -f /workspace/.fc-health && echo ok`
	out, err := ex.execOnce(probeCtx, probeCmd, 10*time.Second)
	if err != nil {
		return fmt.Errorf("/workspace probe: %w (out=%s)", err, strings.TrimSpace(out))
	}
	if !strings.Contains(out, "ok") {
		return fmt.Errorf("/workspace not writable as exec user (out=%s)", strings.TrimSpace(out))
	}
	return nil
}

func (e *E2BExecutor) Close() error {
	return e.closeSandboxByID(e.identSnapshot().id)
}

// closeSandboxByID destroys one instance. Pulled out of Close so a failed
// rebuild can destroy the replacement it is abandoning by id, without having
// to pretend the executor's current identity switched to it.
//
// The answer is checked rather than assumed. Reporting success on a rejected
// DELETE is how a "released" sandbox keeps running: its lease row is already
// gone, so nothing points at it any more and nothing will ever close it. A 404
// is success — the instance is not running, which is the whole point.
//
// The call is bounded because one caller (Get's post-create failure path) runs
// it while holding the pool mutex: an unanswered DELETE must not stall every
// other agent's sandbox binding.
func (e *E2BExecutor) closeSandboxByID(sandboxID string) error {
	if e.closeSandboxFn != nil {
		return e.closeSandboxFn(sandboxID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "DELETE",
		fmt.Sprintf("%s/sandboxes/%s", e2bBaseURL, sandboxID), nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", e.apiKey)
	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("e2b close sandbox %s: %w", sandboxID, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		// Already reaped — by the provider's timeout, or a sibling pod that
		// won the destroy race. Either way it is not running.
		slog.Info("e2b sandbox already gone", "sandboxID", sandboxID)
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("e2b close sandbox %s: HTTP %d: %s", sandboxID, resp.StatusCode, string(body))
	}
	slog.Info("e2b sandbox closed", "sandboxID", sandboxID)
	return nil
}

// Backend returns "e2b" — used by the per-exec log line so operators can
// confirm at a glance which provider handled a given tool call.
func (e *E2BExecutor) Backend() string { return "e2b" }

// Backend on the pool mirrors E2BExecutor.Backend so the LifecyclePool can
// surface the provider identity without resolving a lazy executor.
func (p *E2BExecutorPool) Backend() string { return "e2b" }

// E2BExecutorPool manages per-user E2B sandboxes.
type E2BExecutorPool struct {
	mu        sync.Mutex
	executors map[string]*E2BExecutor
	// leaseEpochs mirrors executors: the fencing epoch this pod last
	// received for the scope. Destroy passes it to ReleaseSandboxLease so a
	// stale eviction can never win against a newer renew/adopt.
	leaseEpochs map[string]int64
	// scopeLocks serialize the whole Get/Release path per scope, striped by
	// key (see scopeLock). Striped rather than one mutex per key: a map of
	// locks needs refcounting to avoid leaking an entry per session ever
	// served, and two scopes sharing a stripe only queue behind each other —
	// they never break each other's correctness.
	scopeLocks [scopeLockStripes]sync.Mutex
	apiKey     string
	template   string
	timeout    time.Duration
	home       string          // workspace root used to resolve per-agent skill dirs
	workspace  workspace.Store // optional — when set, /workspace is hydrated alongside /skills

	// Cross-pod lease coordination. When set, Get() first consults the
	// shared store and adopts an existing sandbox for the scope instead of
	// creating a duplicate per pod; Release() only destroys a sandbox it
	// still owns. Nil keeps the historical per-pod behavior (local mode,
	// docker, or registry disabled).
	leaseStore SandboxLeaseStore
	ownerID    string
	leaseTTL   time.Duration

	// Test seams: production defaults call the real e2b API; unit tests
	// override them so the create/adopt/reconcile decision paths can be
	// exercised without network access.
	newSandboxExecutor func(ctx context.Context, apiKey, template string, timeout time.Duration) (*E2BExecutor, error)
	// newAdoptedExecutor builds the executor for a sandbox another pod created.
	// Defaults to newAdoptedE2BExecutor; a seam for the same reason as the one
	// above — adoption now touches the network (it resumes a paused instance),
	// and that decision path should be testable without it.
	newAdoptedExecutor func(apiKey, sandboxID, accessToken, template string, timeout time.Duration) *E2BExecutor
	hydrateSandbox     func(ctx context.Context, ex *E2BExecutor) error
	verifySandbox      func(ctx context.Context, ex *E2BExecutor) error
}

// E2BLeaseOptions configures cross-pod sandbox sharing for the E2B pool.
type E2BLeaseOptions struct {
	Store    SandboxLeaseStore
	Owner    string
	LeaseTTL time.Duration
}

// WithSandboxLeases attaches a shared lease store. Requires non-empty Owner
// (unique per pod); TTL zero falls back to DefaultSandboxLeaseTTL.
func WithSandboxLeases(o E2BLeaseOptions) func(*E2BExecutorPool) {
	return func(p *E2BExecutorPool) {
		if o.Store == nil || o.Owner == "" {
			return
		}
		p.leaseStore = o.Store
		p.ownerID = o.Owner
		p.leaseTTL = o.LeaseTTL
		if p.leaseTTL <= 0 {
			p.leaseTTL = DefaultSandboxLeaseTTL
		}
	}
}

// newAdoptedE2BExecutor wraps an existing e2b sandbox (created by another
// pod) without calling the create API. Hydration is skipped on adoption —
// the sandbox was hydrated by its creator with the same (user, agent,
// session) skills/workspace; skill or workspace changes take effect on the
// next recreate, matching single-pod behavior.
//
// apiKey is required even though adoption never calls create: the shared
// lease row deliberately carries only sandbox_id + envd_token (the
// account-level key never touches the DB), and exec/read/write authenticate
// with the envd token alone — so an adopted executor "works" until the
// sandbox idles out. recreate() then needs the account key to mint a
// replacement, and Close() needs it to destroy one. An adopted executor
// without the key therefore fails at exactly that point, and e2b reports it
// as `401 authorization header is missing` (an empty X-API-Key header).
// Passing it at construction keeps "every E2BExecutor can rebuild/destroy
// itself" an invariant instead of a property callers must remember to patch
// in afterwards.
func newAdoptedE2BExecutor(apiKey, sandboxID, accessToken, template string, timeout time.Duration) *E2BExecutor {
	if template == "" {
		template = "base"
	}
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	return &E2BExecutor{
		apiKey:   apiKey,
		ident:    sandboxIdent{id: sandboxID, token: accessToken},
		client:   &http.Client{},
		template: template,
		timeout:  timeout,
		createFn: newE2BExecutor,
	}
}

// NewE2BExecutorPool — `home` is the FASTAGENT_HOME the docker backend
// would have used for `-v` mounts; the pool uses it to resolve which
// skill dirs to push into each fresh sandbox.
//
// Locking: p.mu guards only the executor/epoch maps — every critical section
// on it is a map lookup or assignment. Provisioning work (lease reads and
// writes, create, hydrate, verify) runs under the per-scope lock
// returned by scopeLock, so a slow or cold start for one scope cannot stall
// sandbox binding for every other agent in the process.
func NewE2BExecutorPool(apiKey, template, home string, timeout time.Duration, opts ...func(*E2BExecutorPool)) *E2BExecutorPool {
	p := &E2BExecutorPool{
		executors:          make(map[string]*E2BExecutor),
		leaseEpochs:        make(map[string]int64),
		apiKey:             apiKey,
		template:           template,
		timeout:            timeout,
		home:               home,
		newSandboxExecutor: newE2BExecutor,
		newAdoptedExecutor: newAdoptedE2BExecutor,
		hydrateSandbox: func(ctx context.Context, ex *E2BExecutor) error {
			return ex.Hydrate(ctx)
		},
		verifySandbox: verifyWorkspaceWritable,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// SetWorkspace plugs in the workspace.Store whose contents should be
// mirrored to /workspace inside every fresh sandbox. Optional — when
// nil, only /skills is hydrated. Called by the gateway after
// LifecyclePool's own workspace is wired so the inner pool and the
// lifecycle layer see the same source of truth.
func (p *E2BExecutorPool) SetWorkspace(ws workspace.Store) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.workspace = ws
}

// scopeLockStripes is the number of per-scope locks. Sized so unrelated scopes
// rarely collide while the array stays cheap to hold for the pool's lifetime.
const scopeLockStripes = 64

// scopeLock returns the lock that serializes work for one scope. Same key →
// same lock, always, which is what keeps "exactly one sandbox per scope" true
// now that the lock is per-scope instead of process-wide.
func (p *E2BExecutorPool) scopeLock(key string) *sync.Mutex {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(key))
	return &p.scopeLocks[hasher.Sum32()%scopeLockStripes]
}

// cachedExecutor / registerExecutor / recordEpoch / takeExecutor are the only
// ways the maps are touched. Keeping them tiny is the point: p.mu must never be
// held across I/O.
func (p *E2BExecutorPool) cachedExecutor(key string) (*E2BExecutor, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ex, ok := p.executors[key]
	return ex, ok
}

func (p *E2BExecutorPool) registerExecutor(key string, ex *E2BExecutor) {
	// Read the id BEFORE taking p.mu: identSnapshot takes the executor's own
	// lock, and the two locks must not nest (nothing establishes an order
	// between them). The line itself is logged after unlocking for the same
	// reason — it is the one place where a scope and a sandbox id are both in
	// hand, which is exactly the correlation a past incident had to be
	// reconstructed by hand (docs/sandbox-scope-leak.md §9.7).
	id := ex.identSnapshot().id
	p.mu.Lock()
	p.executors[key] = ex
	p.mu.Unlock()
	slog.Info("e2b sandbox bound to scope", "scopeKey", key, "sandboxID", id)
}

func (p *E2BExecutorPool) recordEpoch(key string, epoch int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.leaseEpochs[key] = epoch
}

// LiveExecutor implements LiveExecutorPool: the scope's cached instance, or
// false. No lease read, no create, no hydrate — see the interface's comment for
// why the panel's delete must never pay for a sandbox.
func (p *E2BExecutorPool) LiveExecutor(agentID, projectID, sessionID string) (Executor, bool) {
	ex, ok := p.cachedExecutor(poolKey(agentID, projectID, sessionID))
	if !ok {
		return nil, false
	}
	return ex, true
}

// LiveProjectExecutors implements LiveExecutorPool: every cached instance whose
// scope belongs to this project — "agent:p:<pid>" and "agent:p:<pid>:s:<sid>"
// alike, and nothing that merely starts with the same prefix (a project named
// "p1" must not drag in "p10").
func (p *E2BExecutorPool) LiveProjectExecutors(agentID, projectID string) []Executor {
	if agentID == "" || projectID == "" {
		return nil
	}
	prefix := agentID + ":p:" + projectID
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Executor, 0, 2)
	for key, ex := range p.executors {
		if key == prefix || strings.HasPrefix(key, prefix+":") {
			out = append(out, ex)
		}
	}
	return out
}

// publishUnhydrated records on the instance's lease row that this sandbox came
// up with a /workspace nobody could fill.
//
// Why it has to be the row and not this pod's memory: adoption does not replay
// hydration ("the creating pod hydrated the same scope"), so an adopting
// replica has no way to learn the fact — and the loss is silent, which is the
// one direction this whole mechanism must not fail in. The row already names
// the instance, so the flag rides the identity it describes, and a replacement
// clears it by publishing a new identity (docs 10 §4, G19).
//
// Best-effort, but never quiet about failing: a missed write means a sibling
// replica will not be able to declare the state, so it is logged with the scope
// and instance it would have described.
func (p *E2BExecutorPool) publishUnhydrated(ctx context.Context, key string, ex *E2BExecutor) {
	if p.leaseStore == nil || ex == nil || !ex.WorkspaceUnhydrated() {
		return
	}
	id := ex.identSnapshot().id
	if err := p.leaseStore.SetSandboxLeaseUnhydrated(ctx, key, p.ownerID, id, true); err != nil {
		slog.Warn("e2b could not record that this sandbox's /workspace was never filled; "+
			"a replica adopting it would not be able to say so",
			"scopeKey", key, "sandboxID", id, "error", err)
	}
}

// takeExecutor removes and returns the scope's executor together with the
// epoch this pod last received for it.
func (p *E2BExecutorPool) takeExecutor(key string) (*E2BExecutor, int64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ex, ok := p.executors[key]
	if !ok {
		return nil, 0, false
	}
	epoch := p.leaseEpochs[key]
	delete(p.executors, key)
	delete(p.leaseEpochs, key)
	return ex, epoch, true
}

func (p *E2BExecutorPool) Get(ctx context.Context, agentID, projectID, sessionID string) (Executor, error) {
	key := poolKey(agentID, projectID, sessionID)

	// Serialize per scope, not process-wide. Everything below can do network
	// I/O — lease reads/writes, create, hydrate, verify — and a process-wide
	// lock across that delays sandbox binding for every other agent behind one
	// cold scope. Same-scope callers still queue here, so "one sandbox per
	// scope" keeps the guarantee the global lock used to provide as a side
	// effect.
	scope := p.scopeLock(key)
	scope.Lock()
	defer scope.Unlock()

	if ex, ok := p.cachedExecutor(key); ok {
		if p.leaseStore != nil {
			// A cached executor may be stale: its lease can expire while we
			// were idle and another pod can take over the scope with a
			// different sandbox. Reconcile against the shared lease before
			// blindly renewing (blind renewal would steal ownership of the
			// wrong sandbox and orphan the real one on eviction).
			return p.reconcileLocalLease(ctx, key, ex, agentID, projectID, sessionID)
		}
		return ex, nil
	}
	if p.leaseStore != nil {
		if rec, err := p.leaseStore.GetSandboxLease(ctx, key); err != nil {
			slog.Warn("e2b lease lookup failed (falling back to local create)", "scopeKey", key, "error", err)
		} else if rec != nil {
			if ex, ok := p.adoptFromLease(ctx, key, rec, agentID, projectID, sessionID); ok {
				slog.Info("e2b sandbox adopted from shared lease",
					"sandboxID", rec.SandboxID, "scopeKey", key, "owner", p.ownerID)
				return ex, nil
			}
			slog.Warn("e2b lease changed during adoption (falling back to local create)",
				"scopeKey", key, "owner", p.ownerID)
		}
	}
	provisionStarted := time.Now()
	ex, err := p.newSandboxExecutor(ctx, p.apiKey, p.template, p.timeout)
	if err != nil {
		return nil, err
	}
	ex.SetHydrationSources(skillDirsForAgent(p.home, agentID), p.workspace, agentID, projectID, sessionID)
	if err := p.hydrateSandbox(ctx, ex); err != nil {
		// Hydrate is what chowns /workspace to the non-root `user`
		// account exec runs as; without it every agent write to
		// /workspace gets Permission denied silently — see the
		// reproducer in e2b_probe_diag_test.go. Used to be a WARN
		// here; the sandbox would still get cached and every
		// subsequent turn would surface as "agent wrote files but
		// they vanished". Tear it down so the caller retries or
		// fails loudly.
		_ = ex.Close()
		return nil, fmt.Errorf("e2b hydrate: %w", err)
	}
	if err := p.verifySandbox(ctx, ex); err != nil {
		_ = ex.Close()
		return nil, fmt.Errorf("e2b sandbox unusable: %w", err)
	}
	// No browser warm-up here (removed 2026-09-22, was ~24s of a ~26s cold
	// start). The daemon's cold-start race is absorbed by the shim in the
	// sandbox image — deploy/docker/sandbox/camoufox-cli-shim.sh — which
	// covers every launch path, including the rebuilds this never did.
	if p.leaseStore != nil {
		created := ex.identSnapshot()
		rec, acquired, lerr := p.leaseStore.AcquireSandboxLease(
			ctx, key, p.ownerID, created.id, created.token, p.template, p.leaseTTL)
		if lerr != nil {
			slog.Warn("e2b lease acquire failed (keeping local sandbox)", "scopeKey", key, "error", lerr)
		} else if acquired && rec != nil {
			p.recordEpoch(key, rec.Epoch)
			p.publishUnhydrated(ctx, key, ex)
		} else if !acquired && rec != nil {
			// Another replica won the race for this scope; use its sandbox
			// when the CAS adoption succeeds, otherwise keep our own.
			if adopted, ok := p.adoptFromLease(ctx, key, rec, agentID, projectID, sessionID); ok {
				_ = ex.Close()
				ex = adopted
				slog.Info("e2b sandbox adopted after lease race",
					"sandboxID", rec.SandboxID, "scopeKey", key, "owner", p.ownerID)
			} else {
				slog.Warn("e2b adoption after race failed; keeping local sandbox unregistered",
					"scopeKey", key, "owner", p.ownerID)
			}
		}
	}
	// One line per cold provision, naming the scope and the total. Counting
	// instances needs the two kinds separated: "e2b sandbox created" fires for
	// cold starts and rebuilds alike, while "e2b sandbox rebuilt" only covers
	// the second — this is the first.
	slog.Info("e2b sandbox provisioned",
		"scopeKey", key,
		"sandboxID", ex.identSnapshot().id,
		"elapsedMs", time.Since(provisionStarted).Milliseconds())
	p.registerExecutor(key, ex)
	return ex, nil
}

// adoptFromLease builds an executor for an existing lease record and
// CAS-renews it under this pod's ownership (fencing epoch bumped). Returns
// ok=false when the row changed between lookup and renew — callers must not
// adopt in that case. On a registry error the executor is returned without a
// recorded epoch (fail-open; release will not destroy the sandbox).
// Hydration is not replayed: the creating pod hydrated the same scope.
//
// Callers hold the scope lock; the maps are still guarded by p.mu inside the
// accessors, so this must not be called with p.mu held.
func (p *E2BExecutorPool) adoptFromLease(
	ctx context.Context,
	key string,
	rec *SandboxLeaseRecord,
	agentID, projectID, sessionID string,
) (*E2BExecutor, bool) {
	build := p.newAdoptedExecutor
	if build == nil {
		build = newAdoptedE2BExecutor
	}
	ex := build(p.apiKey, rec.SandboxID, rec.EnvdToken, rec.Template, p.timeout)
	if rec.Template == "" {
		ex.template = p.template
	}
	// A paused instance has to be woken before use, and in secure mode the row's
	// token may have been superseded by the pause. connect does both: it resumes
	// (a no-op when the sandbox is already running) and returns the current
	// token. Doing it here, rather than leaning on autoResume plus the reactive
	// 401 path, saves a failed round trip on the first call of every resumed
	// scope.
	if rec.State == "paused" {
		token, cerr := ex.Connect(ctx, rec.SandboxID, p.timeout)
		switch {
		case cerr == nil && token != "":
			ex.setIdent(rec.SandboxID, token, false)
		case cerr != nil && sandboxGone(cerr):
			// The paused instance is gone after all: let the caller create.
			slog.Info("e2b leased sandbox is gone before adoption",
				"sandboxID", rec.SandboxID, "scopeKey", key, "error", cerr)
			return nil, false
		case cerr != nil:
			// Carry on with the stored token: if it is stale the first envd call
			// answers 401 and that path reconnects.
			slog.Warn("e2b could not resume a paused sandbox; using the stored token",
				"sandboxID", rec.SandboxID, "scopeKey", key, "error", cerr)
		}
	}
	ex.SetHydrationSources(skillDirsForAgent(p.home, agentID), p.workspace, agentID, projectID, sessionID)
	// The instance's own record of how it came up. Adoption never replays
	// hydration, so without this read the fact "nobody could fill /workspace"
	// would die with the pod that discovered it — silently, and the agent would
	// read the empty tree as "my files are gone" (docs 10 §4, G19).
	ex.setWorkspaceUnhydrated(rec.Unhydrated)
	epoch, err := p.leaseStore.RenewSandboxLease(ctx, key, p.ownerID, rec.SandboxID, p.leaseTTL)
	if err != nil {
		slog.Warn("e2b lease renew after adopt failed; adopting without epoch",
			"scopeKey", key, "owner", p.ownerID, "error", err)
		p.registerExecutor(key, ex)
		return ex, true
	}
	if epoch == 0 {
		return nil, false
	}
	p.recordEpoch(key, epoch)
	p.registerExecutor(key, ex)
	if rec.State == "paused" {
		// The instance is running again; keep the annotation honest for the next
		// reader. Best-effort: nothing depends on it (it is advisory).
		if serr := p.leaseStore.SetSandboxLeaseState(ctx, key, p.ownerID, "running"); serr != nil {
			slog.Warn("could not mark the adopted sandbox running", "scopeKey", key, "error", serr)
		}
	}
	return ex, true
}

// reconcileLocalLease checks the shared lease against a locally cached
// RenewLease re-runs the per-scope reconcile against the shared lease. Used by
// the lifecycle layer after an operation long enough to have outlived the lease
// TTL: reconcile renews the row under this pod, and — because it is the same
// path every use takes — also publishes a rebuild that happened while the
// operation was running. A scope with no cached executor has nothing to renew.
func (p *E2BExecutorPool) RenewLease(ctx context.Context, agentID, projectID, sessionID string) error {
	key := poolKey(agentID, projectID, sessionID)
	scope := p.scopeLock(key)
	scope.Lock()
	defer scope.Unlock()
	ex, ok := p.cachedExecutor(key)
	if !ok {
		return nil
	}
	return p.renewLeaseLocked(ctx, key, ex, agentID, projectID, sessionID)
}

// renewLeaseLocked is the shared body of RenewLease and SleepScope. Callers hold
// the scope lock.
func (p *E2BExecutorPool) renewLeaseLocked(ctx context.Context, key string, ex *E2BExecutor, agentID, projectID, sessionID string) error {
	if p.leaseStore == nil {
		return nil
	}
	_, err := p.reconcileLocalLease(ctx, key, ex, agentID, projectID, sessionID)
	return err
}

// SleepScope pauses the scope's sandbox instead of destroying it, then keeps the
// lease row alive so a sibling that takes the scope over later resumes the SAME
// instance rather than building a replacement.
//
// paused=false with a nil error means there was no cached executor to sleep.
// An error is returned untouched so the caller can distinguish "already gone"
// (sandboxGone) from "could not sleep" — see LifecyclePool.sleepOrRelease.
func (p *E2BExecutorPool) SleepScope(ctx context.Context, agentID, projectID, sessionID string) (bool, error) {
	key := poolKey(agentID, projectID, sessionID)
	scope := p.scopeLock(key)
	scope.Lock()
	defer scope.Unlock()
	ex, ok := p.cachedExecutor(key)
	if !ok {
		return false, nil
	}
	if err := ex.Pause(ctx, ex.identSnapshot().id); err != nil {
		// Already gone: there is nothing to sleep, so drop the row and the local
		// reference rather than leaving a lease that names a dead instance. The
		// caller's fall-through Release then finds nothing and no-ops.
		//
		// The classification lives here, not in the lifecycle layer: "gone" is
		// this adapter's notion (a 502/404 from e2b), and policy should not have
		// to know the provider's status vocabulary.
		if sandboxGone(err) {
			if dead, epoch, ok := p.takeExecutor(key); ok {
				return false, p.releaseExecutor(key, dead, epoch)
			}
			return false, nil
		}
		return false, err
	}
	// The instance is free to keep (paused sandboxes are unbilled and outside
	// the concurrency limit), so keep the row that names it alive too.
	if err := p.renewLeaseLocked(ctx, key, ex, agentID, projectID, sessionID); err != nil {
		slog.Warn("sandbox paused but its lease renew failed",
			"sandboxID", ex.identSnapshot().id, "scopeKey", key, "error", err)
	}
	// Record the lifecycle LAST, and deliberately after the renew: the renew
	// reconciles, and reconciling a pending rebuild publishes the replacement
	// through ReplaceSandboxLease — which stamps the row 'running' because a
	// rebuilt instance is running by definition. Writing 'paused' first would be
	// overwritten by that publish, leaving the row describing a sandbox that is
	// asleep as if it were awake.
	//
	// Advisory and best-effort: a failed annotation must not undo a pause that
	// already happened.
	if serr := p.leaseStore.SetSandboxLeaseState(ctx, key, p.ownerID, "paused"); serr != nil {
		slog.Warn("sandbox paused but its lease state was not recorded",
			"sandboxID", ex.identSnapshot().id, "scopeKey", key, "error", serr)
	}
	return true, nil
}

// ExtendScope makes a scope survive an operation of length d. Two clocks have
// to move, and moving only one is a bug:
//
//   - the instance's provider expiry, or the auto-pause lands mid-operation and
//     cuts the stream;
//   - the lease row's expiry, or the row lapses mid-operation and a sibling
//     replica acquires the scope and starts a SECOND sandbox while this one is
//     still working — which is exactly the duplication the lease exists to
//     prevent.
//
// Implements the lifecycle layer's ScopeExtender. A pool without a lease store
// (single pod) only has the first clock.
func (p *E2BExecutorPool) ExtendScope(ctx context.Context, agentID, projectID, sessionID string, d time.Duration) error {
	key := poolKey(agentID, projectID, sessionID)
	scope := p.scopeLock(key)
	scope.Lock()
	defer scope.Unlock()
	ex, ok := p.cachedExecutor(key)
	if !ok {
		return nil
	}
	id := ex.identSnapshot().id
	if err := ex.ExtendTimeout(ctx, id, d); err != nil {
		return err
	}
	if p.leaseStore == nil {
		return nil
	}
	// Never shorten: an operation that needs 90 seconds should not leave the
	// scope with a 90-second lease when the pool's own TTL is longer.
	ttl := d
	if ttl < p.leaseTTL {
		ttl = p.leaseTTL
	}
	epoch, err := p.leaseStore.RenewSandboxLease(ctx, key, p.ownerID, id, ttl)
	if err != nil {
		return fmt.Errorf("extend lease for %s: %w", id, err)
	}
	if epoch > 0 {
		p.recordEpoch(key, epoch)
	}
	return nil
}

// reconcileLocalLease checks the shared lease against a locally cached
// executor before every use:
//
//   - same sandbox → renew (owner = this pod) and keep the local executor;
//   - no valid lease → re-claim with our local sandbox; on a lost race adopt
//     the winner;
//   - different sandbox owns the scope → close our stale local instance and
//     adopt the current one.
//
// Without the sandboxID check a long-idle pod would renew ownership of a
// lease that now points at another pod's sandbox, then "own" the wrong row
// and orphan the live sandbox when it later evicts.
//
// Callers hold the scope lock; this must not be called with p.mu held.
func (p *E2BExecutorPool) reconcileLocalLease(
	ctx context.Context,
	key string,
	ex *E2BExecutor,
	agentID, projectID, sessionID string,
) (Executor, error) {
	rec, err := p.leaseStore.GetSandboxLease(ctx, key)
	if err != nil {
		// Registry unavailable: fail open on the local executor.
		slog.Warn("e2b lease lookup failed (keeping local executor)", "scopeKey", key, "error", err)
		return ex, nil
	}
	cur := ex.identSnapshot()
	if rec == nil {
		// Our lease expired while idle. Try to reclaim with the sandbox we
		// still hold; if another pod won in the meantime, adopt theirs.
		got, acquired, aerr := p.leaseStore.AcquireSandboxLease(
			ctx, key, p.ownerID, cur.id, cur.token, ex.template, p.leaseTTL)
		if aerr != nil {
			slog.Warn("e2b lease reclaim failed (keeping local executor)", "scopeKey", key, "error", aerr)
			return ex, nil
		}
		if acquired {
			if got != nil {
				p.recordEpoch(key, got.Epoch)
			}
			// The re-acquire stamped this executor's CURRENT sandbox onto the
			// row, so anything a rebuild left unpublished is published now.
			ex.clearRebuild(cur)
			return ex, nil
		}
		if got != nil {
			if adopted, ok := p.adoptFromLease(ctx, key, got, agentID, projectID, sessionID); ok {
				_ = ex.Close()
				slog.Info("e2b sandbox adopted after lease expiry race",
					"sandboxID", got.SandboxID, "scopeKey", key, "owner", p.ownerID)
				return adopted, nil
			}
			slog.Warn("e2b adoption after expiry race failed; keeping local executor",
				"scopeKey", key, "owner", p.ownerID)
			return ex, nil
		}
		return ex, nil
	}
	if rec.SandboxID == cur.id {
		epoch, err := p.leaseStore.RenewSandboxLease(ctx, key, p.ownerID, cur.id, p.leaseTTL)
		if err != nil {
			slog.Warn("e2b lease renew failed", "scopeKey", key, "owner", p.ownerID, "error", err)
		} else if epoch > 0 {
			p.recordEpoch(key, epoch)
		}
		return ex, nil
	}
	if pending, ok := ex.pendingPublish(); ok {
		// This executor replaced its own sandbox, so the row is behind by
		// construction. Move it onto the replacement instead of treating the
		// mismatch as a takeover — adopting back would close the healthy
		// replacement and hand back the instance that just died.
		epoch, rerr := p.leaseStore.ReplaceSandboxLease(
			ctx, key, p.ownerID, pending.id, pending.token, ex.template, p.leaseTTL)
		if rerr != nil {
			// Registry down: keep serving from the local sandbox and retry on
			// the next reconcile (same fail-open rule as every other op).
			slog.Warn("e2b rebuilt sandbox not published to lease; keeping local executor",
				"scopeKey", key, "owner", p.ownerID, "sandboxID", pending.id, "error", rerr)
			return ex, nil
		}
		if epoch > 0 {
			p.recordEpoch(key, epoch)
			p.publishUnhydrated(ctx, key, ex)
			if !ex.clearRebuild(pending) {
				slog.Info("e2b rebuild superseded mid-publish; the newer identity publishes next",
					"scopeKey", key, "publishedSandboxID", pending.id)
			}
			slog.Info("e2b rebuilt sandbox published to shared lease",
				"scopeKey", key, "owner", p.ownerID, "sandboxID", pending.id, "epoch", epoch)
			return ex, nil
		}
		// CAS missed: another replica owns the scope now. Fall through and
		// adopt its sandbox — our replacement is closed by the adopt path.
		slog.Info("e2b rebuilt sandbox superseded by another pod; adopting current lease",
			"scopeKey", key, "leaseSandboxID", rec.SandboxID, "localSandboxID", pending.id)
	}
	// Scope was taken over by a different sandbox. Our cached instance is
	// stale; adopt the current one and only then close our local instance.
	if adopted, ok := p.adoptFromLease(ctx, key, rec, agentID, projectID, sessionID); ok {
		_ = ex.Close()
		slog.Info("e2b sandbox adopted (local cache stale)",
			"sandboxID", rec.SandboxID, "scopeKey", key, "owner", p.ownerID)
		return adopted, nil
	}
	slog.Warn("e2b adoption of current lease failed; keeping stale local executor",
		"scopeKey", key, "owner", p.ownerID)
	return ex, nil
}

func (p *E2BExecutorPool) Release(agentID, projectID, sessionID string) error {
	key := poolKey(agentID, projectID, sessionID)

	// Same scope lock as Get: without it a Get provisioning this scope could
	// register a fresh executor just after we drained the maps, and that
	// sandbox would never be released — its lease would lapse while the
	// instance kept running.
	scope := p.scopeLock(key)
	scope.Lock()
	defer scope.Unlock()

	ex, epoch, ok := p.takeExecutor(key)
	if !ok {
		return nil
	}
	return p.releaseExecutor(key, ex, epoch)
}

func (p *E2BExecutorPool) CloseAll() {
	// Shutdown path: the maps are drained under p.mu and the per-scope locks
	// are deliberately not taken. A Get racing shutdown can still register an
	// executor after the drain; that is the pre-existing "in-flight work dies
	// with the process" behavior, not a new hazard.
	p.mu.Lock()
	execs := make([]struct {
		key   string
		ex    *E2BExecutor
		epoch int64
	}, 0, len(p.executors))
	for key, ex := range p.executors {
		execs = append(execs, struct {
			key   string
			ex    *E2BExecutor
			epoch int64
		}{key: key, ex: ex, epoch: p.leaseEpochs[key]})
	}
	p.executors = make(map[string]*E2BExecutor)
	p.leaseEpochs = make(map[string]int64)
	p.mu.Unlock()
	for _, e := range execs {
		_ = p.releaseExecutor(e.key, e.ex, e.epoch)
	}
}

// releaseExecutor drops the shared lease when this pod still owns it, and
// only destroys the sandbox when the lease deletion succeeded (or no lease
// store is configured). If another pod adopted the scope, we drop our local
// reference without closing the sandbox out from under it.
func (p *E2BExecutorPool) releaseExecutor(key string, ex *E2BExecutor, epoch int64) error {
	if p.leaseStore == nil {
		return ex.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	deleted, err := p.leaseStore.ReleaseSandboxLease(ctx, key, p.ownerID, epoch)
	if err != nil {
		// Registry unavailable: keep the sandbox alive (fail open) rather
		// than risk destroying an instance another pod just adopted.
		slog.Warn("e2b lease release failed; leaving sandbox alive", "scopeKey", key, "error", err)
		return nil
	}
	if !deleted {
		return nil // another owner holds the lease — do not close shared sandbox
	}
	return ex.Close()
}

var (
	_ Executor             = (*E2BExecutor)(nil)
	_ ExecutorPool         = (*E2BExecutorPool)(nil)
	_ WorkspaceSnapshotter = (*E2BExecutor)(nil)
	_ RemoteWorkspace      = (*E2BExecutor)(nil)
)
