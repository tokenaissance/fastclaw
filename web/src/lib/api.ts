export interface StatusResponse {
  configured: boolean;
  running: boolean;
  port: number;
  mode?: string;
  uptime: string;
  agents: AgentInfo[];
  channels: ChannelInfo[];
  provider: ProviderInfo;
  cronJobs?: number;
  plugins?: number;
  userId?: string;
  isAdmin?: boolean;
}

export interface AgentInfo {
  id: string;
  name?: string;
  model: string;
  workspace: string;
}

export interface ChannelInfo {
  type: string;
  botUsername: string;
  enabled?: boolean;
  status?: string;
}

export interface ProviderInfo {
  name: string;
  model: string;
  apiBase: string;
  apiKey: string;
}

export interface AgentDetail {
  id: string;
  name?: string;
  description?: string;
  avatarUrl?: string;       // /api/agents/{id}/files/avatar.png — may 404
  userId?: string;
  model: string;
  workspace?: string;
  maxTokens?: number;
  temperature?: number;
  maxToolIterations?: number;
  thinking?: string;
  soul?: string;
  skills?: string[];
  tools?: string[];
}

export interface SkillEnvSpec {
  name: string;
  description?: string;
  required?: boolean;
  secret?: boolean;
}

export interface SkillInfo {
  name: string;
  description: string;
  location: string;
  type: string;
  envSpec?: SkillEnvSpec[];
}

export interface SkillEntryCfg {
  enabled?: boolean;
  apiKey?: string;
  env?: Record<string, string>;
}

// updateSkillEntries persists skill env / apiKey patches. When agentId
// is set the patch lands in cfg.Skills.AgentEntries[agentId] (per-agent
// override), otherwise in cfg.Skills.Entries (global default). The
// runtime resolves agent-scoped first, falling back to global.
export async function updateSkillEntries(
  entries: Record<string, SkillEntryCfg>,
  agentId?: string,
) {
  const body = agentId
    ? { skills: { agentEntries: { [agentId]: entries } } }
    : { skills: { entries } };
  const res = await apiFetch("/api/config", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  return res.json();
}

export interface PluginInfo {
  id: string;
  type: string;
  version: string;
  status: string;
  enabled: boolean;
  config?: Record<string, unknown>;
}

export interface CronJobInfo {
  id: string;
  name: string;
  type: string;
  schedule: string;
  agentId: string;
  channel: string;
  chatId: string;
  message: string;
  enabled: boolean;
  lastRun?: string;
  nextRun?: string;
}

export interface ModelCost {
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
}

export interface ModelEntry {
  id: string;
  name: string;
  reasoning: boolean;
  input: string[];
  cost: ModelCost;
  contextWindow: number;
  maxTokens: number;
}

export interface ProviderData {
  apiKey: string;
  apiBase: string;
  apiType?: string;
  authType?: string;
  models?: ModelEntry[];
}

export interface ConfigResponse {
  providers: Record<string, ProviderData>;
  agents: {
    defaults: {
      model: string;
      maxTokens: number;
      temperature: number;
      maxToolIterations: number;
    };
    list: Array<{ id: string; model?: string }>;
  };
  channels: Record<string, { enabled: boolean; botToken?: string }>;
  storage: { type: string; dsn?: string };
  sandbox?: { enabled: boolean; backend?: string; image?: string; e2bKey?: string };
  hooks: { enabled: boolean; token?: string; path?: string; port?: number };
  cronJobs?: Array<Record<string, unknown>>;
  skills?: {
    entries?: Record<string, SkillEntryCfg>;
    // Per-agent overrides, keyed agentID → skillName → entry. The UI
    // surfaces these only on the agent-scoped /agents/<id>/skills page;
    // SkillsLoader.SkillEnvVars resolves agentEntries[<agent>][<skill>]
    // first, falling back to the global entries map.
    agentEntries?: Record<string, Record<string, SkillEntryCfg>>;
  };
}

// Auth token for cloud mode. Set via setAuthToken() on login; empty in local mode.
let authToken = "";

export function setAuthToken(token: string) {
  authToken = token;
  if (token) {
    localStorage.setItem("fastclaw_token", token);
  } else {
    localStorage.removeItem("fastclaw_token");
  }
}

export function getAuthToken(): string {
  if (!authToken) {
    authToken = localStorage.getItem("fastclaw_token") || "";
  }
  return authToken;
}

// Wrapper around fetch that injects Authorization header when a token is set
// and always includes the cookie session for username/password logins. Cookie
// is the primary credential for the web UI; the bearer is only used by
// programmatic clients that put the token into localStorage manually.
export async function apiFetch(url: string, init?: RequestInit): Promise<Response> {
  const token = getAuthToken();
  const headers: Record<string, string> = {
    ...(init?.headers as Record<string, string> || {}),
  };
  if (token) {
    headers["Authorization"] = `Bearer ${token}`;
  }
  return fetch(url, { credentials: "same-origin", ...init, headers });
}

// Login + logout + me

export interface MeResponse {
  ok: boolean;
  user?: {
    id: string;
    username: string;
    email: string;
    role: string;
    displayName?: string;
    status: string;
  };
  authMethod?: string;
  actAsUserId?: string;
  readOnly?: boolean;
  error?: string;
}

export async function login(loginField: string, password: string): Promise<MeResponse> {
  const res = await fetch("/api/login", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ login: loginField, password }),
  });
  return res.json();
}

export async function logout(): Promise<void> {
  await apiFetch("/api/logout", { method: "POST" });
  setAuthToken("");
}

export async function getMe(): Promise<MeResponse> {
  const res = await apiFetch("/api/me");
  return res.json();
}

// Onboard

export interface OnboardRequest {
  username: string;
  email: string;
  password: string;
  displayName?: string;
  provider?: string;
  apiBase?: string;
  apiKey?: string;
  apiType?: string;
  authType?: string;
  model?: string;
  agentName?: string;
  sandboxEnabled?: boolean;
  sandboxBackend?: string;
  sandboxImage?: string;
  sandboxE2BKey?: string;
}

export async function onboard(req: OnboardRequest): Promise<{ ok: boolean; error?: string }> {
  const res = await fetch("/api/onboard", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

// Admin

export async function adminListUsers() {
  const res = await apiFetch("/api/admin/users");
  return res.json();
}

export async function adminCreateUser(req: {
  username: string;
  email: string;
  password: string;
  displayName?: string;
  role?: string;
}) {
  const res = await apiFetch("/api/admin/users", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

export async function adminUpdateUser(id: string, req: { displayName?: string; role?: string; status?: string }) {
  const res = await apiFetch(`/api/admin/users/${id}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

export async function adminDeleteUser(id: string) {
  const res = await apiFetch(`/api/admin/users/${id}`, { method: "DELETE" });
  return res.json();
}

export async function adminResetPassword(id: string, password: string) {
  const res = await apiFetch(`/api/admin/users/${id}/password`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ password }),
  });
  return res.json();
}

// Apikeys (per-user)

export async function listApikeys() {
  const res = await apiFetch("/api/apikeys");
  return res.json();
}

export async function createApikey(req: { name: string; agentIds?: string[] }) {
  const res = await apiFetch("/api/apikeys", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

export async function deleteApikey(id: string) {
  const res = await apiFetch(`/api/apikeys/${id}`, { method: "DELETE" });
  return res.json();
}

export async function rotateApikey(id: string) {
  const res = await apiFetch(`/api/apikeys/${id}/rotate`, { method: "POST" });
  return res.json();
}

export async function setApikeyAgents(id: string, agentIds: string[]) {
  const res = await apiFetch(`/api/apikeys/${id}/agents`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ agentIds }),
  });
  return res.json();
}

// Scoped providers + channels

export type ScopeName = "system" | "user" | "agent";

export interface ProviderRow {
  id: string;
  scope: ScopeName;
  scopeId: string;
  name: string;
  apiBase?: string;
  apiKey?: string;       // masked on read
  apiType?: string;
  authType?: string;
  models?: ModelEntry[];
  updatedAt?: string;
}

export interface ChannelRow {
  id: string;
  scope: ScopeName;
  scopeId: string;
  type: string;
  enabled: boolean;
  botToken?: string;     // masked on read
  appToken?: string;
  credentialKey?: string;
  updatedAt?: string;
}

export async function listProviders(scope?: ScopeName, scopeId?: string) {
  const params = new URLSearchParams();
  if (scope) params.set("scope", scope);
  if (scopeId) params.set("scopeId", scopeId);
  const qs = params.toString();
  const url = "/api/providers" + (qs ? `?${qs}` : "");
  const res = await apiFetch(url);
  return res.json();
}

export async function createProvider(req: {
  scope: ScopeName;
  scopeId: string;
  name: string;
  apiBase?: string;
  apiKey?: string;
  apiType?: string;
  authType?: string;
  models?: ModelEntry[];
}) {
  const res = await apiFetch("/api/providers", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

export async function updateProvider(id: string, req: Partial<ProviderRow>) {
  const res = await apiFetch(`/api/providers/${id}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

export async function deleteProvider(id: string) {
  const res = await apiFetch(`/api/providers/${id}`, { method: "DELETE" });
  return res.json();
}

// testStoredProvider hits the saved provider row server-side using its
// own apiKey, so the Edit dialog can verify a model id without forcing
// the user to re-paste the secret. The backend never returns unmasked
// keys to the browser, so this is the only way to test from edit mode.
export async function testStoredProvider(
  providerId: string,
  model: string,
): Promise<{ ok: boolean; error?: string }> {
  const res = await apiFetch(`/api/providers/${providerId}/test`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ model }),
  });
  return res.json();
}

export async function listScopedChannels(scope?: ScopeName, scopeId?: string) {
  const params = new URLSearchParams();
  if (scope) params.set("scope", scope);
  if (scopeId) params.set("scopeId", scopeId);
  const qs = params.toString();
  const url = "/api/scoped-channels" + (qs ? `?${qs}` : "");
  const res = await apiFetch(url);
  return res.json();
}

export async function createScopedChannel(req: {
  scope: ScopeName;
  scopeId: string;
  type: string;
  enabled: boolean;
  botToken?: string;
  appToken?: string;
  credentialKey?: string;
}) {
  const res = await apiFetch("/api/scoped-channels", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

export async function updateScopedChannel(id: string, req: Partial<ChannelRow>) {
  const res = await apiFetch(`/api/scoped-channels/${id}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

export async function deleteScopedChannel(id: string) {
  const res = await apiFetch(`/api/scoped-channels/${id}`, { method: "DELETE" });
  return res.json();
}

// Status
export async function getStatus(): Promise<StatusResponse> {
  const res = await apiFetch("/api/status");
  return res.json();
}

// Provider
export async function testProvider(config: { apiBase: string; apiKey: string; model: string; apiType?: string; authType?: string }) {
  const res = await apiFetch("/api/test-provider", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(config),
  });
  return res.json();
}

// Config — persisted system_settings block (super_admin only).
export async function saveConfig(config: Record<string, unknown>) {
  const res = await apiFetch("/api/config", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(config),
  });
  return res.json();
}

export async function getConfig(): Promise<ConfigResponse> {
  const res = await apiFetch("/api/config");
  return res.json();
}

export async function updateConfig(config: Record<string, unknown>) {
  const res = await apiFetch("/api/config", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(config),
  });
  return res.json();
}

// Workspace files listing — used to diff a turn's outputs so the chat
// UI can surface produced files under the final reply.
export interface WorkspaceFile {
  path: string;
  size: number;
  modTime: number;
}

export async function listAgentFiles(agentId: string): Promise<WorkspaceFile[]> {
  const res = await apiFetch(`/api/agents/${encodeURIComponent(agentId)}/files`);
  if (!res.ok) return [];
  const data = await res.json();
  return (data.files || []) as WorkspaceFile[];
}

// Chat
export interface ChatHistoryMessage {
  role: "user" | "assistant" | "tool";
  content?: string;
  toolCalls?: { id: string; name: string; arguments: string }[];
  name?: string;
  toolCallId?: string;
  metadata?: ToolResultMetadata;
  // Set on user-role messages whose original turn carried image
  // attachments. The chat UI renders these as inline thumbnails on
  // bubbles loaded from history.
  imageUrls?: string[];
}

export async function getChatHistory(agentId: string, sessionId: string): Promise<ChatHistoryMessage[]> {
  const res = await apiFetch(`/api/chat/history?agentId=${encodeURIComponent(agentId)}&sessionId=${encodeURIComponent(sessionId)}`);
  if (!res.ok) return [];
  const data = await res.json();
  // Backend wraps in { history: [...] }; older shape was a raw array.
  if (Array.isArray(data?.history)) return data.history;
  return Array.isArray(data) ? data : [];
}

export async function getChatSessions(agentId: string): Promise<{ id: string; title?: string; preview: string; thumbnailUrl?: string }[]> {
  const res = await apiFetch(`/api/chat/sessions?agentId=${encodeURIComponent(agentId)}`);
  if (!res.ok) return [];
  const data = await res.json();
  // Backend wraps the list in { sessions: [...] }. Tolerate raw array
  // shape too in case an older deployment is still around.
  if (Array.isArray(data?.sessions)) return data.sessions;
  return Array.isArray(data) ? data : [];
}

export async function renameChatSession(agentId: string, sessionId: string, title: string) {
  const res = await apiFetch(`/api/chat/sessions/${encodeURIComponent(sessionId)}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ agentId, title }),
  });
  return res.json();
}

export async function deleteChatSession(agentId: string, sessionId: string) {
  const res = await apiFetch(
    `/api/chat/sessions/${encodeURIComponent(sessionId)}?agentId=${encodeURIComponent(agentId)}`,
    { method: "DELETE" },
  );
  return res.json();
}

export async function sendChat(agentId: string, sessionId: string, message: string): Promise<{ response: string }> {
  const res = await apiFetch("/api/chat", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ agentId, sessionId, message }),
  });
  return res.json();
}

export interface ToolResultMetadata {
  sandbox?: boolean;
}

export interface ChatStreamEvent {
  type: "content" | "tool_call" | "tool_result" | "error" | "done";
  data?: {
    content?: string;
    id?: string;
    name?: string;
    arguments?: string;
    result?: string;
    message?: string;
    metadata?: ToolResultMetadata;
  };
}

export async function sendChatStream(
  agentId: string,
  sessionId: string,
  message: string,
  onEvent: (evt: ChatStreamEvent) => void,
  signal?: AbortSignal,
  imageUrls?: string[],
): Promise<void> {
  const res = await apiFetch("/api/chat/stream", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ agentId, sessionId, message, imageUrls: imageUrls ?? [] }),
    signal,
  });
  if (!res.ok || !res.body) throw new Error("stream failed");

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";

  // Reader loop exits on either an explicit {type:"done"} event from the
  // server or a clean stream end (done flag from getReader). We tear down
  // early on "done" so any trailing bytes that may have been queued behind
  // the final flush don't get re-parsed and surfaced as spurious errors.
  let finished = false;
  while (!finished) {
    const { done, value } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });

    const lines = buffer.split("\n");
    buffer = lines.pop() || "";

    for (const line of lines) {
      if (!line.startsWith("data: ")) continue;
      try {
        const evt = JSON.parse(line.slice(6)) as ChatStreamEvent;
        onEvent(evt);
        if (evt.type === "done") {
          finished = true;
        }
      } catch { /* skip malformed frames */ }
    }
  }
  try { await reader.cancel(); } catch { /* ignore */ }
}

export interface UploadedFile {
  path: string;
  size: number;
}

export async function uploadAgentFiles(
  agentId: string,
  sessionId: string,
  files: File[],
): Promise<UploadedFile[]> {
  const fd = new FormData();
  for (const f of files) fd.append("file", f, f.name);
  const qs = sessionId ? `?sessionId=${encodeURIComponent(sessionId)}` : "";
  const res = await apiFetch(`/api/agents/${encodeURIComponent(agentId)}/files${qs}`, {
    method: "POST",
    body: fd,
  });
  if (!res.ok) throw new Error(`upload failed: ${res.status}`);
  const data = await res.json();
  return (data.files || []) as UploadedFile[];
}

// Agents
export async function getAgents(): Promise<AgentDetail[]> {
  const res = await apiFetch("/api/agents");
  if (!res.ok) {
    // 401 etc. return a JSON error envelope — throw so callers fall back
    // to [] instead of crashing on .map of a non-array.
    throw new Error(`getAgents failed: ${res.status}`);
  }
  const data = await res.json();
  // Backend returns { agents: [...] }. Tolerate raw array too in case an
  // older handler is still around.
  if (Array.isArray(data?.agents)) return data.agents as AgentDetail[];
  return Array.isArray(data) ? (data as AgentDetail[]) : [];
}

export async function createAgent(agent: Partial<AgentDetail>) {
  const res = await apiFetch("/api/agents", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(agent),
  });
  return res.json();
}

export interface AgentSkillsConfig {
  disabled?: string[];
  alwaysLoad?: string[];
}

// The backend accepts model / soul / skills / providers on update.
// `AgentDetail.skills` is a flat string[] (legacy), but per-agent skills
// config is really { disabled, alwaysLoad } — use an explicit payload
// type so the two shapes don't collide in the type system.
export interface AgentUpdatePayload {
  name?: string;
  description?: string;
  model?: string;
  soul?: string;
  skills?: AgentSkillsConfig;
  // Whole-map replace: omit to leave providers untouched, send {} to
  // clear them, or send the full desired map to replace.
  providers?: Record<string, ProviderData>;
}

export async function updateAgent(id: string, agent: AgentUpdatePayload) {
  const res = await apiFetch(`/api/agents/${id}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(agent),
  });
  return res.json();
}

export interface AgentFileConfig {
  model?: string;
  maxTokens?: number;
  temperature?: number;
  maxToolIterations?: number;
  workspace?: string;
  skills?: AgentSkillsConfig;
  providers?: Record<string, ProviderData>;
}

// Fetch the raw agent.json for one agent (per-agent overrides only — not
// the merged/resolved config). Used by the per-agent Models and Skills
// admin pages.
export async function getAgentConfig(id: string): Promise<AgentFileConfig> {
  const res = await apiFetch(`/api/agents/${id}/config`);
  return res.json();
}

export async function deleteAgent(id: string) {
  const res = await apiFetch(`/api/agents/${id}`, {
    method: "DELETE",
  });
  return res.json();
}

// Skills
export async function getSkills(): Promise<SkillInfo[]> {
  const res = await apiFetch("/api/skills");
  return res.json();
}

export async function deleteSkill(name: string) {
  const res = await apiFetch(`/api/skills/${name}`, {
    method: "DELETE",
  });
  return res.json();
}

// Per-agent skills: list what's installed in an agent's own home/skills dir.
// Agent-scoped skills shadow global ones with the same name.
export async function getAgentSkills(agentId: string): Promise<SkillInfo[]> {
  const res = await apiFetch(`/api/agents/${encodeURIComponent(agentId)}/skills`);
  return res.json();
}

export async function deleteAgentSkill(agentId: string, name: string) {
  const res = await apiFetch(
    `/api/agents/${encodeURIComponent(agentId)}/skills/${encodeURIComponent(name)}`,
    { method: "DELETE" },
  );
  return res.json();
}

// Search results use skills.sh's shape; clawhub has a different shape but the
// admin UI only wires skills.sh (primary registry). Callers that want clawhub
// go through installSkill with source="clawhub".
export interface SkillSearchResult {
  id: string;       // "<owner>/<repo>/<skillId>"
  skillId: string;  // folder name — also the slug passed to installSkill
  name: string;
  source: string;   // "<owner>/<repo>"
  installs: number;
}

export async function searchSkills(query: string): Promise<SkillSearchResult[]> {
  if (!query.trim()) return [];
  const res = await apiFetch(`/api/skills/search?source=skillssh&q=${encodeURIComponent(query)}`);
  if (!res.ok) return [];
  const data = await res.json();
  return (data.results || []) as SkillSearchResult[];
}

export interface InstallSkillRequest {
  name: string;
  source?: "skillssh" | "clawhub" | "github" | "auto";
  repo?: string;
  agent?: string;  // omit for global install (admin only)
}

export interface InstallSkillResponse {
  ok: boolean;
  source?: string;
  name?: string;
  version?: string;
  installedAt?: string;
  files?: number;
  error?: string;
}

export async function installSkill(req: InstallSkillRequest): Promise<InstallSkillResponse> {
  const res = await apiFetch("/api/skills/install", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  return res.json();
}

// --- Tools (provider-backed capabilities: web_search, image_gen, tts, ...) ---

export interface ToolProviderCatalog {
  name: string;
  label: string;
  needsKey: boolean;
  needsUrl: boolean;
  models: string[];
}

export interface ToolCategoryCatalog {
  name: string;
  label: string;
  providers: ToolProviderCatalog[];
}

export interface ToolProviderSettings {
  apiKey?: string;
  endpoint?: string;
  options?: Record<string, string>;
}

export interface ToolCategorySettings {
  primary?: string;
  fallbacks?: string[];
  autoFallback?: boolean;
}

export interface ToolsConfig {
  categories: ToolCategoryCatalog[];
  toolProviders: Record<string, ToolProviderSettings>;
  tools: Record<string, ToolCategorySettings>;
}

export async function getTools(): Promise<ToolsConfig> {
  const res = await apiFetch("/api/tools");
  return res.json();
}

export async function saveTools(payload: {
  toolProviders: Record<string, ToolProviderSettings>;
  tools: Record<string, ToolCategorySettings>;
}): Promise<{ ok: boolean; error?: string }> {
  const res = await apiFetch("/api/tools", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(payload),
  });
  return res.json();
}

// Plugins
export async function getPlugins(): Promise<PluginInfo[]> {
  const res = await apiFetch("/api/plugins");
  return res.json();
}

export async function updatePlugin(id: string, data: Partial<PluginInfo>) {
  const res = await apiFetch(`/api/plugins/${id}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(data),
  });
  return res.json();
}

// Channels
export async function getChannels(): Promise<ChannelInfo[]> {
  const res = await apiFetch("/api/channels");
  return res.json();
}

// Cron Jobs
export async function getCronJobs(): Promise<CronJobInfo[]> {
  const res = await apiFetch("/api/cron");
  return res.json();
}

export async function createCronJob(job: Partial<CronJobInfo>) {
  const res = await apiFetch("/api/cron", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(job),
  });
  return res.json();
}

export async function updateCronJob(id: string, job: Partial<CronJobInfo>) {
  const res = await apiFetch(`/api/cron/${id}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(job),
  });
  return res.json();
}

export async function deleteCronJob(id: string) {
  const res = await apiFetch(`/api/cron/${id}`, {
    method: "DELETE",
  });
  return res.json();
}

// --- Admin API: API keys ---

// APIKey is one entry returned by GET /v1/admin/apikeys. The `key` field is
// masked by the server for everyone except the create/rotate response, which
// returns the freshly-issued plaintext key under a separate `key` field.
export interface APIKey {
  id: string;
  name: string;
  key: string; // masked for list responses (e.g. "fc_abcd****wxyz")
  createdAt: string;
}

// Helper: pull a server-supplied {error} message out of a non-OK response so
// callers can surface the real reason (auth failure, duplicate id, etc.)
// instead of crashing on `.apikey` being undefined.
async function readError(res: Response, fallback: string): Promise<string> {
  try {
    const body = await res.json();
    if (body && typeof body.error === "string") return body.error;
  } catch {}
  return `${fallback} (HTTP ${res.status})`;
}

export async function listAPIKeys(): Promise<APIKey[]> {
  const res = await apiFetch("/v1/admin/apikeys");
  if (!res.ok) return [];
  const data = await res.json();
  return data.apikeys || [];
}

export async function createAPIKey(id: string, name: string): Promise<{ apikey: APIKey; key: string }> {
  const res = await apiFetch("/v1/admin/apikeys", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ id, name }),
  });
  if (!res.ok) throw new Error(await readError(res, "create API key failed"));
  const data = await res.json();
  if (!data.apikey || !data.key) throw new Error("malformed response from server");
  return data;
}

export async function deleteAPIKey(id: string): Promise<void> {
  const res = await apiFetch(`/v1/admin/apikeys/${id}`, { method: "DELETE" });
  if (!res.ok) throw new Error(await readError(res, "delete API key failed"));
}

export async function rotateAPIKey(id: string): Promise<string> {
  const res = await apiFetch(`/v1/admin/apikeys/${id}/rotate`, { method: "POST" });
  if (!res.ok) throw new Error(await readError(res, "rotate API key failed"));
  const data = await res.json();
  if (!data.key) throw new Error("malformed response from server");
  return data.key;
}

// --- Admin API: agent ↔ apikey bindings ---

// Map of agent id → apikey id. Empty value means agent is admin-only.
export type AgentBindings = Record<string, string>;

export async function listAgentBindings(): Promise<AgentBindings> {
  const res = await apiFetch("/api/agent-bindings");
  if (!res.ok) return {};
  const data = await res.json();
  return data.bindings || {};
}

// Pass apiKeyId="" to unbind (agent returns to admin-only access).
export async function bindAgent(agentId: string, apiKeyId: string): Promise<{ ok: boolean; error?: string }> {
  const res = await apiFetch(`/api/agents/${agentId}/binding`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ apiKeyId }),
  });
  return res.json();
}
