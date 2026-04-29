"use client";

import { useState, useEffect } from "react";
import { useRouter, usePathname } from "next/navigation";
import { getMe } from "@/lib/api";
import { LoginScreen } from "./login-screen";

interface AuthGuardProps {
  children: React.ReactNode;
}

export function AuthGuard({ children }: AuthGuardProps) {
  const router = useRouter();
  const pathname = usePathname();
  const [checked, setChecked] = useState(false);
  const [authed, setAuthed] = useState(false);

  useEffect(() => {
    let aborted = false;
    (async () => {
      // Decide between three states:
      //   - users table empty → /onboard
      //   - users exist, caller has a session → render children
      //   - users exist, caller has no session → show LoginScreen
      let configured = false;
      try {
        const res = await fetch("/api/status", { credentials: "same-origin" });
        if (res.ok) {
          const status = await res.json();
          configured = !!status.configured;
        }
      } catch {
        // server down — fall through to LoginScreen
      }
      if (aborted) return;

      if (!configured) {
        const onOnboard = pathname === "/onboard" || pathname.startsWith("/onboard/");
        if (!onOnboard) {
          router.replace("/onboard/");
          return;
        }
        setAuthed(true);
        setChecked(true);
        return;
      }

      try {
        const me = await getMe();
        if (me.ok) {
          setAuthed(true);
        }
      } catch {
        // network failure — fall through to LoginScreen
      }
      if (!aborted) setChecked(true);
    })();
    return () => { aborted = true; };
  }, [router, pathname]);

  if (!checked) {
    return (
      <div className="flex min-h-screen items-center justify-center bg-zinc-950">
        <div className="h-8 w-8 animate-spin rounded-full border-2 border-zinc-700 border-t-violet-500" />
      </div>
    );
  }
  if (!authed) {
    return <LoginScreen onSuccess={() => setAuthed(true)} />;
  }
  return <>{children}</>;
}
