-- One-off backfill: move the legacy `mcpServers` object out of
-- agents.config (TEXT JSON) into the per-key agent_mcp_servers table.
--
-- Why this exists: agent_mcp_servers is the successor to the
-- mcpServers key inside agents.config (workspace code has not shipped,
-- so there is no in-code migration). Dev databases that already hold
-- servers in the JSON blob need this run once BEFORE the first deploy
-- that stops reading the JSON.
--
-- Idempotent: ON CONFLICT ... DO NOTHING makes re-runs safe.
--
-- Run against the dev Postgres (fastagent-dev-pool):
--   psql "$FASTAGENT_E2E_PG_DSN" -f scripts/backfill_agent_mcp_servers.sql
-- or via a k8s pg proxy port-forward, same file.

INSERT INTO agent_mcp_servers (agent_id, server_name, config)
SELECT a.id, s.key, s.value::text
FROM agents a
CROSS JOIN LATERAL jsonb_each(a.config::jsonb -> 'mcpServers') AS s(key, value)
ON CONFLICT (agent_id, server_name) DO NOTHING;

-- Optional, once the rows above are verified: drop the legacy key so the
-- JSON blob stops carrying a stale copy. The table is authoritative and
-- the runtime no longer reads this key; keeping it is harmless but stale.
-- UPDATE agents
-- SET config = (config::jsonb - 'mcpServers')::text
-- WHERE config::jsonb ? 'mcpServers';
