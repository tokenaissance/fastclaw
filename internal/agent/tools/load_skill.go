package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type loadSkillArgs struct {
	Name string `json:"name"`
}

// RegisterLoadSkill registers the load_skill tool that reads full SKILL.md content.
func RegisterLoadSkill(r *Registry, homeDir, agentDir, teamDir string) {
	r.Register("load_skill", "Load the full content of a skill by name. Use this when you need detailed instructions for a specific skill.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"name": map[string]interface{}{
				"type":        "string",
				"description": "The skill name to load",
			},
		},
		"required": []string{"name"},
	}, makeLoadSkill(homeDir, agentDir, teamDir))
}

func makeLoadSkill(homeDir, agentDir, teamDir string) ToolFunc {
	// Directories to search in priority order (agent > team > global)
	searchDirs := []string{
		filepath.Join(agentDir, "skills"),
	}
	if teamDir != "" {
		searchDirs = append(searchDirs, filepath.Join(teamDir, "skills"))
	}
	searchDirs = append(searchDirs, filepath.Join(homeDir, "skills"))

	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args loadSkillArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}

		if args.Name == "" {
			return "", fmt.Errorf("skill name is required")
		}

		// Search through directories in priority order
		for _, dir := range searchDirs {
			skillPath := filepath.Join(dir, args.Name, "SKILL.md")
			data, err := os.ReadFile(skillPath)
			if err == nil {
				return string(data), nil
			}
		}

		return "", fmt.Errorf("skill %q not found", args.Name)
	}
}
