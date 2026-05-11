import { SidebarLayout } from "@/components/sidebar";

export default function AgentsLayout({ children }: { children: React.ReactNode }) {
  return <SidebarLayout>{children}</SidebarLayout>;
}
