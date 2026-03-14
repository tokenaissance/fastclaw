package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

// JobType defines the type of cron schedule.
type JobType string

const (
	JobTypeExact    JobType = "exact"    // specific time: "14:30"
	JobTypeInterval JobType = "interval" // duration: "5m", "1h"
	JobTypeCron     JobType = "cron"     // cron expression: "*/5 * * * *"
)

// Job defines a scheduled job.
type Job struct {
	Name     string  `json:"name"`
	Type     JobType `json:"type"`
	Schedule string  `json:"schedule"` // depends on type
	AgentID  string  `json:"agentId"`
	Channel  string  `json:"channel"`  // channel to send results back through
	ChatID   string  `json:"chatId"`   // chat to send results to
	Message  string  `json:"message"`  // message to send to the agent
}

// CronConfig holds cron job configuration.
type CronConfig struct {
	Jobs []Job `json:"jobs"`
}

// Scheduler manages cron job execution.
type Scheduler struct {
	jobs []Job
	bus  *bus.MessageBus
}

// NewScheduler creates a scheduler from config.
func NewScheduler(jobs []Job, mb *bus.MessageBus) *Scheduler {
	return &Scheduler{
		jobs: jobs,
		bus:  mb,
	}
}

// LoadJobs reads cron jobs from a JSON file.
func LoadJobs(path string) ([]Job, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read cron config: %w", err)
	}

	var cfg CronConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse cron config: %w", err)
	}

	return cfg.Jobs, nil
}

// Start begins the scheduler. It blocks until ctx is cancelled.
func (s *Scheduler) Start(ctx context.Context) {
	if len(s.jobs) == 0 {
		slog.Info("no cron jobs configured")
		return
	}

	slog.Info("cron scheduler started", "jobs", len(s.jobs))

	// Start a goroutine for each job
	for _, job := range s.jobs {
		go s.runJob(ctx, job)
	}

	<-ctx.Done()
	slog.Info("cron scheduler stopped")
}

func (s *Scheduler) runJob(ctx context.Context, job Job) {
	slog.Info("cron job registered", "name", job.Name, "type", job.Type, "schedule", job.Schedule)

	switch job.Type {
	case JobTypeInterval:
		s.runInterval(ctx, job)
	case JobTypeExact:
		s.runExact(ctx, job)
	case JobTypeCron:
		s.runCronExpr(ctx, job)
	default:
		slog.Warn("unknown cron job type", "name", job.Name, "type", job.Type)
	}
}

func (s *Scheduler) runInterval(ctx context.Context, job Job) {
	dur, err := time.ParseDuration(job.Schedule)
	if err != nil {
		slog.Error("invalid interval duration", "name", job.Name, "schedule", job.Schedule, "error", err)
		return
	}

	ticker := time.NewTicker(dur)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.fireJob(job)
		}
	}
}

func (s *Scheduler) runExact(ctx context.Context, job Job) {
	// Parse time in HH:MM format
	parts := strings.Split(job.Schedule, ":")
	if len(parts) != 2 {
		slog.Error("invalid exact time format (expected HH:MM)", "name", job.Name, "schedule", job.Schedule)
		return
	}

	hour, err1 := strconv.Atoi(parts[0])
	minute, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		slog.Error("invalid exact time values", "name", job.Name, "schedule", job.Schedule)
		return
	}

	for {
		now := time.Now()
		next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
		if !next.After(now) {
			next = next.Add(24 * time.Hour)
		}

		waitDur := time.Until(next)
		slog.Info("cron exact job scheduled", "name", job.Name, "next_fire", next.Format("2006-01-02 15:04:05"))

		select {
		case <-ctx.Done():
			return
		case <-time.After(waitDur):
			s.fireJob(job)
		}
	}
}

func (s *Scheduler) runCronExpr(ctx context.Context, job Job) {
	// Simple cron expression parser: "minute hour day month weekday"
	// Supports * and */N for each field
	fields := strings.Fields(job.Schedule)
	if len(fields) != 5 {
		slog.Error("invalid cron expression (expected 5 fields)", "name", job.Name, "schedule", job.Schedule)
		return
	}

	// Check every minute
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			if cronMatch(fields, t) {
				s.fireJob(job)
			}
		}
	}
}

// cronMatch checks if the current time matches a 5-field cron expression.
func cronMatch(fields []string, t time.Time) bool {
	values := []int{t.Minute(), t.Hour(), t.Day(), int(t.Month()), int(t.Weekday())}
	for i, field := range fields {
		if !fieldMatch(field, values[i]) {
			return false
		}
	}
	return true
}

// fieldMatch checks if a value matches a cron field (* or */N or exact number).
func fieldMatch(field string, value int) bool {
	if field == "*" {
		return true
	}
	if strings.HasPrefix(field, "*/") {
		n, err := strconv.Atoi(field[2:])
		if err != nil || n <= 0 {
			return false
		}
		return value%n == 0
	}
	n, err := strconv.Atoi(field)
	if err != nil {
		return false
	}
	return n == value
}

func (s *Scheduler) fireJob(job Job) {
	slog.Info("cron job firing", "name", job.Name, "agent", job.AgentID)

	text := job.Message
	if text == "" {
		text = fmt.Sprintf("[Cron Job: %s] This is a scheduled task trigger.", job.Name)
	}

	s.bus.Inbound <- bus.InboundMessage{
		Channel:  job.Channel,
		ChatID:   job.ChatID,
		UserID:   "cron",
		Text:     text,
		PeerKind: "dm",
	}
}
