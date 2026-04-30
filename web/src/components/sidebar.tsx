"use client";

import * as React from "react";
import { AppSidebar } from "@/components/app-sidebar";
import {
  SidebarInset,
  SidebarProvider,
  SidebarTrigger,
} from "@/components/ui/sidebar";
// Page-header slot: pages call `usePageHeader(<jsx/>)` to render content
// to the right of the sidebar-trigger in the global sticky header. When
// the page unmounts the slot empties. Chat uses this to show an editable
// session title next to the sidebar toggle.
interface PageHeaderContextValue {
  setNode: (node: React.ReactNode) => void;
}
const PageHeaderContext = React.createContext<PageHeaderContextValue | null>(
  null,
);

export function usePageHeader(node: React.ReactNode, deps: React.DependencyList = []) {
  const ctx = React.useContext(PageHeaderContext);
  React.useEffect(() => {
    if (!ctx) return;
    ctx.setNode(node);
    return () => ctx.setNode(null);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);
}

export function SidebarLayout({ children }: { children: React.ReactNode }) {
  const [headerNode, setHeaderNode] = React.useState<React.ReactNode>(null);

  const headerCtx = React.useMemo<PageHeaderContextValue>(
    () => ({ setNode: setHeaderNode }),
    [],
  );

  return (
    <PageHeaderContext.Provider value={headerCtx}>
      <SidebarProvider>
        <AppSidebar />
        <SidebarInset>
          <header className="sticky top-0 z-20 flex h-12 items-center gap-2 bg-background/80 px-3 backdrop-blur">
            <SidebarTrigger className="-ml-1" />
            {headerNode}
          </header>
          <div className="flex-1">{children}</div>
        </SidebarInset>
      </SidebarProvider>
    </PageHeaderContext.Provider>
  );
}
