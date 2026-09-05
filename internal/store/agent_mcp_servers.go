package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/config"
)

// agent_mcp_servers stores one row per declared MCP server, keyed by
// (agent_id, server_name). This is the per-key successor to the
// mcpServers object that used to live inside agents.config: operations on
// distinct servers touch distinct rows, so concurrent adds/removes of
// different servers commute instead of racing on one whole-document write.

// ListMCPServers returns every declared MCP server for the agent as the
// typed map the runtime consumes (rc.MCPServers shape).
func (d *DBStore) ListMCPServers(ctx context.Context, agentID string) (map[string]config.MCPServerConfig, error) {
	rows, err := d.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT server_name, config FROM agent_mcp_servers WHERE agent_id = %s ORDER BY server_name`, d.ph(1)),
		agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]config.MCPServerConfig{}
	for rows.Next() {
		var name, cfgStr string
		if err := rows.Scan(&name, &cfgStr); err != nil {
			return nil, err
		}
		var cfg config.MCPServerConfig
		if err := json.Unmarshal([]byte(cfgStr), &cfg); err != nil {
			return nil, fmt.Errorf("decode agent mcp server %q: %w", name, err)
		}
		out[name] = cfg
	}
	return out, rows.Err()
}

// AddMCPServer inserts a new server declaration. The coeffect-table
// precondition "cannot provide twice" (k ∉ dom) is enforced by the
// primary key: a duplicate returns ErrMCPServerExists and no row changes.
func (d *DBStore) AddMCPServer(ctx context.Context, agentID, serverName string, cfg config.MCPServerConfig) error {
	if agentID == "" || serverName == "" {
		return errors.New("store: agentID and serverName are required")
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	res, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO agent_mcp_servers (agent_id, server_name, config, created_at, updated_at)
			VALUES (%s, %s, %s, %s, %s)
			ON CONFLICT (agent_id, server_name) DO NOTHING`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)),
		agentID, serverName, string(data), now, now)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrMCPServerExists
	}
	return nil
}

// DeleteMCPServer removes a server declaration. The restriction
// precondition (k ∈ dom) is enforced: deleting an absent server returns
// ErrNotFound and no row changes.
func (d *DBStore) DeleteMCPServer(ctx context.Context, agentID, serverName string) error {
	if agentID == "" || serverName == "" {
		return errors.New("store: agentID and serverName are required")
	}
	res, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM agent_mcp_servers WHERE agent_id = %s AND server_name = %s`, d.ph(1), d.ph(2)),
		agentID, serverName)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ReplaceMCPServers atomically makes the agent's declaration set equal to
// the given map (dashboard whole-list save / reset semantics). A nil or
// empty map clears every row.
func (d *DBStore) ReplaceMCPServers(ctx context.Context, agentID string, servers map[string]config.MCPServerConfig) error {
	if agentID == "" {
		return errors.New("store: agentID is required")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM agent_mcp_servers WHERE agent_id = %s`, d.ph(1)), agentID); err != nil {
		return err
	}
	now := time.Now().UTC()
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cfg := servers[name]
		data, err := json.Marshal(cfg)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`INSERT INTO agent_mcp_servers (agent_id, server_name, config, created_at, updated_at)
				VALUES (%s, %s, %s, %s, %s)`,
				d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)),
			agentID, name, string(data), now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}
