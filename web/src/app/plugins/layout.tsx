import { SidebarLayout } from "@/components/sidebar";

export default function PluginsLayout({ children }: { children: React.ReactNode }) {
  return <SidebarLayout>{children}</SidebarLayout>;
}
