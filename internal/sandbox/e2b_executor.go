package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// E2B API: https://e2b.dev/docs/api

const e2bBaseURL = "https://api.e2b.dev"

// E2BExecutor implements Executor using E2B hosted sandboxes. Each executor
// maps to one E2B sandbox instance (a lightweight Firecracker microVM).
type E2BExecutor struct {
	apiKey    string
	sandboxID string
	client    *http.Client
}

// newE2BExecutor creates a sandbox via the E2B API and returns an executor
// connected to it.
func newE2BExecutor(ctx context.Context, apiKey, template string, timeout time.Duration) (*E2BExecutor, error) {
	if template == "" {
		template = "base" // E2B default template
	}
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}

	client := &http.Client{Timeout: 30 * time.Second}

	body, _ := json.Marshal(map[string]interface{}{
		"templateID":     template,
		"timeoutMs":      int(timeout.Milliseconds()),
	})
	req, err := http.NewRequestWithContext(ctx, "POST", e2bBaseURL+"/sandboxes", bytes.NewReader(body))
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
		SandboxID string `json:"sandboxID"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("e2b parse response: %w", err)
	}

	slog.Info("e2b sandbox created", "sandboxID", result.SandboxID, "template", template)

	return &E2BExecutor{
		apiKey:    apiKey,
		sandboxID: result.SandboxID,
		client:    client,
	}, nil
}

func (e *E2BExecutor) Exec(ctx context.Context, command string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	body, _ := json.Marshal(map[string]interface{}{
		"cmd":       command,
		"timeoutMs": int(timeout.Milliseconds()),
	})
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/sandboxes/%s/commands", e2bBaseURL, e.sandboxID),
		bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", e.apiKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("e2b exec: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		ExitCode int    `json:"exitCode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("e2b exec parse: %w", err)
	}

	output := result.Stdout
	if result.Stderr != "" {
		output += "\n" + result.Stderr
	}
	if result.ExitCode != 0 {
		return output, fmt.Errorf("exit code %d", result.ExitCode)
	}
	return output, nil
}

func (e *E2BExecutor) ReadFile(ctx context.Context, path string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("%s/sandboxes/%s/files/%s", e2bBaseURL, e.sandboxID, path), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-API-Key", e.apiKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("e2b read file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("e2b read file %s: HTTP %d: %s", path, resp.StatusCode, string(body))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (e *E2BExecutor) WriteFile(ctx context.Context, path, content string) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"content": content,
	})
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/sandboxes/%s/files/%s", e2bBaseURL, e.sandboxID, path),
		bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", e.apiKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("e2b write file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("e2b write file %s: HTTP %d: %s", path, resp.StatusCode, string(respBody))
	}
	return fmt.Sprintf("Written to %s", path), nil
}

func (e *E2BExecutor) ListDir(ctx context.Context, path string) (string, error) {
	if path == "" {
		path = "/"
	}
	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("%s/sandboxes/%s/files?path=%s", e2bBaseURL, e.sandboxID, path), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-API-Key", e.apiKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("e2b list dir: %w", err)
	}
	defer resp.Body.Close()

	var entries []struct {
		Name  string `json:"name"`
		IsDir bool   `json:"isDir"`
		Size  int64  `json:"size"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return "", fmt.Errorf("e2b list dir parse: %w", err)
	}

	var sb strings.Builder
	for _, ent := range entries {
		if ent.IsDir {
			fmt.Fprintf(&sb, "d %s/\n", ent.Name)
		} else {
			fmt.Fprintf(&sb, "f %s (%d bytes)\n", ent.Name, ent.Size)
		}
	}
	return sb.String(), nil
}

func (e *E2BExecutor) Close() error {
	req, _ := http.NewRequest("DELETE",
		fmt.Sprintf("%s/sandboxes/%s", e2bBaseURL, e.sandboxID), nil)
	req.Header.Set("X-API-Key", e.apiKey)
	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("e2b destroy sandbox: %w", err)
	}
	resp.Body.Close()
	slog.Info("e2b sandbox destroyed", "sandboxID", e.sandboxID)
	return nil
}

// E2BExecutorPool manages per-user E2B sandboxes.
type E2BExecutorPool struct {
	mu        sync.Mutex
	executors map[string]*E2BExecutor
	apiKey    string
	template  string
	timeout   time.Duration
}

// NewE2BExecutorPool creates a pool of E2B-backed executors.
func NewE2BExecutorPool(apiKey, template string, timeout time.Duration) *E2BExecutorPool {
	return &E2BExecutorPool{
		executors: make(map[string]*E2BExecutor),
		apiKey:    apiKey,
		template:  template,
		timeout:   timeout,
	}
}

func (p *E2BExecutorPool) Get(ctx context.Context, userID string) (Executor, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if ex, ok := p.executors[userID]; ok {
		return ex, nil
	}

	ex, err := newE2BExecutor(ctx, p.apiKey, p.template, p.timeout)
	if err != nil {
		return nil, err
	}
	p.executors[userID] = ex
	return ex, nil
}

func (p *E2BExecutorPool) Release(userID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ex, ok := p.executors[userID]; ok {
		delete(p.executors, userID)
		return ex.Close()
	}
	return nil
}

func (p *E2BExecutorPool) CloseAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for uid, ex := range p.executors {
		ex.Close()
		delete(p.executors, uid)
	}
}

var (
	_ Executor     = (*E2BExecutor)(nil)
	_ ExecutorPool = (*E2BExecutorPool)(nil)
)
