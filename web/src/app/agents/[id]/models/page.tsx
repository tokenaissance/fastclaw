"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Brain, Plus, Pencil, Trash2, Check, Key, Globe, Cpu, Layers } from "lucide-react";
import {
  getAgentConfig,
  testProvider,
  updateAgent,
  type ModelEntry,
  type ProviderData,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useAgentName } from "@/hooks/use-agent-name";

const PROVIDER_PRESETS: Record<string, { apiBase: string; apiType: string }> = {
  openrouter: { apiBase: "https://openrouter.ai/api/v1", apiType: "openai-chat" },
  openai: { apiBase: "https://api.openai.com/v1", apiType: "openai-chat" },
  anthropic: { apiBase: "https://api.anthropic.com/v1", apiType: "anthropic-messages" },
  deepseek: { apiBase: "https://api.deepseek.com/v1", apiType: "openai-chat" },
  groq: { apiBase: "https://api.groq.com/openai/v1", apiType: "openai-chat" },
  ollama: { apiBase: "http://localhost:11434/v1", apiType: "openai-chat" },
  custom: { apiBase: "", apiType: "openai-chat" },
};

const API_TYPE_OPTIONS = [
  { value: "openai-chat", label: "OpenAI Completions" },
  { value: "anthropic-messages", label: "Anthropic Messages" },
];

const AUTH_TYPE_OPTIONS = [
  { value: "api-key", label: "API Key" },
  { value: "bearer-token", label: "Bearer Token" },
];

interface ProviderEntry {
  name: string;
  apiBase: string;
  apiKey: string;     // plain-text (only set when editing)
  maskedKey: string;  // what the server returned, for display
  apiType: string;
  authType: string;
  models: ModelEntry[];
}

function emptyModel(): ModelEntry {
  return {
    id: "",
    name: "",
    reasoning: false,
    input: ["text"],
    cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
    contextWindow: 200000,
    maxTokens: 8192,
  };
}

function toProviderEntry(name: string, p: ProviderData): ProviderEntry {
  return {
    name,
    apiBase: p.apiBase || "",
    apiKey: "",
    maskedKey: p.apiKey || "",
    apiType: p.apiType || "openai-chat",
    authType: p.authType || "api-key",
    models: p.models || [],
  };
}

function providersMapFrom(list: ProviderEntry[]): Record<string, ProviderData> {
  const out: Record<string, ProviderData> = {};
  for (const p of list) {
    out[p.name] = {
      apiBase: p.apiBase,
      // If user didn't type a new key, preserve whatever was there (the
      // masked string is what the server gave us back).
      apiKey: p.apiKey || p.maskedKey,
      apiType: p.apiType,
      authType: p.authType,
      models: p.models,
    };
  }
  return out;
}

export default function AgentModelsPage() {
  const agentId = useAgentIdFromURL();
  const agentName = useAgentName(agentId);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);

  const [providers, setProviders] = useState<ProviderEntry[]>([]);
  const [selectedModel, setSelectedModel] = useState<string>("");

  const [dialogOpen, setDialogOpen] = useState(false);
  const [editingName, setEditingName] = useState<string | null>(null);
  const [formPreset, setFormPreset] = useState("openrouter");
  const [formName, setFormName] = useState("");
  const [formApiBase, setFormApiBase] = useState("");
  const [formApiKey, setFormApiKey] = useState("");
  const [formApiType, setFormApiType] = useState("openai-chat");
  const [formAuthType, setFormAuthType] = useState("api-key");
  const [formModels, setFormModels] = useState<ModelEntry[]>([]);
  const [testStatus, setTestStatus] = useState<"idle" | "testing" | "success" | "error">("idle");
  const [testError, setTestError] = useState("");

  const fetchConfig = useCallback(() => {
    setLoading(true);
    getAgentConfig(agentId)
      .then((cfg) => {
        const list: ProviderEntry[] = [];
        for (const [name, p] of Object.entries(cfg.providers || {})) {
          list.push(toProviderEntry(name, p));
        }
        setProviders(list);
        setSelectedModel(cfg.model || "");
      })
      .catch(() => {})
      .finally(() => setLoading(false));
  }, [agentId]);

  useEffect(() => {
    fetchConfig();
  }, [fetchConfig]);

  const allModelOptions: { value: string; label: string }[] = useMemo(() => {
    const out: { value: string; label: string }[] = [];
    for (const p of providers) {
      for (const m of p.models) {
        out.push({
          value: `${p.name}/${m.id}`,
          label: `${p.name}/${m.name || m.id}`,
        });
      }
    }
    return out;
  }, [providers]);

  const saveAll = useCallback(
    async (nextProviders: ProviderEntry[], nextModel?: string) => {
      setSaving(true);
      try {
        await updateAgent(agentId, {
          providers: providersMapFrom(nextProviders),
          model: nextModel ?? selectedModel,
        });
        setSaved(true);
        setTimeout(() => setSaved(false), 1800);
      } finally {
        setSaving(false);
      }
      fetchConfig();
    },
    [agentId, selectedModel, fetchConfig],
  );

  const openAddDialog = () => {
    setEditingName(null);
    setFormPreset("openrouter");
    setFormName("openrouter");
    setFormApiBase(PROVIDER_PRESETS["openrouter"].apiBase);
    setFormApiType(PROVIDER_PRESETS["openrouter"].apiType);
    setFormAuthType("api-key");
    setFormApiKey("");
    setFormModels([]);
    setTestStatus("idle");
    setTestError("");
    setDialogOpen(true);
  };

  const openEditDialog = (p: ProviderEntry) => {
    setEditingName(p.name);
    const preset = Object.keys(PROVIDER_PRESETS).includes(p.name) ? p.name : "custom";
    setFormPreset(preset);
    setFormName(p.name);
    setFormApiBase(p.apiBase);
    setFormApiType(p.apiType);
    setFormAuthType(p.authType || "api-key");
    setFormApiKey("");
    setFormModels(p.models.map((m) => ({ ...m, cost: { ...m.cost }, input: [...m.input] })));
    setTestStatus("idle");
    setTestError("");
    setDialogOpen(true);
  };

  const handlePresetChange = (preset: string) => {
    setFormPreset(preset);
    if (preset !== "custom") {
      setFormName(preset);
      setFormApiBase(PROVIDER_PRESETS[preset].apiBase);
      setFormApiType(PROVIDER_PRESETS[preset].apiType);
    } else {
      setFormName("");
      setFormApiBase("");
      setFormApiType("openai-chat");
    }
    setTestStatus("idle");
    setTestError("");
  };

  const handleTestConnection = async () => {
    setTestStatus("testing");
    setTestError("");
    try {
      const result = await testProvider({
        apiBase: formApiBase,
        apiKey: formApiKey,
        model: "",
        apiType: formApiType,
        authType: formAuthType,
      });
      setTestStatus(result.ok ? "success" : "error");
      if (!result.ok) {
        const urlInfo = result.url ? `\nRequest URL: ${result.url}` : "";
        setTestError((result.error || "Connection failed") + urlInfo);
      }
    } catch {
      setTestStatus("error");
      setTestError("Connection failed");
    }
  };

  const handleAddModel = () =>
    setFormModels((prev) => [...prev, emptyModel()]);

  const handleUpdateModel = (idx: number, field: string, value: unknown) => {
    setFormModels((prev) => {
      const next = [...prev];
      const m = { ...next[idx], cost: { ...next[idx].cost }, input: [...next[idx].input] };
      if (field === "id") m.id = value as string;
      else if (field === "name") m.name = value as string;
      else if (field === "reasoning") m.reasoning = value as boolean;
      else if (field === "contextWindow") m.contextWindow = Number(value) || 0;
      else if (field === "maxTokens") m.maxTokens = Number(value) || 0;
      next[idx] = m;
      return next;
    });
  };

  const handleRemoveModel = (idx: number) =>
    setFormModels((prev) => prev.filter((_, i) => i !== idx));

  const handleSaveProvider = async () => {
    const name = formName.toLowerCase().trim().replace(/\s+/g, "-");
    if (!name) return;
    const filtered = editingName
      ? providers.filter((p) => p.name !== editingName)
      : providers;
    const next: ProviderEntry[] = [
      ...filtered,
      {
        name,
        apiBase: formApiBase,
        apiKey: formApiKey,
        maskedKey: formApiKey ? "sk-****" : (editingName ? providers.find((p) => p.name === editingName)?.maskedKey || "" : ""),
        apiType: formApiType,
        authType: formAuthType,
        models: formModels.filter((m) => m.id.trim()),
      },
    ];
    setProviders(next);
    setDialogOpen(false);
    await saveAll(next);
  };

  const handleDeleteProvider = async (name: string) => {
    const next = providers.filter((p) => p.name !== name);
    setProviders(next);
    // If the active model was from this provider, clear it.
    const nextModel = selectedModel.startsWith(`${name}/`) ? "" : selectedModel;
    await saveAll(next, nextModel);
  };

  const handleDefaultModelChange = async (value: string) => {
    setSelectedModel(value);
    await saveAll(providers, value);
  };

  if (loading) {
    return (
      <div className="p-6 space-y-6 max-w-5xl mx-auto">
        <Skeleton className="h-10 w-48" />
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          {[1, 2, 3].map((i) => (
            <Skeleton key={i} className="h-48" />
          ))}
        </div>
      </div>
    );
  }

  return (
    <div className="p-6 space-y-6 max-w-5xl mx-auto">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">Models</h2>
          <p className="text-sm text-muted-foreground mt-1">
            LLM providers and models scoped to{" "}
            <strong>{agentName}</strong> — only this agent uses them
          </p>
        </div>
        <div className="flex items-center gap-2">
          {saved && (
            <span className="inline-flex items-center gap-1.5 text-xs text-emerald-600 dark:text-emerald-400 mr-2">
              <Check className="h-3.5 w-3.5" /> Saved
            </span>
          )}
          <Button variant="outline" onClick={openAddDialog} disabled={saving}>
            <Plus className="h-4 w-4 mr-2" />
            Add Provider
          </Button>
        </div>
      </div>

      {/* Active model */}
      <div className="rounded-lg border border-border bg-card p-5">
        <div className="flex items-center gap-2 mb-3">
          <Cpu className="h-4 w-4 text-primary" />
          <h3 className="font-medium">Active Model</h3>
        </div>
        {allModelOptions.length > 0 ? (
          <Select
            value={selectedModel}
            onValueChange={(v: string | null) => v && handleDefaultModelChange(v)}
            disabled={saving}
          >
            <SelectTrigger className="font-mono text-sm max-w-md">
              <SelectValue placeholder="Select a model" />
            </SelectTrigger>
            <SelectContent>
              {allModelOptions.map((opt) => (
                <SelectItem key={opt.value} value={opt.value}>
                  <span className="font-mono text-sm">{opt.value}</span>
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        ) : (
          <Input
            value={selectedModel}
            onChange={(e) => setSelectedModel(e.target.value)}
            onBlur={() => handleDefaultModelChange(selectedModel)}
            placeholder="Add a provider with models below"
            className="font-mono text-sm max-w-md"
          />
        )}
        <p className="text-xs text-muted-foreground mt-2">
          Used by <strong>{agentName}</strong>. Format{" "}
          <code className="text-[11px]">provider/modelId</code>.
        </p>
      </div>

      {/* Providers */}
      {providers.length === 0 ? (
        <div className="rounded-lg border border-border bg-card">
          <div className="flex flex-col items-center justify-center py-16">
            <div className="flex h-14 w-14 items-center justify-center rounded-2xl bg-amber-500/10 mb-4">
              <Brain className="h-7 w-7 text-amber-500" />
            </div>
            <p className="text-sm text-muted-foreground mb-1">No providers configured</p>
            <p className="text-xs text-muted-foreground/60 mb-4 max-w-sm text-center">
              Add an LLM provider below to give this agent its own API credentials.
            </p>
            <Button variant="outline" size="sm" onClick={openAddDialog}>
              <Plus className="h-4 w-4 mr-2" />
              Add Provider
            </Button>
          </div>
        </div>
      ) : (
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          {providers.map((p) => (
            <div
              key={p.name}
              className="group rounded-lg border border-border bg-card p-5 transition-colors hover:bg-muted/50"
            >
              <div className="flex items-start justify-between mb-4">
                <div className="flex h-12 w-12 items-center justify-center rounded-xl bg-gradient-to-br from-amber-500 to-orange-600">
                  <Brain className="h-6 w-6 text-white" />
                </div>
                <Badge
                  variant="outline"
                  className="bg-emerald-500/10 text-emerald-600 dark:text-emerald-400 border-emerald-500/20"
                >
                  <span className="mr-1.5 inline-block h-1.5 w-1.5 rounded-full bg-emerald-500" />
                  Active
                </Badge>
              </div>
              <p className="text-base font-medium mb-2">{p.name}</p>
              <div className="space-y-1 text-sm text-muted-foreground">
                <div className="flex items-center gap-1.5">
                  <Globe className="h-3 w-3" />
                  <span className="font-mono text-xs truncate">
                    {p.apiBase || "Not set"}
                  </span>
                </div>
                <div className="flex items-center gap-1.5">
                  <Key className="h-3 w-3" />
                  <span className="font-mono text-xs">{p.maskedKey || "Not set"}</span>
                </div>
                <div className="flex items-center gap-1.5">
                  <Layers className="h-3 w-3" />
                  <span className="font-mono text-xs">
                    {p.models.length} model{p.models.length !== 1 ? "s" : ""}
                  </span>
                </div>
              </div>
              <div className="flex items-center gap-2 mt-4 pt-3 border-t border-border">
                <Button
                  variant="ghost"
                  size="sm"
                  className="h-8 text-xs"
                  onClick={() => openEditDialog(p)}
                  disabled={saving}
                >
                  <Pencil className="h-3 w-3 mr-1.5" />
                  Edit
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  className="h-8 text-xs text-destructive hover:text-destructive"
                  onClick={() => handleDeleteProvider(p.name)}
                  disabled={saving}
                >
                  <Trash2 className="h-3 w-3 mr-1.5" />
                  Remove
                </Button>
              </div>
            </div>
          ))}
        </div>
      )}

      {/* Add / Edit Dialog */}
      <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
        <DialogContent className="sm:max-w-2xl max-h-[85vh] overflow-y-auto">
          <DialogHeader>
            <DialogTitle>{editingName ? "Edit Provider" : "Add Provider"}</DialogTitle>
            <DialogDescription>
              Configure an LLM provider that only <strong>{agentName}</strong>{" "}
              will use
            </DialogDescription>
          </DialogHeader>

          <div className="space-y-4 py-2">
            <div className="space-y-2">
              <Label>Provider</Label>
              <Select
                value={formPreset}
                onValueChange={(v: string | null) => v && handlePresetChange(v)}
                disabled={!!editingName}
              >
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {Object.keys(PROVIDER_PRESETS).map((p) => (
                    <SelectItem key={p} value={p}>
                      <span className="capitalize">{p}</span>
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            {formPreset === "custom" && (
              <div className="space-y-2">
                <Label>Provider Name</Label>
                <Input
                  value={formName}
                  onChange={(e) => setFormName(e.target.value)}
                  placeholder="e.g. my-provider"
                  className="font-mono text-sm"
                  disabled={!!editingName}
                />
              </div>
            )}

            <div className="space-y-2">
              <Label>API Base URL</Label>
              <Input
                value={formApiBase}
                onChange={(e) => setFormApiBase(e.target.value)}
                placeholder="https://api.openai.com/v1"
                className="font-mono text-sm"
              />
            </div>

            <div className="space-y-2">
              <Label>API Key</Label>
              <Input
                type="password"
                value={formApiKey}
                onChange={(e) => setFormApiKey(e.target.value)}
                placeholder="sk-..."
                className="font-mono text-sm"
              />
              {editingName && (
                <p className="text-[11px] text-muted-foreground/60">
                  Leave empty to keep existing key
                </p>
              )}
            </div>

            <div className="grid grid-cols-2 gap-4">
              <div className="space-y-2">
                <Label>API Type</Label>
                <Select
                  value={formApiType}
                  onValueChange={(v: string | null) => v && setFormApiType(v)}
                >
                  <SelectTrigger className="w-full text-sm">
                    <SelectValue>
                      {API_TYPE_OPTIONS.find((o) => o.value === formApiType)?.label}
                    </SelectValue>
                  </SelectTrigger>
                  <SelectContent>
                    {API_TYPE_OPTIONS.map((opt) => (
                      <SelectItem key={opt.value} value={opt.value}>
                        {opt.label}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-2">
                <Label>Auth Type</Label>
                <Select
                  value={formAuthType}
                  onValueChange={(v: string | null) => v && setFormAuthType(v)}
                >
                  <SelectTrigger className="w-full text-sm">
                    <SelectValue>
                      {AUTH_TYPE_OPTIONS.find((o) => o.value === formAuthType)?.label}
                    </SelectValue>
                  </SelectTrigger>
                  <SelectContent>
                    {AUTH_TYPE_OPTIONS.map((opt) => (
                      <SelectItem key={opt.value} value={opt.value}>
                        {opt.label}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
            </div>

            <div className="space-y-2">
              <div className="flex items-center gap-3">
                <Button
                  variant="outline"
                  size="sm"
                  onClick={handleTestConnection}
                  disabled={testStatus === "testing" || !formApiBase}
                >
                  {testStatus === "testing" ? "Testing..." : "Test Connection"}
                </Button>
                {testStatus === "success" && (
                  <Badge
                    variant="outline"
                    className="bg-emerald-500/10 text-emerald-600 dark:text-emerald-400 border-emerald-500/20"
                  >
                    Connected
                  </Badge>
                )}
              </div>
              {testStatus === "error" && (
                <p className="text-sm text-destructive break-all">{testError}</p>
              )}
            </div>

            {/* Models */}
            <div className="space-y-3 pt-2 border-t border-border">
              <div className="flex items-center justify-between">
                <Label className="text-base">Models</Label>
                <Button variant="outline" size="sm" onClick={handleAddModel}>
                  <Plus className="h-3 w-3 mr-1.5" />
                  Add Model
                </Button>
              </div>

              {formModels.length === 0 && (
                <p className="text-sm text-muted-foreground/60 text-center py-4">
                  No models configured. Add models to use with this provider.
                </p>
              )}

              {formModels.map((m, idx) => (
                <div
                  key={idx}
                  className="rounded-lg border border-border bg-muted/30 p-4 space-y-3"
                >
                  <div className="flex items-center justify-between">
                    <span className="text-sm font-medium text-muted-foreground">
                      Model {idx + 1}
                    </span>
                    <Button
                      variant="ghost"
                      size="sm"
                      className="h-7 text-xs text-destructive hover:text-destructive"
                      onClick={() => handleRemoveModel(idx)}
                    >
                      <Trash2 className="h-3 w-3 mr-1" />
                      Remove
                    </Button>
                  </div>
                  <div className="grid grid-cols-2 gap-3">
                    <div className="space-y-1">
                      <Label className="text-xs">Model ID</Label>
                      <Input
                        value={m.id}
                        onChange={(e) => handleUpdateModel(idx, "id", e.target.value)}
                        placeholder="e.g. gpt-4o"
                        className="font-mono text-xs h-8"
                      />
                    </div>
                    <div className="space-y-1">
                      <Label className="text-xs">Display Name</Label>
                      <Input
                        value={m.name}
                        onChange={(e) => handleUpdateModel(idx, "name", e.target.value)}
                        placeholder="e.g. GPT-4o"
                        className="text-xs h-8"
                      />
                    </div>
                  </div>
                </div>
              ))}
            </div>
          </div>

          <DialogFooter>
            <Button variant="outline" onClick={() => setDialogOpen(false)}>
              Cancel
            </Button>
            <Button onClick={handleSaveProvider} disabled={!formName.trim() || saving}>
              {editingName ? "Update" : "Add"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
