package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FileStore implements Store using the local filesystem under ~/.fastclaw/.
// User-scoped state lives under ~/.fastclaw/users/{userID}/ so that the
// same layout trivially supports both local (single default user) and
// self-hosted multi-user installs.
type FileStore struct {
	rootDir string // ~/.fastclaw
}

// NewFileStore creates a file-based store rooted at the given global directory.
// rootDir should normally be the value returned by config.HomeDir().
func NewFileStore(rootDir string) *FileStore {
	return &FileStore{rootDir: rootDir}
}

func (f *FileStore) Close() error { return nil }

// userRoot returns ~/.fastclaw/users/{userID}, defaulting to the local user
// when userID is empty. This is where all user-scoped files live.
func (f *FileStore) userRoot(userID string) string {
	if userID == "" {
		userID = DefaultUserID
	}
	return filepath.Join(f.rootDir, "users", userID)
}

// usersDir returns ~/.fastclaw/users (parent of all per-user dirs).
func (f *FileStore) usersDir() string {
	return filepath.Join(f.rootDir, "users")
}

// --- Config ---

func (f *FileStore) GetConfig(ctx context.Context, userID string) (*UserConfig, error) {
	path := filepath.Join(f.userRoot(userID), "fastclaw.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	info, _ := os.Stat(path)
	return &UserConfig{
		UserID:    userID,
		Data:      raw,
		UpdatedAt: info.ModTime(),
	}, nil
}

func (f *FileStore) SaveConfig(ctx context.Context, userID string, cfg *UserConfig) error {
	dir := f.userRoot(userID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "fastclaw.json")
	data, err := json.MarshalIndent(cfg.Data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func (f *FileStore) DeleteConfig(ctx context.Context, userID string) error {
	return os.Remove(filepath.Join(f.userRoot(userID), "fastclaw.json"))
}

// --- Agents ---

func (f *FileStore) ListAgents(ctx context.Context, userID string) ([]AgentRecord, error) {
	agentsDir := filepath.Join(f.userRoot(userID), "agents")
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var agents []AgentRecord
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		ag, err := f.GetAgent(ctx, userID, e.Name())
		if err != nil {
			continue
		}
		agents = append(agents, *ag)
	}
	return agents, nil
}

func (f *FileStore) GetAgent(ctx context.Context, userID, agentID string) (*AgentRecord, error) {
	wsDir := filepath.Join(f.userRoot(userID), "agents", agentID, "agent")
	if _, err := os.Stat(wsDir); err != nil {
		return nil, fmt.Errorf("agent not found: %s", agentID)
	}

	rec := &AgentRecord{
		ID:        agentID,
		Name:      agentID,
		Workspace: make(map[string]string),
	}

	// Read agent.json
	if data, err := os.ReadFile(filepath.Join(wsDir, "agent.json")); err == nil {
		var cfg map[string]interface{}
		json.Unmarshal(data, &cfg)
		rec.Config = cfg
		if m, ok := cfg["model"].(string); ok {
			rec.Model = m
		}
	}

	// Read workspace files
	for _, name := range []string{"SOUL.md", "IDENTITY.md", "AGENTS.md", "TOOLS.md",
		"USER.md", "BOOTSTRAP.md", "HEARTBEAT.md", "MEMORY.md"} {
		if data, err := os.ReadFile(filepath.Join(wsDir, name)); err == nil {
			rec.Workspace[name] = string(data)
		}
	}

	return rec, nil
}

func (f *FileStore) SaveAgent(ctx context.Context, userID string, agent *AgentRecord) error {
	wsDir := filepath.Join(f.userRoot(userID), "agents", agent.ID, "agent")
	os.MkdirAll(wsDir, 0o755)

	// Write agent.json
	if agent.Config != nil {
		data, _ := json.MarshalIndent(agent.Config, "", "  ")
		os.WriteFile(filepath.Join(wsDir, "agent.json"), data, 0o644)
	}

	// Write workspace files
	for name, content := range agent.Workspace {
		os.WriteFile(filepath.Join(wsDir, name), []byte(content), 0o644)
	}

	return nil
}

func (f *FileStore) DeleteAgent(ctx context.Context, userID, agentID string) error {
	return os.RemoveAll(filepath.Join(f.userRoot(userID), "agents", agentID))
}

// --- Sessions ---

func (f *FileStore) GetSession(ctx context.Context, userID, agentID, sessionKey string) (*SessionRecord, error) {
	path := f.sessionPath(userID, agentID, sessionKey)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var msgs []SessionMessage
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var msg SessionMessage
		if json.Unmarshal([]byte(line), &msg) == nil {
			msgs = append(msgs, msg)
		}
	}

	info, _ := os.Stat(path)
	return &SessionRecord{
		Messages:  msgs,
		UpdatedAt: info.ModTime(),
	}, nil
}

func (f *FileStore) SaveSession(ctx context.Context, userID, agentID, sessionKey string, session *SessionRecord) error {
	path := f.sessionPath(userID, agentID, sessionKey)
	os.MkdirAll(filepath.Dir(path), 0o755)

	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	enc := json.NewEncoder(file)
	for _, msg := range session.Messages {
		enc.Encode(msg)
	}
	return nil
}

func (f *FileStore) ListSessions(ctx context.Context, userID, agentID string) ([]SessionMeta, error) {
	sessDir := filepath.Join(f.userRoot(userID), "agents", agentID, "agent", "sessions")
	entries, err := os.ReadDir(sessDir)
	if err != nil {
		return nil, nil
	}

	var metas []SessionMeta
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, _ := e.Info()
		metas = append(metas, SessionMeta{
			Key:       strings.TrimSuffix(e.Name(), ".jsonl"),
			UpdatedAt: info.ModTime(),
		})
	}
	return metas, nil
}

func (f *FileStore) DeleteSession(ctx context.Context, userID, agentID, sessionKey string) error {
	return os.Remove(f.sessionPath(userID, agentID, sessionKey))
}

func (f *FileStore) sessionPath(userID, agentID, sessionKey string) string {
	return filepath.Join(f.userRoot(userID), "agents", agentID, "agent", "sessions", sessionKey+".jsonl")
}

// --- Memory ---

func (f *FileStore) GetMemory(ctx context.Context, userID, agentID string) (string, error) {
	path := filepath.Join(f.userRoot(userID), "agents", agentID, "agent", "MEMORY.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil
	}
	return string(data), nil
}

func (f *FileStore) SaveMemory(ctx context.Context, userID, agentID, content string) error {
	path := filepath.Join(f.userRoot(userID), "agents", agentID, "agent", "MEMORY.md")
	return os.WriteFile(path, []byte(content), 0o644)
}

func (f *FileStore) SearchMemory(ctx context.Context, userID, agentID, query string, limit int) ([]MemoryEntry, error) {
	memDir := filepath.Join(f.userRoot(userID), "agents", agentID, "agent", "memory")
	entries, err := os.ReadDir(memDir)
	if err != nil {
		return nil, nil
	}

	queryLower := strings.ToLower(query)
	var results []MemoryEntry

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(memDir, e.Name()))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(strings.ToLower(line), queryLower) {
				results = append(results, MemoryEntry{
					Content:   line,
					SessionID: strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())),
				})
				if limit > 0 && len(results) >= limit {
					return results, nil
				}
			}
		}
	}
	return results, nil
}

func (f *FileStore) AppendMemoryLog(ctx context.Context, userID, agentID string, entry MemoryEntry) error {
	memDir := filepath.Join(f.userRoot(userID), "agents", agentID, "agent", "memory")
	os.MkdirAll(memDir, 0o755)

	filename := entry.Timestamp.Format("2006-01-02") + ".jsonl"
	path := filepath.Join(memDir, filename)

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()

	return json.NewEncoder(file).Encode(entry)
}

// --- Workspace Files ---

func (f *FileStore) GetWorkspaceFile(ctx context.Context, userID, agentID, filename string) ([]byte, error) {
	path := filepath.Join(f.userRoot(userID), "agents", agentID, "agent", filename)
	return os.ReadFile(path)
}

func (f *FileStore) SaveWorkspaceFile(ctx context.Context, userID, agentID, filename string, data []byte) error {
	path := filepath.Join(f.userRoot(userID), "agents", agentID, "agent", filename)
	os.MkdirAll(filepath.Dir(path), 0o755)
	return os.WriteFile(path, data, 0o644)
}

func (f *FileStore) ListWorkspaceFiles(ctx context.Context, userID, agentID string) ([]string, error) {
	wsDir := filepath.Join(f.userRoot(userID), "agents", agentID, "agent")
	entries, err := os.ReadDir(wsDir)
	if err != nil {
		return nil, nil
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, e.Name())
		}
	}
	return files, nil
}

// --- Cron Jobs ---
// Cron jobs are stored per-user in {userRoot}/cron_jobs.json. GetDueCronJobs
// scans across all users under ~/.fastclaw/users/.

func (f *FileStore) cronJobsPath(userID string) string {
	return filepath.Join(f.userRoot(userID), "cron_jobs.json")
}

func (f *FileStore) loadCronJobs(userID string) ([]CronJobRecord, error) {
	data, err := os.ReadFile(f.cronJobsPath(userID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var jobs []CronJobRecord
	if err := json.Unmarshal(data, &jobs); err != nil {
		return nil, err
	}
	// Stamp UserID onto any legacy records missing it.
	for i := range jobs {
		if jobs[i].UserID == "" {
			jobs[i].UserID = userID
		}
	}
	return jobs, nil
}

func (f *FileStore) saveCronJobs(userID string, jobs []CronJobRecord) error {
	dir := f.userRoot(userID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(f.cronJobsPath(userID), data, 0o644)
}

// listUserIDs enumerates existing user directories under ~/.fastclaw/users/.
func (f *FileStore) listUserIDs() []string {
	entries, err := os.ReadDir(f.usersDir())
	if err != nil {
		return nil
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	return ids
}

func (f *FileStore) ListCronJobs(ctx context.Context, userID string) ([]CronJobRecord, error) {
	return f.loadCronJobs(userID)
}

func (f *FileStore) GetCronJob(ctx context.Context, userID, jobID string) (*CronJobRecord, error) {
	jobs, err := f.loadCronJobs(userID)
	if err != nil {
		return nil, err
	}
	for i := range jobs {
		if jobs[i].ID == jobID {
			return &jobs[i], nil
		}
	}
	return nil, fmt.Errorf("cron job not found: %s", jobID)
}

func (f *FileStore) SaveCronJob(ctx context.Context, userID string, job *CronJobRecord) error {
	if job.UserID == "" {
		job.UserID = userID
	}
	jobs, err := f.loadCronJobs(userID)
	if err != nil {
		return err
	}
	found := false
	for i := range jobs {
		if jobs[i].ID == job.ID {
			jobs[i] = *job
			found = true
			break
		}
	}
	if !found {
		jobs = append(jobs, *job)
	}
	return f.saveCronJobs(userID, jobs)
}

func (f *FileStore) DeleteCronJob(ctx context.Context, userID, jobID string) error {
	jobs, err := f.loadCronJobs(userID)
	if err != nil {
		return err
	}
	for i := range jobs {
		if jobs[i].ID == jobID {
			jobs = append(jobs[:i], jobs[i+1:]...)
			return f.saveCronJobs(userID, jobs)
		}
	}
	return fmt.Errorf("cron job not found: %s", jobID)
}

// GetDueCronJobs scans all users for due jobs. In local mode only the "local"
// user exists so this is cheap; in multi-user mode consider a DB backend.
func (f *FileStore) GetDueCronJobs(ctx context.Context, now time.Time) ([]CronJobRecord, error) {
	var due []CronJobRecord
	for _, uid := range f.listUserIDs() {
		jobs, err := f.loadCronJobs(uid)
		if err != nil {
			continue
		}
		for _, j := range jobs {
			if j.Enabled && j.NextRun != nil && !j.NextRun.After(now) {
				due = append(due, j)
			}
		}
	}
	return due, nil
}

func (f *FileStore) LockCronJob(ctx context.Context, jobID, instanceID string) (bool, error) {
	// Single instance: always succeed
	return true, nil
}

func (f *FileStore) UpdateCronJobRun(ctx context.Context, jobID string, lastRun, nextRun time.Time) error {
	// The job's user is unknown here; scan across users.
	for _, uid := range f.listUserIDs() {
		jobs, err := f.loadCronJobs(uid)
		if err != nil {
			continue
		}
		for i := range jobs {
			if jobs[i].ID == jobID {
				jobs[i].LastRun = &lastRun
				jobs[i].NextRun = &nextRun
				return f.saveCronJobs(uid, jobs)
			}
		}
	}
	return fmt.Errorf("cron job not found: %s", jobID)
}

// Ensure FileStore implements Store.
var _ Store = (*FileStore)(nil)
