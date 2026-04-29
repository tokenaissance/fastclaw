"use client";

import * as React from "react";
import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import {
  SidebarGroup,
  SidebarGroupLabel,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
} from "@/components/ui/sidebar";
import type { LucideIcon } from "lucide-react";

export interface NavItem {
  title: string;
  url: string;
  icon: LucideIcon;
  // active overrides the default pathname-based prefix match. Use this
  // when two items share the same pathname and only differ in query
  // (e.g. "New chat" vs an open session under /agents/<id>/chat/, where
  // the prefix rule would highlight both).
  active?: boolean;
}

function isActive(pathname: string, href: string) {
  const norm = (s: string) => s.replace(/\/$/, "");
  return norm(pathname) === norm(href) || norm(pathname).startsWith(norm(href) + "/");
}

export function NavMain({
  label = "Platform",
  items,
}: {
  label?: string;
  items: NavItem[];
}) {
  const pathname = usePathname();
  const router = useRouter();

  // Prefetch target routes on idle so soft nav is ready when the user
  // clicks — mirrors what <Link> does automatically, but we're opting out
  // of Link below to guarantee client-side nav.
  React.useEffect(() => {
    items.forEach((item) => router.prefetch(item.url));
  }, [items, router]);

  // The Base UI SidebarMenuButton `render` prop merges through
  // React.cloneElement, which intermittently dropped Next <Link>'s
  // internal click handler (every click became a full page reload →
  // visible sidebar flicker). A plain <button> + programmatic
  // router.push gives a guaranteed client-side transition.
  return (
    <SidebarGroup>
      <SidebarGroupLabel>{label}</SidebarGroupLabel>
      <SidebarMenu>
        {items.map((item) => {
          const active = item.active ?? isActive(pathname, item.url);
          return (
            <SidebarMenuItem key={item.url}>
              <SidebarMenuButton
                isActive={active}
                tooltip={item.title}
                onClick={() => router.push(item.url)}
                onMouseEnter={() => router.prefetch(item.url)}
              >
                <item.icon />
                <span>{item.title}</span>
              </SidebarMenuButton>
            </SidebarMenuItem>
          );
        })}
      </SidebarMenu>
    </SidebarGroup>
  );
}

// Exported for pages that want a real anchor with Next client-nav
// without the sidebar button chrome.
export { Link as NavLink };
