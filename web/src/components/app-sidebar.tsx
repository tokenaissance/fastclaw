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
  UsersIcon,
  Wand2Icon,
} from "lucide-react";
import {
  getAgent,
  getAgents,
  getChatSessions,
  getMe,
  getStatus,
  type MeResponse,
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

// Platform nav for regular users — kept minimal so non-admins only see
// what they can actually do (chat with their agents). Models / Skills /
// API Keys / Settings are admin-only platform plumbing.
const USER_NAV: NavItem[] = [
  { title: "Overview", url: "/overview/", icon: LayoutDashboardIcon },
  { title: "Agents", url: "/agents/", icon: BotIcon },
];

const ADMIN_NAV: NavItem[] = [
  { title: "Overview", url: "/overview/", icon: LayoutDashboardIcon },
  { title: "Agents", url: "/agents/", icon: BotIcon },
  { title: "Models", url: "/models/", icon: BrainIcon },
  { title: "Skills", url: "/skills/", icon: SparklesIcon },
  { title: "Users", url: "/admin/users/", icon: UsersIcon },
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
  const [me, setMe] = React.useState<MeResponse | null>(null);
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

  // Fetch current user once so the footer can show their name + role.
  React.useEffect(() => {
    getMe().then(setMe).catch(() => {});
  }, []);

  // Agent list drives the switcher dropdown at the top of the sidebar.
  React.useEffect(() => {
    getAgents()
      .then((list) =>
        setAgents(list.map((a) => ({ id: a.id, name: a.name, model: a.model }))),
      )
      .catch(() => {});
  }, []);

  // When the active agent isn't in the caller's owned list — e.g. a
  // super_admin chatting with another user's agent — fetch its name
  // separately and splice it in so the switcher header shows the real
  // name instead of falling back to "FastClaw".
  React.useEffect(() => {
    if (!activeAgentId) return;
    if (agents.some((a) => a.id === activeAgentId)) return;
    let aborted = false;
    getAgent(activeAgentId)
      .then((a) => {
        if (aborted || !a) return;
        setAgents((prev) =>
          prev.some((x) => x.id === a.id)
            ? prev
            : [...prev, { id: a.id, name: a.name, model: a.model }],
        );
      })
      .catch(() => {});
    return () => {
      aborted = true;
    };
  }, [activeAgentId, agents]);

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
  const platformItems = isAdmin ? ADMIN_NAV : USER_NAV;

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
          name={
            me?.user?.displayName ||
            me?.user?.username ||
            (isAdmin ? "Admin" : "User")
          }
          subtitle={me?.user?.role || (isAdmin ? "super_admin" : "user")}
        />
      </SidebarFooter>
      <SidebarRail />
    </Sidebar>
  );
}
