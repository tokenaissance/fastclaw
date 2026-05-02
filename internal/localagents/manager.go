package localagents

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/config"
)

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// Instance is the persisted process metadata for a local agent instance.
type Instance struct {
	Name      string     `json:"name"`
	AgentID   string     `json:"agentId,omitempty"`
	AgentName string     `json:"agentName,omitempty"`
	UserID    string     `json:"userId,omitempty"`
	PID       int        `json:"pid,omitempty"`
	Port      int        `json:"port,omitempty"`
	Home      string     `json:"home"`
	LogFile   string     `json:"logFile"`
	URL       string     `json:"url,omitempty"`
	Command   []string   `json:"command,omitempty"`
	StartedAt time.Time  `json:"startedAt"`
	StoppedAt *time.Time `json:"stoppedAt,omitempty"`
}

// Status combines persisted metadata with live process state.
type Status struct {
	Instance
	Running bool
	Uptime  time.Duration
}

// StartOptions controls how a local agent instance is launched.
type StartOptions struct {
	Port int
	Home string
}

type paths struct {
	stateDir string
	logDir   string
	homeDir  string
	metaFile string
	pidFile  string
	logFile  string
}

// Start launches a named local FastClaw gateway instance in the background.
func Start(name string, opts StartOptions) (*Instance, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	st, err := GetStatus(name)
	if err != nil {
		return nil, err
	}
	if st.Running {
		return nil, fmt.Errorf("agent %q already running (PID %d)", name, st.PID)
	}

	p, err := instancePaths(name)
	if err != nil {
		return nil, err
	}
	existing := Instance{Name: name}
	if st != nil {
		existing = st.Instance
	}
	home := opts.Home
	if home == "" {
		home = existing.Home
	}
	if home == "" {
		home = p.homeDir
	}
	home = expandHome(home)

	port := opts.Port
	if port <= 0 {
		port = existing.Port
	}
	if port <= 0 {
		port, err = freePort()
		if err != nil {
			return nil, err
		}
	} else if err := ensurePortAvailable(port); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(p.stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	if err := os.MkdirAll(p.logDir, 0o755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return nil, fmt.Errorf("create agent home: %w", err)
	}

	bin, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("find executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(bin); err == nil {
		bin = resolved
	}

	lf, err := os.OpenFile(p.logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	defer lf.Close()

	args := []string{"gateway", "--port", strconv.Itoa(port)}
	cmd := exec.Command(bin, args...)
	cmd.Stdout = lf
	cmd.Stderr = lf
	// Keep temporary instances isolated even when the operator's shell has
	// FASTCLAW_STORAGE_* set for a shared gateway.
	cmd.Env = withEnv(os.Environ(), map[string]string{
		"FASTCLAW_HOME":                 home,
		"FASTCLAW_AGENT_NAME":           name,
		"FASTCLAW_NO_OPEN":              "1",
		"FASTCLAW_PORT":                 strconv.Itoa(port),
		"FASTCLAW_STORAGE_TYPE":         "sqlite",
		"FASTCLAW_STORAGE_DSN":          "",
		"FASTCLAW_STORAGE_AUTO_MIGRATE": "true",
	})
	setSysProcAttr(cmd)

	if _, err := fmt.Fprintf(lf, "\n[agents] starting %q at %s\n", name, time.Now().Format(time.RFC3339)); err != nil {
		return nil, fmt.Errorf("write log banner: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start agent %q: %w", name, err)
	}

	now := time.Now()
	inst := &Instance{
		Name:      name,
		AgentID:   existing.AgentID,
		AgentName: existing.AgentName,
		UserID:    existing.UserID,
		PID:       cmd.Process.Pid,
		Port:      port,
		Home:      home,
		LogFile:   p.logFile,
		URL:       fmt.Sprintf("http://localhost:%d", port),
		Command:   append([]string{bin}, args...),
		StartedAt: now,
	}
	if err := writePID(p.pidFile, cmd.Process.Pid); err != nil {
		_ = signalPID(cmd.Process.Pid, "KILL")
		return nil, fmt.Errorf("write PID file: %w", err)
	}
	if err := saveInstance(p.metaFile, inst); err != nil {
		_ = signalPID(cmd.Process.Pid, "KILL")
		_ = os.Remove(p.pidFile)
		return nil, err
	}
	_ = cmd.Process.Release()
	return inst, nil
}

// Stop terminates a named local agent instance.
func Stop(name string) (*Instance, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	p, err := instancePaths(name)
	if err != nil {
		return nil, err
	}
	inst, err := loadInstance(p.metaFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("agent %q is not known", name)
		}
		return nil, err
	}
	if inst.PID <= 0 || !isProcessAlive(inst.PID) {
		markStopped(p, inst)
		return inst, fmt.Errorf("agent %q is not running", name)
	}

	pid := inst.PID
	if err := signalPID(pid, "TERM"); err != nil {
		return inst, fmt.Errorf("signal agent %q (PID %d): %w", name, pid, err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !isProcessAlive(pid) {
			markStopped(p, inst)
			return inst, nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	if err := signalPID(pid, "KILL"); err != nil && isProcessAlive(pid) {
		return inst, fmt.Errorf("kill agent %q (PID %d): %w", name, pid, err)
	}
	markStopped(p, inst)
	return inst, nil
}

// RemoveOptions controls how local agent state is reclaimed.
type RemoveOptions struct {
	// Force stops the agent first if it is still running.
	Force bool
	// Purge also removes the agent's FASTCLAW_HOME directory (sqlite DB and all).
	Purge bool
}

// Remove deletes persisted state for a local agent instance. By default it
// keeps the agent's home directory and log file untouched so a later
// `agents init <name>` can recover prior data; pass Purge to wipe them.
func Remove(name string, opts RemoveOptions) (*Instance, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	p, err := instancePaths(name)
	if err != nil {
		return nil, err
	}
	inst, err := loadInstance(p.metaFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("agent %q is not known", name)
	}
	if err != nil {
		return nil, err
	}
	if isProcessAlive(inst.PID) {
		if !opts.Force {
			return inst, fmt.Errorf("agent %q is running; stop it first or pass --force", name)
		}
		if _, err := Stop(name); err != nil {
			return inst, err
		}
	}
	_ = os.Remove(p.metaFile)
	_ = os.Remove(p.pidFile)
	if opts.Purge {
		_ = os.Remove(p.logFile)
		if inst.Home != "" {
			_ = os.RemoveAll(inst.Home)
		}
	}
	return inst, nil
}

// List returns all known local agent instances.
func List() ([]Status, error) {
	base, err := basePaths()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(base.stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	out := make([]Status, 0, len(entries))
	for _, ent := range entries {
		if ent.IsDir() || filepath.Ext(ent.Name()) != ".json" {
			continue
		}
		inst, err := loadInstance(filepath.Join(base.stateDir, ent.Name()))
		if err != nil {
			continue
		}
		st := statusFromInstance(*inst)
		if !st.Running && inst.PID > 0 {
			p, err := instancePaths(inst.Name)
			if err == nil {
				markStopped(p, inst)
				st.Instance = *inst
			}
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// GetStatus returns the current status for one local agent instance.
func GetStatus(name string) (*Status, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	p, err := instancePaths(name)
	if err != nil {
		return nil, err
	}
	inst, err := loadInstance(p.metaFile)
	if errors.Is(err, os.ErrNotExist) {
		return &Status{Instance: Instance{Name: name}, Running: false}, nil
	}
	if err != nil {
		return nil, err
	}
	st := statusFromInstance(*inst)
	if !st.Running && inst.PID > 0 {
		markStopped(p, inst)
		st.Instance = *inst
	}
	return &st, nil
}

// LogFile returns the expected log file path for a local agent.
func LogFile(name string) (string, error) {
	if err := validateName(name); err != nil {
		return "", err
	}
	p, err := instancePaths(name)
	if err != nil {
		return "", err
	}
	return p.logFile, nil
}

func statusFromInstance(inst Instance) Status {
	running := inst.PID > 0 && isProcessAlive(inst.PID)
	st := Status{Instance: inst, Running: running}
	if running && !inst.StartedAt.IsZero() {
		st.Uptime = time.Since(inst.StartedAt)
	}
	return st
}

func markStopped(p paths, inst *Instance) {
	now := time.Now()
	inst.PID = 0
	inst.StoppedAt = &now
	_ = os.Remove(p.pidFile)
	_ = saveInstance(p.metaFile, inst)
}

func validateName(name string) error {
	if name == "" {
		return errors.New("agent name is required")
	}
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid agent name %q: use letters, numbers, dot, dash, or underscore", name)
	}
	return nil
}

func basePaths() (paths, error) {
	base, err := config.HomeDir()
	if err != nil {
		return paths{}, err
	}
	return paths{
		stateDir: filepath.Join(base, "agent-runs"),
		logDir:   filepath.Join(base, "logs", "agents"),
		homeDir:  filepath.Join(base, "local-agents"),
	}, nil
}

func instancePaths(name string) (paths, error) {
	base, err := basePaths()
	if err != nil {
		return paths{}, err
	}
	base.homeDir = filepath.Join(base.homeDir, name)
	base.metaFile = filepath.Join(base.stateDir, name+".json")
	base.pidFile = filepath.Join(base.stateDir, name+".pid")
	base.logFile = filepath.Join(base.logDir, name+".log")
	return base, nil
}

func loadInstance(path string) (*Instance, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var inst Instance
	if err := json.Unmarshal(data, &inst); err != nil {
		return nil, err
	}
	return &inst, nil
}

func saveInstance(path string, inst *Instance) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(inst, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writePID(path string, pid int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("unexpected listener address %q", ln.Addr())
	}
	return addr.Port, nil
}

func ensurePortAvailable(port int) error {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("port %d is not available: %w", port, err)
	}
	return ln.Close()
}

func expandHome(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func withEnv(base []string, values map[string]string) []string {
	out := make([]string, 0, len(base)+len(values))
	seen := make(map[string]bool, len(values))
	for _, kv := range base {
		key, _, ok := strings.Cut(kv, "=")
		if ok {
			if v, exists := values[key]; exists {
				out = append(out, key+"="+v)
				seen[key] = true
				continue
			}
		}
		out = append(out, kv)
	}
	for k, v := range values {
		if !seen[k] {
			out = append(out, k+"="+v)
		}
	}
	return out
}
