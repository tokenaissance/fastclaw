package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "github.com/lib/pq"          // PostgreSQL driver
	_ "modernc.org/sqlite"         // SQLite driver (pure Go)
)

// DBStore implements Store using a SQL database (PostgreSQL or SQLite).
type DBStore struct {
	db      *sql.DB
	dialect string // "postgres" or "sqlite"
}

// NewDBStore creates a database-backed store.
func NewDBStore(dialect, dsn string) (*DBStore, error) {
	db, err := sql.Open(driverName(dialect), dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dialect, err)
	}
	// SQLite serializes all writes through a single global lock; granting
	// 25 parallel connections makes the userland queue *deeper* without
	// adding throughput, and on a busy install (cron scheduler + web
	// traffic) it quickly stacks up SQLITE_BUSY past the busy_timeout.
	// Postgres handles real concurrency, so we keep the wider pool there.
	if dialect == "sqlite" {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	} else {
		db.SetMaxOpenConns(25)
		db.SetMaxIdleConns(5)
	}
	db.SetConnMaxLifetime(5 * time.Minute)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping %s: %w", dialect, err)
	}
	return &DBStore{db: db, dialect: dialect}, nil
}

func driverName(dialect string) string {
	switch dialect {
	case "postgres":
		return "postgres"
	case "sqlite":
		return "sqlite"
	default:
		return dialect
	}
}

// Migrate creates tables if they don't exist. The schema is the canonical
// shape — there are no in-place ALTERs because there is no installed base
// from before this rewrite.
func (d *DBStore) Migrate(ctx context.Context) error {
	for _, stmt := range d.migrationSQL() {
		if _, err := d.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate: %w\nSQL: %s", err, stmt)
		}
	}
	if err := d.migrateAgentFilesUserID(ctx); err != nil {
		return fmt.Errorf("migrate agent_files.user_id: %w", err)
	}
	if err := d.migrateAgentsDropTemplateID(ctx); err != nil {
		return fmt.Errorf("migrate agents.template_id drop: %w", err)
	}
	if err := d.migrateAgentsDropModel(ctx); err != nil {
		return fmt.Errorf("migrate agents.model drop: %w", err)
	}
	if err := d.migrateSkillsAgentEntriesSplit(ctx); err != nil {
		return fmt.Errorf("migrate skills.agentEntries split: %w", err)
	}
	if err := d.migrateAgentFilesDropTemplate(ctx); err != nil {
		return fmt.Errorf("migrate agent_files drop template: %w", err)
	}
	if err := d.migrateUsersAppUserCols(ctx); err != nil {
		return fmt.Errorf("migrate users app_user cols: %w", err)
	}
	if err := d.migrateAPIKeysAddType(ctx); err != nil {
		return fmt.Errorf("migrate apikeys.type: %w", err)
	}
	if err := d.migrateUsersAvatarURL(ctx); err != nil {
		return fmt.Errorf("migrate users.avatar_url: %w", err)
	}
	return nil
}

// migrateUsersAvatarURL retrofits the avatar_url column onto pre-feature
// installs. Stored as a data: URL so the file lives inline with the row
// — no separate blob store path or cleanup. Empty string means "no
// avatar"; the UI falls back to initials.
func (d *DBStore) migrateUsersAvatarURL(ctx context.Context) error {
	has, err := d.tableHasColumn(ctx, "users", "avatar_url")
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	if _, err := d.db.ExecContext(ctx,
		`ALTER TABLE users ADD COLUMN avatar_url TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add avatar_url: %w", err)
	}
	return nil
}

// migrateAPIKeysAddType retrofits the `type` column onto apikeys for
// pre-tier installs. Every legacy row was an explicit-agent-list key, so
// backfilling DEFAULT 'agent' preserves behavior — admin/user tiers can
// only be created from this point forward.
func (d *DBStore) migrateAPIKeysAddType(ctx context.Context) error {
	has, err := d.tableHasColumn(ctx, "apikeys", "type")
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	if _, err := d.db.ExecContext(ctx,
		`ALTER TABLE apikeys ADD COLUMN type TEXT NOT NULL DEFAULT 'agent'`); err != nil {
		return fmt.Errorf("add type: %w", err)
	}
	return nil
}

// migrateUsersAppUserCols retrofits the apikey_id + external_id columns
// onto the users table for existing installs, and creates the partial
// unique index used for idempotent provisioning. CREATE TABLE only fires
// on a fresh DB; older databases reach this with the legacy 7-column
// users table and exit it with the new 9-column shape. Idempotent: each
// step probes for existing state before mutating.
func (d *DBStore) migrateUsersAppUserCols(ctx context.Context) error {
	hasAPIKey, err := d.tableHasColumn(ctx, "users", "apikey_id")
	if err != nil {
		return err
	}
	if !hasAPIKey {
		if _, err := d.db.ExecContext(ctx,
			`ALTER TABLE users ADD COLUMN apikey_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add apikey_id: %w", err)
		}
	}
	hasExt, err := d.tableHasColumn(ctx, "users", "external_id")
	if err != nil {
		return err
	}
	if !hasExt {
		if _, err := d.db.ExecContext(ctx,
			`ALTER TABLE users ADD COLUMN external_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add external_id: %w", err)
		}
	}
	// CREATE UNIQUE INDEX IF NOT EXISTS is supported by both backends.
	if _, err := d.db.ExecContext(ctx,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_apikey_external
			ON users (apikey_id, external_id)
			WHERE apikey_id <> '' AND external_id <> ''`); err != nil {
		return fmt.Errorf("create idx_users_apikey_external: %w", err)
	}
	return nil
}

// migrateAgentFilesDropTemplate clears the legacy user_id='' template
// rows from agent_files. Each row is reparented to the agent's owner
// when no per-user row already exists for that (agent_id, filename) —
// preserves existing content as the owner's personal copy. After this
// pass the table holds (agent_id, real_user_id, filename) tuples only;
// any "shared SOUL.md across all users" use case should live in a local
// FS file at <agent_home>/<name>, which the runtime falls back to.
// Idempotent: re-runs find no user_id='' rows and exit clean.
func (d *DBStore) migrateAgentFilesDropTemplate(ctx context.Context) error {
	rows, err := d.db.QueryContext(ctx,
		`SELECT agent_files.agent_id, agent_files.filename, agent_files.content, agents.user_id
			FROM agent_files
			LEFT JOIN agents ON agents.id = agent_files.agent_id
			WHERE agent_files.user_id = ''`)
	if err != nil {
		return fmt.Errorf("scan template rows: %w", err)
	}
	type tmpl struct {
		agentID, filename, content string
		ownerID                    sql.NullString
	}
	var pending []tmpl
	for rows.Next() {
		var t tmpl
		if err := rows.Scan(&t.agentID, &t.filename, &t.content, &t.ownerID); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, t)
	}
	rows.Close()
	now := time.Now().UTC()
	for _, t := range pending {
		if t.ownerID.Valid && t.ownerID.String != "" {
			// Reparent only when the owner has no row of their own
			// for this (agent_id, filename) — never clobber an
			// existing personal copy.
			var exists int
			row := d.db.QueryRowContext(ctx,
				fmt.Sprintf(`SELECT 1 FROM agent_files
					WHERE agent_id = %s AND user_id = %s AND filename = %s LIMIT 1`,
					d.ph(1), d.ph(2), d.ph(3)),
				t.agentID, t.ownerID.String, t.filename)
			if err := row.Scan(&exists); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("probe existing row: %w", err)
			}
			if exists != 1 {
				if d.dialect == "postgres" {
					if _, err := d.db.ExecContext(ctx,
						`INSERT INTO agent_files (agent_id, user_id, filename, content, updated_at)
							VALUES ($1, $2, $3, $4, $5)`,
						t.agentID, t.ownerID.String, t.filename, t.content, now); err != nil {
						return fmt.Errorf("reparent template row: %w", err)
					}
				} else {
					if _, err := d.db.ExecContext(ctx,
						`INSERT INTO agent_files (agent_id, user_id, filename, content, updated_at)
							VALUES (?, ?, ?, ?, ?)`,
						t.agentID, t.ownerID.String, t.filename, t.content, now); err != nil {
						return fmt.Errorf("reparent template row: %w", err)
					}
				}
			}
		}
	}
	if _, err := d.db.ExecContext(ctx,
		`DELETE FROM agent_files WHERE user_id = ''`); err != nil {
		return fmt.Errorf("delete template rows: %w", err)
	}
	return nil
}

// migrateSkillsAgentEntriesSplit relocates per-agent skill env overrides
// off the single user/system-scope skills.agentEntries row (a JSON blob
// keyed by agent_id, which grew unboundedly with each agent × skill)
// into one row per agent at scope=agent name=skills.entries — the same
// shape the runtime now reads via scope.GetConfigByName per agent.
// Idempotent: every legacy row found gets split + deleted in a single
// pass; subsequent runs find no legacy rows and exit clean.
func (d *DBStore) migrateSkillsAgentEntriesSplit(ctx context.Context) error {
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, scope, scope_id, data FROM configs WHERE kind='setting' AND name='skills.agentEntries'`)
	if err != nil {
		return fmt.Errorf("scan legacy: %w", err)
	}
	type legacy struct{ id, scopeID, dataJSON string }
	var legacyRows []legacy
	for rows.Next() {
		var l legacy
		var sc string
		if err := rows.Scan(&l.id, &sc, &l.scopeID, &l.dataJSON); err != nil {
			rows.Close()
			return err
		}
		_ = sc
		legacyRows = append(legacyRows, l)
	}
	rows.Close()
	now := time.Now().UTC()
	for _, l := range legacyRows {
		// data shape: { "<agent_id>": { "<skill_name>": { ...entry } } }
		var byAgent map[string]map[string]interface{}
		if err := json.Unmarshal([]byte(l.dataJSON), &byAgent); err != nil {
			// Malformed row — drop it; not worth aborting migration.
			if _, derr := d.db.ExecContext(ctx,
				fmt.Sprintf(`DELETE FROM configs WHERE id=%s`, d.ph(1)), l.id); derr != nil {
				return fmt.Errorf("drop malformed legacy row: %w", derr)
			}
			continue
		}
		for agentID, inner := range byAgent {
			if len(inner) == 0 {
				continue
			}
			cid := configRowID("setting", "agent", agentID, "skills.entries")
			innerBlob, _ := json.Marshal(inner)
			// Skip if a per-agent row already exists (manual edit, prior
			// partial migration, etc.) — don't clobber.
			var exists int
			err := d.db.QueryRowContext(ctx,
				fmt.Sprintf(`SELECT 1 FROM configs WHERE id=%s LIMIT 1`, d.ph(1)), cid).Scan(&exists)
			if err == nil {
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("check existing per-agent row: %w", err)
			}
			insert := fmt.Sprintf(`INSERT INTO configs (id, kind, scope, scope_id, name, enabled, credential_key, data, created_at, updated_at)
				VALUES (%s, 'setting', 'agent', %s, 'skills.entries', TRUE, '', %s, %s, %s)`,
				d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5))
			if _, err := d.db.ExecContext(ctx, insert, cid, agentID, string(innerBlob), now, now); err != nil {
				return fmt.Errorf("insert per-agent row for %s: %w", agentID, err)
			}
		}
		if _, err := d.db.ExecContext(ctx,
			fmt.Sprintf(`DELETE FROM configs WHERE id=%s`, d.ph(1)), l.id); err != nil {
			return fmt.Errorf("drop legacy row: %w", err)
		}
	}
	return nil
}

// migrateAgentsDropModel relocates per-agent model overrides off the
// agents.model column into the configs table (kind=setting, scope=agent,
// scope_id=<aid>, name="agents.defaults", data={"model":"..."}). The
// configs path is what the runtime now reads via scope.SettingInto, so
// keeping the column would just duplicate state. Idempotent: silently
// no-ops once the column is gone.
func (d *DBStore) migrateAgentsDropModel(ctx context.Context) error {
	has, err := d.tableHasColumn(ctx, "agents", "model")
	if err != nil {
		return err
	}
	if !has {
		return nil
	}
	rows, err := d.db.QueryContext(ctx, `SELECT id, model FROM agents WHERE model <> ''`)
	if err != nil {
		return fmt.Errorf("scan legacy models: %w", err)
	}
	type row struct{ id, model string }
	var legacy []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.model); err != nil {
			rows.Close()
			return err
		}
		legacy = append(legacy, r)
	}
	rows.Close()
	now := time.Now().UTC()
	for _, r := range legacy {
		// Don't overwrite an already-existing configs row — the runtime
		// has been writing there since this migration shipped, so an
		// existing row is the source of truth.
		var exists int
		err := d.db.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT 1 FROM configs WHERE kind='setting' AND scope='agent' AND scope_id=%s AND name='agents.defaults' LIMIT 1`,
				d.ph(1)),
			r.id).Scan(&exists)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check existing setting: %w", err)
		}
		cid := configRowID("setting", "agent", r.id, "agents.defaults")
		blob, _ := json.Marshal(map[string]string{"model": r.model})
		insertSQL := fmt.Sprintf(`INSERT INTO configs (id, kind, scope, scope_id, name, enabled, credential_key, data, created_at, updated_at)
			VALUES (%s, 'setting', 'agent', %s, 'agents.defaults', TRUE, '', %s, %s, %s)`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5))
		if _, err := d.db.ExecContext(ctx, insertSQL, cid, r.id, string(blob), now, now); err != nil {
			return fmt.Errorf("relocate model for agent %s: %w", r.id, err)
		}
	}
	stmt := `ALTER TABLE agents DROP COLUMN model`
	if d.dialect == "postgres" {
		stmt = `ALTER TABLE agents DROP COLUMN IF EXISTS model`
	}
	if _, err := d.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("drop column: %w\nSQL: %s", err, stmt)
	}
	return nil
}

// migrateAgentsDropTemplateID removes the never-read template_id column
// from existing installs. Idempotent: silently no-ops when the column
// is already gone. SQLite needs 3.35+ for DROP COLUMN — every supported
// runtime here ships well above that, so we don't fall back to rebuild.
func (d *DBStore) migrateAgentsDropTemplateID(ctx context.Context) error {
	has, err := d.tableHasColumn(ctx, "agents", "template_id")
	if err != nil {
		return err
	}
	if !has {
		return nil
	}
	stmt := `ALTER TABLE agents DROP COLUMN template_id`
	if d.dialect == "postgres" {
		stmt = `ALTER TABLE agents DROP COLUMN IF EXISTS template_id`
	}
	if _, err := d.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("drop column: %w\nSQL: %s", err, stmt)
	}
	return nil
}

// migrateAgentFilesUserID retrofits the per-user override column on
// pre-existing installs. CREATE TABLE IF NOT EXISTS only fires on a
// fresh DB, so legacy databases keep the old (agent_id, filename) PK
// until this runs. Idempotent: detects the missing column and rebuilds
// the table once. SQLite has no ALTER TABLE for changing PKs, so we
// copy-rename. Postgres can ALTER directly.
func (d *DBStore) migrateAgentFilesUserID(ctx context.Context) error {
	hasUserID, err := d.tableHasColumn(ctx, "agent_files", "user_id")
	if err != nil {
		return err
	}
	if hasUserID {
		return nil
	}
	if d.dialect == "postgres" {
		stmts := []string{
			`ALTER TABLE agent_files ADD COLUMN user_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE agent_files DROP CONSTRAINT IF EXISTS agent_files_pkey`,
			`ALTER TABLE agent_files ADD PRIMARY KEY (agent_id, user_id, filename)`,
		}
		for _, s := range stmts {
			if _, err := d.db.ExecContext(ctx, s); err != nil {
				return fmt.Errorf("postgres migrate: %w\nSQL: %s", err, s)
			}
		}
		return nil
	}
	// SQLite: rebuild the table to widen the PK.
	stmts := []string{
		`CREATE TABLE agent_files_new (
			agent_id TEXT NOT NULL,
			user_id TEXT NOT NULL DEFAULT '',
			filename TEXT NOT NULL,
			content TEXT NOT NULL DEFAULT '',
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (agent_id, user_id, filename)
		)`,
		`INSERT INTO agent_files_new (agent_id, user_id, filename, content, updated_at)
			SELECT agent_id, '', filename, content, updated_at FROM agent_files`,
		`DROP TABLE agent_files`,
		`ALTER TABLE agent_files_new RENAME TO agent_files`,
	}
	for _, s := range stmts {
		if _, err := d.db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("sqlite rebuild: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

// tableHasColumn returns true when the named column exists on the table.
// Backend-specific: Postgres reads information_schema; SQLite uses the
// PRAGMA table_info() pseudo-table.
func (d *DBStore) tableHasColumn(ctx context.Context, table, column string) (bool, error) {
	if d.dialect == "postgres" {
		row := d.db.QueryRowContext(ctx,
			`SELECT 1 FROM information_schema.columns
				WHERE table_name = $1 AND column_name = $2 LIMIT 1`,
			table, column)
		var x int
		if err := row.Scan(&x); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (d *DBStore) migrationSQL() []string {
	return []string{
		// users holds first-party humans (role=super_admin/user) AND
		// app-provisioned end-users (role=app_user). The latter are
		// minted by an api_key on behalf of a downstream application;
		// they cannot log in (password_hash='' is rejected by the
		// password login path). apikey_id + external_id together
		// identify "which calling app, which of its end-users", and
		// the partial UNIQUE makes provisioning idempotent on that
		// pair so the same external user always resolves to the same
		// fastclaw user_id.
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			username TEXT NOT NULL UNIQUE,
			email TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL DEFAULT '',
			display_name TEXT NOT NULL DEFAULT '',
			role TEXT NOT NULL DEFAULT 'user',
			status TEXT NOT NULL DEFAULT 'active',
			apikey_id TEXT NOT NULL DEFAULT '',
			external_id TEXT NOT NULL DEFAULT '',
			avatar_url TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		// Idempotency lookup for app_user provisioning lives in
		// migrateUsersAppUserCols, not here — on existing installs the
		// CREATE TABLE above is a no-op and the apikey_id column
		// doesn't exist yet, so the index has to wait until the
		// column-add step has run.
		`CREATE TABLE IF NOT EXISTS web_sessions (
			sid TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			expires_at TIMESTAMP NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_web_sessions_user ON web_sessions (user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_web_sessions_expires ON web_sessions (expires_at)`,
		// type values: "admin" | "user" | "agent". The default 'agent'
		// preserves the pre-tier behavior on existing rows — every legacy
		// key was implicitly an "agent-scoped" key (explicit list in
		// apikey_agents), so the migration can backfill blindly. See
		// migrateAPIKeysAddType for the ALTER on existing installs.
		`CREATE TABLE IF NOT EXISTS apikeys (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			key_hash TEXT NOT NULL,
			key_prefix TEXT NOT NULL DEFAULT '',
			type TEXT NOT NULL DEFAULT 'agent',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_apikeys_user ON apikeys (user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_apikeys_key_hash ON apikeys (key_hash)`,
		`CREATE TABLE IF NOT EXISTS apikey_agents (
			apikey_id TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			PRIMARY KEY (apikey_id, agent_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_apikey_agents_agent ON apikey_agents (agent_id)`,
		`CREATE TABLE IF NOT EXISTS agents (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			config TEXT NOT NULL DEFAULT '{}',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_agents_user ON agents (user_id)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			user_id TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			session_key TEXT NOT NULL,
			title TEXT NOT NULL DEFAULT '',
			messages TEXT NOT NULL DEFAULT '[]',
			message_count INTEGER NOT NULL DEFAULT 0,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (user_id, agent_id, session_key)
		)`,
		// session_messages is the append-only archive of every turn ever
		// written to a session. The sessions row above stores the
		// LLM-facing working set (post-compaction); session_messages
		// stores the original full history so UI / audit / multi-tenant
		// recovery has a source of truth that compaction never touches.
		// seq is a per-session monotonic counter assigned at INSERT time
		// via COALESCE(MAX(seq), -1)+1 so callers don't need a separate
		// SELECT round-trip. Composite PK doubles as the natural order.
		`CREATE TABLE IF NOT EXISTS session_messages (
			user_id TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			session_key TEXT NOT NULL,
			seq INTEGER NOT NULL,
			role TEXT NOT NULL,
			content TEXT NOT NULL DEFAULT '',
			content_parts TEXT NOT NULL DEFAULT '',
			tool_calls TEXT NOT NULL DEFAULT '',
			tool_call_id TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL DEFAULT '',
			metadata TEXT NOT NULL DEFAULT '',
			thinking TEXT NOT NULL DEFAULT '',
			raw_assistant TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (user_id, agent_id, session_key, seq)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_session_messages_lookup ON session_messages (user_id, agent_id, session_key, seq)`,
		// agent_files holds the agent's own files: SOUL.md, IDENTITY.md,
		// MEMORY.md, AGENTS.md, BOOTSTRAP.md, etc.
		//
		// user_id splits "agent template" from "per-user override":
		//   user_id='' — shared template, edited by the agent owner via
		//                the Customize page, visible to every chatter
		//                that didn't author their own override
		//   user_id=u_xxx — that user's personal copy (USER.md / MEMORY.md
		//                during chat, or a Personalize-for-me override)
		// Read path picks `user_id IN (chatter, '') ORDER BY user_id DESC
		// LIMIT 1`, so a user-specific row wins and missing rows fall
		// back to the template.
		`CREATE TABLE IF NOT EXISTS agent_files (
			agent_id TEXT NOT NULL,
			user_id TEXT NOT NULL DEFAULT '',
			filename TEXT NOT NULL,
			content TEXT NOT NULL DEFAULT '',
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (agent_id, user_id, filename)
		)`,
		`CREATE TABLE IF NOT EXISTS configs (
			id TEXT PRIMARY KEY,
			kind TEXT NOT NULL,
			scope TEXT NOT NULL,
			scope_id TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL,
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			credential_key TEXT NOT NULL DEFAULT '',
			data TEXT NOT NULL DEFAULT '{}',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE (kind, scope, scope_id, name)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_configs_lookup ON configs (kind, scope, scope_id)`,
		`CREATE INDEX IF NOT EXISTS idx_configs_credential ON configs (kind, credential_key)`,
		`CREATE TABLE IF NOT EXISTS cron_jobs (
			id TEXT PRIMARY KEY,
			agent_id TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			type TEXT NOT NULL DEFAULT 'cron',
			schedule TEXT NOT NULL,
			message TEXT NOT NULL,
			channel TEXT NOT NULL,
			chat_id TEXT NOT NULL,
			account_id TEXT NOT NULL DEFAULT '',
			timezone TEXT NOT NULL DEFAULT 'UTC',
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			last_run TIMESTAMP,
			next_run TIMESTAMP,
			locked_by TEXT,
			locked_at TIMESTAMP,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_cron_jobs_schedule ON cron_jobs (enabled, next_run)`,
		`CREATE INDEX IF NOT EXISTS idx_cron_jobs_agent ON cron_jobs (agent_id)`,
	}
}

func (d *DBStore) Close() error {
	return d.db.Close()
}

// ph returns the correct placeholder for the dialect.
func (d *DBStore) ph(n int) string {
	if d.dialect == "postgres" {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}

// scanErr wraps sql.ErrNoRows in our public ErrNotFound.
func scanErr(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// --- Users ---

// userColumns is the canonical select list — keep ordering aligned with
// the Scan calls below so adding a column means editing both lines.
const userColumns = `id, username, email, password_hash, display_name, role, status, apikey_id, external_id, avatar_url, created_at, updated_at`

func scanUser(scanner interface{ Scan(dest ...any) error }) (*UserRecord, error) {
	var u UserRecord
	if err := scanner.Scan(&u.ID, &u.Username, &u.Email, &u.PasswordHash, &u.DisplayName, &u.Role, &u.Status, &u.APIKeyID, &u.ExternalID, &u.AvatarURL, &u.CreatedAt, &u.UpdatedAt); err != nil {
		return nil, err
	}
	return &u, nil
}

func (d *DBStore) CreateUser(ctx context.Context, u *UserRecord) error {
	now := time.Now().UTC()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	u.UpdatedAt = now
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO users (id, username, email, password_hash, display_name, role, status, apikey_id, external_id, avatar_url, created_at, updated_at)
			VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s)`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12)),
		u.ID, u.Username, u.Email, u.PasswordHash, u.DisplayName, u.Role, u.Status, u.APIKeyID, u.ExternalID, u.AvatarURL, u.CreatedAt, u.UpdatedAt)
	return err
}

func (d *DBStore) GetUser(ctx context.Context, id string) (*UserRecord, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT `+userColumns+` FROM users WHERE id = %s`, d.ph(1)), id)
	u, err := scanUser(row)
	if err != nil {
		return nil, scanErr(err)
	}
	return u, nil
}

func (d *DBStore) GetUserByLogin(ctx context.Context, usernameOrEmail string) (*UserRecord, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT `+userColumns+` FROM users WHERE username = %s OR email = %s LIMIT 1`, d.ph(1), d.ph(2)),
		usernameOrEmail, usernameOrEmail)
	u, err := scanUser(row)
	if err != nil {
		return nil, scanErr(err)
	}
	return u, nil
}

// GetUserByExternal looks up an app_user by (apikey_id, external_id).
// Returns ErrNotFound when nothing matches — used by the lazy-mint
// flow on api_key chat calls and by the explicit provisioning endpoint
// to make creation idempotent on re-entry.
func (d *DBStore) GetUserByExternal(ctx context.Context, apikeyID, externalID string) (*UserRecord, error) {
	if apikeyID == "" || externalID == "" {
		return nil, ErrNotFound
	}
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT `+userColumns+` FROM users WHERE apikey_id = %s AND external_id = %s LIMIT 1`,
			d.ph(1), d.ph(2)),
		apikeyID, externalID)
	u, err := scanUser(row)
	if err != nil {
		return nil, scanErr(err)
	}
	return u, nil
}

func (d *DBStore) ListUsers(ctx context.Context) ([]UserRecord, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+userColumns+` FROM users ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserRecord
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

func (d *DBStore) UpdateUser(ctx context.Context, u *UserRecord) error {
	u.UpdatedAt = time.Now().UTC()
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE users SET username = %s, email = %s, password_hash = %s, display_name = %s,
			role = %s, status = %s, avatar_url = %s, updated_at = %s WHERE id = %s`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9)),
		u.Username, u.Email, u.PasswordHash, u.DisplayName, u.Role, u.Status, u.AvatarURL, u.UpdatedAt, u.ID)
	return err
}

func (d *DBStore) DeleteUser(ctx context.Context, id string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// First, find every agent owned by this user — we'll cascade through
	// per-agent state (cron, agent_files, sessions, configs) before
	// dropping the agents themselves.
	rows, err := tx.QueryContext(ctx,
		fmt.Sprintf("SELECT id FROM agents WHERE user_id = %s", d.ph(1)), id)
	if err != nil {
		return err
	}
	var ownedAgents []string
	for rows.Next() {
		var aid string
		if err := rows.Scan(&aid); err != nil {
			rows.Close()
			return err
		}
		ownedAgents = append(ownedAgents, aid)
	}
	rows.Close()
	for _, aid := range ownedAgents {
		for _, t := range []string{"agent_files", "sessions", "cron_jobs"} {
			if _, err := tx.ExecContext(ctx,
				fmt.Sprintf("DELETE FROM %s WHERE agent_id = %s", t, d.ph(1)), aid); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf("DELETE FROM apikey_agents WHERE agent_id = %s", d.ph(1)), aid); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf("DELETE FROM configs WHERE scope = 'agent' AND scope_id = %s", d.ph(1)), aid); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf("DELETE FROM agents WHERE user_id = %s", d.ph(1)), id); err != nil {
		return err
	}
	// Per-user state that's not agent-scoped (agent_files is now agent-only).
	for _, t := range []string{"web_sessions", "apikeys", "sessions"} {
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf("DELETE FROM %s WHERE user_id = %s", t, d.ph(1)), id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf("DELETE FROM configs WHERE scope = 'user' AND scope_id = %s", d.ph(1)), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM apikey_agents WHERE apikey_id NOT IN (SELECT id FROM apikeys)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf("DELETE FROM users WHERE id = %s", d.ph(1)), id); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DBStore) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// --- Web sessions ---

func (d *DBStore) CreateWebSession(ctx context.Context, s *WebSessionRecord) error {
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	}
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO web_sessions (sid, user_id, created_at, expires_at) VALUES (%s, %s, %s, %s)`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4)),
		s.SID, s.UserID, s.CreatedAt, s.ExpiresAt)
	return err
}

func (d *DBStore) GetWebSession(ctx context.Context, sid string) (*WebSessionRecord, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT sid, user_id, created_at, expires_at FROM web_sessions WHERE sid = %s`, d.ph(1)), sid)
	var s WebSessionRecord
	if err := row.Scan(&s.SID, &s.UserID, &s.CreatedAt, &s.ExpiresAt); err != nil {
		return nil, scanErr(err)
	}
	return &s, nil
}

func (d *DBStore) DeleteWebSession(ctx context.Context, sid string) error {
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM web_sessions WHERE sid = %s`, d.ph(1)), sid)
	return err
}

func (d *DBStore) DeleteExpiredWebSessions(ctx context.Context, before time.Time) error {
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM web_sessions WHERE expires_at < %s`, d.ph(1)), before)
	return err
}

// --- API keys ---

func (d *DBStore) ListAPIKeys(ctx context.Context, userID string) ([]APIKeyRecord, error) {
	rows, err := d.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT id, user_id, name, key_hash, key_prefix, type, created_at FROM apikeys WHERE user_id = %s ORDER BY created_at`, d.ph(1)),
		userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKeyRecord
	for rows.Next() {
		var ak APIKeyRecord
		if err := rows.Scan(&ak.ID, &ak.UserID, &ak.Name, &ak.KeyHash, &ak.KeyPrefix, &ak.Type, &ak.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, ak)
	}
	return out, rows.Err()
}

func (d *DBStore) GetAPIKey(ctx context.Context, id string) (*APIKeyRecord, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT id, user_id, name, key_hash, key_prefix, type, created_at FROM apikeys WHERE id = %s`, d.ph(1)), id)
	var ak APIKeyRecord
	if err := row.Scan(&ak.ID, &ak.UserID, &ak.Name, &ak.KeyHash, &ak.KeyPrefix, &ak.Type, &ak.CreatedAt); err != nil {
		return nil, scanErr(err)
	}
	return &ak, nil
}

func (d *DBStore) CreateAPIKey(ctx context.Context, ak *APIKeyRecord) error {
	if ak.CreatedAt.IsZero() {
		ak.CreatedAt = time.Now().UTC()
	}
	if ak.Type == "" {
		ak.Type = "agent"
	}
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO apikeys (id, user_id, name, key_hash, key_prefix, type, created_at) VALUES (%s, %s, %s, %s, %s, %s, %s)`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7)),
		ak.ID, ak.UserID, ak.Name, ak.KeyHash, ak.KeyPrefix, ak.Type, ak.CreatedAt)
	return err
}

func (d *DBStore) DeleteAPIKey(ctx context.Context, id string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM apikey_agents WHERE apikey_id = %s`, d.ph(1)), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM apikeys WHERE id = %s`, d.ph(1)), id); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DBStore) RotateAPIKey(ctx context.Context, id, keyHash, keyPrefix string) error {
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE apikeys SET key_hash = %s, key_prefix = %s WHERE id = %s`,
			d.ph(1), d.ph(2), d.ph(3)),
		keyHash, keyPrefix, id)
	return err
}

func (d *DBStore) LookupAPIKeyByHash(ctx context.Context, keyHash string) (*APIKeyRecord, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT id, user_id, name, key_hash, key_prefix, type, created_at FROM apikeys WHERE key_hash = %s`, d.ph(1)),
		keyHash)
	var ak APIKeyRecord
	if err := row.Scan(&ak.ID, &ak.UserID, &ak.Name, &ak.KeyHash, &ak.KeyPrefix, &ak.Type, &ak.CreatedAt); err != nil {
		return nil, scanErr(err)
	}
	return &ak, nil
}

// --- API key ↔ agent permissions ---

func (d *DBStore) SetAPIKeyAgents(ctx context.Context, apikeyID string, agentIDs []string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM apikey_agents WHERE apikey_id = %s`, d.ph(1)), apikeyID); err != nil {
		return err
	}
	for _, aid := range agentIDs {
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`INSERT INTO apikey_agents (apikey_id, agent_id) VALUES (%s, %s)`, d.ph(1), d.ph(2)),
			apikeyID, aid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DBStore) ListAPIKeyAgents(ctx context.Context, apikeyID string) ([]string, error) {
	rows, err := d.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT agent_id FROM apikey_agents WHERE apikey_id = %s ORDER BY agent_id`, d.ph(1)),
		apikeyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var aid string
		if err := rows.Scan(&aid); err != nil {
			return nil, err
		}
		out = append(out, aid)
	}
	return out, rows.Err()
}

func (d *DBStore) APIKeyCanAccessAgent(ctx context.Context, apikeyID, agentID string) (bool, error) {
	var n int
	err := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM apikey_agents WHERE apikey_id = %s AND agent_id = %s`, d.ph(1), d.ph(2)),
		apikeyID, agentID).Scan(&n)
	return n > 0, err
}

// --- Agents ---

const agentSelectCols = `id, user_id, name, config, created_at, updated_at`

func (d *DBStore) ListAgents(ctx context.Context, ownerUserID string) ([]AgentRecord, error) {
	rows, err := d.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT `+agentSelectCols+` FROM agents WHERE user_id = %s ORDER BY created_at`, d.ph(1)),
		ownerUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAgents(rows)
}

func (d *DBStore) GetAgent(ctx context.Context, agentID string) (*AgentRecord, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT `+agentSelectCols+` FROM agents WHERE id = %s`, d.ph(1)), agentID)
	var ag AgentRecord
	var cfgStr string
	if err := row.Scan(&ag.ID, &ag.UserID, &ag.Name, &cfgStr, &ag.CreatedAt, &ag.UpdatedAt); err != nil {
		return nil, scanErr(err)
	}
	json.Unmarshal([]byte(cfgStr), &ag.Config)
	return &ag, nil
}

func (d *DBStore) SaveAgent(ctx context.Context, agent *AgentRecord) error {
	if agent.ID == "" {
		return errors.New("store: agent.id is required")
	}
	if agent.UserID == "" {
		return errors.New("store: agent.user_id is required")
	}
	cfgData, _ := json.Marshal(agent.Config)
	now := time.Now().UTC()
	if agent.CreatedAt.IsZero() {
		agent.CreatedAt = now
	}
	agent.UpdatedAt = now
	if d.dialect == "postgres" {
		_, err := d.db.ExecContext(ctx,
			`INSERT INTO agents (id, user_id, name, config, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (id) DO UPDATE
				SET user_id=$2, name=$3, config=$4, updated_at=$6`,
			agent.ID, agent.UserID, agent.Name, string(cfgData), agent.CreatedAt, agent.UpdatedAt)
		return err
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO agents (id, user_id, name, config, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET
			  user_id=excluded.user_id, name=excluded.name,
			  config=excluded.config, updated_at=excluded.updated_at`,
		agent.ID, agent.UserID, agent.Name, string(cfgData), agent.CreatedAt, agent.UpdatedAt)
	return err
}

func (d *DBStore) DeleteAgent(ctx context.Context, agentID string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range []string{"agent_files", "sessions", "cron_jobs"} {
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`DELETE FROM %s WHERE agent_id = %s`, t, d.ph(1)), agentID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM apikey_agents WHERE agent_id = %s`, d.ph(1)), agentID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM configs WHERE scope = 'agent' AND scope_id = %s`, d.ph(1)), agentID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM agents WHERE id = %s`, d.ph(1)), agentID); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DBStore) ListAllAgents(ctx context.Context) ([]AgentRecord, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+agentSelectCols+` FROM agents ORDER BY user_id, created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAgents(rows)
}

func scanAgents(rows *sql.Rows) ([]AgentRecord, error) {
	var out []AgentRecord
	for rows.Next() {
		var ag AgentRecord
		var cfgStr string
		if err := rows.Scan(&ag.ID, &ag.UserID, &ag.Name, &cfgStr, &ag.CreatedAt, &ag.UpdatedAt); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(cfgStr), &ag.Config)
		out = append(out, ag)
	}
	return out, rows.Err()
}

// --- Sessions ---

func (d *DBStore) GetSession(ctx context.Context, userID, agentID, sessionKey string) (*SessionRecord, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT messages, updated_at FROM sessions WHERE user_id = %s AND agent_id = %s AND session_key = %s`,
			d.ph(1), d.ph(2), d.ph(3)),
		userID, agentID, sessionKey)
	var msgsStr string
	var rec SessionRecord
	if err := row.Scan(&msgsStr, &rec.UpdatedAt); err != nil {
		return nil, scanErr(err)
	}
	json.Unmarshal([]byte(msgsStr), &rec.Messages)
	return &rec, nil
}

func (d *DBStore) SaveSession(ctx context.Context, userID, agentID, sessionKey string, session *SessionRecord) error {
	if userID == "" {
		return errors.New("store: SaveSession requires user_id")
	}
	msgsData, _ := json.Marshal(session.Messages)
	now := time.Now().UTC()
	count := len(session.Messages)
	if d.dialect == "postgres" {
		_, err := d.db.ExecContext(ctx,
			`INSERT INTO sessions (user_id, agent_id, session_key, messages, message_count, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (user_id, agent_id, session_key) DO UPDATE
				SET messages=$4, message_count=$5, updated_at=$6`,
			userID, agentID, sessionKey, string(msgsData), count, now)
		return err
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO sessions (user_id, agent_id, session_key, messages, message_count, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (user_id, agent_id, session_key) DO UPDATE SET
			  messages=excluded.messages, message_count=excluded.message_count, updated_at=excluded.updated_at`,
		userID, agentID, sessionKey, string(msgsData), count, now)
	return err
}

func (d *DBStore) ListSessions(ctx context.Context, userID, agentID string) ([]SessionMeta, error) {
	rows, err := d.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT session_key, title, message_count, updated_at FROM sessions
			WHERE user_id = %s AND agent_id = %s ORDER BY updated_at DESC`, d.ph(1), d.ph(2)),
		userID, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var metas []SessionMeta
	for rows.Next() {
		var m SessionMeta
		if err := rows.Scan(&m.Key, &m.Title, &m.MessageCount, &m.UpdatedAt); err != nil {
			return nil, err
		}
		metas = append(metas, m)
	}
	return metas, rows.Err()
}

func (d *DBStore) DeleteSession(ctx context.Context, userID, agentID, sessionKey string) error {
	if _, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM session_messages WHERE user_id = %s AND agent_id = %s AND session_key = %s`,
			d.ph(1), d.ph(2), d.ph(3)),
		userID, agentID, sessionKey); err != nil {
		return err
	}
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM sessions WHERE user_id = %s AND agent_id = %s AND session_key = %s`,
			d.ph(1), d.ph(2), d.ph(3)),
		userID, agentID, sessionKey)
	return err
}

// AppendSessionMessage writes one message to the per-session archive.
// seq is computed atomically inside the INSERT via
// `COALESCE(MAX(seq), -1) + 1`, so two concurrent appenders racing on
// the same session can't collide on the unique key — the second insert
// reads MAX after the first commits. Multi-pod safety relies on the
// engine's write serialization (sqlite global, postgres MVCC + the
// composite PK uniqueness check on commit).
func (d *DBStore) AppendSessionMessage(ctx context.Context, userID, agentID, sessionKey string, msg SessionMessage) error {
	if userID == "" {
		return errors.New("store: AppendSessionMessage requires user_id")
	}
	contentParts, _ := json.Marshal(msg.ContentParts)
	toolCalls, _ := json.Marshal(msg.ToolCalls)
	metadata, _ := json.Marshal(msg.Metadata)
	rawAssistant := string(msg.RawAssistant)
	ts := msg.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	if d.dialect == "postgres" {
		_, err := d.db.ExecContext(ctx,
			`INSERT INTO session_messages
				(user_id, agent_id, session_key, seq, role, content, content_parts, tool_calls, tool_call_id, name, metadata, thinking, raw_assistant, created_at)
			SELECT $1, $2, $3, COALESCE(MAX(seq), -1) + 1, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13
				FROM session_messages
				WHERE user_id = $1 AND agent_id = $2 AND session_key = $3`,
			userID, agentID, sessionKey,
			msg.Role, msg.Content, string(contentParts), string(toolCalls),
			msg.ToolCallID, msg.Name, string(metadata), msg.Thinking, rawAssistant, ts)
		return err
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO session_messages
			(user_id, agent_id, session_key, seq, role, content, content_parts, tool_calls, tool_call_id, name, metadata, thinking, raw_assistant, created_at)
		SELECT ?, ?, ?, COALESCE(MAX(seq), -1) + 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
			FROM session_messages
			WHERE user_id = ? AND agent_id = ? AND session_key = ?`,
		userID, agentID, sessionKey,
		msg.Role, msg.Content, string(contentParts), string(toolCalls),
		msg.ToolCallID, msg.Name, string(metadata), msg.Thinking, rawAssistant, ts,
		userID, agentID, sessionKey)
	return err
}

// ListSessionMessages returns every archived turn for one session in
// ascending seq order. Empty slice on a session that has no archive
// yet (e.g. rows pre-dating the table). Callers that want a fallback
// to sessions.messages should check len() and decide.
func (d *DBStore) ListSessionMessages(ctx context.Context, userID, agentID, sessionKey string) ([]SessionMessage, error) {
	rows, err := d.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT role, content, content_parts, tool_calls, tool_call_id, name, metadata, thinking, raw_assistant, created_at
			FROM session_messages
			WHERE user_id = %s AND agent_id = %s AND session_key = %s
			ORDER BY seq ASC`, d.ph(1), d.ph(2), d.ph(3)),
		userID, agentID, sessionKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionMessage
	for rows.Next() {
		var m SessionMessage
		var contentParts, toolCalls, metadata, rawAssistant string
		if err := rows.Scan(&m.Role, &m.Content, &contentParts, &toolCalls, &m.ToolCallID, &m.Name, &metadata, &m.Thinking, &rawAssistant, &m.Timestamp); err != nil {
			return nil, err
		}
		if contentParts != "" && contentParts != "null" {
			var v interface{}
			if json.Unmarshal([]byte(contentParts), &v) == nil {
				m.ContentParts = v
			}
		}
		if toolCalls != "" && toolCalls != "null" {
			var v interface{}
			if json.Unmarshal([]byte(toolCalls), &v) == nil {
				m.ToolCalls = v
			}
		}
		if metadata != "" && metadata != "null" {
			_ = json.Unmarshal([]byte(metadata), &m.Metadata)
		}
		if rawAssistant != "" && rawAssistant != "null" {
			m.RawAssistant = json.RawMessage(rawAssistant)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (d *DBStore) RenameSession(ctx context.Context, userID, agentID, sessionKey, title string) error {
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE sessions SET title = %s WHERE user_id = %s AND agent_id = %s AND session_key = %s`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4)),
		title, userID, agentID, sessionKey)
	return err
}

// --- Agent files ---
//
// SOUL.md / IDENTITY.md / MEMORY.md / AGENTS.md / BOOTSTRAP.md / etc.
// Keyed on (agent_id, user_id, filename). Every row carries a real
// user_id — there is no shared template row.
//
// Read path: prefer the caller's own row; fall back to the agent
// owner's row when the caller has no override. This lets non-owner
// callers (other humans the agent is shared with, or app_user accounts
// minted on behalf of a downstream app's end-users) inherit the
// owner's customized SOUL.md / IDENTITY.md while still being able to
// fork their own MEMORY.md / USER.md by saving — saves always go to
// the caller's exact row, never the owner's. The runtime additionally
// falls through to a local FS file at <agent_home>/<name> for installs
// that want a global default for an agent.

// GetAgentFile returns the file for (agent_id, filename), preferring
// the caller's own row and falling back to the agent owner's row.
// userID is required.
func (d *DBStore) GetAgentFile(ctx context.Context, agentID, userID, filename string) ([]byte, error) {
	if agentID == "" {
		return nil, errors.New("store: GetAgentFile requires agent_id")
	}
	if userID == "" {
		return nil, errors.New("store: GetAgentFile requires user_id")
	}
	// Single round-trip: pick caller's row if present (sort key 0),
	// else owner's (sort key 1). LIMIT 1 returns the winning row.
	// The subselect resolves the agent's owner; if the agent is gone
	// it just produces NULL and the IN ignores it — caller's row is
	// still returned when present, otherwise NoRows.
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT content FROM agent_files
			WHERE agent_id = %s AND filename = %s
			  AND user_id IN (%s, COALESCE((SELECT user_id FROM agents WHERE id = %s), ''))
			ORDER BY CASE WHEN user_id = %s THEN 0 ELSE 1 END
			LIMIT 1`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)),
		agentID, filename, userID, agentID, userID)
	var content string
	if err := row.Scan(&content); err != nil {
		return nil, scanErr(err)
	}
	return []byte(content), nil
}

// GetAgentFileExact bypasses the owner-fallback overlay and returns
// only the (agent_id, user_id, filename) row, or ErrNotFound. Used
// when a caller explicitly needs to know whether *their own* override
// row exists (e.g. a Customize page that distinguishes "you've
// authored an override" from "you're seeing the owner's content").
func (d *DBStore) GetAgentFileExact(ctx context.Context, agentID, userID, filename string) ([]byte, error) {
	if agentID == "" {
		return nil, errors.New("store: GetAgentFileExact requires agent_id")
	}
	if userID == "" {
		return nil, errors.New("store: GetAgentFileExact requires user_id")
	}
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT content FROM agent_files
			WHERE agent_id = %s AND user_id = %s AND filename = %s`,
			d.ph(1), d.ph(2), d.ph(3)),
		agentID, userID, filename)
	var content string
	if err := row.Scan(&content); err != nil {
		return nil, scanErr(err)
	}
	return []byte(content), nil
}

// SaveAgentFile writes to the (agent_id, user_id, filename) row exactly.
// userID is required — every write is per-user. Use a local FS file
// at <agent_home>/<name> if you want one shared default for the agent.
func (d *DBStore) SaveAgentFile(ctx context.Context, agentID, userID, filename string, data []byte) error {
	if agentID == "" {
		return errors.New("store: SaveAgentFile requires agent_id")
	}
	if userID == "" {
		return errors.New("store: SaveAgentFile requires user_id")
	}
	now := time.Now().UTC()
	if d.dialect == "postgres" {
		_, err := d.db.ExecContext(ctx,
			`INSERT INTO agent_files (agent_id, user_id, filename, content, updated_at)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (agent_id, user_id, filename) DO UPDATE SET content=$4, updated_at=$5`,
			agentID, userID, filename, string(data), now)
		return err
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO agent_files (agent_id, user_id, filename, content, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (agent_id, user_id, filename) DO UPDATE SET
			  content=excluded.content, updated_at=excluded.updated_at`,
		agentID, userID, filename, string(data), now)
	return err
}

func (d *DBStore) DeleteAgentFile(ctx context.Context, agentID, userID, filename string) error {
	if agentID == "" {
		return errors.New("store: DeleteAgentFile requires agent_id")
	}
	if userID == "" {
		return errors.New("store: DeleteAgentFile requires user_id")
	}
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM agent_files WHERE agent_id = %s AND user_id = %s AND filename = %s`,
			d.ph(1), d.ph(2), d.ph(3)),
		agentID, userID, filename)
	return err
}

// ListAgentFiles returns the filenames stored for (agent_id, user_id).
// userID is required — there is no shared template fallback.
func (d *DBStore) ListAgentFiles(ctx context.Context, agentID, userID string) ([]string, error) {
	if agentID == "" {
		return nil, errors.New("store: ListAgentFiles requires agent_id")
	}
	if userID == "" {
		return nil, errors.New("store: ListAgentFiles requires user_id")
	}
	rows, err := d.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT filename FROM agent_files
			WHERE agent_id = %s AND user_id = %s ORDER BY filename`,
			d.ph(1), d.ph(2)),
		agentID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var files []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, rows.Err()
}

// --- Scoped configs (providers + channels + settings) ---

// ListConfigs returns all rows of the given (kind, scope) tuple. When
// scopeID is empty, it matches any scope_id within the scope — used by
// boot-time enumeration paths (registerChannelsFromStore) that want
// "every agent's channels" across all users without enumerating users
// first. Existing callers that pass a real scopeID continue to get
// exact-match semantics. System rows have scope_id="" anyway so
// system-scope queries are unaffected by this widening.
func (d *DBStore) ListConfigs(ctx context.Context, kind, scope, scopeID string) ([]ConfigRecord, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if scopeID == "" {
		rows, err = d.db.QueryContext(ctx,
			fmt.Sprintf(`SELECT id, kind, scope, scope_id, name, enabled, credential_key, data, created_at, updated_at
				FROM configs WHERE kind = %s AND scope = %s ORDER BY name`,
				d.ph(1), d.ph(2)),
			kind, scope)
	} else {
		rows, err = d.db.QueryContext(ctx,
			fmt.Sprintf(`SELECT id, kind, scope, scope_id, name, enabled, credential_key, data, created_at, updated_at
				FROM configs WHERE kind = %s AND scope = %s AND scope_id = %s ORDER BY name`,
				d.ph(1), d.ph(2), d.ph(3)),
			kind, scope, scopeID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConfigs(rows)
}

func (d *DBStore) GetConfig(ctx context.Context, id string) (*ConfigRecord, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT id, kind, scope, scope_id, name, enabled, credential_key, data, created_at, updated_at
			FROM configs WHERE id = %s`, d.ph(1)), id)
	return scanConfigRow(row)
}

func (d *DBStore) GetConfigByName(ctx context.Context, kind, scope, scopeID, name string) (*ConfigRecord, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT id, kind, scope, scope_id, name, enabled, credential_key, data, created_at, updated_at
			FROM configs WHERE kind = %s AND scope = %s AND scope_id = %s AND name = %s`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4)),
		kind, scope, scopeID, name)
	return scanConfigRow(row)
}

func (d *DBStore) SaveConfig(ctx context.Context, c *ConfigRecord) error {
	if c.Kind == "" || c.Scope == "" || c.Name == "" {
		return errors.New("store: scoped_config requires kind/scope/name")
	}
	now := time.Now().UTC()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	if c.ID == "" {
		// Deterministic id from (kind, scope, scope_id, name) so callers
		// that create-or-update without tracking ids land on the same row.
		c.ID = configRowID(c.Kind, c.Scope, c.ScopeID, c.Name)
	}
	dataBytes, _ := json.Marshal(c.Data)
	if d.dialect == "postgres" {
		_, err := d.db.ExecContext(ctx,
			`INSERT INTO configs (id, kind, scope, scope_id, name, enabled, credential_key, data, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
				ON CONFLICT (id) DO UPDATE SET
				  enabled=$6, credential_key=$7, data=$8, updated_at=$10`,
			c.ID, c.Kind, c.Scope, c.ScopeID, c.Name, c.Enabled, c.CredentialKey, string(dataBytes), c.CreatedAt, c.UpdatedAt)
		return err
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO configs (id, kind, scope, scope_id, name, enabled, credential_key, data, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET
			  enabled=excluded.enabled, credential_key=excluded.credential_key,
			  data=excluded.data, updated_at=excluded.updated_at`,
		c.ID, c.Kind, c.Scope, c.ScopeID, c.Name, c.Enabled, c.CredentialKey, string(dataBytes), c.CreatedAt, c.UpdatedAt)
	return err
}

func (d *DBStore) DeleteConfig(ctx context.Context, id string) error {
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM configs WHERE id = %s`, d.ph(1)), id)
	return err
}

func (d *DBStore) LookupChannelByCredential(ctx context.Context, channelType, credKey string) (*ConfigRecord, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT id, kind, scope, scope_id, name, enabled, credential_key, data, created_at, updated_at
			FROM configs WHERE kind = 'channel' AND name = %s AND credential_key = %s LIMIT 1`,
			d.ph(1), d.ph(2)),
		channelType, credKey)
	return scanConfigRow(row)
}

// configRowID produces a stable id for a (kind, scope, scope_id, name)
// tuple. Hex-encoded SHA-256 keeps it URL-safe and DB-friendly.
func configRowID(kind, scope, scopeID, name string) string {
	h := sha256.New()
	h.Write([]byte(kind))
	h.Write([]byte{0})
	h.Write([]byte(scope))
	h.Write([]byte{0})
	h.Write([]byte(scopeID))
	h.Write([]byte{0})
	h.Write([]byte(name))
	return "sc_" + hex.EncodeToString(h.Sum(nil)[:10])
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanConfigRow(row rowScanner) (*ConfigRecord, error) {
	var c ConfigRecord
	var dataStr string
	if err := row.Scan(&c.ID, &c.Kind, &c.Scope, &c.ScopeID, &c.Name, &c.Enabled, &c.CredentialKey, &dataStr, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, scanErr(err)
	}
	json.Unmarshal([]byte(dataStr), &c.Data)
	return &c, nil
}

func scanConfigs(rows *sql.Rows) ([]ConfigRecord, error) {
	var out []ConfigRecord
	for rows.Next() {
		var c ConfigRecord
		var dataStr string
		if err := rows.Scan(&c.ID, &c.Kind, &c.Scope, &c.ScopeID, &c.Name, &c.Enabled, &c.CredentialKey, &dataStr, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(dataStr), &c.Data)
		out = append(out, c)
	}
	return out, rows.Err()
}

// --- Cron jobs ---

const cronSelectCols = `id, agent_id, name, type, schedule, message, channel, chat_id, account_id, timezone, enabled, last_run, next_run, created_at`

func (d *DBStore) ListCronJobsByOwner(ctx context.Context, ownerUserID string) ([]CronJobRecord, error) {
	rows, err := d.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT cj.id, cj.agent_id, cj.name, cj.type, cj.schedule, cj.message, cj.channel, cj.chat_id, cj.account_id, cj.timezone, cj.enabled, cj.last_run, cj.next_run, cj.created_at
			FROM cron_jobs cj JOIN agents a ON cj.agent_id = a.id
			WHERE a.user_id = %s ORDER BY cj.created_at`, d.ph(1)),
		ownerUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCronJobs(rows)
}

func (d *DBStore) ListCronJobsByAgent(ctx context.Context, agentID string) ([]CronJobRecord, error) {
	rows, err := d.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT `+cronSelectCols+` FROM cron_jobs WHERE agent_id = %s ORDER BY created_at`, d.ph(1)),
		agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCronJobs(rows)
}

func (d *DBStore) GetCronJob(ctx context.Context, jobID string) (*CronJobRecord, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT `+cronSelectCols+` FROM cron_jobs WHERE id = %s`, d.ph(1)), jobID)
	var j CronJobRecord
	var lastRun, nextRun sql.NullTime
	if err := row.Scan(&j.ID, &j.AgentID, &j.Name, &j.Type, &j.Schedule, &j.Message, &j.Channel, &j.ChatID, &j.AccountID, &j.Timezone, &j.Enabled, &lastRun, &nextRun, &j.CreatedAt); err != nil {
		return nil, scanErr(err)
	}
	if lastRun.Valid {
		j.LastRun = &lastRun.Time
	}
	if nextRun.Valid {
		j.NextRun = &nextRun.Time
	}
	return &j, nil
}

func (d *DBStore) SaveCronJob(ctx context.Context, job *CronJobRecord) error {
	if job.AgentID == "" {
		return errors.New("store: cron job.agent_id is required")
	}
	if job.CreatedAt.IsZero() {
		job.CreatedAt = time.Now().UTC()
	}
	if d.dialect == "postgres" {
		_, err := d.db.ExecContext(ctx,
			`INSERT INTO cron_jobs (id, agent_id, name, type, schedule, message, channel, chat_id, account_id, timezone, enabled, last_run, next_run, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
				ON CONFLICT (id) DO UPDATE SET
				  agent_id=$2, name=$3, type=$4, schedule=$5, message=$6, channel=$7,
				  chat_id=$8, account_id=$9, timezone=$10, enabled=$11, last_run=$12, next_run=$13`,
			job.ID, job.AgentID, job.Name, job.Type, job.Schedule, job.Message, job.Channel, job.ChatID, job.AccountID, job.Timezone, job.Enabled, job.LastRun, job.NextRun, job.CreatedAt)
		return err
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO cron_jobs (id, agent_id, name, type, schedule, message, channel, chat_id, account_id, timezone, enabled, last_run, next_run, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET
			  agent_id=excluded.agent_id, name=excluded.name, type=excluded.type,
			  schedule=excluded.schedule, message=excluded.message, channel=excluded.channel,
			  chat_id=excluded.chat_id, account_id=excluded.account_id, timezone=excluded.timezone,
			  enabled=excluded.enabled, last_run=excluded.last_run, next_run=excluded.next_run`,
		job.ID, job.AgentID, job.Name, job.Type, job.Schedule, job.Message, job.Channel, job.ChatID, job.AccountID, job.Timezone, job.Enabled, job.LastRun, job.NextRun, job.CreatedAt)
	return err
}

func (d *DBStore) DeleteCronJob(ctx context.Context, jobID string) error {
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM cron_jobs WHERE id = %s`, d.ph(1)), jobID)
	return err
}

func (d *DBStore) GetDueCronJobs(ctx context.Context, now time.Time) ([]CronJobRecord, error) {
	var rows *sql.Rows
	var err error
	if d.dialect == "postgres" {
		rows, err = d.db.QueryContext(ctx,
			`SELECT `+cronSelectCols+` FROM cron_jobs WHERE enabled = true AND next_run <= $1 ORDER BY next_run`, now)
	} else {
		rows, err = d.db.QueryContext(ctx,
			`SELECT `+cronSelectCols+` FROM cron_jobs WHERE enabled = 1 AND next_run <= ? ORDER BY next_run`, now)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCronJobs(rows)
}

func (d *DBStore) LockCronJob(ctx context.Context, jobID, instanceID string) (bool, error) {
	now := time.Now()
	fiveMinAgo := now.Add(-5 * time.Minute)
	var res sql.Result
	var err error
	if d.dialect == "postgres" {
		res, err = d.db.ExecContext(ctx,
			`UPDATE cron_jobs SET locked_by=$1, locked_at=$2 WHERE id=$3 AND (locked_by IS NULL OR locked_at < $4)`,
			instanceID, now, jobID, fiveMinAgo)
	} else {
		res, err = d.db.ExecContext(ctx,
			`UPDATE cron_jobs SET locked_by=?, locked_at=? WHERE id=? AND (locked_by IS NULL OR locked_at < ?)`,
			instanceID, now, jobID, fiveMinAgo)
	}
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (d *DBStore) UpdateCronJobRun(ctx context.Context, jobID string, lastRun, nextRun time.Time) error {
	if d.dialect == "postgres" {
		_, err := d.db.ExecContext(ctx,
			`UPDATE cron_jobs SET last_run=$1, next_run=$2, locked_by=NULL, locked_at=NULL WHERE id=$3`,
			lastRun, nextRun, jobID)
		return err
	}
	_, err := d.db.ExecContext(ctx,
		`UPDATE cron_jobs SET last_run=?, next_run=?, locked_by=NULL, locked_at=NULL WHERE id=?`,
		lastRun, nextRun, jobID)
	return err
}

func scanCronJobs(rows *sql.Rows) ([]CronJobRecord, error) {
	var jobs []CronJobRecord
	for rows.Next() {
		var j CronJobRecord
		var lastRun, nextRun sql.NullTime
		if err := rows.Scan(&j.ID, &j.AgentID, &j.Name, &j.Type, &j.Schedule, &j.Message, &j.Channel, &j.ChatID, &j.AccountID, &j.Timezone, &j.Enabled, &lastRun, &nextRun, &j.CreatedAt); err != nil {
			return nil, err
		}
		if lastRun.Valid {
			j.LastRun = &lastRun.Time
		}
		if nextRun.Valid {
			j.NextRun = &nextRun.Time
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

var _ Store = (*DBStore)(nil)
