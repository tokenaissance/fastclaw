"use client";

import { useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Separator } from "@/components/ui/separator";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Save,
  Check,
  Clock,
  Container,
  Brain,
  GraduationCap,
  ShieldCheck,
} from "lucide-react";
import { getConfig, updateConfig, getMe, type ConfigResponse } from "@/lib/api";

export default function RuntimeSettingsPage() {
  const router = useRouter();
  const [config, setConfig] = useState<ConfigResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  const [saveError, setSaveError] = useState("");

  const [sandboxEnabled, setSandboxEnabled] = useState(false);
  const [sandboxBackend, setSandboxBackend] = useState("docker");
  const [sandboxDockerImage, setSandboxDockerImage] = useState("");
  const [sandboxE2BTemplate, setSandboxE2BTemplate] = useState("base");
  const [sandboxE2BKey, setSandboxE2BKey] = useState("");
  const [sandboxBoxliteImage, setSandboxBoxliteImage] = useState("");
  const [sandboxBoxliteKey, setSandboxBoxliteKey] = useState("");
  const [sandboxBoxliteURL, setSandboxBoxliteURL] = useState("");
  const [defaultTimezone, setDefaultTimezone] = useState("");
  const [autoPersistEnabled, setAutoPersistEnabled] = useState(false);
  const [autoPersistEveryNTurns, setAutoPersistEveryNTurns] = useState("5");
  const [autoPersistModel, setAutoPersistModel] = useState("");
  const [skillsLearnerEnabled, setSkillsLearnerEnabled] = useState(false);
  const [skillsLearnerMinToolCalls, setSkillsLearnerMinToolCalls] = useState("");
  const [skillsLearnerModel, setSkillsLearnerModel] = useState("");
  const [piiScrubbingEnabled, setPiiScrubbingEnabled] = useState(false);

  useEffect(() => {
    // Belt-and-suspenders gate: the layout already hides the nav item,
    // but a direct URL hit needs to bounce too.
    getMe().then((m) => {
      if (m?.user?.role !== "super_admin") {
        router.replace("/settings/general");
        return;
      }
      setLoading(true);
      getConfig()
        .then((cfg) => {
          setConfig(cfg);
          setSandboxEnabled(cfg.sandbox?.enabled || false);
          const backend = cfg.sandbox?.backend || "docker";
          setSandboxBackend(backend);
          // Each backend has its own persisted field. For configs
          // predating the split there's only the legacy `image` slot,
          // so we migrate it into the backend it belonged to (the saved
          // `backend`) and leave the other two empty.
          const savedImage = cfg.sandbox?.image || "";
          setSandboxDockerImage(
            cfg.sandbox?.dockerImage ?? (backend === "docker" ? savedImage : ""),
          );
          setSandboxE2BTemplate(
            cfg.sandbox?.e2bTemplate ?? (backend === "e2b" ? savedImage || "base" : "base"),
          );
          setSandboxBoxliteImage(
            cfg.sandbox?.boxliteSnapshot ?? (backend === "boxlite" ? savedImage : ""),
          );
          setSandboxE2BKey(cfg.sandbox?.e2bKey || "");
          setSandboxBoxliteKey(cfg.sandbox?.boxliteKey || "");
          setSandboxBoxliteURL(cfg.sandbox?.boxliteUrl || "");
          setDefaultTimezone(cfg.prefs?.timezone || "");
          // Auto-persist and the skills learner are system-scope rows with no
          // per-agent counterpart for their cadence/model — before this page
          // the only writer was POST /api/config by hand.
          const autoPersist = cfg.memory?.autoPersist;
          setAutoPersistEnabled(autoPersist?.enabled ?? false);
          // 0 is "unset" on the wire (the runtime stamps its own default), so
          // show the default the operator would actually get.
          setAutoPersistEveryNTurns(
            autoPersist?.everyNTurns && autoPersist.everyNTurns > 0
              ? String(autoPersist.everyNTurns)
              : "5",
          );
          setAutoPersistModel(autoPersist?.model || "");
          setSkillsLearnerEnabled(cfg.skillsLearner?.enabled ?? false);
          setSkillsLearnerMinToolCalls(
            cfg.skillsLearner?.minToolCalls && cfg.skillsLearner.minToolCalls > 0
              ? String(cfg.skillsLearner.minToolCalls)
              : "",
          );
          setSkillsLearnerModel(cfg.skillsLearner?.model || "");
          setPiiScrubbingEnabled(cfg.privacy?.piiScrubbing?.enabled ?? false);
        })
        .catch(() => {})
        .finally(() => setLoading(false));
    });
  }, [router]);

  const handleSave = async () => {
    setSaving(true);
    setSaved(false);
    setSaveError("");
    // Persist every backend's field so switching the dropdown after a
    // save still surfaces the value the user typed for that backend.
    // Also mirror the active backend's value into the legacy `image`
    // slot so consumers that haven't migrated still resolve correctly.
    const activeImage =
      sandboxBackend === "e2b"
        ? sandboxE2BTemplate
        : sandboxBackend === "boxlite"
          ? sandboxBoxliteImage
          : sandboxDockerImage;
    // 0 is the wire's "unset": the runtime stamps auto-persist's cadence
    // default (5) and the learner's tool-call floor (3) at build time, so an
    // emptied box resets to the documented default instead of freezing the
    // stored number the operator just deleted.
    const cadence = Number.parseInt(autoPersistEveryNTurns, 10);
    const minToolCalls = Number.parseInt(skillsLearnerMinToolCalls, 10);
    try {
      const result = await updateConfig({
        prefs: {
          timezone: defaultTimezone.trim() || undefined,
        },
        memory: {
          autoPersist: {
            enabled: autoPersistEnabled,
            everyNTurns: Number.isFinite(cadence) && cadence > 0 ? cadence : 0,
            // '' means "use the agent's own model" and it has to travel: the pod MERGES the patch,
            // so an omitted key would keep whatever model was set before and this form could never
            // clear one (fastagent internal/setup/handlers.go, handleUpdateConfig).
            model: autoPersistModel.trim(),
          },
        },
        skillsLearner: {
          enabled: skillsLearnerEnabled,
          minToolCalls:
            Number.isFinite(minToolCalls) && minToolCalls > 0 ? minToolCalls : 0,
          // Same rule as the distiller's model above: '' clears, omitting cannot.
          model: skillsLearnerModel.trim(),
        },
        privacy: {
          piiScrubbing: {
            enabled: piiScrubbingEnabled,
          },
        },
        sandbox: {
          enabled: sandboxEnabled,
          backend: sandboxBackend,
          image: activeImage || undefined,
          dockerImage: sandboxDockerImage || undefined,
          e2bTemplate: sandboxE2BTemplate || undefined,
          boxliteSnapshot: sandboxBoxliteImage || undefined,
          e2bKey: sandboxE2BKey || undefined,
          boxliteKey: sandboxBoxliteKey || undefined,
          boxliteUrl: sandboxBoxliteURL || undefined,
        },
      });
      if (result?.ok === false) {
        setSaveError(result.error || "Save failed");
        return;
      }
    } catch (err) {
      setSaveError(err instanceof Error ? err.message : "Save failed");
      return;
    } finally {
      setSaving(false);
    }
    setSaved(true);
    setTimeout(() => setSaved(false), 2000);
  };

  if (loading) {
    return (
      <div className="space-y-6">
        <Skeleton className="h-10 w-48" />
        <Skeleton className="h-64 w-full" />
      </div>
    );
  }
  if (!config) return null;

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h3 className="text-xl font-semibold tracking-tight">Runtime</h3>
          <p className="text-sm text-muted-foreground mt-1">
            Gateway, sandbox, memory and learning configuration. Saved at
            system scope, so a save reaches every agent that has not overridden
            the setting.
          </p>
        </div>
        <Button
          onClick={handleSave}
          disabled={saving}
          variant={saved ? "outline" : "default"}
          className={saved ? "border-emerald-500/30 text-emerald-600 dark:text-emerald-400" : ""}
        >
          {saved ? (
            <>
              <Check className="h-4 w-4 mr-2" />
              Saved
            </>
          ) : (
            <>
              <Save className="h-4 w-4 mr-2" />
              {saving ? "Saving..." : "Save"}
            </>
          )}
        </Button>
      </div>
      {saveError && (
        <div className="rounded-md border border-destructive/30 bg-destructive/10 px-3 py-2 text-sm text-destructive">
          {saveError}
        </div>
      )}

      <div className="rounded-lg border border-border bg-card">
        <div className="p-5">
          <div className="flex items-start gap-3">
            <Clock className="mt-0.5 h-4 w-4 text-sky-500" />
            <div className="grid flex-1 gap-4 sm:grid-cols-[1fr_260px] sm:items-start">
              <div>
                <h3 className="font-medium">Default timezone</h3>
                <p className="mt-1 text-sm text-muted-foreground">
                  System preference used before falling back to the deployment
                  TZ. Current deployment fallback: {config.meta?.serverTimezone || "Local"}.
                </p>
              </div>
              <div className="space-y-2">
                <Label htmlFor="default-timezone">IANA timezone</Label>
                <Input
                  id="default-timezone"
                  value={defaultTimezone}
                  onChange={(e) => setDefaultTimezone(e.target.value)}
                  placeholder="Asia/Shanghai"
                  className="font-mono text-sm"
                />
              </div>
            </div>
          </div>
        </div>
        <Separator />
        <div className="p-5">
          <div className="flex items-center justify-between">
            <div>
              <div className="flex items-center gap-2 mb-1">
                <Container className="h-4 w-4 text-purple-500" />
                <h3 className="font-medium">Sandbox</h3>
              </div>
              <p className="text-sm text-muted-foreground">
                Execute code in isolated sandbox environments
              </p>
            </div>
            <Switch checked={sandboxEnabled} onCheckedChange={setSandboxEnabled} />
          </div>
        </div>
        {sandboxEnabled && (
          <div className="px-5 pb-5 space-y-4">
            <Separator />
            <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
              <div className="space-y-2">
                <Label>Backend</Label>
                <Select value={sandboxBackend} onValueChange={(v) => v && setSandboxBackend(v)}>
                  <SelectTrigger>
                    <SelectValue>
                      {(v: unknown) =>
                        ({ docker: "Docker", e2b: "E2B (cloud)", boxlite: "BoxLite (cloud)" } as Record<string, string>)[
                          v as string
                        ] ?? (v as string) ?? ""
                      }
                    </SelectValue>
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="docker">Docker</SelectItem>
                    <SelectItem value="e2b">E2B (cloud)</SelectItem>
                    <SelectItem value="boxlite">BoxLite (cloud)</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              {sandboxBackend === "e2b" ? (
                <>
                  <div className="space-y-2">
                    <Label>E2B API Key</Label>
                    <Input
                      type="password"
                      value={sandboxE2BKey}
                      onChange={(e) => setSandboxE2BKey(e.target.value)}
                      placeholder="e2b_..."
                      className="font-mono text-sm"
                    />
                  </div>
                  <div className="space-y-2">
                    <Label>E2B Template</Label>
                    <Input
                      value={sandboxE2BTemplate}
                      onChange={(e) => setSandboxE2BTemplate(e.target.value)}
                      placeholder="base"
                      className="font-mono text-sm"
                    />
                  </div>
                </>
              ) : sandboxBackend === "boxlite" ? (
                <>
                  <div className="space-y-2">
                    <Label>BoxLite API Key</Label>
                    <Input
                      type="password"
                      value={sandboxBoxliteKey}
                      onChange={(e) => setSandboxBoxliteKey(e.target.value)}
                      placeholder="client_secret"
                      className="font-mono text-sm"
                    />
                  </div>
                  <div className="space-y-2">
                    <Label>Snapshot</Label>
                    <Input
                      value={sandboxBoxliteImage}
                      onChange={(e) => setSandboxBoxliteImage(e.target.value)}
                      placeholder="fastagent-sandbox"
                      className="font-mono text-sm"
                    />
                    <p className="text-xs text-muted-foreground">
                      BoxLite snapshot name (imported via the BoxLite Dashboard),
                      not a Docker Hub image reference.
                    </p>
                  </div>
                  <div className="space-y-2 sm:col-span-2">
                    <Label>API URL (optional)</Label>
                    <Input
                      value={sandboxBoxliteURL}
                      onChange={(e) => setSandboxBoxliteURL(e.target.value)}
                      placeholder="https://api.dev.boxlite.ai/api/v1"
                      className="font-mono text-sm"
                    />
                  </div>
                </>
              ) : (
                <div className="space-y-2">
                  <Label>Docker Image</Label>
                  <Input
                    value={sandboxDockerImage}
                    onChange={(e) => setSandboxDockerImage(e.target.value)}
                    placeholder="thinkany/fastclaw-sandbox:latest"
                    className="font-mono text-sm"
                  />
                </div>
              )}
            </div>
          </div>
        )}
      </div>

      <div className="rounded-lg border border-border bg-card">
        <div className="p-5">
          <div className="flex items-center justify-between">
            <div>
              <div className="flex items-center gap-2 mb-1">
                <Brain className="h-4 w-4 text-amber-500" />
                <h3 className="font-medium">Memory auto-persist</h3>
              </div>
              <p className="text-sm text-muted-foreground">
                Every N chatter turns, a small model call distills the recent
                conversation into that chatter&apos;s USER.md / MEMORY.md. Applies
                to every agent that has not overridden it; the per-agent override
                is the toggle in the agent&apos;s Context settings.
              </p>
            </div>
            <Switch
              aria-label="Memory auto-persist"
              checked={autoPersistEnabled}
              onCheckedChange={setAutoPersistEnabled}
            />
          </div>
        </div>
        {autoPersistEnabled && (
          <div className="px-5 pb-5 space-y-4">
            <Separator />
            <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
              <div className="space-y-2">
                <Label htmlFor="auto-persist-cadence">Every N chatter turns</Label>
                <Input
                  id="auto-persist-cadence"
                  type="number"
                  min={1}
                  value={autoPersistEveryNTurns}
                  onChange={(e) => setAutoPersistEveryNTurns(e.target.value)}
                  placeholder="5"
                  className="font-mono text-sm"
                />
                <p className="text-xs text-muted-foreground">
                  Leave empty for the default (5).
                </p>
              </div>
              <div className="space-y-2">
                <Label htmlFor="auto-persist-model">Distill model (optional)</Label>
                <Input
                  id="auto-persist-model"
                  value={autoPersistModel}
                  onChange={(e) => setAutoPersistModel(e.target.value)}
                  placeholder="provider/model"
                  className="font-mono text-sm"
                />
                <p className="text-xs text-muted-foreground">
                  Leave empty to use each agent&apos;s own model.
                </p>
              </div>
            </div>
          </div>
        )}
        <Separator />
        <div className="p-5">
          <div className="flex items-center justify-between">
            <div>
              <div className="flex items-center gap-2 mb-1">
                <GraduationCap className="h-4 w-4 text-emerald-500" />
                <h3 className="font-medium">Skills learner</h3>
              </div>
              <p className="text-sm text-muted-foreground">
                After a turn that used enough tools, a background pass writes the
                procedure it worked out as a skill (SKILL.md) the agent can reuse.
                Learned skills land in the agent owner&apos;s skills folder.
              </p>
            </div>
            <Switch
              aria-label="Skills learner"
              checked={skillsLearnerEnabled}
              onCheckedChange={setSkillsLearnerEnabled}
            />
          </div>
        </div>
        {skillsLearnerEnabled && (
          <div className="px-5 pb-5 space-y-4">
            <Separator />
            <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
              <div className="space-y-2">
                <Label htmlFor="skills-learner-min-tools">Minimum tool calls</Label>
                <Input
                  id="skills-learner-min-tools"
                  type="number"
                  min={1}
                  value={skillsLearnerMinToolCalls}
                  onChange={(e) => setSkillsLearnerMinToolCalls(e.target.value)}
                  placeholder="3"
                  className="font-mono text-sm"
                />
                <p className="text-xs text-muted-foreground">
                  Leave empty for the default (3).
                </p>
              </div>
              <div className="space-y-2">
                <Label htmlFor="skills-learner-model">Extraction model (optional)</Label>
                <Input
                  id="skills-learner-model"
                  value={skillsLearnerModel}
                  onChange={(e) => setSkillsLearnerModel(e.target.value)}
                  placeholder="provider/model"
                  className="font-mono text-sm"
                />
                <p className="text-xs text-muted-foreground">
                  Leave empty to use each agent&apos;s own model.
                </p>
              </div>
            </div>
          </div>
        )}
      </div>

      <div className="rounded-lg border border-border bg-card">
        <div className="p-5">
          <div className="flex items-center justify-between">
            <div>
              <div className="flex items-center gap-2 mb-1">
                <ShieldCheck className="h-4 w-4 text-rose-500" />
                <h3 className="font-medium">Redact PII before the model</h3>
              </div>
              <p className="text-sm text-muted-foreground">
                Emails, phone numbers, card numbers, SSNs, API keys, tokens, IPs and
                private keys are replaced with placeholders in everything an agent
                sends to a provider — the turn, the streaming turn, delegated
                sub-agents, compaction and the memory extractor all pass through the
                same provider, so the rule is installed once.
              </p>
              <p className="text-xs text-muted-foreground mt-1">
                Known limit: the model&apos;s own earlier reply is replayed to it
                byte-for-byte (prompt-cache identity), so a PII echo inside that reply
                still travels. The session itself keeps the original text; only what
                leaves is rewritten.
              </p>
            </div>
            <Switch
              aria-label="Redact PII before the model"
              checked={piiScrubbingEnabled}
              onCheckedChange={setPiiScrubbingEnabled}
            />
          </div>
        </div>
      </div>
    </div>
  );
}
