import type { MCPServerConfig } from "./api";

// mergeMCPServersForSave merges a locally-edited server map with the
// latest server-side state so per-entry edits never clobber servers that
// were added concurrently (e.g. by the `mcp add` agent tool or another
// dashboard session) since the page last fetched.
//
// Merge rule (per-entry semantics): the user's edit `next` wins for
// everything it names; servers present in `latest` but absent from BOTH
// the local baseline the user edited from AND the edit result are
// concurrent additions and are preserved. Servers the user deleted from
// the baseline stay deleted (they are in `baseline`, so they are never
// carried back over).
export function mergeMCPServersForSave(
  baseline: Record<string, MCPServerConfig>,
  next: Record<string, MCPServerConfig>,
  latest: Record<string, MCPServerConfig>,
): Record<string, MCPServerConfig> {
  const merged = { ...next };
  for (const [name, cfg] of Object.entries(latest)) {
    if (!(name in baseline) && !(name in next)) {
      merged[name] = cfg;
    }
  }
  return merged;
}
