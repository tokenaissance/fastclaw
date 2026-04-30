"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { Badge } from "@/components/ui/badge";
import { Separator } from "@/components/ui/separator";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { getStatus, type StatusResponse } from "@/lib/api";
import {
  Activity,
  Bot,
  Radio,
  Server,
  Brain,
  RefreshCw,
  ArrowRight,
  Settings,
} from "lucide-react";
import { Button } from "@/components/ui/button";

export default function OverviewPage() {
  const [status, setStatus] = useState<StatusResponse | null>(null);
  const [loading, setLoading] = useState(true);

  const fetchStatus = () => {
    setLoading(true);
    getStatus()
      .then(setStatus)
      .catch(() => setStatus(null))
      .finally(() => setLoading(false));
  };

  useEffect(() => {
    fetchStatus();
    const interval = setInterval(fetchStatus, 10000);
    return () => clearInterval(interval);
  }, []);

  if (loading && !status) {
    return (
      <div className="flex h-full items-center justify-center">
        <div className="h-8 w-8 animate-spin rounded-full border-2 border-muted border-t-primary" />
      </div>
    );
  }

  // Hide empty / not-yet-connected sections so the dashboard reflects
  // what's actually configured: Channels stat when none connected,
  // Model column when no agent reports a model.
  const channelCount = status?.channels?.length || 0;
  const showChannels = channelCount > 0;
  const showAgentModel = (status?.agents || []).some((a) => a.model);

  return (
    <div className="p-6 space-y-6 max-w-5xl mx-auto">
      {/* Header */}
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">Dashboard</h2>
          <p className="text-sm text-muted-foreground mt-1">
            Monitor your FastClaw gateway
          </p>
        </div>
        <Button
          variant="outline"
          size="sm"
          onClick={fetchStatus}
        >
          <RefreshCw
            className={`h-4 w-4 mr-2 ${loading ? "animate-spin" : ""}`}
          />
          Refresh
        </Button>
      </div>

      {/* Stats Cards — Channels card collapses out when nothing is
          connected so the dashboard doesn't advertise an empty integration
          on a fresh install. */}
      <div
        className={`grid gap-4 grid-cols-2 ${showChannels ? "md:grid-cols-4" : "md:grid-cols-3"}`}
      >
        {/* Status */}
        <div className="rounded-lg border border-border bg-card p-5">
          <div className="flex items-center justify-between mb-3">
            <span className="text-sm text-muted-foreground">Status</span>
            <div className="flex h-8 w-8 items-center justify-center rounded-full bg-emerald-500/10">
              <Activity className="h-4 w-4 text-emerald-500" />
            </div>
          </div>
          <div className="flex items-center gap-2">
            <Badge
              variant={status?.running ? "default" : "secondary"}
              className={
                status?.running
                  ? "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400 border-emerald-500/20 hover:bg-emerald-500/15"
                  : ""
              }
            >
              <span
                className={`mr-1.5 inline-block h-1.5 w-1.5 rounded-full ${
                  status?.running ? "bg-emerald-500" : "bg-muted-foreground"
                }`}
              />
              {status?.running ? "Running" : "Stopped"}
            </Badge>
          </div>
          {status?.uptime && (
            <p className="text-xs text-muted-foreground mt-2">
              Uptime: {status.uptime}
            </p>
          )}
        </div>

        {/* Agents */}
        <div className="rounded-lg border border-border bg-card p-5">
          <div className="flex items-center justify-between mb-3">
            <span className="text-sm text-muted-foreground">Agents</span>
            <div className="flex h-8 w-8 items-center justify-center rounded-full bg-violet-500/10">
              <Bot className="h-4 w-4 text-violet-500" />
            </div>
          </div>
          <p className="text-3xl font-semibold tracking-tight">
            {status?.agents?.length || 0}
          </p>
          <p className="text-xs text-muted-foreground mt-1">Active agents</p>
        </div>

        {/* Channels */}
        {showChannels && (
          <div className="rounded-lg border border-border bg-card p-5">
            <div className="flex items-center justify-between mb-3">
              <span className="text-sm text-muted-foreground">Channels</span>
              <div className="flex h-8 w-8 items-center justify-center rounded-full bg-blue-500/10">
                <Radio className="h-4 w-4 text-blue-500" />
              </div>
            </div>
            <p className="text-3xl font-semibold tracking-tight">{channelCount}</p>
            <p className="text-xs text-muted-foreground mt-1">Connected</p>
          </div>
        )}

        {/* Port */}
        <div className="rounded-lg border border-border bg-card p-5">
          <div className="flex items-center justify-between mb-3">
            <span className="text-sm text-muted-foreground">Port</span>
            <div className="flex h-8 w-8 items-center justify-center rounded-full bg-amber-500/10">
              <Server className="h-4 w-4 text-amber-500" />
            </div>
          </div>
          <p className="text-3xl font-semibold tracking-tight font-mono">
            {status?.port || "\u2014"}
          </p>
          <p className="text-xs text-muted-foreground mt-1">Gateway port</p>
        </div>
      </div>

      {/* Quick Actions — chat lives per-agent now (under /agents/<id>/chat),
          so the standalone /chat shortcut is gone. */}
      <div className="grid gap-3 md:grid-cols-2">
        <Link href="/agents/">
          <div className="group flex items-center gap-4 rounded-lg border border-border bg-card p-4 transition-colors hover:bg-muted/50">
            <div className="flex h-10 w-10 items-center justify-center rounded-lg bg-blue-500/10 transition-colors group-hover:bg-blue-500/15">
              <Bot className="h-5 w-5 text-blue-500" />
            </div>
            <div className="flex-1 min-w-0">
              <p className="text-sm font-medium">Agents</p>
              <p className="text-xs text-muted-foreground">
                Manage agent configs
              </p>
            </div>
            <ArrowRight className="h-4 w-4 text-muted-foreground/50 group-hover:text-muted-foreground transition-colors" />
          </div>
        </Link>

        <Link href="/settings/">
          <div className="group flex items-center gap-4 rounded-lg border border-border bg-card p-4 transition-colors hover:bg-muted/50">
            <div className="flex h-10 w-10 items-center justify-center rounded-lg bg-amber-500/10 transition-colors group-hover:bg-amber-500/15">
              <Settings className="h-5 w-5 text-amber-500" />
            </div>
            <div className="flex-1 min-w-0">
              <p className="text-sm font-medium">Settings</p>
              <p className="text-xs text-muted-foreground">
                Gateway configuration
              </p>
            </div>
            <ArrowRight className="h-4 w-4 text-muted-foreground/50 group-hover:text-muted-foreground transition-colors" />
          </div>
        </Link>
      </div>

      {/* Agents & Provider */}
      <div className="grid gap-4 md:grid-cols-2">
        {/* Agents */}
        <div className="rounded-lg border border-border bg-card">
          <div className="p-5 pb-3">
            <div className="flex items-center gap-2 mb-1">
              <Bot className="h-4 w-4 text-primary" />
              <h3 className="font-medium">Agents</h3>
            </div>
            <p className="text-sm text-muted-foreground">
              Configured AI agents
            </p>
          </div>
          <div className="px-2 pb-2 overflow-x-auto">
            {status?.agents && status.agents.length > 0 ? (
              <Table>
                <TableHeader>
                  <TableRow className="hover:bg-transparent">
                    <TableHead className="text-muted-foreground h-9">Name</TableHead>
                    {showAgentModel && (
                      <TableHead className="text-muted-foreground h-9">Model</TableHead>
                    )}
                    <TableHead className="text-muted-foreground h-9 text-right">Status</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {status.agents.map((agent) => (
                    <TableRow
                      key={agent.id}
                      className="hover:bg-muted/50 transition-colors"
                    >
                      <TableCell className="font-medium py-2.5">
                        {agent.name || agent.id}
                      </TableCell>
                      {showAgentModel && (
                        <TableCell className="py-2.5">
                          {agent.model ? (
                            <code className="bg-muted px-2 py-0.5 rounded font-mono text-xs">
                              {agent.model}
                            </code>
                          ) : (
                            <span className="text-xs text-muted-foreground">—</span>
                          )}
                        </TableCell>
                      )}
                      <TableCell className="text-right py-2.5">
                        <Badge
                          variant="outline"
                          className="bg-emerald-500/10 text-emerald-600 dark:text-emerald-400 border-emerald-500/20"
                        >
                          Active
                        </Badge>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            ) : (
              <p className="text-sm text-muted-foreground px-3 pb-3">No agents configured</p>
            )}
          </div>
        </div>

        {/* Provider */}
        <div className="rounded-lg border border-border bg-card">
          <div className="p-5 pb-3">
            <div className="flex items-center gap-2 mb-1">
              <Brain className="h-4 w-4 text-amber-500" />
              <h3 className="font-medium">Provider</h3>
            </div>
            <p className="text-sm text-muted-foreground">
              LLM provider configuration
            </p>
          </div>
          <div className="px-5 pb-5">
            {status?.provider ? (
              <div className="space-y-3">
                <div className="flex items-center justify-between">
                  <span className="text-sm text-muted-foreground">Provider</span>
                  <span className="text-sm capitalize">
                    {status.provider.name || "default"}
                  </span>
                </div>
                <Separator />
                <div className="flex items-center justify-between">
                  <span className="text-sm text-muted-foreground">Model</span>
                  <code className="text-sm font-mono bg-muted px-2 py-0.5 rounded">
                    {status.provider.model}
                  </code>
                </div>
                <Separator />
                <div className="flex items-center justify-between">
                  <span className="text-sm text-muted-foreground">API Base</span>
                  <span className="text-sm font-mono truncate max-w-48">
                    {status.provider.apiBase}
                  </span>
                </div>
                <Separator />
                <div className="flex items-center justify-between">
                  <span className="text-sm text-muted-foreground">API Key</span>
                  <span className="text-sm font-mono">
                    {status.provider.apiKey}
                  </span>
                </div>
              </div>
            ) : (
              <p className="text-sm text-muted-foreground">No provider configured</p>
            )}
          </div>
        </div>
      </div>
    </div>
  );
}
