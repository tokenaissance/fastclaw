export interface StatusResponse {
  configured: boolean;
  running: boolean;
  port: number;
  uptime: string;
  agents: AgentInfo[];
  channels: ChannelInfo[];
  provider: ProviderInfo;
  cronJobs?: number;
  plugins?: number;
}

export interface AgentInfo {
  id: string;
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
  model: string;
  workspace: string;
  maxTokens?: number;
  temperature?: number;
  maxToolIterations?: number;
  thinking?: string;
  soul?: string;
  skills?: string[];
  tools?: string[];
}

export interface SkillInfo {
  name: string;
  description: string;
  location: string;
  type: string;
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
  api?: string;
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
  hooks: { enabled: boolean; token?: string; path?: string; port?: number };
  cronJobs?: Array<Record<string, unknown>>;
}

// Status
export async function getStatus(): Promise<StatusResponse> {
  const res = await fetch("/api/status");
  return res.json();
}

// Provider
export async function testProvider(config: { apiBase: string; apiKey: string; model: string }) {
  const res = await fetch("/api/test-provider", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(config),
  });
  return res.json();
}

// Config
export async function saveConfig(config: Record<string, unknown>) {
  const res = await fetch("/api/save-config", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(config),
  });
  return res.json();
}

export async function getConfig(): Promise<ConfigResponse> {
  const res = await fetch("/api/config");
  return res.json();
}

export async function updateConfig(config: Record<string, unknown>) {
  const res = await fetch("/api/config", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(config),
  });
  return res.json();
}

// Chat
export async function sendChat(agentId: string, message: string): Promise<{ response: string }> {
  const res = await fetch("/api/chat", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ agentId, message }),
  });
  return res.json();
}

// Agents
export async function getAgents(): Promise<AgentDetail[]> {
  const res = await fetch("/api/agents");
  return res.json();
}

export async function createAgent(agent: Partial<AgentDetail>) {
  const res = await fetch("/api/agents", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(agent),
  });
  return res.json();
}

export async function updateAgent(id: string, agent: Partial<AgentDetail>) {
  const res = await fetch(`/api/agents/${id}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(agent),
  });
  return res.json();
}

export async function deleteAgent(id: string) {
  const res = await fetch(`/api/agents/${id}`, {
    method: "DELETE",
  });
  return res.json();
}

// Skills
export async function getSkills(): Promise<SkillInfo[]> {
  const res = await fetch("/api/skills");
  return res.json();
}

export async function deleteSkill(name: string) {
  const res = await fetch(`/api/skills/${name}`, {
    method: "DELETE",
  });
  return res.json();
}

// Plugins
export async function getPlugins(): Promise<PluginInfo[]> {
  const res = await fetch("/api/plugins");
  return res.json();
}

export async function updatePlugin(id: string, data: Partial<PluginInfo>) {
  const res = await fetch(`/api/plugins/${id}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(data),
  });
  return res.json();
}

// Channels
export async function getChannels(): Promise<ChannelInfo[]> {
  const res = await fetch("/api/channels");
  return res.json();
}

// Cron Jobs
export async function getCronJobs(): Promise<CronJobInfo[]> {
  const res = await fetch("/api/cron");
  return res.json();
}

export async function createCronJob(job: Partial<CronJobInfo>) {
  const res = await fetch("/api/cron", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(job),
  });
  return res.json();
}

export async function updateCronJob(id: string, job: Partial<CronJobInfo>) {
  const res = await fetch(`/api/cron/${id}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(job),
  });
  return res.json();
}

export async function deleteCronJob(id: string) {
  const res = await fetch(`/api/cron/${id}`, {
    method: "DELETE",
  });
  return res.json();
}
