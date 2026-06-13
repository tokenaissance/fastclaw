// Package runtime is the coding-agent "live app" layer that sits on top
// of a project's shared workspace folder. Where the existing project
// feature (internal/store + internal/setup/handlers_projects.go) owns a
// project's source tree, this package owns a long-lived sandbox
// container that *runs* that tree: it scaffolds from a template, boots a
// dev server, publishes the server's port to the host, and records a
// preview URL.
//
// Why a separate package and a separate long-lived container instead of
// reusing the per-turn ExecutorPool:
//
//   - The turn-scoped sandbox (sandbox.ExecutorPool) is created and torn
//     down around each agent turn. A dev server must outlive a turn, so
//     it can't live there. This package keeps its own container alive
//     across turns, evicted only on idle/sleep.
//   - Both containers bind-mount the SAME host dir
//     (workspaces/<agent>/projects/<pid>/). So when the agent edits a
//     file during a turn (through its turn sandbox), the dev server in
//     this package's container sees the change immediately and HMR
//     reloads. No file sync, no copy — the bind mount is the channel.
//
// Nothing here touches the turn-scoped pool, so the existing sandbox
// behavior is byte-for-byte unchanged: this is purely additive.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/sandbox"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// Runtime status values, mirrored in store.ProjectRuntimeRecord.Status.
const (
	StatusNone        = "none"
	StatusScaffolding = "scaffolding"
	StatusStarting    = "starting"
	StatusRunning     = "running"
	StatusSleeping    = "sleeping"
	StatusCrashed     = "crashed"
)

// devLogPath is where each runtime tees its dev-server output inside the
// container, so Logs() can tail it without attaching to the process.
const devLogPath = "/workspace/.fastclaw-dev.log"

// AppSubdir is the folder, under a scope's workspace, that the app is
// scaffolded into and served from — so the template doesn't pollute the
// chat/project workspace root with 30+ files. The agent's file tools are
// redirected into it too (registry codingSubdir) so edits land where the
// dev server serves. Exported so the agent package uses the same name.
const AppSubdir = "app"

// TemplateSpec describes how to scaffold and run one template. It is
// intentionally a small shell-command contract so the runtime stays
// template-agnostic: register a spec per template ref and the manager
// never needs to know what "shipany-tanstack" is.
type TemplateSpec struct {
	// DevPort is the container-internal port the dev server binds to
	// (e.g. 3000 for ShipAny / Vite).
	DevPort int
	// ScaffoldCmd populates an empty /workspace with the template's
	// source and installs deps. Run once, in /workspace, only when the
	// workspace looks empty (see needsScaffold). Example:
	//   cp -a /template/. /workspace/ && pnpm install
	ScaffoldCmd string
	// DevCmd starts the dev server bound to 0.0.0.0:DevPort, backgrounded
	// by the manager. IMPORTANT for HMR: the dev server's websocket must
	// advertise the *published host port*, not DevPort — configure that
	// template-side (e.g. Vite server.hmr.clientPort). Example:
	//   pnpm dev --host 0.0.0.0 --port 3000
	DevCmd string
	// TemplateMount, when set, bind-mounts this host directory read-only
	// at /template in the runtime container, so the default
	// `cp -a /template/.` scaffold works from a local template checkout
	// (no image bake, no git clone). Empty → scaffold must self-source
	// (git clone, or /template already baked into the image).
	TemplateMount string
}

// Manager owns every project runtime. Safe for concurrent use.
type Manager struct {
	store         store.Store
	workspaceRoot string // FASTCLAW_HOME (workspaces/ lives under it)
	image         string
	policy        *sandbox.Policy
	// previewBase templates the user-facing preview URL. Two shapes:
	//   - contains "{project}" → wildcard-gateway mode; the gateway maps
	//     <project>.preview.example.com to the host port, so the URL
	//     carries no port: "https://{project}.preview.example.com"
	//   - empty → local mode; URL points straight at the published host
	//     port: "http://127.0.0.1:<hostPort>"
	previewBase string

	mu        sync.Mutex
	templates map[string]TemplateSpec
	live      map[string]*sandbox.DockerSandbox // rtKey → container
}

// NewManager builds a runtime manager. image is the sandbox container
// image (same one the turn pool uses is fine, as long as it has the
// template toolchain — node/pnpm for ShipAny). previewBase is documented
// on Manager.previewBase.
func NewManager(st store.Store, workspaceRoot, image string, policy *sandbox.Policy, previewBase string) *Manager {
	if image == "" {
		image = "thinkany/fastclaw-sandbox:latest"
	}
	return &Manager{
		store:         st,
		workspaceRoot: workspaceRoot,
		image:         image,
		policy:        policy,
		previewBase:   previewBase,
		templates:     make(map[string]TemplateSpec),
		live:          make(map[string]*sandbox.DockerSandbox),
	}
}

// RegisterTemplate registers (or overrides) how a template ref is
// scaffolded and run. Call at boot for each supported template.
func (m *Manager) RegisterTemplate(ref string, spec TemplateSpec) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.templates[ref] = spec
}

// DefaultTemplate returns the sole registered template ref, or "" when
// zero or more than one are registered (the caller must then pass an
// explicit ref). Lets the agent tool default sensibly in the common
// single-template deployment without baking a template name into the
// agent package.
func (m *Manager) DefaultTemplate() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.templates) != 1 {
		return ""
	}
	for ref := range m.templates {
		return ref
	}
	return ""
}

func rtKey(userID, agentID, scopeID string) string {
	return userID + "|" + agentID + "|" + scopeID
}

// scopeFor resolves how a runtime is addressed. A project, when present,
// is the app's home — shared across all the project's chats and serving
// projects/<pid>/. Otherwise the chat's own session dir is the home
// (sessions/<sid>/), so a preview works in ANY chat without first
// creating a project. Both mirror the dirs the agent's turn sandbox /
// workspace store write to, so the dev server sees the agent's edits.
//
// scopeID is the stable key (store row + live-container map). Session
// scopes are namespaced "sess:<sid>" so they can never collide with a
// real project id in the shared project_runtimes table.
func (m *Manager) scopeFor(agentID, projectID, sessionID string) (scopeID, workspaceDir string, err error) {
	base := filepath.Join(m.workspaceRoot, "workspaces", agentID)
	switch {
	case projectID != "":
		return projectID, filepath.Join(base, "projects", projectID), nil
	case sessionID != "":
		return "sess:" + sessionID, filepath.Join(base, "sessions", sessionID), nil
	default:
		return "", "", errors.New("runtime: need a project or a session to address a runtime")
	}
}

// Get returns the persisted runtime record for a project (or session)
// scope, or store.ErrNotFound when none exists yet. Pass sessionID="" for
// the project-addressed (HTTP/SaaS) path.
func (m *Manager) Get(ctx context.Context, userID, agentID, projectID, sessionID string) (*store.ProjectRuntimeRecord, error) {
	scopeID, _, err := m.scopeFor(agentID, projectID, sessionID)
	if err != nil {
		return nil, err
	}
	return m.store.GetProjectRuntime(ctx, userID, agentID, scopeID)
}

// Up brings a runtime to StatusRunning and returns the record with a live
// PreviewURL. Idempotent: if the container is already up it just re-reads
// state; if the workspace is empty it scaffolds first. The runtime is
// homed in the project when projectID is set, else in the chat's own
// session dir (so a preview works without a pre-created project).
//
// templateRef may be empty when the runtime already exists (the stored
// ref is reused); it is required on first provisioning.
func (m *Manager) Up(ctx context.Context, userID, agentID, projectID, sessionID, templateRef string) (*store.ProjectRuntimeRecord, error) {
	scopeID, ws, err := m.scopeFor(agentID, projectID, sessionID)
	if err != nil {
		return nil, err
	}
	rec, err := m.store.GetProjectRuntime(ctx, userID, agentID, scopeID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if rec == nil {
		rec = &store.ProjectRuntimeRecord{
			UserID:    userID,
			AgentID:   agentID,
			ProjectID: scopeID,
			Status:    StatusNone,
		}
	}
	if templateRef != "" {
		rec.TemplateRef = templateRef
	}
	if rec.TemplateRef == "" {
		return nil, errors.New("runtime: templateRef is required on first Up")
	}

	m.mu.Lock()
	spec, ok := m.templates[rec.TemplateRef]
	if !ok && len(m.templates) == 1 {
		// Lenient resolution: the model commonly passes a looser name
		// ("shipany" for the registered "shipany-tanstack"). With a single
		// template configured, treat any ref as that one and canonicalize.
		for ref, s := range m.templates {
			rec.TemplateRef, spec, ok = ref, s, true
		}
	}
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("runtime: unknown template %q (register it first)", rec.TemplateRef)
	}
	if spec.DevPort == 0 {
		spec.DevPort = 3000
	}

	// Already live? Re-read host port and return without churning the
	// container.
	m.mu.Lock()
	sb := m.live[rtKey(userID, agentID, scopeID)]
	m.mu.Unlock()
	if sb != nil {
		if hp, perr := sb.HostPortFor(spec.DevPort); perr == nil {
			rec.HostPort = hp
			rec.DevPort = spec.DevPort
			rec.PreviewURL = m.previewURL(scopeID, hp)
			rec.Status = StatusRunning
			rec.LastError = ""
			_ = m.store.SaveProjectRuntime(ctx, rec)
			return rec, nil
		}
		// Stale handle (container died) — drop it and rebuild below.
		m.evict(userID, agentID, scopeID)
	}

	// Home the app in an AppSubdir of the scope workspace, not the root —
	// keeps the chat/project workspace from being buried under the
	// template's files. The agent's file tools target the same subdir
	// (registry codingSubdir), so edits still align with what's served.
	ws = filepath.Join(ws, AppSubdir)
	if mkErr := os.MkdirAll(ws, 0o755); mkErr != nil {
		rec.Status = StatusCrashed
		rec.LastError = "create app dir: " + mkErr.Error()
		_ = m.store.SaveProjectRuntime(ctx, rec)
		return nil, fmt.Errorf("runtime: create app dir: %w", mkErr)
	}

	// Create + start a fresh long-lived container with the dev port
	// published, homed on the app subdir.
	sb = sandbox.NewDockerSandbox(m.image, ws, m.policy)
	sb.SetPublishPorts(map[int]int{spec.DevPort: 0})
	if spec.TemplateMount != "" {
		sb.SetTemplateMount(spec.TemplateMount)
	}

	rec.Status = StatusStarting
	rec.DevPort = spec.DevPort
	_ = m.store.SaveProjectRuntime(ctx, rec)

	if err := sb.Create(); err != nil {
		rec.Status = StatusCrashed
		rec.LastError = "create sandbox: " + err.Error()
		_ = m.store.SaveProjectRuntime(ctx, rec)
		return nil, fmt.Errorf("runtime: create sandbox: %w", err)
	}
	rec.ContainerID = sb.ContainerID()

	m.mu.Lock()
	m.live[rtKey(userID, agentID, scopeID)] = sb
	m.mu.Unlock()

	// Scaffold on an empty workspace.
	if m.needsScaffold(ctx, sb) && spec.ScaffoldCmd != "" {
		rec.Status = StatusScaffolding
		_ = m.store.SaveProjectRuntime(ctx, rec)
		if out, serr := sb.Exec(ctx, spec.ScaffoldCmd, "/workspace"); serr != nil {
			rec.Status = StatusCrashed
			rec.LastError = "scaffold: " + serr.Error() + ": " + tail(out, 500)
			_ = m.store.SaveProjectRuntime(ctx, rec)
			return nil, fmt.Errorf("runtime: scaffold: %w", serr)
		}
	}

	// Start the dev server, backgrounded so Exec returns immediately. The
	// container's PID 1 is `tail -f /dev/null`, so the nohup'd process
	// keeps running after this docker exec returns.
	if err := m.startDevServer(ctx, sb, spec.DevCmd); err != nil {
		rec.Status = StatusCrashed
		rec.LastError = "start dev server: " + err.Error()
		_ = m.store.SaveProjectRuntime(ctx, rec)
		return nil, fmt.Errorf("runtime: start dev server: %w", err)
	}

	// Don't declare "running" until the dev server actually answers on the
	// published port — otherwise the preview URL 404s/refuses while Vite is
	// still booting (or forever, if it crashed / bound a different port
	// because something rewrote the framework config). Poll inside the
	// container; on timeout, surface the real bound port from the log so
	// the failure is actionable instead of a silent dead link. The budget
	// covers SSR templates (vite+nitro) whose FIRST request triggers a
	// cold compile that alone runs well past a minute in a container.
	if err := m.waitForDevServer(ctx, sb, spec.DevPort, 5*time.Minute); err != nil {
		rec.Status = StatusCrashed
		rec.LastError = err.Error()
		_ = m.store.SaveProjectRuntime(ctx, rec)
		return nil, fmt.Errorf("runtime: %w", err)
	}

	hp, err := sb.HostPortFor(spec.DevPort)
	if err != nil {
		rec.Status = StatusCrashed
		rec.LastError = "resolve host port: " + err.Error()
		_ = m.store.SaveProjectRuntime(ctx, rec)
		return nil, fmt.Errorf("runtime: resolve host port: %w", err)
	}
	rec.HostPort = hp
	rec.PreviewURL = m.previewURL(scopeID, hp)
	rec.Status = StatusRunning
	rec.LastError = ""
	if err := m.store.SaveProjectRuntime(ctx, rec); err != nil {
		return nil, err
	}
	slog.Info("project runtime up", "agent", agentID, "scope", scopeID,
		"hostPort", hp, "preview", rec.PreviewURL)
	return rec, nil
}

// Sleep stops the container to free compute but keeps the workspace and
// the record. Wake (or the next Up) recreates it. Status → sleeping,
// host port cleared (the old binding is gone).
func (m *Manager) Sleep(ctx context.Context, userID, agentID, projectID, sessionID string) error {
	scopeID, _, err := m.scopeFor(agentID, projectID, sessionID)
	if err != nil {
		return err
	}
	m.evict(userID, agentID, scopeID)
	rec, err := m.store.GetProjectRuntime(ctx, userID, agentID, scopeID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	rec.Status = StatusSleeping
	rec.HostPort = 0
	rec.ContainerID = ""
	rec.PreviewURL = ""
	return m.store.SaveProjectRuntime(ctx, rec)
}

// Wake is Up without a template ref (reuses the stored one).
func (m *Manager) Wake(ctx context.Context, userID, agentID, projectID, sessionID string) (*store.ProjectRuntimeRecord, error) {
	return m.Up(ctx, userID, agentID, projectID, sessionID, "")
}

// Stop tears the container down AND deletes the runtime record. The
// workspace files are left on disk (the project/chat still owns them);
// only the live-app layer is removed.
func (m *Manager) Stop(ctx context.Context, userID, agentID, projectID, sessionID string) error {
	scopeID, _, err := m.scopeFor(agentID, projectID, sessionID)
	if err != nil {
		return err
	}
	m.evict(userID, agentID, scopeID)
	return m.store.DeleteProjectRuntime(ctx, userID, agentID, scopeID)
}

// Exec runs a one-shot command inside the runtime container (for git
// snapshots, deploys, package installs the agent triggers out-of-band).
// Returns combined stdout+stderr. Fails if the runtime isn't live.
func (m *Manager) Exec(ctx context.Context, userID, agentID, projectID, sessionID, command string, timeout time.Duration) (string, error) {
	scopeID, _, err := m.scopeFor(agentID, projectID, sessionID)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	sb := m.live[rtKey(userID, agentID, scopeID)]
	m.mu.Unlock()
	if sb == nil {
		return "", errors.New("runtime: not live (call Up first)")
	}
	ctxExec := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		ctxExec, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return sb.Exec(ctxExec, command, "/workspace")
}

// Logs tails the dev server log. tail<=0 returns the whole file.
func (m *Manager) Logs(ctx context.Context, userID, agentID, projectID, sessionID string, tailLines int) (string, error) {
	scopeID, _, err := m.scopeFor(agentID, projectID, sessionID)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	sb := m.live[rtKey(userID, agentID, scopeID)]
	m.mu.Unlock()
	if sb == nil {
		return "", errors.New("runtime: not live")
	}
	cmd := "cat " + devLogPath
	if tailLines > 0 {
		cmd = fmt.Sprintf("tail -n %d %s", tailLines, devLogPath)
	}
	return sb.Exec(ctx, cmd+" 2>/dev/null || true", "/workspace")
}

// --- internals ---

// waitForDevServer polls the dev port from inside the container until it
// answers an HTTP request, or the timeout elapses. On timeout it inspects
// the dev log for the port Vite actually bound (e.g. it fell back to 5173
// because the app's vite.config server.port was rewritten) and returns an
// actionable error instead of letting Up report a dead "running" preview.
func (m *Manager) waitForDevServer(ctx context.Context, sb *sandbox.DockerSandbox, port int, timeout time.Duration) error {
	// --max-time must comfortably exceed an SSR dev server's cold-compile
	// time for the first request (vite+nitro takes tens of seconds): with a
	// short cap every probe aborts mid-compile and the server never gets to
	// answer, even though it's healthy.
	probe := fmt.Sprintf(
		`curl -s -o /dev/null -w '%%{http_code}' --max-time 60 http://127.0.0.1:%d/ 2>/dev/null || echo 000`,
		port)
	deadline := timeout
	const step = 3 * time.Second
	for waited := time.Duration(0); waited < deadline; waited += step {
		out, _ := sb.Exec(ctx, probe, "/workspace")
		code := strings.TrimSpace(out)
		// Any HTTP response (even a redirect/4xx) means the server is up
		// and the published port reaches it.
		if len(code) == 3 && code[0] >= '2' && code[0] <= '4' {
			return nil
		}
		// Cheap sleep without importing a timer into the loop body: a
		// no-op exec that blocks ~step. `sleep` exists in the image.
		_, _ = sb.Exec(ctx, fmt.Sprintf("sleep %d", int(step.Seconds())), "/workspace")
	}
	// Timed out — figure out what actually happened.
	logTail, _ := sb.Exec(ctx, "tail -n 40 "+devLogPath+" 2>/dev/null", "/workspace")
	if boundPort := detectVitePort(logTail); boundPort != 0 && boundPort != port {
		return fmt.Errorf("dev server bound port %d, not the published %d — something rewrote the framework config's server.port; revert it so the preview maps correctly. Log tail:\n%s",
			boundPort, port, tail(logTail, 600))
	}
	return fmt.Errorf("dev server did not come up on port %d within %s. Log tail:\n%s",
		port, timeout, tail(logTail, 600))
}

// detectVitePort scrapes the "Local: http://localhost:NNNN/" line Vite
// prints on boot, returning the port it actually bound (0 if not found).
func detectVitePort(log string) int {
	const marker = "localhost:"
	for _, line := range strings.Split(log, "\n") {
		i := strings.Index(line, marker)
		if i < 0 {
			continue
		}
		rest := line[i+len(marker):]
		end := 0
		for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
			end++
		}
		if end == 0 {
			continue
		}
		n := 0
		for _, c := range rest[:end] {
			n = n*10 + int(c-'0')
		}
		return n
	}
	return 0
}

func (m *Manager) startDevServer(ctx context.Context, sb *sandbox.DockerSandbox, devCmd string) error {
	if devCmd == "" {
		return errors.New("template DevCmd is empty")
	}
	// nohup + & so the dev server outlives this docker exec; tee output
	// to the log file for Logs(). setsid keeps it off our process group.
	wrapped := fmt.Sprintf("setsid sh -c %s > %s 2>&1 < /dev/null & echo started",
		shellSingleQuote(devCmd), devLogPath)
	_, err := sb.Exec(ctx, wrapped, "/workspace")
	return err
}

// needsScaffold reports whether /workspace looks empty enough to warrant
// running the scaffold command. Cheap heuristic: no package.json.
func (m *Manager) needsScaffold(ctx context.Context, sb *sandbox.DockerSandbox) bool {
	out, err := sb.Exec(ctx, "test -e /workspace/package.json && echo yes || echo no", "/workspace")
	if err != nil {
		// If we can't tell, assume it needs scaffolding — a redundant
		// scaffold on a populated dir is the template's problem to make
		// idempotent, but skipping a needed one leaves a dead preview.
		return true
	}
	return !strings.Contains(out, "yes")
}

func (m *Manager) previewURL(scopeID string, hostPort int) string {
	if m.previewBase == "" {
		return fmt.Sprintf("http://127.0.0.1:%d", hostPort)
	}
	// Gateway mode: the scope id becomes a subdomain, so strip the
	// "sess:" namespace colon (invalid in a hostname) to a dash.
	host := strings.ReplaceAll(scopeID, ":", "-")
	return strings.ReplaceAll(m.previewBase, "{project}", host)
}

// evict closes and forgets the live container without touching the
// record (callers update status as needed). scopeID is the project or
// session scope key.
func (m *Manager) evict(userID, agentID, scopeID string) {
	m.mu.Lock()
	sb := m.live[rtKey(userID, agentID, scopeID)]
	delete(m.live, rtKey(userID, agentID, scopeID))
	m.mu.Unlock()
	if sb != nil {
		if err := sb.Close(); err != nil {
			slog.Warn("runtime evict: close container", "scope", scopeID, "error", err)
		}
	}
}

// CloseAll tears down every live container. Call on shutdown.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	live := m.live
	m.live = make(map[string]*sandbox.DockerSandbox)
	m.mu.Unlock()
	for _, sb := range live {
		_ = sb.Close()
	}
}

// tail returns the last n bytes of s (for trimming error blobs).
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// shellSingleQuote wraps s in single quotes, escaping embedded ones, so
// it survives one layer of `sh -c`.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
