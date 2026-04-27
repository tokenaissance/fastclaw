package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	skillDirs   []string // host paths to mount read-only at /skills/<name>/
	policy      *Policy
	env         map[string]string
	mu          sync.Mutex
}

// NewDockerSandbox creates a new sandbox configuration (container is created lazily).
//
// Default policy leaves NetMode unset, which means Docker uses the
// default bridge network (= internet access). Most product agents
// need outbound HTTP for LLM provider calls / image APIs / pip
// installs; locking the sandbox down with NetMode="none" used to be
// the default and silently broke generate-image-style skills with
// DNS resolution errors. Operators that want hard isolation pass an
// explicit policy with NetMode: "none".
func NewDockerSandbox(image, workspace string, policy *Policy) *DockerSandbox {
	if image == "" {
		image = "fastclaw/sandbox:latest"
	}
	if policy == nil {
		policy = &Policy{}
	}
	return &DockerSandbox{
		image:     image,
		workspace: workspace,
		policy:    policy,
		env:       make(map[string]string),
	}
}

// SetSkillDirs configures host paths whose contents (skill folders)
// should be visible inside the sandbox at /skills/<skill-name>/. The
// LLM is told to invoke skills via `python /skills/<name>/main.py`,
// so without these mounts the script files don't exist in the
// container. Passed paths are mounted read-only.
func (s *DockerSandbox) SetSkillDirs(dirs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.skillDirs = append(s.skillDirs[:0], dirs...)
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

	// Mount each skill dir read-only at /skills/<basename>/. The LLM
	// is told to invoke skills via `python /skills/<name>/main.py`,
	// so without these mounts the script files don't exist in the
	// container. Auto-default to FASTCLAW_HOME/skills/ when no dirs
	// are explicitly set, so a freshly-installed product agent works
	// without operators having to wire SetSkillDirs themselves.
	dirs := s.skillDirs
	if len(dirs) == 0 {
		if h := os.Getenv("FASTCLAW_HOME"); h != "" {
			dirs = []string{filepath.Join(h, "skills")}
		} else if home, err := os.UserHomeDir(); err == nil {
			dirs = []string{filepath.Join(home, ".fastclaw", "skills")}
		}
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			host := filepath.Join(dir, e.Name())
			args = append(args, "-v", fmt.Sprintf("%s:/skills/%s:ro", host, e.Name()))
		}
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
