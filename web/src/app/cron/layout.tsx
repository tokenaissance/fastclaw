import { SidebarLayout } from "@/components/sidebar";

export default function CronLayout({ children }: { children: React.ReactNode }) {
  return <SidebarLayout>{children}</SidebarLayout>;
}
