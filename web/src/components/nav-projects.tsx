"use client";

import * as React from "react";
import { usePathname, useRouter } from "next/navigation";
import {
  SidebarGroup,
  SidebarGroupLabel,
  SidebarMenu,
  SidebarMenuAction,
  SidebarMenuButton,
  SidebarMenuItem,
  useSidebar,
} from "@/components/ui/sidebar";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { MoreHorizontalIcon, PencilIcon, Trash2Icon } from "lucide-react";
import { deleteChatSession, renameChatSession } from "@/lib/api";

export interface SessionItem {
  id: string;
  title: string;
  // Set when the session's first user turn carried an image attachment.
  // Renders as a small thumbnail before the title so multimodal chats
  // show "image + text" instead of just the text label.
  thumbnailUrl?: string;
}

export function NavSessions({
  agentId,
  sessions,
}: {
  agentId: string | null;
  sessions: SessionItem[];
}) {
  const pathname = usePathname();
  const router = useRouter();
  const [editTarget, setEditTarget] = React.useState<SessionItem | null>(null);
  const [deleteTarget, setDeleteTarget] = React.useState<SessionItem | null>(null);

  if (!agentId) return null;

  const chatBase = `/agents/${agentId}/chat/`;

  // Any mutation (rename / delete) broadcasts so AppSidebar re-fetches and
  // the chat page (if open) also re-syncs its local sessions list.
  const broadcastChange = () => {
    if (typeof window !== "undefined") {
      window.dispatchEvent(
        new CustomEvent("fastclaw:sessions-changed", {
          detail: { agentId },
        }),
      );
    }
  };

  const onConfirmDelete = async () => {
    if (!deleteTarget) return;
    const target = deleteTarget;
    setDeleteTarget(null);
    try {
      await deleteChatSession(agentId, target.id);
    } finally {
      // If the deleted session is the one currently open, bounce back to
      // the fresh chat URL so the UI doesn't hang on a stale session.
      if (
        typeof window !== "undefined" &&
        new URLSearchParams(window.location.search).get("session") === target.id
      ) {
        router.replace(chatBase);
      }
      broadcastChange();
    }
  };

  return (
    <>
      <SidebarGroup className="group-data-[collapsible=icon]:hidden">
        <SidebarGroupLabel>Chats</SidebarGroupLabel>
        <SidebarMenu>
          {sessions.map((s) => {
            const href = `${chatBase}?session=${encodeURIComponent(s.id)}`;
            const active =
              pathname.startsWith(chatBase) &&
              typeof window !== "undefined" &&
              new URLSearchParams(window.location.search).get("session") === s.id;
            return (
              <SessionRow
                key={s.id}
                session={s}
                active={active}
                onOpen={() => router.push(href)}
                onEdit={() => setEditTarget(s)}
                onDelete={() => setDeleteTarget(s)}
              />
            );
          })}
          {sessions.length === 0 && (
            <SidebarMenuItem>
              <div className="px-2 py-1.5 text-xs text-muted-foreground">
                No chats yet
              </div>
            </SidebarMenuItem>
          )}
        </SidebarMenu>
      </SidebarGroup>

      <EditTitleDialog
        target={editTarget}
        agentId={agentId}
        onClose={() => setEditTarget(null)}
        onSaved={broadcastChange}
      />

      <AlertDialog
        open={!!deleteTarget}
        onOpenChange={(v) => !v && setDeleteTarget(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete chat</AlertDialogTitle>
            <AlertDialogDescription>
              Delete <strong>{deleteTarget?.title || deleteTarget?.id}</strong>? The
              full message history for this chat will be removed and cannot be
              recovered.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={onConfirmDelete}
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
            >
              Delete
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}

function SessionRow({
  session,
  active,
  onOpen,
  onEdit,
  onDelete,
}: {
  session: SessionItem;
  active: boolean;
  onOpen: () => void;
  onEdit: () => void;
  onDelete: () => void;
}) {
  const { isMobile } = useSidebar();
  return (
    <SidebarMenuItem>
      <SidebarMenuButton
        isActive={active}
        tooltip={session.title}
        onClick={onOpen}
      >
        {session.thumbnailUrl && (
          // eslint-disable-next-line @next/next/no-img-element
          <img
            src={session.thumbnailUrl}
            alt=""
            className="h-5 w-5 shrink-0 rounded object-cover"
          />
        )}
        <span className="truncate">{session.title || session.id}</span>
      </SidebarMenuButton>
      <DropdownMenu>
        <DropdownMenuTrigger
          render={
            <SidebarMenuAction showOnHover>
              <MoreHorizontalIcon />
              <span className="sr-only">Chat actions</span>
            </SidebarMenuAction>
          }
        />
        <DropdownMenuContent
          className="w-40 rounded-lg"
          side={isMobile ? "bottom" : "right"}
          align={isMobile ? "end" : "start"}
        >
          <DropdownMenuItem onClick={onEdit}>
            <PencilIcon className="text-muted-foreground" />
            <span>Edit</span>
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem
            onClick={onDelete}
            className="text-destructive focus:text-destructive"
          >
            <Trash2Icon className="text-destructive" />
            <span>Delete</span>
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    </SidebarMenuItem>
  );
}

function EditTitleDialog({
  target,
  agentId,
  onClose,
  onSaved,
}: {
  target: SessionItem | null;
  agentId: string;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [draft, setDraft] = React.useState("");
  const [saving, setSaving] = React.useState(false);

  React.useEffect(() => {
    setDraft(target?.title ?? "");
  }, [target]);

  if (!target) return null;

  const save = async () => {
    const next = draft.trim();
    if (!next || next === target.title) {
      onClose();
      return;
    }
    setSaving(true);
    try {
      await renameChatSession(agentId, target.id, next);
      onSaved();
    } finally {
      setSaving(false);
      onClose();
    }
  };

  return (
    <Dialog open={!!target} onOpenChange={(v) => !v && onClose()}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Edit chat title</DialogTitle>
          <DialogDescription>
            Rename this chat so it's easier to find in the sidebar.
          </DialogDescription>
        </DialogHeader>
        <Input
          autoFocus
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            // Ignore Enter while a CJK IME composition is active — otherwise
            // selecting a candidate would submit the dialog prematurely.
            if (e.nativeEvent.isComposing || e.keyCode === 229) return;
            if (e.key === "Enter") {
              e.preventDefault();
              save();
            }
          }}
          placeholder="Chat title"
        />
        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={saving}>
            Cancel
          </Button>
          <Button onClick={save} disabled={saving || !draft.trim()}>
            {saving ? "Saving…" : "Save"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
