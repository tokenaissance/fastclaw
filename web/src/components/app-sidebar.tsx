"use client";

import * as React from "react";
import { usePathname, useSearchParams } from "next/navigation";
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarHeader,
  SidebarRail,
} from "@/components/ui/sidebar";
import { AgentSwitcher, AgentSwitcherItem } from "@/components/team-switcher";
import { NavMain, NavItem } from "@/components/nav-main";
import { NavSessions, SessionItem } from "@/components/nav-projects";
import { NavUser } from "@/components/nav-user";
import {
  BotIcon,
  BrainIcon,
  KeyRoundIcon,
  LayoutDashboardIcon,
  PlusIcon,
  SettingsIcon,
  SparklesIcon,
  Wand2Icon,
} from "lucide-react";
import {
  getAgents,
  getChatSessions,
  getStatus,
  type StatusResponse,
} from "@/lib/api";

// Extract agent ID from pathname like /agents/default/chat/. The second
// capture is an explicit allow-list of sub-routes so the bare /agents/
// index keeps the Platform nav instead of flipping to Agent nav.
function extractAgentId(pathname: string): string | null {
  const match = pathname.match(
    /^\/agents\/([^/]+)\/(chat|customize|skills|models|sessions)/,
  );
  return match ? match[1] : null;
}

const PLATFORM_NAV: NavItem[] = [
  { title: "Overview", url: "/overview/", icon: LayoutDashboardIcon },
  { title: "Agents", url: "/agents/", icon: BotIcon },
  { title: "Models", url: "/models/", icon: BrainIcon },
  { title: "Skills", url: "/skills/", icon: SparklesIcon },
  { title: "API Keys", url: "/apikeys/", icon: KeyRoundIcon },
  { title: "Settings", url: "/settings/", icon: SettingsIcon },
];

// "New chat" is active iff we're on the chat route AND no session is
// open. Two corrections vs. the default prefix matching:
//   1. ?session=… on /chat/ → suppress (otherwise New chat lights up
//      while a specific session is open).
//   2. /customize/ and /skills/ → suppress (`!hasSession` alone made
//      New chat light up on every sibling agent page since pathname
//      didn't match anyway).
const AGENT_NAV = (
  agentId: string,
  pathname: string,
  hasSession: boolean,
): NavItem[] => {
  const onChatRoute = pathname.startsWith(`/agents/${agentId}/chat`);
  return [
    {
      title: "New chat",
      url: `/agents/${agentId}/chat/`,
      icon: PlusIcon,
      active: onChatRoute && !hasSession,
    },
    { title: "Customize", url: `/agents/${agentId}/customize/`, icon: Wand2Icon },
    { title: "Skills", url: `/agents/${agentId}/skills/`, icon: SparklesIcon },
  ];
};

export function AppSidebar(props: React.ComponentProps<typeof Sidebar>) {
  const pathname = usePathname();
  const searchParams = useSearchParams();
  const activeAgentId = extractAgentId(pathname);
  const hasOpenSession = !!searchParams?.get("session");

  const [status, setStatus] = React.useState<StatusResponse | null>(null);
  const [agents, setAgents] = React.useState<AgentSwitcherItem[]>([]);
  const [sessions, setSessions] = React.useState<SessionItem[]>([]);

  // Keep status polling so the online dot / admin flag stay fresh.
  React.useEffect(() => {
    getStatus().then(setStatus).catch(() => {});
    const iv = setInterval(() => {
      getStatus().then(setStatus).catch(() => {});
    }, 15000);
    return () => clearInterval(iv);
  }, []);

  // Agent list drives the switcher dropdown at the top of the sidebar.
  React.useEffect(() => {
    getAgents()
      .then((list) =>
        setAgents(list.map((a) => ({ id: a.id, model: a.model }))),
      )
      .catch(() => {});
  }, []);

  // Sessions only matter while a specific agent is selected. We re-run
  // whenever the active agent changes *or* the chat page broadcasts a
  // `fastclaw:sessions-changed` event (e.g. after rename / new chat) so
  // the sidebar title list stays in sync without a page refresh.
  React.useEffect(() => {
    if (!activeAgentId) {
      setSessions([]);
      return;
    }
    const refetch = () => {
      getChatSessions(activeAgentId)
        .then((list) =>
          setSessions(
            list.map((s) => ({
              id: s.id,
              title: s.title || s.preview || s.id,
              thumbnailUrl: s.thumbnailUrl,
            })),
          ),
        )
        .catch(() => {});
    };
    refetch();
    const onChange = (e: Event) => {
      const detail = (e as CustomEvent<{ agentId?: string }>).detail;
      if (!detail || !detail.agentId || detail.agentId === activeAgentId) {
        refetch();
      }
    };
    window.addEventListener("fastclaw:sessions-changed", onChange);
    return () => {
      window.removeEventListener("fastclaw:sessions-changed", onChange);
    };
  }, [activeAgentId]);

  const isAdmin = status?.isAdmin ?? false;
  const platformItems = PLATFORM_NAV;

  return (
    <Sidebar collapsible="icon" {...props}>
      <SidebarHeader>
        <AgentSwitcher agents={agents} activeAgentId={activeAgentId} />
      </SidebarHeader>
      <SidebarContent>
        {activeAgentId ? (
          <NavMain label="Agent" items={AGENT_NAV(activeAgentId, pathname, hasOpenSession)} />
        ) : (
          <NavMain label="Platform" items={platformItems} />
        )}
        <NavSessions agentId={activeAgentId} sessions={sessions} />
      </SidebarContent>
      <SidebarFooter>
        <NavUser
          name={isAdmin ? "Admin" : "User"}
          subtitle={status?.running ? "Gateway running" : "Gateway stopped"}
        />
      </SidebarFooter>
      <SidebarRail />
    </Sidebar>
  );
}
