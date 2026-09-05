package store

import (
	"context"
	"database/sql"
	"errors"
)

// AgentReloadEpochRow is one user's reload epoch: the user's
// account-scoped state changed at Epoch, so replicas should drop that
// user's cached UserSpace.
//
// Epoch rows live in configs_kv under a dedicated namespace
// (kind="mcp_oauth_reload", scope="user", scope_id=<userID>,
// name="epoch") so cross-replica polling can filter them without
// scanning unrelated config rows. The dedicated agent_reload_epochs
// table from earlier drafts was folded back into configs_kv per the
// design doc.
type AgentReloadEpochRow struct {
	UserID string
	Epoch  string
}

const (
	reloadEpochKind  = "mcp_oauth_reload"
	reloadEpochScope = "user"
	reloadEpochName  = "epoch"
)

// UpsertAgentReloadEpoch stamps (or replaces) a user's reload epoch.
func (d *DBStore) UpsertAgentReloadEpoch(ctx context.Context, userID, epoch string) error {
	q := "INSERT INTO configs_kv (kind, scope, scope_id, name, value) VALUES (" +
		d.ph(1) + ", " + d.ph(2) + ", " + d.ph(3) + ", " + d.ph(4) + ", " + d.ph(5) + ") " +
		"ON CONFLICT (kind, scope, scope_id, name) DO UPDATE SET value = EXCLUDED.value"
	_, err := d.db.ExecContext(ctx, q, reloadEpochKind, reloadEpochScope, userID, reloadEpochName, epoch)
	return err
}

// GetAgentReloadEpoch returns a user's current epoch ("" if none).
func (d *DBStore) GetAgentReloadEpoch(ctx context.Context, userID string) (string, error) {
	var epoch string
	err := d.db.QueryRowContext(ctx,
		"SELECT value FROM configs_kv WHERE kind = "+d.ph(1)+" AND scope = "+d.ph(2)+" AND scope_id = "+d.ph(3)+" AND name = "+d.ph(4),
		reloadEpochKind, reloadEpochScope, userID, reloadEpochName).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return epoch, nil
}

// ListAgentReloadEpochs returns every user's current epoch.
func (d *DBStore) ListAgentReloadEpochs(ctx context.Context) ([]AgentReloadEpochRow, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT scope_id, value FROM configs_kv WHERE kind = "+d.ph(1)+" AND scope = "+d.ph(2)+" AND name = "+d.ph(3),
		reloadEpochKind, reloadEpochScope, reloadEpochName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentReloadEpochRow
	for rows.Next() {
		var r AgentReloadEpochRow
		if err := rows.Scan(&r.UserID, &r.Epoch); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
