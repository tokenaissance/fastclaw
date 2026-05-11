"use client";

import { SidebarLayout } from "@/components/sidebar";

export default function OverviewLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return <SidebarLayout>{children}</SidebarLayout>;
}
