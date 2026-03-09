package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fastclaw-ai/fastclaw/internal/provider"
)

// ToolFunc is a function that executes a tool with JSON arguments and returns a result string.
type ToolFunc func(ctx context.Context, args json.RawMessage) (string, error)

// Registry holds all registered tools.
type Registry struct {
	tools map[string]registeredTool
}

type registeredTool struct {
	def provider.Tool
	fn  ToolFunc
}

// NewRegistry creates a new tool registry with built-in tools.
func NewRegistry(workspace string) *Registry {
	r := &Registry{
		tools: make(map[string]registeredTool),
	}
	r.registerBuiltins(workspace)
	return r
}

// Register adds a tool to the registry.
func (r *Registry) Register(name, description string, parameters interface{}, fn ToolFunc) {
	r.tools[name] = registeredTool{
		def: provider.Tool{
			Type: "function",
			Function: provider.ToolFunction{
				Name:        name,
				Description: description,
				Parameters:  parameters,
			},
		},
		fn: fn,
	}
}

// Definitions returns all tool definitions for the LLM.
func (r *Registry) Definitions() []provider.Tool {
	defs := make([]provider.Tool, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, t.def)
	}
	return defs
}

// Execute runs a tool by name with the given arguments.
func (r *Registry) Execute(ctx context.Context, name string, args string) (string, error) {
	tool, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("unknown tool: %s", name)
	}

	result, err := tool.fn(ctx, json.RawMessage(args))
	if err != nil {
		return result + "\n[Analyze the error above and try a different approach.]", err
	}
	return result, nil
}

func (r *Registry) registerBuiltins(workspace string) {
	registerExec(r)
	registerFile(r, workspace)
	registerMessage(r)
}
