import { SidebarLayout } from "@/components/sidebar";

export default function ModelsLayout({ children }: { children: React.ReactNode }) {
  return <SidebarLayout>{children}</SidebarLayout>;
}
