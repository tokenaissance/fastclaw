// Package store provides a pluggable storage backend for FastClaw.
// Default: file-based (single-user, local). For cloud multi-user: database-backed.
// All persistent state is partitioned by user ID; in local mode the default user
// is used automatically.
package store

import (
	"context"
	"time"
)

// Store is the unified interface for all persistent data.
// File-based impl reads/writes to ~/.fastclaw/users/{userID}/; DB impl uses SQL tables
// partitioned by user_id for multi-user cloud deployments.
type Store interface {
	// Config
	GetConfig(ctx context.Context, userID string) (*UserConfig, error)
	SaveConfig(ctx context.Context, userID string, cfg *UserConfig) error
	DeleteConfig(ctx context.Context, userID string) error

	// Agents
	ListAgents(ctx context.Context, userID string) ([]AgentRecord, error)
	GetAgent(ctx context.Context, userID, agentID string) (*AgentRecord, error)
	SaveAgent(ctx context.Context, userID string, agent *AgentRecord) error
	DeleteAgent(ctx context.Context, userID, agentID string) error

	// Sessions
	GetSession(ctx context.Context, userID, agentID, sessionKey string) (*SessionRecord, error)
	SaveSession(ctx context.Context, userID, agentID, sessionKey string, session *SessionRecord) error
	ListSessions(ctx context.Context, userID, agentID string) ([]SessionMeta, error)
	DeleteSession(ctx context.Context, userID, agentID, sessionKey string) error

	// Memory
	GetMemory(ctx context.Context, userID, agentID string) (string, error) // MEMORY.md content
	SaveMemory(ctx context.Context, userID, agentID, content string) error
	SearchMemory(ctx context.Context, userID, agentID, query string, limit int) ([]MemoryEntry, error)
	AppendMemoryLog(ctx context.Context, userID, agentID string, entry MemoryEntry) error

	// Workspace files (SOUL.md, AGENTS.md, etc.)
	GetWorkspaceFile(ctx context.Context, userID, agentID, filename string) ([]byte, error)
	SaveWorkspaceFile(ctx context.Context, userID, agentID, filename string, data []byte) error
	ListWorkspaceFiles(ctx context.Context, userID, agentID string) ([]string, error)

	// Cron Jobs
	ListCronJobs(ctx context.Context, userID string) ([]CronJobRecord, error)
	GetCronJob(ctx context.Context, userID, jobID string) (*CronJobRecord, error)
	SaveCronJob(ctx context.Context, userID string, job *CronJobRecord) error
	DeleteCronJob(ctx context.Context, userID, jobID string) error
	GetDueCronJobs(ctx context.Context, now time.Time) ([]CronJobRecord, error) // across all users
	LockCronJob(ctx context.Context, jobID, instanceID string) (bool, error)
	UpdateCronJobRun(ctx context.Context, jobID string, lastRun, nextRun time.Time) error

	// Close releases resources.
	Close() error
}

// CronJobRecord holds a scheduled job.
type CronJobRecord struct {
	ID        string     `json:"id"`
	UserID    string     `json:"userId"`
	AgentID   string     `json:"agentId"`
	Name      string     `json:"name"`
	Type      string     `json:"type"`      // cron, interval, once
	Schedule  string     `json:"schedule"`
	Message   string     `json:"message"`
	Channel   string     `json:"channel"`
	ChatID    string     `json:"chatId"`
	AccountID string     `json:"accountId"`
	Timezone  string     `json:"timezone"`
	Enabled   bool       `json:"enabled"`
	LastRun   *time.Time `json:"lastRun,omitempty"`
	NextRun   *time.Time `json:"nextRun,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}

// UserConfig holds the full config for one user (maps to fastclaw.json for file store).
type UserConfig struct {
	UserID    string                 `json:"userId"`
	Data      map[string]interface{} `json:"data"` // raw config JSON
	CreatedAt time.Time              `json:"createdAt"`
	UpdatedAt time.Time              `json:"updatedAt"`
}

// AgentRecord is the persisted state for one agent.
type AgentRecord struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Model       string            `json:"model"`
	Config      map[string]interface{} `json:"config"` // agent.json content
	Workspace   map[string]string `json:"workspace"` // filename -> content (SOUL.md, etc.)
	CreatedAt   time.Time         `json:"createdAt"`
	UpdatedAt   time.Time         `json:"updatedAt"`
}

// SessionRecord holds a conversation session.
type SessionRecord struct {
	Messages  []SessionMessage `json:"messages"`
	UpdatedAt time.Time        `json:"updatedAt"`
}

// SessionMessage is a single message in a session.
type SessionMessage struct {
	Role       string      `json:"role"`
	Content    string      `json:"content"`
	ToolCalls  interface{} `json:"toolCalls,omitempty"`
	ToolCallID string      `json:"toolCallId,omitempty"`
	Timestamp  time.Time   `json:"timestamp"`
}

// SessionMeta is summary info for a session (for listing).
type SessionMeta struct {
	Key          string    `json:"key"`
	MessageCount int       `json:"messageCount"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// MemoryEntry is one searchable memory log entry.
type MemoryEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	SessionID string    `json:"sessionId,omitempty"`
}

// StorageType identifies the storage backend.
type StorageType string

const (
	StorageFile     StorageType = "file"
	StoragePostgres StorageType = "postgres"
	StorageSQLite   StorageType = "sqlite"
)

// StorageConfig is the config block for choosing and configuring the store.
type StorageConfig struct {
	Type     StorageType `json:"type"`               // "file" (default), "postgres", "sqlite"
	DSN      string      `json:"dsn,omitempty"`       // database connection string
	AutoMigrate bool    `json:"autoMigrate,omitempty"` // auto-create tables on startup
}

// DefaultUserID is used for single-user file-based (local) mode.
const DefaultUserID = "local"
