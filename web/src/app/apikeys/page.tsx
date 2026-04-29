"use client";

import { useEffect, useState } from "react";
import {
  listApikeys,
  createApikey,
  deleteApikey,
  rotateApikey,
  setApikeyAgents,
  apiFetch,
} from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent } from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { KeyRound, RotateCw, Trash2, Copy, Check, Plus } from "lucide-react";

interface ApiKey {
  id: string;
  userId: string;
  name?: string;
  key: string;
  agents: string[];
  createdAt: string;
}

interface AgentMeta {
  id: string;
  name: string;
}

export default function ApikeysPage() {
  const [keys, setKeys] = useState<ApiKey[]>([]);
  const [agents, setAgents] = useState<AgentMeta[]>([]);
  const [error, setError] = useState("");
  const [createName, setCreateName] = useState("");
  const [createAgents, setCreateAgents] = useState<string[]>([]);
  const [showToken, setShowToken] = useState<{ id: string; token: string } | null>(null);
  const [copied, setCopied] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<ApiKey | null>(null);
  const [createOpen, setCreateOpen] = useState(false);

  async function refresh() {
    setError("");
    const r = await listApikeys();
    if (r.apikeys) setKeys(r.apikeys);
    if (r.error) setError(r.error);
    const a = await apiFetch("/api/agents");
    const aj = await a.json();
    if (aj.agents) setAgents(aj.agents);
  }
  useEffect(() => {
    refresh();
  }, []);

  async function handleCreate(e: React.FormEvent) {
    e.preventDefault();
    setError("");
    if (!createName.trim()) return;
    const res = await createApikey({ name: createName.trim(), agentIds: createAgents });
    if (res.error) {
      setError(res.error);
      return;
    }
    if (res.token) setShowToken({ id: res.apikey.id, token: res.token });
    setCreateName("");
    setCreateAgents([]);
    setCreateOpen(false);
    refresh();
  }

  async function handleDelete(row: ApiKey) {
    const res = await deleteApikey(row.id);
    if (res.error) setError(res.error);
    setDeleteTarget(null);
    refresh();
  }

  async function handleRotate(id: string) {
    const res = await rotateApikey(id);
    if (res.error) {
      setError(res.error);
      return;
    }
    if (res.token) setShowToken({ id, token: res.token });
    refresh();
  }

  async function handleSetAgents(id: string, agentIds: string[]) {
    const res = await setApikeyAgents(id, agentIds);
    if (res.error) setError(res.error);
    refresh();
  }

  async function copyToken() {
    if (!showToken) return;
    await navigator.clipboard.writeText(showToken.token);
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  }

  function openCreateDialog() {
    setCreateName("");
    setCreateAgents([]);
    setError("");
    setCreateOpen(true);
  }

  return (
    <div className="p-6 space-y-6 max-w-5xl mx-auto">
      <div className="flex items-center justify-between">
        <div>
          <div className="flex items-center gap-2">
            <KeyRound className="size-5 text-muted-foreground" />
            <h2 className="text-2xl font-semibold tracking-tight">API Keys</h2>
          </div>
          <p className="text-sm text-muted-foreground mt-1">
            Issue programmatic credentials. Each key is bound to a subset of your agents — the bearer can only call
            <code className="mx-1 rounded bg-muted px-1.5 py-0.5 text-xs">/v1/chat/completions</code>
            for those agents.
          </p>
        </div>
        <Button variant="outline" onClick={openCreateDialog}>
          <Plus className="h-4 w-4 mr-2" />
          Add API Key
        </Button>
      </div>

      {showToken && (
        <Card className="border-amber-500/40 bg-amber-500/5">
          <CardContent className="space-y-3 pt-6">
            <p className="text-sm font-medium">Token issued — copy it now, you won&apos;t see it again.</p>
            <div className="flex items-center gap-2">
              <code className="flex-1 break-all rounded border bg-background px-3 py-2 font-mono text-xs">
                {showToken.token}
              </code>
              <Button size="sm" variant="outline" onClick={copyToken}>
                {copied ? <Check className="size-4" /> : <Copy className="size-4" />}
              </Button>
            </div>
            <Button size="sm" variant="ghost" onClick={() => setShowToken(null)}>
              Got it
            </Button>
          </CardContent>
        </Card>
      )}

      {error && (
        <Card className="border-destructive/40 bg-destructive/5">
          <CardContent className="pt-6">
            <p className="text-sm text-destructive">{error}</p>
          </CardContent>
        </Card>
      )}

      {keys.length === 0 ? (
        <div className="rounded-lg border border-border bg-card">
          <div className="flex flex-col items-center justify-center py-16">
            <div className="flex h-14 w-14 items-center justify-center rounded-2xl bg-primary/10 mb-4">
              <KeyRound className="h-7 w-7 text-primary" />
            </div>
            <p className="text-sm text-muted-foreground mb-1">No API keys yet</p>
            <p className="text-xs text-muted-foreground/60 mb-4">
              Issue one to let an external client call your agents
            </p>
            <Button variant="outline" size="sm" onClick={openCreateDialog}>
              <Plus className="h-4 w-4 mr-2" />
              Add API Key
            </Button>
          </div>
        </div>
      ) : (
        <div className="rounded-lg border border-border bg-card overflow-hidden">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Key</TableHead>
                <TableHead>Agents</TableHead>
                <TableHead>Created</TableHead>
                <TableHead className="text-right">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {keys.map((k) => (
                <TableRow key={k.id}>
                  <TableCell className="font-medium">{k.name || k.id}</TableCell>
                  <TableCell>
                    <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs">{k.key}</code>
                  </TableCell>
                  <TableCell>
                    <div className="flex flex-wrap gap-1.5">
                      {agents.length === 0 && (
                        <span className="text-xs text-muted-foreground">—</span>
                      )}
                      {agents.map((a) => {
                        const active = (k.agents || []).includes(a.id);
                        return (
                          <button
                            key={a.id}
                            type="button"
                            onClick={() => {
                              const next = active
                                ? (k.agents || []).filter((x) => x !== a.id)
                                : [...(k.agents || []), a.id];
                              handleSetAgents(k.id, next);
                            }}
                            className="cursor-pointer"
                          >
                            <Badge variant={active ? "default" : "outline"} className="text-xs">
                              {a.name || a.id}
                            </Badge>
                          </button>
                        );
                      })}
                    </div>
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {new Date(k.createdAt).toLocaleString()}
                  </TableCell>
                  <TableCell className="text-right">
                    <div className="flex justify-end gap-1">
                      <Button size="icon" variant="ghost" onClick={() => handleRotate(k.id)} title="Rotate">
                        <RotateCw className="size-4" />
                      </Button>
                      <Button
                        size="icon"
                        variant="ghost"
                        className="text-destructive hover:text-destructive"
                        onClick={() => setDeleteTarget(k)}
                        title="Delete"
                      >
                        <Trash2 className="size-4" />
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}

      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>Add API Key</DialogTitle>
            <DialogDescription>
              Issue a new bearer token scoped to a subset of your agents.
            </DialogDescription>
          </DialogHeader>
          <form onSubmit={handleCreate} className="space-y-4 py-2">
            <div className="space-y-1.5">
              <Label htmlFor="key-name">Name</Label>
              <Input
                id="key-name"
                value={createName}
                onChange={(e) => setCreateName(e.target.value)}
                placeholder="e.g. production"
                autoFocus
              />
            </div>
            <div className="space-y-1.5">
              <Label>Allowed agents</Label>
              {agents.length === 0 ? (
                <p className="text-xs text-muted-foreground">
                  No agents yet — create one from the Agents page first.
                </p>
              ) : (
                <div className="flex flex-wrap gap-2">
                  {agents.map((a) => {
                    const active = createAgents.includes(a.id);
                    return (
                      <button
                        key={a.id}
                        type="button"
                        onClick={() =>
                          setCreateAgents((l) =>
                            l.includes(a.id) ? l.filter((x) => x !== a.id) : [...l, a.id],
                          )
                        }
                        className={
                          "rounded-md border px-2.5 py-1 text-xs transition " +
                          (active
                            ? "border-primary bg-primary/10 text-primary"
                            : "border-border hover:bg-muted")
                        }
                      >
                        {a.name || a.id}
                      </button>
                    );
                  })}
                </div>
              )}
            </div>
            <DialogFooter>
              <Button type="button" variant="outline" onClick={() => setCreateOpen(false)}>
                Cancel
              </Button>
              <Button type="submit" disabled={!createName.trim()}>
                Create key
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      <AlertDialog open={deleteTarget !== null} onOpenChange={(o) => !o && setDeleteTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete API key?</AlertDialogTitle>
            <AlertDialogDescription>
              <code className="rounded bg-muted px-1.5 py-0.5 text-xs">{deleteTarget?.name || deleteTarget?.id}</code>{" "}
              will stop working immediately for any client using it.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={() => deleteTarget && handleDelete(deleteTarget)}>Delete</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
