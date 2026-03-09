package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type execArgs struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"` // seconds, default 30
}

var dangerousCommands = []string{
	"rm -rf /",
	"mkfs",
	"dd if=",
	":(){:|:&};:",
	"> /dev/sda",
}

func registerExec(r *Registry) {
	r.Register("exec", "Execute a shell command and return stdout/stderr", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"command": map[string]interface{}{
				"type":        "string",
				"description": "The shell command to execute",
			},
			"timeout": map[string]interface{}{
				"type":        "integer",
				"description": "Timeout in seconds (default 30)",
			},
		},
		"required": []string{"command"},
	}, execTool)
}

func execTool(ctx context.Context, rawArgs json.RawMessage) (string, error) {
	var args execArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	if args.Command == "" {
		return "", fmt.Errorf("command is required")
	}

	// Check for dangerous commands
	lower := strings.ToLower(args.Command)
	for _, dc := range dangerousCommands {
		if strings.Contains(lower, dc) {
			return "", fmt.Errorf("dangerous command blocked: %s", args.Command)
		}
	}

	timeout := 30
	if args.Timeout > 0 {
		timeout = args.Timeout
	}

	execCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(execCtx, "sh", "-c", args.Command)
	output, err := cmd.CombinedOutput()

	result := string(output)
	if err != nil {
		return fmt.Sprintf("%s\nError: %s", result, err.Error()), err
	}

	return result, nil
}
