import { SidebarLayout } from "@/components/sidebar";

export default function ToolsLayout({ children }: { children: React.ReactNode }) {
  return <SidebarLayout>{children}</SidebarLayout>;
}
