package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// Policy holds resource/network constraints for a sandbox container.
type Policy struct {
	MaxCPU    string // e.g. "2"
	MaxMemory string // e.g. "512m"
	NetMode   string // "none", "host", "bridge"
}

// DockerSandbox manages a single Docker container for sandboxed execution.
type DockerSandbox struct {
	containerID string
	image       string
	workspace   string
	policy      *Policy
	env         map[string]string
	mu          sync.Mutex
}

// NewDockerSandbox creates a new sandbox configuration (container is created lazily).
func NewDockerSandbox(image, workspace string, policy *Policy) *DockerSandbox {
	if image == "" {
		image = "fastclaw/sandbox:latest"
	}
	if policy == nil {
		policy = &Policy{NetMode: "none"}
	}
	return &DockerSandbox{
		image:     image,
		workspace: workspace,
		policy:    policy,
		env:       make(map[string]string),
	}
}

// SetEnv sets environment variables to inject into the container.
func (s *DockerSandbox) SetEnv(env map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range env {
		s.env[k] = v
	}
}

// Create creates the Docker container.
func (s *DockerSandbox) Create() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.containerID != "" {
		return nil // already created
	}

	args := []string{
		"create",
		"--interactive",
		"--label", "fastclaw=sandbox",
	}

	// Mount workspace
	if s.workspace != "" {
		args = append(args, "-v", fmt.Sprintf("%s:/workspace:rw", s.workspace))
		args = append(args, "-w", "/workspace")
	}

	// Resource limits
	if s.policy.MaxCPU != "" {
		args = append(args, "--cpus", s.policy.MaxCPU)
	}
	if s.policy.MaxMemory != "" {
		args = append(args, "--memory", s.policy.MaxMemory)
	}

	// Network mode
	if s.policy.NetMode != "" {
		args = append(args, "--network", s.policy.NetMode)
	}

	// Environment variables
	for k, v := range s.env {
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	}

	args = append(args, s.image, "tail", "-f", "/dev/null")

	cmd := exec.Command("docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker create: %s: %w", strings.TrimSpace(stderr.String()), err)
	}

	s.containerID = strings.TrimSpace(stdout.String())

	// Start the container
	startCmd := exec.Command("docker", "start", s.containerID)
	if out, err := startCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker start: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return nil
}

// Exec runs a command inside the container.
func (s *DockerSandbox) Exec(ctx context.Context, command string, workdir string) (string, error) {
	s.mu.Lock()
	if s.containerID == "" {
		s.mu.Unlock()
		if err := s.Create(); err != nil {
			return "", err
		}
		s.mu.Lock()
	}
	id := s.containerID
	s.mu.Unlock()

	args := []string{"exec"}
	if workdir != "" {
		args = append(args, "-w", workdir)
	}
	args = append(args, id, "sh", "-c", command)

	cmd := exec.CommandContext(ctx, "docker", args...)
	output, err := cmd.CombinedOutput()
	result := string(output)
	if err != nil {
		return fmt.Sprintf("%s\nError: %s", result, err.Error()), err
	}
	return result, nil
}

// Close stops and removes the container.
func (s *DockerSandbox) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.containerID == "" {
		return nil
	}

	cmd := exec.Command("docker", "rm", "-f", s.containerID)
	cmd.CombinedOutput() // best effort
	s.containerID = ""
	return nil
}

// ContainerID returns the current container ID, or empty if not created.
func (s *DockerSandbox) ContainerID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.containerID
}
