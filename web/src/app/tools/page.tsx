"use client";

import { useEffect, useMemo, useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Wrench,
  Save,
  Check,
  Loader2,
  ChevronUp,
  ChevronDown,
  X,
  Plus,
  Search,
  ImageIcon,
  Volume2,
  ChevronRight,
  KeyRound,
  Link as LinkIcon,
} from "lucide-react";
import {
  getTools,
  saveTools,
  type ToolsConfig,
  type ToolCategoryCatalog,
  type ToolProviderCatalog,
  type ToolProviderSettings,
  type ToolCategorySettings,
} from "@/lib/api";

// categoryIcons picks a glyph for the left rail. Unknown categories fall
// back to the generic wrench so plugin-added categories still render.
const categoryIcons: Record<string, typeof Search> = {
  web_search: Search,
  image_gen: ImageIcon,
  tts: Volume2,
};

export default function ToolsPage() {
  const [cfg, setCfg] = useState<ToolsConfig | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Mutable copies of the two config maps. The catalog itself is immutable.
  const [providers, setProviders] = useState<Record<string, ToolProviderSettings>>({});
  const [tools, setTools] = useState<Record<string, ToolCategorySettings>>({});
  const [active, setActive] = useState<string>("");

  useEffect(() => {
    getTools()
      .then((data) => {
        setCfg(data);
        setProviders(data.toolProviders || {});
        setTools(data.tools || {});
        if (data.categories.length > 0) setActive(data.categories[0].name);
      })
      .catch((e) => setError(e instanceof Error ? e.message : "load failed"))
      .finally(() => setLoading(false));
  }, []);

  const updateProvider = (name: string, patch: Partial<ToolProviderSettings>) => {
    setProviders((prev) => ({ ...prev, [name]: { ...(prev[name] || {}), ...patch } }));
  };

  const updateCategory = (cat: string, patch: Partial<ToolCategorySettings>) => {
    setTools((prev) => ({ ...prev, [cat]: { ...(prev[cat] || {}), ...patch } }));
  };

  const handleSave = async () => {
    setSaving(true);
    setError(null);
    try {
      // Drop empty provider entries so the config stays tidy.
      const cleaned: Record<string, ToolProviderSettings> = {};
      for (const [name, p] of Object.entries(providers)) {
        const hasKey = p.apiKey && p.apiKey.trim();
        const hasURL = p.endpoint && p.endpoint.trim();
        const hasOpts = p.options && Object.keys(p.options).length > 0;
        if (hasKey || hasURL || hasOpts) cleaned[name] = p;
      }
      const resp = await saveTools({ toolProviders: cleaned, tools });
      if (!resp.ok) {
        setError(resp.error || "save failed");
      } else {
        setSaved(true);
        setTimeout(() => setSaved(false), 2000);
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : "save failed");
    } finally {
      setSaving(false);
    }
  };

  if (loading) {
    return (
      <div className="p-6 space-y-4 max-w-6xl mx-auto">
        <Skeleton className="h-8 w-40" />
        <Skeleton className="h-96" />
      </div>
    );
  }

  const activeCat = cfg?.categories.find((c) => c.name === active);

  return (
    <div className="p-6 max-w-6xl mx-auto">
      <div className="flex items-center justify-between mb-6">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">Tools</h2>
          <p className="text-sm text-muted-foreground mt-1">
            Configure provider API keys and fallback order. Tools with no configured provider are hidden from agents.
          </p>
        </div>
        <Button onClick={handleSave} disabled={saving} variant={saved ? "outline" : "default"}>
          {saved ? (
            <><Check className="h-4 w-4 mr-2" /> Saved</>
          ) : saving ? (
            <><Loader2 className="h-4 w-4 mr-2 animate-spin" /> Saving…</>
          ) : (
            <><Save className="h-4 w-4 mr-2" /> Save</>
          )}
        </Button>
      </div>

      {error && (
        <div className="mb-4 rounded-md border border-destructive/40 bg-destructive/10 px-4 py-2 text-sm text-destructive">
          {error}
        </div>
      )}

      {!cfg || cfg.categories.length === 0 ? (
        <div className="rounded-lg border border-border bg-card p-8 text-center">
          <Wrench className="h-8 w-8 text-muted-foreground/40 mx-auto mb-3" />
          <p className="text-sm text-muted-foreground">
            No tool categories available in this build.
          </p>
        </div>
      ) : (
        <div className="grid grid-cols-[220px_1fr] gap-6 items-start">
          <CategoryRail
            categories={cfg.categories}
            active={active}
            onSelect={setActive}
            configuredCount={(catName) => countConfiguredProviders(cfg.categories.find((c) => c.name === catName)!, providers)}
          />
          {activeCat ? (
            <CategoryPanel
              catalog={activeCat}
              providers={providers}
              setProvider={updateProvider}
              tools={tools[activeCat.name] || {}}
              setTools={(patch) => updateCategory(activeCat.name, patch)}
            />
          ) : null}
        </div>
      )}
    </div>
  );
}

function countConfiguredProviders(
  cat: ToolCategoryCatalog,
  providers: Record<string, ToolProviderSettings>,
): number {
  let n = 0;
  for (const p of cat.providers) {
    const v = providers[p.name];
    if (!v) continue;
    const hasKey = p.needsKey && v.apiKey && v.apiKey.trim();
    const hasURL = p.needsUrl && v.endpoint && v.endpoint.trim();
    if (hasKey || hasURL) n++;
  }
  return n;
}

function CategoryRail({
  categories,
  active,
  onSelect,
  configuredCount,
}: {
  categories: ToolCategoryCatalog[];
  active: string;
  onSelect: (name: string) => void;
  configuredCount: (name: string) => number;
}) {
  return (
    <nav className="space-y-1 sticky top-6">
      {categories.map((c) => {
        const Icon = categoryIcons[c.name] || Wrench;
        const isActive = c.name === active;
        const count = configuredCount(c.name);
        return (
          <button
            key={c.name}
            onClick={() => onSelect(c.name)}
            className={`w-full flex items-center gap-3 rounded-md px-3 py-2.5 text-left transition-colors ${
              isActive
                ? "bg-primary/10 text-primary"
                : "text-muted-foreground hover:bg-muted/40 hover:text-foreground"
            }`}
          >
            <Icon className="h-4 w-4 shrink-0" />
            <div className="flex-1 min-w-0">
              <p className={`text-sm ${isActive ? "font-medium" : ""}`}>{c.label}</p>
              <p className="text-[10px] font-mono opacity-70 truncate">{c.name}</p>
            </div>
            {count > 0 && (
              <span className={`text-[10px] rounded-full px-1.5 py-0.5 ${isActive ? "bg-primary/20" : "bg-muted"}`}>
                {count}
              </span>
            )}
            {isActive && <ChevronRight className="h-3.5 w-3.5 shrink-0" />}
          </button>
        );
      })}
    </nav>
  );
}

function CategoryPanel({
  catalog,
  providers,
  setProvider,
  tools,
  setTools,
}: {
  catalog: ToolCategoryCatalog;
  providers: Record<string, ToolProviderSettings>;
  setProvider: (name: string, patch: Partial<ToolProviderSettings>) => void;
  tools: ToolCategorySettings;
  setTools: (patch: Partial<ToolCategorySettings>) => void;
}) {
  return (
    <div className="space-y-6">
      <div>
        <h3 className="text-xl font-semibold">{catalog.label}</h3>
        <p className="text-sm text-muted-foreground font-mono mt-0.5">{catalog.name}</p>
      </div>

      {/* One card per provider */}
      <div className="space-y-3">
        {catalog.providers.map((p) => (
          <ProviderCard
            key={p.name}
            provider={p}
            settings={providers[p.name] || {}}
            onChange={(patch) => setProvider(p.name, patch)}
          />
        ))}
      </div>

      {/* Fallback chain editor */}
      <ChainEditor catalog={catalog} tools={tools} setTools={setTools} />
    </div>
  );
}

function ProviderCard({
  provider,
  settings,
  onChange,
}: {
  provider: ToolProviderCatalog;
  settings: ToolProviderSettings;
  onChange: (patch: Partial<ToolProviderSettings>) => void;
}) {
  const [showAdvanced, setShowAdvanced] = useState(false);
  const hasValue =
    (settings.apiKey && settings.apiKey.trim()) ||
    (settings.endpoint && settings.endpoint.trim());

  const defaultModel = settings.options?.model || "";

  const setOption = (k: string, v: string) => {
    const opts = { ...(settings.options || {}) };
    if (v === "") delete opts[k];
    else opts[k] = v;
    onChange({ options: opts });
  };

  return (
    <div
      className={`rounded-lg border p-5 transition-colors ${
        hasValue ? "border-primary/40 bg-card" : "border-border bg-card"
      }`}
    >
      <div className="flex items-start justify-between gap-3 mb-3">
        <div className="flex items-center gap-3">
          <div className="flex h-9 w-9 items-center justify-center rounded-md bg-primary/10">
            {provider.needsKey ? (
              <KeyRound className="h-4 w-4 text-primary" />
            ) : (
              <LinkIcon className="h-4 w-4 text-primary" />
            )}
          </div>
          <div>
            <p className="text-sm font-medium">{provider.label}</p>
            <p className="text-[11px] text-muted-foreground font-mono">{provider.name}</p>
          </div>
        </div>
        {hasValue && (
          <span className="text-[10px] uppercase tracking-wider text-primary/80 font-medium">
            Configured
          </span>
        )}
      </div>

      <div className="space-y-2.5">
        {provider.needsKey && (
          <div>
            <Label className="text-xs text-muted-foreground">API key</Label>
            <Input
              type="password"
              placeholder="sk-…"
              value={settings.apiKey || ""}
              onChange={(e) => onChange({ apiKey: e.target.value })}
              className="mt-1 font-mono text-sm"
            />
          </div>
        )}
        {provider.needsUrl && (
          <div>
            <Label className="text-xs text-muted-foreground">Endpoint</Label>
            <Input
              type="url"
              placeholder="https://searxng.example.com"
              value={settings.endpoint || ""}
              onChange={(e) => onChange({ endpoint: e.target.value })}
              className="mt-1 font-mono text-sm"
            />
          </div>
        )}
        {provider.models.length > 1 && (
          <div>
            <Label className="text-xs text-muted-foreground">Default model</Label>
            <Select value={defaultModel} onValueChange={(v) => setOption("model", v || "")}>
              <SelectTrigger className="mt-1 h-9">
                <SelectValue placeholder="Pick a default model" />
              </SelectTrigger>
              <SelectContent>
                {provider.models.map((m) => (
                  <SelectItem key={m} value={m}>
                    {m}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <p className="text-[10px] text-muted-foreground mt-1">
              Used when the chain reference omits a model (e.g. just <code className="font-mono">{provider.name}</code>).
            </p>
          </div>
        )}

        <button
          onClick={() => setShowAdvanced((v) => !v)}
          className="text-[11px] text-muted-foreground hover:text-foreground transition-colors"
        >
          {showAdvanced ? "Hide" : "Show"} advanced options
        </button>

        {showAdvanced && (
          <AdvancedOptionsEditor
            options={settings.options || {}}
            onChange={(next) => onChange({ options: next })}
          />
        )}
      </div>
    </div>
  );
}

function AdvancedOptionsEditor({
  options,
  onChange,
}: {
  options: Record<string, string>;
  onChange: (next: Record<string, string>) => void;
}) {
  const [newKey, setNewKey] = useState("");
  const [newVal, setNewVal] = useState("");

  const addPair = () => {
    if (!newKey.trim()) return;
    onChange({ ...options, [newKey.trim()]: newVal });
    setNewKey("");
    setNewVal("");
  };

  const removeKey = (k: string) => {
    const next = { ...options };
    delete next[k];
    onChange(next);
  };

  const entries = Object.entries(options).filter(([k]) => k !== "model");

  return (
    <div className="rounded-md border border-border/70 bg-muted/20 p-3 space-y-2">
      <p className="text-[10px] uppercase tracking-wider text-muted-foreground">
        Provider-specific options
      </p>
      {entries.length === 0 && (
        <p className="text-[11px] text-muted-foreground italic">
          No custom options. Provider uses its defaults.
        </p>
      )}
      {entries.map(([k, v]) => (
        <div key={k} className="flex items-center gap-2">
          <Input
            readOnly
            value={k}
            className="h-8 text-xs font-mono w-40"
          />
          <Input
            value={v}
            onChange={(e) => onChange({ ...options, [k]: e.target.value })}
            className="h-8 text-xs font-mono flex-1"
          />
          <Button
            size="icon"
            variant="ghost"
            className="h-7 w-7 text-muted-foreground hover:text-destructive"
            onClick={() => removeKey(k)}
          >
            <X className="h-3.5 w-3.5" />
          </Button>
        </div>
      ))}
      <div className="flex items-center gap-2 pt-1">
        <Input
          placeholder="key"
          value={newKey}
          onChange={(e) => setNewKey(e.target.value)}
          className="h-8 text-xs font-mono w-40"
        />
        <Input
          placeholder="value"
          value={newVal}
          onChange={(e) => setNewVal(e.target.value)}
          className="h-8 text-xs font-mono flex-1"
        />
        <Button size="icon" variant="ghost" className="h-7 w-7" onClick={addPair}>
          <Plus className="h-3.5 w-3.5" />
        </Button>
      </div>
    </div>
  );
}

function ChainEditor({
  catalog,
  tools,
  setTools,
}: {
  catalog: ToolCategoryCatalog;
  tools: ToolCategorySettings;
  setTools: (patch: Partial<ToolCategorySettings>) => void;
}) {
  const refOptions = useMemo(() => {
    const opts: { value: string; label: string }[] = [];
    for (const p of catalog.providers) {
      for (const m of p.models) {
        opts.push({ value: `${p.name}/${m}`, label: `${p.label} — ${m}` });
      }
    }
    return opts;
  }, [catalog]);

  const autoFallback = tools.autoFallback !== false;
  const chain = [tools.primary, ...(tools.fallbacks || [])].filter(Boolean) as string[];

  const setChain = (next: string[]) => {
    setTools({ primary: next[0] || "", fallbacks: next.slice(1) });
  };

  const addToChain = (ref: string) => {
    if (!ref || chain.includes(ref)) return;
    setChain([...chain, ref]);
  };

  const removeFromChain = (i: number) => {
    const next = [...chain];
    next.splice(i, 1);
    setChain(next);
  };

  const move = (i: number, delta: number) => {
    const j = i + delta;
    if (j < 0 || j >= chain.length) return;
    const next = [...chain];
    [next[i], next[j]] = [next[j], next[i]];
    setChain(next);
  };

  const unusedOptions = refOptions.filter((o) => !chain.includes(o.value));

  return (
    <div className="rounded-lg border border-border bg-card p-5">
      <div className="flex items-center justify-between mb-3">
        <Label className="text-xs uppercase tracking-wider text-muted-foreground">
          Fallback chain (top → bottom)
        </Label>
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted-foreground">Auto fallback</span>
          <Switch
            checked={autoFallback}
            onCheckedChange={(v) => setTools({ autoFallback: v })}
          />
        </div>
      </div>

      <div className="space-y-1.5">
        {chain.length === 0 ? (
          <p className="text-xs text-muted-foreground italic px-2 py-4">
            No providers selected. The <code className="font-mono">{catalog.name}</code> tool
            won&apos;t be available to agents until you add at least one.
          </p>
        ) : (
          chain.map((ref, i) => {
            const label = refOptions.find((o) => o.value === ref)?.label || ref;
            return (
              <div
                key={ref}
                className="flex items-center gap-2 rounded-md border border-border bg-muted/30 px-3 py-2"
              >
                <span className="text-[11px] font-mono text-muted-foreground w-6">
                  {i === 0 ? "1°" : `${i + 1}`}
                </span>
                <span className="text-sm flex-1">{label}</span>
                <span className="text-[11px] font-mono text-muted-foreground">{ref}</span>
                <div className="flex gap-0.5">
                  <Button size="icon" variant="ghost" className="h-6 w-6" onClick={() => move(i, -1)} disabled={i === 0}>
                    <ChevronUp className="h-3.5 w-3.5" />
                  </Button>
                  <Button size="icon" variant="ghost" className="h-6 w-6" onClick={() => move(i, 1)} disabled={i === chain.length - 1}>
                    <ChevronDown className="h-3.5 w-3.5" />
                  </Button>
                  <Button size="icon" variant="ghost" className="h-6 w-6 text-muted-foreground hover:text-destructive" onClick={() => removeFromChain(i)}>
                    <X className="h-3.5 w-3.5" />
                  </Button>
                </div>
              </div>
            );
          })
        )}
      </div>

      {unusedOptions.length > 0 && (
        <div className="mt-3 flex items-center gap-2">
          <Plus className="h-3.5 w-3.5 text-muted-foreground" />
          <Select onValueChange={(v) => v && addToChain(v)} value="">
            <SelectTrigger className="w-64 h-8 text-xs">
              <SelectValue placeholder="Add provider to chain…" />
            </SelectTrigger>
            <SelectContent>
              {unusedOptions.map((o) => (
                <SelectItem key={o.value} value={o.value}>
                  {o.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      )}
    </div>
  );
}
