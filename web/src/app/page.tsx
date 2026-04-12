"use client";

import { useEffect } from "react";
import { useRouter } from "next/navigation";
import { getStatus } from "@/lib/api";
import { isLoggedIn } from "@/lib/auth";

export default function RootRedirect() {
  const router = useRouter();

  useEffect(() => {
    // Cloud users (logged in with token) are always pre-provisioned —
    // skip onboard and go straight to overview.
    if (isLoggedIn()) {
      router.replace("/overview/");
      return;
    }

    // Local mode: check if config exists to decide onboard vs overview.
    getStatus()
      .then((status) => {
        if (status.configured) {
          router.replace("/overview/");
        } else {
          router.replace("/onboard/");
        }
      })
      .catch(() => {
        router.replace("/onboard/");
      });
  }, [router]);

  return (
    <div className="flex min-h-screen items-center justify-center bg-zinc-950">
      <div className="h-8 w-8 animate-spin rounded-full border-2 border-zinc-700 border-t-violet-500" />
    </div>
  );
}
