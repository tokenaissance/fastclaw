package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/fastclaw-ai/fastclaw/internal/agent/tools"
)

// RegisterPluginTools queries a tool plugin for its tools and registers them
// in the given tool registry. Tool names are prefixed with the plugin ID
// to avoid collisions (e.g. "echo.echo_tool").
func RegisterPluginTools(ctx context.Context, mgr *Manager, pluginID string, registry *tools.Registry) error {
	toolDefs, err := mgr.ListTools(ctx, pluginID)
	if err != nil {
		return fmt.Errorf("list tools from plugin %s: %w", pluginID, err)
	}

	for _, td := range toolDefs {
		qualifiedName := pluginID + "." + td.Name
		desc := td.Description
		params := td.Parameters
		toolName := td.Name

		registry.Register(qualifiedName, desc, params,
			func(ctx context.Context, args json.RawMessage) (string, error) {
				var argsMap map[string]interface{}
				if len(args) > 0 {
					if err := json.Unmarshal(args, &argsMap); err != nil {
						return "", fmt.Errorf("parse tool args: %w", err)
					}
				}
				if argsMap == nil {
					argsMap = make(map[string]interface{})
				}
				return mgr.ExecuteTool(ctx, pluginID, toolName, argsMap)
			},
		)

		slog.Info("plugin: registered tool", "plugin", pluginID, "tool", qualifiedName)
	}

	return nil
}
