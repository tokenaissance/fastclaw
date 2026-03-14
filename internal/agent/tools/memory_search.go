package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type memorySearchArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"` // default 10
}

type searchResult struct {
	File      string  `json:"file"`
	Line      int     `json:"line"`
	Content   string  `json:"content"`
	Timestamp string  `json:"timestamp,omitempty"`
	Score     float64 `json:"-"`
}

// RegisterMemorySearch registers the memory_search tool.
func RegisterMemorySearch(r *Registry, workspace string) {
	r.Register("memory_search", "Search through conversation history logs using keyword matching with recency weighting", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"query": map[string]interface{}{
				"type":        "string",
				"description": "Keywords to search for in memory logs",
			},
			"limit": map[string]interface{}{
				"type":        "integer",
				"description": "Maximum number of results to return (default 10)",
			},
		},
		"required": []string{"query"},
	}, makeMemorySearch(workspace))
}

func makeMemorySearch(workspace string) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args memorySearchArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}

		if args.Query == "" {
			return "", fmt.Errorf("query is required")
		}

		limit := args.Limit
		if limit <= 0 {
			limit = 10
		}

		results := searchMemoryLogs(workspace, args.Query, limit)

		if len(results) == 0 {
			return "No matching entries found.", nil
		}

		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Found %d results for %q:\n\n", len(results), args.Query))
		for i, r := range results {
			sb.WriteString(fmt.Sprintf("--- Result %d (file: %s, line: %d) ---\n", i+1, filepath.Base(r.File), r.Line))
			sb.WriteString(r.Content)
			sb.WriteString("\n\n")
		}

		return sb.String(), nil
	}
}

func searchMemoryLogs(workspace, query string, limit int) []searchResult {
	logDir := filepath.Join(workspace, "memory", "logs")
	files, err := filepath.Glob(filepath.Join(logDir, "*.jsonl"))
	if err != nil || len(files) == 0 {
		return nil
	}

	keywords := strings.Fields(strings.ToLower(query))
	if len(keywords) == 0 {
		return nil
	}

	now := time.Now()
	var results []searchResult

	for _, file := range files {
		fileResults := searchFile(file, keywords, now)
		results = append(results, fileResults...)
	}

	// Sort by score (higher = better match + more recent)
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	if len(results) > limit {
		results = results[:limit]
	}

	return results
}

func searchFile(filePath string, keywords []string, now time.Time) []searchResult {
	f, err := os.Open(filePath)
	if err != nil {
		return nil
	}
	defer f.Close()

	// Extract file timestamp for recency weighting
	fileAge := fileRecencyWeight(filePath, now)

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var results []searchResult
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		lower := strings.ToLower(line)

		// Count keyword matches
		matchCount := 0
		for _, kw := range keywords {
			if strings.Contains(lower, kw) {
				matchCount++
			}
		}

		if matchCount == 0 {
			continue
		}

		// Score: keyword match ratio * recency weight
		score := float64(matchCount) / float64(len(keywords)) * fileAge

		// Extract content preview
		content := line
		if len(content) > 500 {
			content = content[:500] + "..."
		}

		results = append(results, searchResult{
			File:    filePath,
			Line:    lineNum,
			Content: content,
			Score:   score,
		})
	}

	return results
}

// fileRecencyWeight returns a weight based on how recent the file is.
// Files from today get weight 1.0, older files decay.
func fileRecencyWeight(filePath string, now time.Time) float64 {
	info, err := os.Stat(filePath)
	if err != nil {
		return 0.5
	}

	age := now.Sub(info.ModTime())
	days := age.Hours() / 24

	// Exponential decay: half-life of 7 days
	if days <= 0 {
		return 1.0
	}
	weight := 1.0 / (1.0 + days/7.0)
	if weight < 0.1 {
		return 0.1
	}
	return weight
}
