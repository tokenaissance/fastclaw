"use client";

import * as React from "react";
import { usePathname } from "next/navigation";
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
  GlobeIcon,
  LayoutDashboardIcon,
  PlusIcon,
  SparklesIcon,
  UsersIcon,
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
  // Platform layer: shared SOUL/IDENTITY/TOOLS that every per-user agent
  // inherits when its own row is empty (#3). The link reuses the standard
  // agent customize page since the system-files API accepts any agent_id;
  // admin always has access via canAccessAgent.
  { title: "Platform", url: "/agents/__platform__/customize/", icon: GlobeIcon },
  { title: "Models", url: "/models/", icon: BrainIcon },
  { title: "Skills", url: "/skills/", icon: SparklesIcon },
  // Cloud-mode user registry. Hidden in 4812569 to declutter local-only
  // installs; restoring now that #12 makes it the canonical way to mint
  // per-user tokens (instead of hand-editing users.json).
  { title: "Users", url: "/users/", icon: UsersIcon },
];

const AGENT_NAV = (agentId: string): NavItem[] => [
  { title: "New chat", url: `/agents/${agentId}/chat/`, icon: PlusIcon },
  { title: "Customize", url: `/agents/${agentId}/customize/`, icon: Wand2Icon },
  { title: "Model", url: `/agents/${agentId}/models/`, icon: BrainIcon },
];

export function AppSidebar(props: React.ComponentProps<typeof Sidebar>) {
  const pathname = usePathname();
  const activeAgentId = extractAgentId(pathname);

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
          <NavMain label="Agent" items={AGENT_NAV(activeAgentId)} />
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
