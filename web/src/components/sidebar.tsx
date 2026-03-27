"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import {
  LayoutDashboard,
  MessageSquare,
  Bot,
  Sparkles,
  Puzzle,
  Radio,
  Clock,
  Settings,
  Brain,
  Menu,
  X,
  Sun,
  Moon,
} from "lucide-react";
import { useState, useEffect } from "react";
import { useTheme } from "@/components/theme-provider";

const navItems = [
  { href: "/overview/", label: "Overview", icon: LayoutDashboard },
  { href: "/chat/", label: "Chat", icon: MessageSquare },
  { href: "/agents/", label: "Agents", icon: Bot },
  { href: "/skills/", label: "Skills", icon: Sparkles },
  { href: "/plugins/", label: "Plugins", icon: Puzzle },
  { href: "/models/", label: "Models", icon: Brain },
  { href: "/channels/", label: "Channels", icon: Radio },
  { href: "/cron/", label: "Cron Jobs", icon: Clock },
  { href: "/settings/", label: "Settings", icon: Settings },
];

export function SidebarLayout({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();
  const [mobileOpen, setMobileOpen] = useState(false);
  const { theme, toggleTheme } = useTheme();
  const [gatewayRunning, setGatewayRunning] = useState(false);

  useEffect(() => {
    fetch("/api/status")
      .then((r) => r.json())
      .then((s) => setGatewayRunning(s.running))
      .catch(() => {});
    const interval = setInterval(() => {
      fetch("/api/status")
        .then((r) => r.json())
        .then((s) => setGatewayRunning(s.running))
        .catch(() => {});
    }, 15000);
    return () => clearInterval(interval);
  }, []);

  const NavLinks = ({ onClick }: { onClick?: () => void }) => (
    <>
      {navItems.map((item) => {
        const isActive =
          pathname === item.href || pathname.startsWith(item.href);
        return (
          <Link
            key={item.href}
            href={item.href}
            onClick={onClick}
            className={`flex items-center gap-3 rounded-lg px-3 py-2 text-sm font-medium transition-colors ${
              isActive
                ? "bg-primary/10 text-primary"
                : "text-muted-foreground hover:bg-muted/50 hover:text-foreground"
            }`}
          >
            <item.icon className="h-4 w-4" />
            {item.label}
          </Link>
        );
      })}
    </>
  );

  return (
    <div className="flex min-h-screen bg-background">
      {/* Desktop sidebar */}
      <aside className="hidden w-60 flex-col border-r border-border bg-card/50 md:flex">
        {/* Logo + status */}
        <div className="flex h-14 items-center gap-2.5 border-b border-border px-4">
          <div className="relative flex h-8 w-8 items-center justify-center">
            <img src="/logo.png" alt="FastClaw" className="h-8 w-8 rounded-lg" />
            <span
              className={`absolute -bottom-0.5 -right-0.5 h-2.5 w-2.5 rounded-full border-2 border-card ${
                gatewayRunning
                  ? "bg-emerald-500 animate-pulse"
                  : "bg-muted-foreground/40"
              }`}
            />
          </div>
          <div>
            <span className="text-sm font-semibold text-foreground">
              FastClaw
            </span>
            <p className="text-[10px] text-muted-foreground leading-none">
              {gatewayRunning ? "Gateway running" : "Gateway stopped"}
            </p>
          </div>
        </div>

        {/* Navigation */}
        <nav className="flex-1 space-y-1 p-3 overflow-y-auto">
          <NavLinks />
        </nav>

        {/* Footer */}
        <div className="border-t border-border p-3">
          <div className="flex items-center justify-between">
            <span className="text-[11px] text-muted-foreground/60 font-mono">
              v0.1.0
            </span>
            <button
              onClick={toggleTheme}
              className="rounded-md p-1.5 text-muted-foreground hover:text-foreground hover:bg-muted/50 transition-colors"
            >
              {theme === "dark" ? (
                <Sun className="h-3.5 w-3.5" />
              ) : (
                <Moon className="h-3.5 w-3.5" />
              )}
            </button>
          </div>
        </div>
      </aside>

      {/* Mobile header */}
      <div className="fixed top-0 left-0 right-0 z-40 flex h-12 items-center justify-between border-b border-border bg-card/95 px-4 backdrop-blur-sm md:hidden">
        <div className="flex items-center gap-2">
          <div className="relative flex h-7 w-7 items-center justify-center">
            <img src="/logo.png" alt="FastClaw" className="h-7 w-7 rounded-md" />
            <span
              className={`absolute -bottom-0.5 -right-0.5 h-2 w-2 rounded-full border-[1.5px] border-card ${
                gatewayRunning ? "bg-emerald-500" : "bg-muted-foreground/40"
              }`}
            />
          </div>
          <span className="text-sm font-semibold text-foreground">FastClaw</span>
        </div>
        <button
          onClick={() => setMobileOpen(!mobileOpen)}
          className="rounded-md p-2 text-muted-foreground hover:text-foreground"
        >
          {mobileOpen ? (
            <X className="h-5 w-5" />
          ) : (
            <Menu className="h-5 w-5" />
          )}
        </button>
      </div>

      {/* Mobile menu overlay */}
      {mobileOpen && (
        <div
          className="fixed inset-0 z-30 bg-background/80 backdrop-blur-sm md:hidden"
          onClick={() => setMobileOpen(false)}
        >
          <div
            className="absolute top-12 right-0 w-64 border-l border-border bg-card p-3 space-y-1 h-[calc(100vh-3rem)] overflow-y-auto"
            onClick={(e) => e.stopPropagation()}
          >
            <NavLinks onClick={() => setMobileOpen(false)} />
          </div>
        </div>
      )}

      {/* Main content */}
      <main className="flex-1 pt-12 md:pt-0 overflow-y-auto">{children}</main>
    </div>
  );
}
