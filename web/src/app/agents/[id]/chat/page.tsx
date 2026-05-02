"use client";

import { useEffect, useState, useRef, useCallback, useMemo } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { getAgent, getChatHistory, getChatSessions, listAgentFiles, renameChatSession, sendChatStream, uploadAgentFiles, getAuthToken, getSkills, type ChatHistoryMessage, type ChatStreamEvent, type SkillInfo, type ToolResultMetadata, type WorkspaceFile } from "@/lib/api";
import { Bot, Send, Copy, Check, Pencil, Wrench, ChevronDown, ChevronRight, Download, X, File, FileText, Image as ImageIcon, FileCode, Film, Music, Puzzle, SlidersHorizontal, ShieldCheck, Paperclip, Square, FolderOpen, RefreshCw } from "lucide-react";
import Link from "next/link";
import ReactMarkdown, { defaultUrlTransform } from "react-markdown";
import remarkGfm from "remark-gfm";

// react-markdown's default urlTransform strips any protocol not in the
// safe-list (http, https, mailto, ircs, xmpp) — including `data:`. We want
// inline base64 images to render, so fall through to the default for
// everything else.
function urlTransform(url: string, key: string): string {
  if (key === "src" && url.startsWith("data:image/")) return url;
  return defaultUrlTransform(url);
}

// makeUrlTransform builds a urlTransform that also remaps sandbox
// `/workspace/<name>` paths to the authenticated file API URL for the
// active agent. Skills that produce a file return a sandbox path like
// /workspace/img_xxx.png; the LLM puts that in `![](/workspace/...)`.
// The docker bind-mount is session-scoped (host:
// ~/.fastclaw/workspaces/<agent>/sessions/<sid>/ ↔ container:/workspace),
// so the workspace.Store sees the file at sessions/<sid>/<name>. We
// must prepend that prefix or the file API resolves against the agent
// root and 404s.
function makeUrlTransform(agentId: string, sessionId: string) {
  return (url: string, key: string): string => {
    if (key === "src") {
      if (url.startsWith("data:image/")) return url;
      if (url.startsWith("/workspace/")) {
        const rel = url.slice("/workspace/".length);
        const scoped = sessionId ? `sessions/${sessionId}/${rel}` : rel;
        return fileUrl(agentId, scoped, false);
      }
    }
    return defaultUrlTransform(url);
  };
}

// Split a string on `![alt](data:image/...;base64,...)` markdown.
//
// Real-world content from models is messier than the grammar: base64
// payloads get wrapped with newlines/spaces, the closing `)` is
// sometimes cut off by truncation, and `]` and `(` may sit on separate
// lines. So we look for the header with a regex but consume the URL
// body with a hand-rolled scan that tolerates whitespace inside base64
// and a missing trailing `)`. Returns [...{type:"text"|"image", ...}].
function splitDataImages(s: string): Array<{ type: "text"; text: string } | { type: "image"; alt: string; src: string }> {
  const out: Array<{ type: "text"; text: string } | { type: "image"; alt: string; src: string }> = [];
  const headerRe = /!\[([^\]]*?)\]\s*\(\s*<?\s*(data:image\/[a-z0-9.+-]+;base64,)/gi;
  let lastIdx = 0;
  let m: RegExpExecArray | null;
  while ((m = headerRe.exec(s)) !== null) {
    const alt = m[1];
    const dataPrefix = m[2];
    // Consume base64 body: A–Z a–z 0–9 + / = and any whitespace.
    let cursor = m.index + m[0].length;
    while (cursor < s.length && /[A-Za-z0-9+/=\s]/.test(s[cursor])) cursor++;
    const rawBody = s.slice(m.index + m[0].length, cursor);
    const body = rawBody.replace(/\s+/g, "");
    if (!body) continue;
    const src = dataPrefix + body;
    // Step past optional `>` and `)` closers (or accept truncation).
    let after = cursor;
    while (after < s.length && /[\s>]/.test(s[after])) after++;
    if (s[after] === ")") after++;

    if (m.index > lastIdx) out.push({ type: "text", text: s.slice(lastIdx, m.index) });
    out.push({ type: "image", alt, src });
    lastIdx = after;
    headerRe.lastIndex = after;
  }
  if (lastIdx < s.length) out.push({ type: "text", text: s.slice(lastIdx) });
  return out;
}

// Models sometimes emit markdown images with a line break between `]` and
// `(`, or with very long base64 destinations that some commonmark parsers
// give up on. Rather than fight the markdown parser, extract
// `![alt](data:image/...)` (tolerating whitespace/newlines between `]`
// and `(`) and render those as native <img>, letting ReactMarkdown
// handle everything else. Returns null if no data-URL images are present
// so the caller can fall through to a plain ReactMarkdown render.
// When `suppressAllInlineImages` is true, every data-URL image inside
// the content is dropped (used when the bubble has tool-output images
// already attached at the top — the model often re-embeds an image in
// its reply, and because LLMs can't reproduce base64 verbatim the
// re-embedded bytes are either a duplicate we already showed or a
// hallucination that renders as a different picture). `surfacedSrcs`
// provides a narrower exact-src match for cases where nothing is
// attached to this bubble but a prior bubble showed the same bytes.
function renderContentWithDataImages(
  content: string,
  surfacedSrcs?: ReadonlySet<string>,
  suppressAllInlineImages?: boolean,
): React.ReactNode | null {
  const parts = splitDataImages(content);
  if (!parts.some((p) => p.type === "image")) return null;
  return (
    <>
      {parts.map((p, i) => {
        if (p.type === "image") {
          if (suppressAllInlineImages || surfacedSrcs?.has(p.src)) return null;
          return (
            // eslint-disable-next-line @next/next/no-img-element
            <img key={i} src={p.src} alt={p.alt} className="rounded-lg max-w-full h-auto my-2" />
          );
        }
        return (
          <ReactMarkdown key={i} remarkPlugins={[remarkGfm]}>
            {p.text}
          </ReactMarkdown>
        );
      })}
    </>
  );
}

import { usePageHeader } from "@/components/sidebar";

interface ProducedFile {
  path: string; // path relative to workspace
  size?: number;
}

interface UserAttachment {
  name: string;
  isImage: boolean;
  // Local blob URL for instant in-bubble preview without a server round-trip.
  // Only set on the live-send turn; reloaded history won't carry it.
  previewUrl?: string;
}

interface ChatMessage {
  id: string;
  role: "user" | "agent" | "tool-group";
  content: string;
  timestamp: number;
  toolCalls?: { id: string; name: string; arguments: string; result?: string; metadata?: ToolResultMetadata }[];
  files?: ProducedFile[];
  attachments?: UserAttachment[];
}

// Single-segment identity filenames that route to the agent's home dir
// (not the workspace) — exclude from the "Your files" panel.
const SYSTEM_FILES = new Set([
  "SOUL.md", "IDENTITY.md", "USER.md", "BOOTSTRAP.md",
  "MEMORY.md", "HEARTBEAT.md", "AGENTS.md", "TOOLS.md", "agent.json",
]);

function isSystemFile(path: string): boolean {
  return !path.includes("/") && SYSTEM_FILES.has(path);
}

function parseWrittenSize(result: string): number | undefined {
  const m = result.match(/^Written (\d+) bytes/);
  return m ? parseInt(m[1], 10) : undefined;
}

interface ChatSession {
  id: string;
  title?: string;
  preview: string;
}

function generateSessionId() {
  return `s-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
}

/** Convert raw history messages into UI ChatMessages, grouping tool calls with results. */
function buildChatMessages(history: ChatHistoryMessage[]): ChatMessage[] {
  const msgs: ChatMessage[] = [];
  let i = 0;
  while (i < history.length) {
    const h = history[i];
    if (h.role === "user") {
      // Surface image attachments on history-loaded user bubbles. The
      // server emits `imageUrls` on user turns whose ContentParts had
      // image_url blocks; map them into UserAttachment entries so the
      // existing bubble renderer (live-send path) handles them with no
      // additional branching.
      const attachments: UserAttachment[] | undefined =
        h.imageUrls && h.imageUrls.length > 0
          ? h.imageUrls.map((url, idx) => ({
              name: `image-${idx + 1}`,
              isImage: true,
              previewUrl: url,
            }))
          : undefined;
      msgs.push({ id: `h-${i}`, role: "user", content: h.content || "", timestamp: 0, attachments });
      i++;
    } else if (h.role === "assistant" && h.toolCalls && h.toolCalls.length > 0) {
      // Group: assistant tool_calls + following tool results + final assistant content
      const calls = h.toolCalls.map((tc) => ({
        ...tc,
        result: undefined as string | undefined,
        metadata: undefined as ToolResultMetadata | undefined,
      }));
      i++;
      // Collect tool results
      while (i < history.length && history[i].role === "tool") {
        const toolMsg = history[i];
        const call = calls.find((c) => c.id === toolMsg.toolCallId);
        if (call) {
          call.result = toolMsg.content;
          if (toolMsg.metadata) call.metadata = toolMsg.metadata;
        }
        i++;
      }
      // Defensive: any tool_use that still has no result by the time
      // we leave the tool-result run got orphaned (client aborted, server
      // crashed mid-turn, persistence path raced). Mark them stopped so
      // the UI shows a terminal state instead of spinning forever.
      // Newer turns will have these padded server-side via
      // padOrphanToolResults; this catches sessions that pre-date the fix.
      for (const c of calls) {
        if (c.result === undefined) {
          c.result = "(stopped)";
        }
      }
      // Show as tool-group
      msgs.push({
        id: `h-tool-${i}`,
        role: "tool-group",
        content: h.content || "",
        timestamp: 0,
        toolCalls: calls,
      });
      // If next is assistant with ONLY content and no tool calls (final
      // answer), add it. Must skip when the next assistant also has tool
      // calls — in a multi-turn conversation that's the *start of the next
      // tool-group*, not a final answer, and consuming it here would drop
      // its tool calls on the floor and leave subsequent tool-result
      // messages orphaned.
      if (
        i < history.length &&
        history[i].role === "assistant" &&
        history[i].content &&
        !(history[i].toolCalls && history[i].toolCalls!.length > 0)
      ) {
        msgs.push({ id: `h-${i}`, role: "agent", content: history[i].content || "", timestamp: 0 });
        i++;
      }
    } else if (h.role === "assistant") {
      msgs.push({ id: `h-${i}`, role: "agent", content: h.content || "", timestamp: 0 });
      i++;
    } else {
      i++; // skip unexpected
    }
  }
  return msgs;
}

export default function AgentChatPage() {
  const router = useRouter();
  const searchParams = useSearchParams();
  // Reactive: re-derives from pathname so switching agents (sidebar
  // dropdown, browser back/forward) immediately updates downstream
  // fetches. The previous useState(() => ...) flavor froze the id at
  // mount, so background loads kept hitting the old agent and the
  // panel showed stale history under the new URL.
  const selectedAgent = useAgentIdFromURL();
  const [agentName, setAgentName] = useState<string>("");
  const [sessionId, setSessionId] = useState<string>(() => searchParams.get("session") || generateSessionId());
  const [sessions, setSessions] = useState<ChatSession[]>([]);
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [input, setInput] = useState("");
  const [sending, setSending] = useState(false);
  const [copiedId, setCopiedId] = useState<string | null>(null);
  const [filesSheetOpen, setFilesSheetOpen] = useState(false);
  const [sessionTitle, setSessionTitle] = useState<string>("");
  const [attachments, setAttachments] = useState<File[]>([]);
  // Lightbox for clicking either an attachment thumbnail (compose box)
  // or an inline image in a sent message bubble. `null` = closed.
  const [lightboxSrc, setLightboxSrc] = useState<string | null>(null);
  // Object URLs for image attachments in the compose box. Keyed by file
  // index so we can revoke on remove without re-computing for every
  // chip on every keystroke. Re-derived whenever `attachments` changes.
  const attachmentPreviews = useMemo(
    () =>
      attachments.map((f) =>
        f.type.startsWith("image/") ? URL.createObjectURL(f) : null,
      ),
    [attachments],
  );
  useEffect(() => {
    return () => {
      for (const url of attachmentPreviews) if (url) URL.revokeObjectURL(url);
    };
  }, [attachmentPreviews]);
  const messagesEndRef = useRef<HTMLDivElement>(null);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);
  // AbortController for the in-flight chat stream so the Stop button can
  // cancel both the upload and the SSE connection. Reset on every new turn.
  const abortRef = useRef<AbortController | null>(null);

  // Slash-command menu state. The menu opens when the textarea holds a
  // token beginning with `/` at the caret; selecting a skill swaps that
  // token for `/<skill-name> `, leaving the cursor after the space so the
  // user can keep typing their prompt.
  const [skills, setSkills] = useState<SkillInfo[]>([]);
  const [slashOpen, setSlashOpen] = useState(false);
  const [slashQuery, setSlashQuery] = useState("");
  const [slashIndex, setSlashIndex] = useState(0);

  useEffect(() => {
    getSkills().then(setSkills).catch(() => setSkills([]));
  }, []);

  // Reset chat-pane state when the active agent changes. Without
  // this, switching agents from the sidebar would briefly render the
  // previous agent's history / sessions / title under the new URL
  // until the load effects below replaced them. Clearing eagerly is
  // cheaper than threading a "loading" flag through every render.
  // We deliberately keep `input` so a half-typed message survives an
  // accidental agent switch.
  const lastAgentRef = useRef(selectedAgent);
  useEffect(() => {
    if (lastAgentRef.current === selectedAgent) return;
    lastAgentRef.current = selectedAgent;
    setMessages([]);
    setSessions([]);
    setSessionTitle("");
    setAgentName("");
    setAttachments([]);
  }, [selectedAgent]);

  // Resolve the agent's display name once. The chat title and any
  // future header bits should show "Chat with My Helper", not the
  // opaque agt_xxx id. Uses /api/agents/{id} (owner or super_admin) so
  // an admin viewing another user's agent still gets the real name —
  // /api/agents (list) is owner-scoped and would miss it.
  useEffect(() => {
    if (!selectedAgent) return;
    let aborted = false;
    getAgent(selectedAgent)
      .then((a) => {
        if (aborted) return;
        setAgentName(a?.name || a?.id || selectedAgent);
      })
      .catch(() => {
        if (!aborted) setAgentName(selectedAgent);
      });
    return () => {
      aborted = true;
    };
  }, [selectedAgent]);

  // Detect whether the caret is inside a /token and, if so, what's been
  // typed after the slash. Cheap enough to run every keystroke.
  const slashContext = (value: string, caret: number): { start: number; query: string } | null => {
    const before = value.slice(0, caret);
    // Require the slash to be at start-of-message or preceded by whitespace
    // so paths / URLs with slashes don't trigger the menu.
    const match = /(^|\s)\/([\w-]*)$/.exec(before);
    if (!match) return null;
    return { start: caret - match[2].length - 1, query: match[2] };
  };

  const filteredSkills = slashOpen
    ? skills
        .filter((s) => {
          const q = slashQuery.toLowerCase();
          if (!q) return true;
          return s.name.toLowerCase().includes(q) || (s.description || "").toLowerCase().includes(q);
        })
        .slice(0, 8)
    : [];

  const selectSkill = useCallback(
    (skill: SkillInfo) => {
      const el = textareaRef.current;
      if (!el) return;
      const caret = el.selectionStart ?? input.length;
      const ctx = slashContext(input, caret);
      if (!ctx) return;
      const before = input.slice(0, ctx.start);
      const after = input.slice(caret);
      const insert = `/${skill.name} `;
      const next = before + insert + after;
      setInput(next);
      setSlashOpen(false);
      setSlashQuery("");
      setSlashIndex(0);
      requestAnimationFrame(() => {
        const pos = before.length + insert.length;
        el.focus();
        el.setSelectionRange(pos, pos);
      });
    },
    [input],
  );

  // Load sessions when agent changes
  const loadSessions = useCallback((agentId: string) => {
    getChatSessions(agentId)
      .then((list) => setSessions(list || []))
      .catch(() => setSessions([]));
  }, []);

  useEffect(() => {
    if (!selectedAgent) return;
    loadSessions(selectedAgent);
  }, [selectedAgent, loadSessions]);

  // Live subscription for cron-fired (and other async) replies. The
  // server's WebChannel fans out every outbound bus message routed to
  // (agent, session) here as an SSE event; we append it to messages so
  // the user sees scheduled jokes / reminders without reloading.
  // Re-runs whenever (agent, session) changes — closing the previous
  // EventSource so we don't double-receive after switching sessions.
  useEffect(() => {
    if (!selectedAgent || !sessionId) return;
    const url = `/api/chat/subscribe?agentId=${encodeURIComponent(selectedAgent)}&sessionId=${encodeURIComponent(sessionId)}`;
    const es = new EventSource(url, { withCredentials: true });
    es.onmessage = (ev) => {
      try {
        const data = JSON.parse(ev.data) as { text?: string };
        const text = data.text || "";
        if (!text) return;
        setMessages((prev) => [
          ...prev,
          {
            id: `async-${Date.now()}-${Math.random().toString(36).slice(2, 7)}`,
            role: "agent",
            content: text,
            timestamp: Date.now(),
          },
        ]);
      } catch {
        // ignore malformed events
      }
    };
    es.onerror = () => {
      // EventSource auto-reconnects on transient errors; only close on
      // unmount. A persistent 404 (session removed, agent gone) will
      // keep flapping but is harmless.
    };
    return () => {
      es.close();
    };
  }, [selectedAgent, sessionId]);

  // Sync URL's ?session= back into local state. Without this, navigating
  // between sessions / to "New chat" from the sidebar (router.push of the
  // same page with a different query string) changes the URL but doesn't
  // remount the page — sessionId / messages would stay on the old value.
  //
  // Three transitions to handle:
  //   - /chat/?session=A → /chat/?session=B  : swap sessionId, history effect reloads messages
  //   - /chat/?session=A → /chat/           : brand-new session, clear messages pane
  //   - /chat/           → /chat/?session=A : open the targeted session
  // We track `prevHadSession` so the initial mount (useState already picked
  // an id) doesn't trigger a redundant reset.
  const prevHadSessionRef = useRef(false);
  useEffect(() => {
    const urlSession = searchParams.get("session");
    if (urlSession) {
      prevHadSessionRef.current = true;
      if (urlSession !== sessionId) {
        setSessionId(urlSession);
      }
      return;
    }
    if (prevHadSessionRef.current) {
      prevHadSessionRef.current = false;
      setSessionId(generateSessionId());
      setMessages([]);
    }
  }, [searchParams, sessionId]);

  // Keep the local sessionTitle in sync with the session list. Unknown
  // sessions (brand-new, not saved yet) fall back to empty so the header
  // can render "New chat".
  useEffect(() => {
    const s = sessions.find((x) => x.id === sessionId);
    setSessionTitle(s?.title || s?.preview || "");
  }, [sessionId, sessions]);

  const handleRenameTitle = useCallback(
    async (next: string) => {
      const trimmed = next.trim();
      if (!trimmed || !selectedAgent || trimmed === sessionTitle) return;
      setSessionTitle(trimmed);
      try {
        await renameChatSession(selectedAgent, sessionId, trimmed);
      } finally {
        loadSessions(selectedAgent);
        // Tell the global sidebar to refetch its Chats list so the new
        // title shows up without a full page reload. AppSidebar's own
        // fetch only re-runs when activeAgentId changes, which doesn't
        // happen on rename.
        if (typeof window !== "undefined") {
          window.dispatchEvent(
            new CustomEvent("fastclaw:sessions-changed", {
              detail: { agentId: selectedAgent },
            }),
          );
        }
      }
    },
    [selectedAgent, sessionId, sessionTitle, loadSessions],
  );

  // Render the editable title into the global sticky header (next to the
  // sidebar toggle). Re-fires whenever the title changes.
  const headerSlot = useMemo(
    () => (
      <ChatHeaderTitle
        title={sessionTitle}
        fallback={`Chat with ${agentName || selectedAgent}`}
        onSave={handleRenameTitle}
      />
    ),
    [sessionTitle, agentName, selectedAgent, handleRenameTitle],
  );
  usePageHeader(headerSlot, [headerSlot]);

  // Load history when session changes. After rebuilding messages from
  // server history we also re-attach this session's workspace files to
  // the trailing assistant bubble so the "Your files" panel survives a
  // refresh — server history doesn't carry per-turn file diffs, so we
  // approximate by listing everything under sessions/<sid>/ once and
  // hanging it off the last agent message.
  useEffect(() => {
    if (!selectedAgent || !sessionId) return;
    getChatHistory(selectedAgent, sessionId)
      .then(async (history) => {
        if (!history || history.length === 0) {
          setMessages([]);
          return;
        }
        const built = buildChatMessages(history);
        try {
          const allFiles = await listAgentFiles(selectedAgent);
          const sessionPrefix = `sessions/${sessionId}/`;
          const sessionFiles: ProducedFile[] = allFiles
            .filter((f) => f.path.startsWith(sessionPrefix) && !isSystemFile(f.path))
            .map((f) => ({ path: f.path, size: f.size }));
          if (sessionFiles.length > 0) {
            for (let i = built.length - 1; i >= 0; i--) {
              if (built[i].role === "agent" || built[i].role === "tool-group") {
                built[i] = { ...built[i], files: sessionFiles };
                break;
              }
            }
          }
        } catch { /* listing failed — fall back to no panel */ }
        setMessages(built);
      })
      .catch(() => setMessages([]));
  }, [selectedAgent, sessionId]);

  useEffect(() => {
    messagesEndRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [messages]);

  useEffect(() => {
    const el = textareaRef.current;
    if (el) {
      el.style.height = "auto";
      el.style.height = Math.min(el.scrollHeight, 200) + "px";
    }
  }, [input]);

  const handleSend = useCallback(async () => {
    const text = input.trim();
    // Allow sending with attachments only (no text), but require at least one.
    if ((!text && attachments.length === 0) || !selectedAgent || sending) return;

    // Pin the sessionId into the URL on the first send so a refresh keeps
    // the user in the same conversation. Use history.replaceState (not
    // router.replace) to avoid Next.js remounting the page and interrupting
    // the stream we're about to start.
    if (typeof window !== "undefined" && !window.location.search.includes("session=")) {
      window.history.replaceState(null, "", `/agents/${selectedAgent}/chat/?session=${sessionId}`);
    }

    // Upload attachments first so the agent can read them by name on its
    // first turn. Files land at sessions/<sid>/<basename> in the workspace
    // store, which is the same dir the docker sandbox bind-mounts as
    // /workspace. We also (a) build user-bubble preview metadata so images
    // render inline in the user's bubble without waiting on the server
    // round-trip, and (b) read images as data URLs so vision-capable
    // models receive them as image_url content parts.
    const filesToUpload = attachments;
    setAttachments([]);

    let userBubbleAttachments: UserAttachment[] = [];
    let imageDataUrls: string[] = [];

    if (filesToUpload.length > 0) {
      userBubbleAttachments = filesToUpload.map((f) => ({
        name: f.name,
        isImage: f.type.startsWith("image/"),
        previewUrl: f.type.startsWith("image/") ? URL.createObjectURL(f) : undefined,
      }));

      try {
        await uploadAgentFiles(selectedAgent, sessionId, filesToUpload);
      } catch (err) {
        setMessages((prev) => [
          ...prev,
          { id: `e-${Date.now()}`, role: "agent", content: `File upload failed: ${err instanceof Error ? err.message : "unknown error"}`, timestamp: Date.now() },
        ]);
        return;
      }

      // Read each image as a base64 data URL. We do this AFTER upload —
      // upload only needs the File object; data URL conversion is for the
      // provider call. Done in parallel for snappy UX on multi-attach.
      imageDataUrls = (
        await Promise.all(
          filesToUpload.map(async (f) => {
            if (!f.type.startsWith("image/")) return null;
            return await new Promise<string | null>((resolve) => {
              const reader = new FileReader();
              reader.onload = () => resolve(typeof reader.result === "string" ? reader.result : null);
              reader.onerror = () => resolve(null);
              reader.readAsDataURL(f);
            });
          }),
        )
      ).filter((s): s is string => !!s);
    }
    // Build the prompt actually sent to the model. Images travel as
    // `imageUrls` for vision, but the model also needs the on-disk path
    // for skills like image-tool that take `input: "/workspace/<file>"`.
    // We prepend `[Attached: /workspace/<name>]` lines for that — the
    // server's StripAttachedPrefix removes them on history read so user
    // bubbles, page titles, and sidebar previews stay clean.
    const attachedPaths = filesToUpload.map((f) => `/workspace/${f.name}`);
    const breadcrumb = attachedPaths
      .map((p) => `[Attached: ${p}]`)
      .join("\n");
    const fullText = breadcrumb
      ? (text ? `${breadcrumb}\n${text}` : breadcrumb)
      : text;

    setInput("");
    setMessages((prev) => [
      ...prev,
      {
        id: `u-${Date.now()}`,
        role: "user",
        content: text, // bubble shows text only; attachments rendered separately above
        timestamp: Date.now(),
        attachments: userBubbleAttachments.length > 0 ? userBubbleAttachments : undefined,
      },
    ]);
    setSending(true);
    abortRef.current = new AbortController();

    // Snapshot the workspace before the turn so we can diff at `done` and
    // attach newly-created / modified files (PDFs, images, …) to the
    // final reply. Fire-and-forget; if the snapshot fails we just won't
    // surface files this turn. `path → size|modTime` key.
    const preTurnFilesPromise = listAgentFiles(selectedAgent)
      .then((items) => {
        const m = new Map<string, string>();
        for (const f of items) m.set(f.path, `${f.size}|${f.modTime}`);
        return m;
      })
      .catch(() => new Map<string, string>());

    let curGroupId = "";
    let curCalls: { id: string; name: string; arguments: string; result?: string; metadata?: ToolResultMetadata }[] = [];
    let curContent = "";
    const turnFiles: ProducedFile[] = [];
    const seenPaths = new Set<string>();

    const startNewGroup = () => {
      curGroupId = `tg-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`;
      curCalls = [];
      curContent = "";
    };
    startNewGroup();

    try {
      await sendChatStream(selectedAgent, sessionId, fullText, (evt: ChatStreamEvent) => {
        switch (evt.type) {
          case "content": {
            const content = evt.data?.content || "";
            if (content === "__NEW_SESSION__") {
              handleNewChat();
              loadSessions(selectedAgent);
              return;
            }
            if (curCalls.length > 0) {
              // Content after tool calls = new round. Finalize current group, start fresh.
              startNewGroup();
            }
            // Store as thinking content (may become part of next tool-group, or stay as final answer)
            curContent = content;
            setMessages((prev) => [
              ...prev,
              { id: `a-${Date.now()}`, role: "agent", content, timestamp: Date.now() },
            ]);
            break;
          }
          case "tool_call": {
            // New round starts if every tool in the current group has
            // already resolved. Without this, two assistant turns that
            // happen back-to-back with no intervening content event get
            // merged into one visual group live — inconsistent with the
            // refresh path (buildChatMessages) which correctly splits
            // per assistant message.
            if (curCalls.length > 0 && curCalls.every((c) => c.result !== undefined)) {
              startNewGroup();
            }
            curCalls.push({
              id: evt.data?.id || "",
              name: evt.data?.name || "",
              arguments: evt.data?.arguments || "{}",
            });
            const groupId = curGroupId;
            const calls = [...curCalls];
            const content = curContent;
            setMessages((prev) => {
              // If last message is the thinking content for this round, replace with tool-group
              const last = prev[prev.length - 1];
              if (content && last?.role === "agent" && last.content === content) {
                return [
                  ...prev.slice(0, -1),
                  { id: groupId, role: "tool-group" as const, content, timestamp: Date.now(), toolCalls: calls },
                ];
              }
              // Update existing tool-group for this round
              const idx = prev.findIndex((m) => m.id === groupId);
              if (idx >= 0) {
                const updated = [...prev];
                updated[idx] = { ...updated[idx], toolCalls: calls };
                return updated;
              }
              // New tool-group
              return [
                ...prev,
                { id: groupId, role: "tool-group" as const, content, timestamp: Date.now(), toolCalls: calls },
              ];
            });
            break;
          }
          case "tool_result": {
            const tc = curCalls.find((c) => c.id === (evt.data?.id || ""));
            const resultText = evt.data?.result || "";
            if (tc) {
              tc.result = resultText;
              if (evt.data?.metadata) tc.metadata = evt.data.metadata;
            }
            // Track successful write_file calls that landed in the workspace
            // (i.e. a relative path that isn't a system identity file).
            if (tc && tc.name === "write_file" && /^Written \d+ bytes/.test(resultText)) {
              try {
                const args = JSON.parse(tc.arguments);
                const p: string = typeof args?.path === "string" ? args.path : "";
                if (p && !p.startsWith("/") && !isSystemFile(p) && !seenPaths.has(p)) {
                  seenPaths.add(p);
                  turnFiles.push({ path: p, size: parseWrittenSize(resultText) });
                }
              } catch { /* ignore bad args */ }
            }
            const groupId = curGroupId;
            const calls = [...curCalls];
            setMessages((prev) => {
              const idx = prev.findIndex((m) => m.id === groupId);
              if (idx < 0) return prev;
              const updated = [...prev];
              updated[idx] = { ...updated[idx], toolCalls: calls };
              return updated;
            });
            break;
          }
          case "error": {
            // Surface backend errors as a chat bubble. Without this the
            // turn just hangs — the model failed (provider 4xx/5xx,
            // serialization mismatch, etc.) and the only signal was a
            // gateway log line the user can't see.
            const msg = evt.data?.message || "Unknown error";
            setMessages((prev) => [
              ...prev,
              { id: `e-${Date.now()}`, role: "agent", content: `Error: ${msg}`, timestamp: Date.now() },
            ]);
            break;
          }
        }
      }, abortRef.current.signal, imageDataUrls);
      // Diff the workspace against the pre-turn snapshot so files
      // produced by *exec* (e.g. a Python script that saves PDFs) get
      // surfaced too — `turnFiles` only catches write_file tool calls
      // with relative, non-identity paths, which misses most real-
      // world flows. Union both sources by path.
      const postTurnFiles = await listAgentFiles(selectedAgent).catch(() => []);
      const preSnap = await preTurnFilesPromise;
      const diffFiles: ProducedFile[] = [];
      for (const f of postTurnFiles) {
        if (isSystemFile(f.path)) continue;
        const key = `${f.size}|${f.modTime}`;
        if (preSnap.get(f.path) === key) continue; // unchanged
        if (seenPaths.has(f.path)) continue;
        diffFiles.push({ path: f.path, size: f.size });
      }
      const allFiles = [...turnFiles, ...diffFiles];
      // Diagnostic: when sandbox-exec produces a file but the Files
      // panel doesn't show, we need to know whether the API returned
      // the file at all and where the diff dropped it. Cheap to keep.
      if (typeof console !== "undefined") {
        console.log("[chat] post-turn files diff", {
          agent: selectedAgent,
          sessionId,
          preSnapSize: preSnap.size,
          postTurnCount: postTurnFiles.length,
          postTurnPaths: postTurnFiles.map((f) => f.path),
          turnFiles,
          diffFiles,
          attached: allFiles.length,
        });
      }
      if (allFiles.length > 0) {
        setMessages((prev) => {
          if (prev.length === 0) return prev;
          const updated = [...prev];
          const last = updated[updated.length - 1];
          updated[updated.length - 1] = { ...last, files: allFiles };
          if (typeof console !== "undefined") {
            console.log("[chat] attached files to last message", {
              lastId: last.id,
              lastRole: last.role,
              files: allFiles,
            });
          }
          return updated;
        });
      }
      loadSessions(selectedAgent);
      // First-turn of a brand-new session just got persisted — tell the
      // global sidebar to refetch its Chats list so the new title shows
      // up without a full page reload.
      if (typeof window !== "undefined") {
        window.dispatchEvent(
          new CustomEvent("fastclaw:sessions-changed", {
            detail: { agentId: selectedAgent },
          }),
        );
      }
    } catch (err) {
      // AbortError from the user clicking Stop is expected — surface a
      // brief "Stopped" line so they see the cancellation took effect,
      // not a generic failure message.
      const isAbort = err instanceof DOMException && err.name === "AbortError";
      // Surface the underlying error in DevTools so future "Failed to
      // get a response" reports come with a concrete cause (network,
      // parse, post-turn fetch, …) rather than the generic message.
      if (typeof console !== "undefined") {
        console.error("[chat] handleSend error", err);
      }
      // Keyboard-stack abort + post-stream tear-down can both throw an
      // AbortError after a successful turn (the SSE reader is
      // released on `done`, then a stray reader.cancel() races with a
      // late server EOF and surfaces as one). Both look identical to
      // user-pressed-Stop here, so we additionally suppress the
      // toast when at least one agent reply already landed for this
      // turn — the user just got their answer; we shouldn't tack on
      // a confusing failure bubble.
      if (isAbort) {
        // Resolve any in-flight tools in the current tool-group so they
        // stop spinning. Server-side padOrphanToolResults will write a
        // matching record on its end; this just keeps the UI consistent
        // until the next history fetch overwrites it.
        setMessages((prev) =>
          prev.map((m) =>
            m.role === "tool-group" && m.toolCalls
              ? {
                  ...m,
                  toolCalls: m.toolCalls.map((tc) =>
                    tc.result === undefined ? { ...tc, result: "(stopped)" } : tc,
                  ),
                }
              : m,
          ),
        );
        setMessages((prev) => [
          ...prev,
          { id: `e-${Date.now()}`, role: "agent", content: "(Stopped)", timestamp: Date.now() },
        ]);
      } else {
        setMessages((prev) => {
          const lastUser = [...prev].reverse().findIndex((m) => m.role === "user");
          if (lastUser >= 0) {
            const replyAfter = prev
              .slice(prev.length - lastUser)
              .some((m) => m.role === "agent" || m.role === "tool-group");
            if (replyAfter) return prev; // turn already produced output
          }
          const errMsg = err instanceof Error && err.message
            ? err.message
            : "Failed to get a response. Is the gateway running?";
          return [
            ...prev,
            {
              id: `e-${Date.now()}`,
              role: "agent",
              content: errMsg,
              timestamp: Date.now(),
            },
          ];
        });
      }
    } finally {
      abortRef.current = null;
      setSending(false);
      textareaRef.current?.focus();
    }
  }, [input, attachments, selectedAgent, sessionId, sending, loadSessions]);

  const handleStop = useCallback(() => {
    abortRef.current?.abort();
  }, []);

  const handleFilePick = useCallback((e: React.ChangeEvent<HTMLInputElement>) => {
    const picked = e.target.files;
    if (!picked || picked.length === 0) return;
    // Snapshot the FileList into a stable File[] BEFORE we reset the
    // input. FileList is tied to the input element — setting value=""
    // empties it. Under StrictMode React invokes the setState updater
    // twice for purity checks; if the closure references the live
    // FileList, the second invocation sees an empty list and the state
    // ends up empty even though the user picked a file.
    const newFiles = Array.from(picked);
    e.target.value = "";
    setAttachments((prev) => [...prev, ...newFiles]);
  }, []);

  const removeAttachment = useCallback((idx: number) => {
    setAttachments((prev) => prev.filter((_, i) => i !== idx));
  }, []);

  const handleKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    // Don't submit while an IME composition is active — Enter in that state
    // is the user confirming the IME candidate (e.g. pinyin → 好), not
    // sending the message. keyCode 229 also signals "composing" on some
    // browsers where isComposing isn't set.
    if (e.nativeEvent.isComposing || e.keyCode === 229) return;

    // Slash menu keyboard handling takes precedence when open: arrows move
    // the selection, Enter confirms, Escape closes without sending.
    if (slashOpen && filteredSkills.length > 0) {
      if (e.key === "ArrowDown") {
        e.preventDefault();
        setSlashIndex((i) => (i + 1) % filteredSkills.length);
        return;
      }
      if (e.key === "ArrowUp") {
        e.preventDefault();
        setSlashIndex((i) => (i - 1 + filteredSkills.length) % filteredSkills.length);
        return;
      }
      if (e.key === "Enter" || e.key === "Tab") {
        e.preventDefault();
        selectSkill(filteredSkills[slashIndex]);
        return;
      }
      if (e.key === "Escape") {
        e.preventDefault();
        setSlashOpen(false);
        return;
      }
    }

    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      handleSend();
    }
  };

  // onChange wrapper: update input + slash menu visibility in one pass.
  const handleInputChange = (e: React.ChangeEvent<HTMLTextAreaElement>) => {
    const next = e.target.value;
    setInput(next);
    const caret = e.target.selectionStart ?? next.length;
    const ctx = slashContext(next, caret);
    if (ctx) {
      setSlashOpen(true);
      setSlashQuery(ctx.query);
      setSlashIndex(0);
    } else {
      setSlashOpen(false);
    }
  };

  const handleCopy = (msg: ChatMessage) => {
    navigator.clipboard.writeText(msg.content);
    setCopiedId(msg.id);
    setTimeout(() => setCopiedId(null), 1500);
  };

  const handleNewChat = () => {
    const newId = generateSessionId();
    setSessionId(newId);
    setMessages([]);
    router.replace(`/agents/${selectedAgent}/chat/`);
  };

  const handleSelectSession = (sid: string) => {
    setSessionId(sid);
    router.replace(`/agents/${selectedAgent}/chat/?session=${sid}`);
  };

  const formatTime = (ts: number) =>
    new Date(ts).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });

  return (
    <div className="flex h-[calc(100vh-3rem)] flex-row">
      <div className="flex flex-1 min-w-0 flex-col">
      {/* Messages */}
        <div className="flex-1 overflow-y-auto min-h-0 px-4 py-4">
          <div className="mx-auto max-w-2xl space-y-3">
            {messages.length === 0 && (
              <div className="flex flex-col items-center justify-center py-24 text-center">
                <div className="flex h-16 w-16 items-center justify-center rounded-full bg-muted mb-4">
                  <Bot className="h-8 w-8 text-muted-foreground" />
                </div>
                <p className="text-lg font-medium mb-1">
                  Chat with {agentName || selectedAgent || "your agent"}
                </p>
                <p className="text-sm text-muted-foreground">
                  Send a message to start a conversation
                </p>
              </div>
            )}

            {(() => {
              // Tool-group artefacts (e.g. an image rendered to base64 by
              // a Python script inside the sandbox) are attached to the
              // *next* agent reply bubble — so they appear as part of the
              // assistant's answer, not inside the tool panel. If no agent
              // reply follows (tool still running or chain didn't finish),
              // we skip surfacing the image; it will show up with the next
              // reply. `surfacedSrcs` tracks every image src we've
              // surfaced so bubbles can suppress duplicate inline copies.
              const attachedImages = new Map<string, Array<{ alt: string; src: string }>>();
              const surfacedSrcs = new Set<string>();
              let pending: Array<{ alt: string; src: string }> = [];
              for (const m of messages) {
                if (m.role === "tool-group" && m.toolCalls) {
                  for (const tc of m.toolCalls) {
                    if (!tc.result) continue;
                    for (const p of splitDataImages(tc.result)) {
                      if (p.type === "image") {
                        pending.push({ alt: p.alt, src: p.src });
                      }
                    }
                  }
                  continue;
                }
                if (m.role === "agent" && pending.length > 0) {
                  attachedImages.set(m.id, pending);
                  for (const img of pending) surfacedSrcs.add(img.src);
                  pending = [];
                }
              }
              return messages.map((msg) =>
              msg.role === "tool-group" ? (
                <div key={msg.id}>
                  <ToolCallGroup msg={msg} surfacedSrcs={surfacedSrcs} agentId={selectedAgent} sessionId={sessionId} />
                  {msg.files && msg.files.length > 0 && (
                    <FilesPanel agentId={selectedAgent} files={msg.files} />
                  )}
                </div>
              ) : (
                <div
                  key={msg.id}
                  className={`flex ${msg.role === "user" ? "justify-end" : "justify-start"}`}
                >
                  <div
                    className={`group relative max-w-[80%] ${
                      msg.role === "user" ? "order-1" : ""
                    }`}
                  >
                    <div
                      className={`rounded-2xl px-4 py-2.5 ${
                        msg.role === "user"
                          ? "bg-sidebar text-sidebar-foreground rounded-br-md"
                          : "bg-muted rounded-bl-md"
                      }`}
                    >
                      {(() => {
                        const attached = attachedImages.get(msg.id);
                        return attached && attached.length > 0 ? (
                          <div className="space-y-2 mb-2">
                            {attached.map((img, i) => (
                              // eslint-disable-next-line @next/next/no-img-element
                              <img
                                key={i}
                                src={img.src}
                                alt={img.alt}
                                className="rounded-lg max-w-full h-auto"
                              />
                            ))}
                          </div>
                        ) : null;
                      })()}
                      {msg.role === "user" && msg.attachments && msg.attachments.length > 0 && (
                        <div className="space-y-2 mb-2">
                          {msg.attachments.map((att, i) =>
                            att.isImage && att.previewUrl ? (
                              <button
                                key={i}
                                type="button"
                                onClick={() => setLightboxSrc(att.previewUrl!)}
                                className="block cursor-zoom-in"
                                aria-label={`Preview ${att.name}`}
                              >
                                {/* eslint-disable-next-line @next/next/no-img-element */}
                                <img
                                  src={att.previewUrl}
                                  alt={att.name}
                                  className="rounded-lg max-w-full h-auto"
                                />
                              </button>
                            ) : (
                              <div
                                key={i}
                                className="flex items-center gap-2 rounded-md bg-sidebar-foreground/10 px-2 py-1.5 text-xs"
                              >
                                <Paperclip className="h-3 w-3 opacity-70" />
                                <span className="truncate">{att.name}</span>
                              </div>
                            ),
                          )}
                        </div>
                      )}
                      {msg.content && (
                        <div
                          className={`text-[15px] leading-relaxed prose prose-sm max-w-none prose-p:my-1 prose-pre:my-2 prose-ul:my-1 prose-ol:my-1 dark:prose-invert`}
                        >
                          {renderContentWithDataImages(
                            msg.content,
                            surfacedSrcs,
                            (attachedImages.get(msg.id)?.length ?? 0) > 0,
                          ) ?? (
                            <ReactMarkdown remarkPlugins={[remarkGfm]} urlTransform={makeUrlTransform(selectedAgent, sessionId)}>
                              {msg.content}
                            </ReactMarkdown>
                          )}
                        </div>
                      )}
                    </div>
                    {msg.files && msg.files.length > 0 && (
                      <FilesPanel agentId={selectedAgent} files={msg.files} />
                    )}
                    <div
                      className={`flex items-center gap-1.5 mt-1 ${
                        msg.role === "user" ? "justify-end" : "justify-start"
                      }`}
                    >
                      {msg.timestamp > 0 && (
                        <span className="text-[10px] text-muted-foreground/60">
                          {formatTime(msg.timestamp)}
                        </span>
                      )}
                      {msg.role === "agent" && (
                        <button
                          onClick={() => handleCopy(msg)}
                          className="opacity-0 group-hover:opacity-100 p-0.5 rounded hover:bg-muted text-muted-foreground/60 hover:text-muted-foreground transition-all"
                          title="Copy"
                        >
                          {copiedId === msg.id ? (
                            <Check className="h-3 w-3 text-emerald-500" />
                          ) : (
                            <Copy className="h-3 w-3" />
                          )}
                        </button>
                      )}
                      {msg.role === "agent" && (
                        <button
                          onClick={() => setFilesSheetOpen(true)}
                          className="opacity-0 group-hover:opacity-100 inline-flex items-center gap-1 px-1.5 py-0.5 rounded hover:bg-muted text-[10px] text-muted-foreground/60 hover:text-muted-foreground transition-all"
                          title="View task files"
                        >
                          <FolderOpen className="h-3 w-3" />
                          <span>Files</span>
                        </button>
                      )}
                    </div>
                  </div>
                </div>
              )
            );
            })()}

            {sending && (
              <div className="flex justify-start">
                <div className="bg-muted rounded-2xl rounded-bl-md px-4 py-3">
                  <div className="flex items-center gap-1">
                    <span className="typing-dot inline-block h-2 w-2 rounded-full bg-muted-foreground/60" style={{ animationDelay: "0ms" }} />
                    <span className="typing-dot inline-block h-2 w-2 rounded-full bg-muted-foreground/60" style={{ animationDelay: "200ms" }} />
                    <span className="typing-dot inline-block h-2 w-2 rounded-full bg-muted-foreground/60" style={{ animationDelay: "400ms" }} />
                  </div>
                </div>
              </div>
            )}

            <div ref={messagesEndRef} />
          </div>
        </div>

        {/* Input */}
        <div className="shrink-0 px-4 pb-6 pt-2">
          <div className="mx-auto max-w-2xl relative">
            {slashOpen && filteredSkills.length > 0 && (
              <SlashMenu
                skills={filteredSkills}
                activeIndex={slashIndex}
                onHover={setSlashIndex}
                onSelect={selectSkill}
              />
            )}
            <div className="rounded-xl border border-border bg-card px-4 py-3 focus-within:ring-2 focus-within:ring-ring/20 transition-shadow">
              {attachments.length > 0 && (
                <div className="flex flex-wrap gap-2 mb-2 pb-2 border-b border-border/60">
                  {attachments.map((f, i) => {
                    const preview = attachmentPreviews[i];
                    if (preview) {
                      return (
                        <div
                          key={`${f.name}-${i}`}
                          className="group relative h-14 w-14 overflow-hidden rounded-md border border-border bg-muted"
                        >
                          <button
                            type="button"
                            onClick={() => setLightboxSrc(preview)}
                            className="block h-full w-full cursor-zoom-in"
                            aria-label={`Preview ${f.name}`}
                          >
                            {/* eslint-disable-next-line @next/next/no-img-element */}
                            <img
                              src={preview}
                              alt={f.name}
                              className="h-full w-full object-cover"
                            />
                          </button>
                          <button
                            type="button"
                            onClick={() => removeAttachment(i)}
                            className="absolute right-0.5 top-0.5 flex h-4 w-4 items-center justify-center rounded-full bg-background/80 text-muted-foreground opacity-0 transition group-hover:opacity-100 hover:text-foreground"
                            aria-label="Remove attachment"
                          >
                            <X className="h-3 w-3" />
                          </button>
                        </div>
                      );
                    }
                    return (
                      <div
                        key={`${f.name}-${i}`}
                        className="flex items-center gap-1.5 rounded-md bg-muted/60 pl-2 pr-1 py-1 text-xs"
                      >
                        <Paperclip className="h-3 w-3 text-muted-foreground" />
                        <span className="max-w-[160px] truncate">{f.name}</span>
                        <button
                          type="button"
                          onClick={() => removeAttachment(i)}
                          className="p-0.5 rounded hover:bg-muted-foreground/15 text-muted-foreground hover:text-foreground"
                          aria-label="Remove attachment"
                        >
                          <X className="h-3 w-3" />
                        </button>
                      </div>
                    );
                  })}
                </div>
              )}
              <div className="flex items-center gap-2">
                <label
                  className={`flex h-8 w-8 shrink-0 items-center justify-center rounded-lg text-muted-foreground transition-colors ${
                    !selectedAgent || sending
                      ? "opacity-50 cursor-not-allowed"
                      : "hover:bg-muted hover:text-foreground cursor-pointer"
                  }`}
                  aria-label="Attach files"
                >
                  <Paperclip className="h-4 w-4" />
                  <input
                    ref={fileInputRef}
                    type="file"
                    multiple
                    className="sr-only"
                    onChange={handleFilePick}
                    disabled={!selectedAgent || sending}
                  />
                </label>
                <textarea
                  ref={textareaRef}
                  value={input}
                  onChange={handleInputChange}
                  onKeyDown={handleKeyDown}
                  onBlur={() => setTimeout(() => setSlashOpen(false), 120)}
                  placeholder={
                    selectedAgent
                      ? `Message ${agentName || selectedAgent}... ("/" to pick a skill)`
                      : "Select an agent first"
                  }
                  disabled={!selectedAgent || sending}
                  rows={1}
                  className="flex-1 resize-none bg-transparent text-[15px] placeholder:text-muted-foreground/50 outline-none disabled:opacity-50"
                  style={{ maxHeight: 200, minHeight: 24 }}
                />
                {sending ? (
                  <Button
                    onClick={handleStop}
                    size="icon"
                    className="h-8 w-8 shrink-0 rounded-lg"
                    aria-label="Stop generating"
                  >
                    <Square className="h-3.5 w-3.5 fill-current" />
                  </Button>
                ) : (
                  <Button
                    onClick={handleSend}
                    disabled={(!input.trim() && attachments.length === 0) || !selectedAgent}
                    size="icon"
                    className="h-8 w-8 shrink-0 rounded-lg"
                    aria-label="Send message"
                  >
                    <Send className="h-4 w-4" />
                  </Button>
                )}
              </div>
            </div>
            <p className="text-center text-[11px] text-muted-foreground/50 mt-2">
              Enter to send, Shift+Enter for new line
            </p>
          </div>
        </div>
        {lightboxSrc && (
          <div
            className="fixed inset-0 z-50 flex items-center justify-center bg-black/80 p-6 cursor-zoom-out"
            onClick={() => setLightboxSrc(null)}
            role="dialog"
            aria-modal="true"
            aria-label="Image preview"
          >
            {/* eslint-disable-next-line @next/next/no-img-element */}
            <img
              src={lightboxSrc}
              alt="Preview"
              className="max-h-full max-w-full rounded-lg shadow-2xl"
              onClick={(e) => e.stopPropagation()}
            />
            <button
              type="button"
              onClick={() => setLightboxSrc(null)}
              className="absolute right-4 top-4 flex h-9 w-9 items-center justify-center rounded-full bg-background/80 text-foreground hover:bg-background"
              aria-label="Close preview"
            >
              <X className="h-5 w-5" />
            </button>
          </div>
        )}
      </div>
      {filesSheetOpen && selectedAgent && sessionId && (
        <SessionFilesPanel
          agentId={selectedAgent}
          sessionId={sessionId}
          onClose={() => setFilesSheetOpen(false)}
        />
      )}
    </div>
  );
}

interface ChatHeaderTitleProps {
  title: string;
  fallback: string;
  onSave: (next: string) => void | Promise<void>;
}

/** Editable chat title rendered into the global sticky header via
 *  usePageHeader. Click / focus to edit; Enter or blur commits. */
function ChatHeaderTitle({ title, fallback, onSave }: ChatHeaderTitleProps) {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(title);
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (!editing) setDraft(title);
  }, [title, editing]);

  useEffect(() => {
    if (editing) inputRef.current?.select();
  }, [editing]);

  const commit = () => {
    setEditing(false);
    const next = draft.trim();
    if (!next || next === title) return;
    onSave(next);
  };

  if (editing) {
    // field-sizing: content grows the input to match its text; min width
    // keeps it reasonable right after entering edit mode even if the
    // current title is very short.
    return (
      <input
        ref={inputRef}
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        onKeyDown={(e) => {
          // Ignore Enter while the user is mid-composition (CJK IME). Both
          // conditions matter: isComposing is the modern signal, keyCode 229
          // is the legacy flag some browsers (and macOS Pinyin in particular)
          // still emit without isComposing set.
          if (e.nativeEvent.isComposing || e.keyCode === 229) return;
          if (e.key === "Enter") {
            e.preventDefault();
            commit();
          } else if (e.key === "Escape") {
            setDraft(title);
            setEditing(false);
          }
        }}
        onBlur={commit}
        style={{ fieldSizing: "content" } as React.CSSProperties}
        className="h-7 min-w-[8ch] max-w-[40ch] rounded-md bg-transparent px-2 text-sm outline-none ring-1 ring-border focus:ring-primary/40"
      />
    );
  }

  return (
    <button
      onClick={() => {
        setDraft(title);
        setEditing(true);
      }}
      // Cap the title width responsively so a long auto-summary doesn't
      // push the whole header off-screen on small viewports. The
      // arbitrary `min(...)` keeps narrow widths on phones (60vw) while
      // capping at ~32rem on desktop; sm:/md: bumps give intermediate
      // breakpoints a deterministic width too.
      className="group flex min-w-0 max-w-[min(60vw,18rem)] sm:max-w-[24rem] md:max-w-[28rem] lg:max-w-[32rem] items-center gap-1.5 rounded-md px-2 py-1 text-sm text-foreground hover:bg-muted/50"
      title={title || fallback}
    >
      <span className="truncate">{title || fallback}</span>
      <Pencil className="h-3 w-3 shrink-0 text-muted-foreground/50 opacity-0 transition-opacity group-hover:opacity-100" />
    </button>
  );
}

/** Renders a group of tool calls as a collapsible summary. */
function ToolCallGroup({ msg, surfacedSrcs, agentId, sessionId }: { msg: ChatMessage; surfacedSrcs?: ReadonlySet<string>; agentId: string; sessionId: string }) {
  const [groupOpen, setGroupOpen] = useState(false);
  const [expandedTool, setExpandedTool] = useState<Record<string, boolean>>({});

  const tools = msg.toolCalls || [];
  const doneCount = tools.filter((tc) => tc.result != null).length;
  const allDone = doneCount === tools.length;

  const toggleTool = (id: string) =>
    setExpandedTool((prev) => ({ ...prev, [id]: !prev[id] }));

  return (
    <div className="flex justify-start">
      <div className="max-w-[85%] space-y-2">
        {/* Content before tools */}
        {msg.content && (
          <div className="bg-muted rounded-2xl rounded-bl-md px-4 py-2.5">
            <div className="text-[15px] leading-relaxed prose prose-sm dark:prose-invert max-w-none prose-p:my-1">
              {renderContentWithDataImages(msg.content, surfacedSrcs) ?? (
                <ReactMarkdown remarkPlugins={[remarkGfm]} urlTransform={makeUrlTransform(agentId, sessionId)}>
                  {msg.content}
                </ReactMarkdown>
              )}
            </div>
          </div>
        )}
        {/* Collapsed tool group summary */}
        <div className="rounded-lg border border-border bg-card/50 overflow-hidden">
          <button
            onClick={() => setGroupOpen(!groupOpen)}
            className="flex w-full items-center gap-2 px-3 py-2 text-xs hover:bg-muted/50 transition-colors"
          >
            {!allDone ? (
              <div className="h-3.5 w-3.5 shrink-0 rounded-full border-2 border-amber-500 border-t-transparent animate-spin" />
            ) : (
              <Wrench className="h-3.5 w-3.5 text-amber-500 shrink-0" />
            )}
            <span className="font-medium text-foreground">
              {allDone
                ? `Executed ${tools.length} tool${tools.length > 1 ? "s" : ""}`
                : `Running tools (${doneCount}/${tools.length})...`}
            </span>
            <span className="text-muted-foreground/60 text-[11px] flex-1 text-left truncate">
              {tools.map((tc) => tc.name).join(", ")}
            </span>
            {groupOpen ? (
              <ChevronDown className="h-3.5 w-3.5 text-muted-foreground shrink-0" />
            ) : (
              <ChevronRight className="h-3.5 w-3.5 text-muted-foreground shrink-0" />
            )}
          </button>

          {groupOpen && (
            <div className="border-t border-border">
              {tools.map((tc) => (
                <div key={tc.id} className="border-b border-border last:border-b-0">
                  <button
                    onClick={() => toggleTool(tc.id)}
                    className="flex w-full items-center gap-2 px-3 py-1.5 text-xs hover:bg-muted/30 transition-colors"
                  >
                    {tc.result === undefined ? (
                      <div className="h-3 w-3 shrink-0 rounded-full border-2 border-amber-500/60 border-t-transparent animate-spin" />
                    ) : (
                      <Check className="h-3 w-3 text-emerald-500 shrink-0" />
                    )}
                    <span className="font-medium text-foreground">{tc.name}</span>
                    {tc.metadata?.sandbox && (
                      <span
                        className="flex items-center gap-0.5 rounded bg-emerald-500/10 px-1 py-0.5 text-[10px] font-medium text-emerald-600 dark:text-emerald-400"
                        title="Executed inside a sandboxed container"
                      >
                        <ShieldCheck className="h-2.5 w-2.5" />
                        sandbox
                      </span>
                    )}
                    <span className="text-muted-foreground/50 font-mono truncate flex-1 text-left text-[11px]">
                      {(() => {
                        try {
                          const args = JSON.parse(tc.arguments);
                          return Object.values(args).join(", ");
                        } catch {
                          return tc.arguments;
                        }
                      })()}
                    </span>
                    {expandedTool[tc.id] ? (
                      <ChevronDown className="h-3 w-3 text-muted-foreground/50 shrink-0" />
                    ) : (
                      <ChevronRight className="h-3 w-3 text-muted-foreground/50 shrink-0" />
                    )}
                  </button>
                  {expandedTool[tc.id] && (
                    <div className="px-3 py-2 space-y-2 bg-muted/20">
                      <div>
                        <p className="text-[10px] font-medium text-muted-foreground uppercase mb-1">Input</p>
                        <pre className="text-xs font-mono bg-muted/50 rounded p-2 overflow-x-auto whitespace-pre-wrap break-all max-h-40">
                          {(() => {
                            try { return JSON.stringify(JSON.parse(tc.arguments), null, 2); }
                            catch { return tc.arguments; }
                          })()}
                        </pre>
                      </div>
                      {tc.result != null ? (
                        <div>
                          <p className="text-[10px] font-medium text-muted-foreground uppercase mb-1">Output</p>
                          <pre className="text-xs font-mono bg-muted/50 rounded p-2 overflow-x-auto whitespace-pre-wrap break-all max-h-60">
                            {tc.result.length > 2000 ? tc.result.slice(0, 2000) + "..." : tc.result}
                          </pre>
                        </div>
                      ) : (
                        <p className="text-xs text-muted-foreground/60 italic">Executing...</p>
                      )}
                    </div>
                  )}
                </div>
              ))}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

/** File extension → icon + preview kind. */
function fileKind(path: string): { icon: typeof File; preview: "image" | "pdf" | "markdown" | "text" | "none" } {
  const ext = path.toLowerCase().split(".").pop() || "";
  if (["png", "jpg", "jpeg", "gif", "svg", "webp", "bmp", "ico"].includes(ext)) return { icon: ImageIcon, preview: "image" };
  if (ext === "pdf") return { icon: FileText, preview: "pdf" };
  if (ext === "md" || ext === "markdown") return { icon: FileText, preview: "markdown" };
  if (["mp4", "webm", "mov", "mkv"].includes(ext)) return { icon: Film, preview: "none" };
  if (["mp3", "wav", "ogg", "flac", "m4a"].includes(ext)) return { icon: Music, preview: "none" };
  if (["js", "ts", "tsx", "jsx", "py", "go", "rs", "c", "cpp", "h", "java", "rb", "sh", "json", "yaml", "yml", "toml", "xml", "html", "css"].includes(ext))
    return { icon: FileCode, preview: "text" };
  if (["txt", "csv", "log"].includes(ext)) return { icon: FileText, preview: "text" };
  return { icon: File, preview: "none" };
}

function formatBytes(n?: number): string {
  if (n === undefined) return "";
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}

function fileUrl(agentId: string, path: string, download: boolean): string {
  const token = getAuthToken();
  const encoded = path.split("/").map(encodeURIComponent).join("/");
  const params = new URLSearchParams();
  if (download) params.set("download", "1");
  if (token) params.set("token", token);
  const qs = params.toString();
  return `/api/agents/${agentId}/files/${encoded}${qs ? "?" + qs : ""}`;
}

function zipUrl(agentId: string, sessionId: string): string {
  const token = getAuthToken();
  const params = new URLSearchParams();
  if (sessionId) params.set("sessionId", sessionId);
  if (token) params.set("token", token);
  const qs = params.toString();
  return `/api/agents/${agentId}/files.zip${qs ? "?" + qs : ""}`;
}

function FilesPanel({ agentId, files }: { agentId: string; files: ProducedFile[] }) {
  const [previewing, setPreviewing] = useState<ProducedFile | null>(null);
  return (
    <>
      <div className="mt-2 space-y-1.5 max-w-[85%]">
        <p className="text-[11px] font-medium uppercase tracking-wider text-muted-foreground/70">
          Your files
        </p>
        <div className="flex flex-col gap-1.5">
          {files.map((f) => {
            const { icon: Icon } = fileKind(f.path);
            const basename = f.path.split("/").pop() || f.path;
            const downloadUrl = fileUrl(agentId, f.path, true);
            return (
              <div
                key={f.path}
                className="group flex items-center gap-2.5 rounded-lg border border-border bg-card/50 px-3 py-2 hover:bg-card/80 transition-colors"
              >
                <Icon className="h-4 w-4 text-muted-foreground shrink-0" />
                <button
                  onClick={() => setPreviewing(f)}
                  className="flex-1 min-w-0 text-left"
                  title="Open preview"
                >
                  <div className="text-sm font-medium text-foreground truncate">{basename}</div>
                  {f.size !== undefined && (
                    <div className="text-[11px] text-muted-foreground/70">{formatBytes(f.size)}</div>
                  )}
                </button>
                <a
                  href={downloadUrl}
                  className="p-1.5 rounded-md text-muted-foreground hover:text-foreground hover:bg-muted/50 transition-colors"
                  title="Download"
                >
                  <Download className="h-4 w-4" />
                </a>
              </div>
            );
          })}
        </div>
      </div>
      {previewing && (
        <FilePreview
          agentId={agentId}
          file={previewing}
          onClose={() => setPreviewing(null)}
        />
      )}
    </>
  );
}

const FILES_PANEL_MIN = 280;
const FILES_PANEL_MAX = 640;
const FILES_PANEL_DEFAULT = 416;
const FILES_PANEL_KEY = "chat:filesPanelWidth";

function SessionFilesPanel({
  agentId,
  sessionId,
  onClose,
}: {
  agentId: string;
  sessionId: string;
  onClose: () => void;
}) {
  const [files, setFiles] = useState<WorkspaceFile[]>([]);
  const [loading, setLoading] = useState(false);
  const [previewing, setPreviewing] = useState<ProducedFile | null>(null);
  const [width, setWidth] = useState<number>(() => {
    if (typeof window === "undefined") return FILES_PANEL_DEFAULT;
    const stored = Number(window.localStorage.getItem(FILES_PANEL_KEY));
    if (Number.isFinite(stored) && stored >= FILES_PANEL_MIN && stored <= FILES_PANEL_MAX) {
      return stored;
    }
    return FILES_PANEL_DEFAULT;
  });
  const [resizing, setResizing] = useState(false);

  useEffect(() => {
    if (!resizing) return;
    const handleMove = (e: MouseEvent) => {
      const next = Math.min(
        FILES_PANEL_MAX,
        Math.max(FILES_PANEL_MIN, window.innerWidth - e.clientX),
      );
      setWidth(next);
    };
    const handleUp = () => {
      setResizing(false);
      try {
        window.localStorage.setItem(FILES_PANEL_KEY, String(width));
      } catch { /* ignore quota errors */ }
    };
    window.addEventListener("mousemove", handleMove);
    window.addEventListener("mouseup", handleUp);
    document.body.style.cursor = "col-resize";
    document.body.style.userSelect = "none";
    return () => {
      window.removeEventListener("mousemove", handleMove);
      window.removeEventListener("mouseup", handleUp);
      document.body.style.cursor = "";
      document.body.style.userSelect = "";
    };
  }, [resizing, width]);

  const refresh = useCallback(async () => {
    if (!agentId || !sessionId) return;
    setLoading(true);
    try {
      const list = await listAgentFiles(agentId, sessionId);
      const cleaned = list
        .filter((f) => !isSystemFile(f.path))
        .sort((a, b) => (b.modTime || 0) - (a.modTime || 0));
      setFiles(cleaned);
    } finally {
      setLoading(false);
    }
  }, [agentId, sessionId]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  return (
    <>
      <aside
        style={{ width }}
        className="relative z-30 hidden md:flex shrink-0 flex-col border-l border-border bg-background -mt-12 h-screen"
      >
        <div
          onMouseDown={(e) => { e.preventDefault(); setResizing(true); }}
          className={`absolute -left-1 top-0 bottom-0 w-2 cursor-col-resize z-10 group ${resizing ? "" : ""}`}
          title="Drag to resize"
        >
          <div
            className={`absolute inset-y-0 left-1/2 w-px -translate-x-1/2 transition-colors ${
              resizing ? "bg-primary" : "bg-transparent group-hover:bg-primary/40"
            }`}
          />
        </div>
        <div className="flex items-center justify-between gap-2 px-4 h-12 border-b border-border">
          <div className="flex items-center gap-2 text-sm font-medium">
            <FolderOpen className="h-4 w-4" />
            Files
          </div>
          <div className="flex items-center gap-1">
            <a
              href={files.length > 0 ? zipUrl(agentId, sessionId) : undefined}
              aria-disabled={files.length === 0}
              className={`p-1.5 rounded-md transition-colors ${
                files.length === 0
                  ? "text-muted-foreground/40 pointer-events-none"
                  : "text-muted-foreground hover:text-foreground hover:bg-muted/50"
              }`}
              title="Download all as zip"
            >
              <Download className="h-4 w-4" />
            </a>
            <button
              onClick={refresh}
              disabled={loading}
              className="p-1.5 rounded-md text-muted-foreground hover:text-foreground hover:bg-muted/50 transition-colors disabled:opacity-50"
              title="Refresh"
            >
              <RefreshCw className={`h-4 w-4 ${loading ? "animate-spin" : ""}`} />
            </button>
            <button
              onClick={onClose}
              className="p-1.5 rounded-md text-muted-foreground hover:text-foreground hover:bg-muted/50 transition-colors"
              title="Close"
            >
              <X className="h-4 w-4" />
            </button>
          </div>
        </div>
        <div className="flex-1 overflow-y-auto p-2">
          {!loading && files.length === 0 ? (
            <p className="px-3 py-8 text-center text-sm text-muted-foreground">
              No files in this session yet.
            </p>
          ) : (
            <div className="flex flex-col">
              <div className="grid grid-cols-[1fr_auto_auto] gap-3 px-3 py-2 text-[11px] font-medium uppercase tracking-wider text-muted-foreground/70 border-b">
                <span>Name</span>
                <span>Modified</span>
                <span>Size</span>
              </div>
              {files.map((f) => {
                const { icon: Icon } = fileKind(f.path);
                const basename = f.path.split("/").pop() || f.path;
                const downloadUrl = fileUrl(agentId, f.path, true);
                return (
                  <div
                    key={f.path}
                    className="group grid grid-cols-[1fr_auto_auto] items-center gap-3 px-3 py-2 hover:bg-muted/40 rounded-md transition-colors"
                  >
                    <button
                      onClick={() => setPreviewing({ path: f.path, size: f.size })}
                      className="flex items-center gap-2 min-w-0 text-left"
                      title="Open preview"
                    >
                      <Icon className="h-4 w-4 text-muted-foreground shrink-0" />
                      <span className="text-sm text-foreground truncate">{basename}</span>
                    </button>
                    <span className="text-[11px] text-muted-foreground/70 whitespace-nowrap">
                      {formatRelativeTime(f.modTime)}
                    </span>
                    <a
                      href={downloadUrl}
                      className="text-[11px] text-muted-foreground/70 whitespace-nowrap hover:text-foreground"
                      title="Download"
                    >
                      {formatBytes(f.size)}
                    </a>
                  </div>
                );
              })}
            </div>
          )}
        </div>
      </aside>
      {previewing && (
        <FilePreview
          agentId={agentId}
          file={previewing}
          onClose={() => setPreviewing(null)}
        />
      )}
    </>
  );
}

function formatRelativeTime(ts?: number): string {
  if (!ts) return "—";
  const d = new Date(ts * 1000);
  const now = Date.now();
  const diff = now - d.getTime();
  if (diff < 60_000) return "just now";
  if (diff < 3600_000) return `${Math.floor(diff / 60_000)}m ago`;
  if (diff < 86400_000) return `${Math.floor(diff / 3600_000)}h ago`;
  if (diff < 7 * 86400_000) return `${Math.floor(diff / 86400_000)}d ago`;
  return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

function FilePreview({ agentId, file, onClose }: { agentId: string; file: ProducedFile; onClose: () => void }) {
  const { preview } = fileKind(file.path);
  const src = fileUrl(agentId, file.path, false);
  const downloadUrl = fileUrl(agentId, file.path, true);
  const basename = file.path.split("/").pop() || file.path;
  const [text, setText] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (preview !== "markdown" && preview !== "text") return;
    let cancelled = false;
    fetch(src)
      .then((r) => { if (!r.ok) throw new Error(`HTTP ${r.status}`); return r.text(); })
      .then((t) => { if (!cancelled) setText(t); })
      .catch((e) => { if (!cancelled) setError(String(e)); });
    return () => { cancelled = true; };
  }, [src, preview]);

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-background/80 backdrop-blur-sm p-4" onClick={onClose}>
      <div className="flex h-[85vh] w-full max-w-4xl flex-col rounded-xl border border-border bg-card shadow-2xl" onClick={(e) => e.stopPropagation()}>
        <div className="flex items-center justify-between border-b border-border px-4 py-3 shrink-0">
          <div className="flex items-center gap-2 min-w-0">
            <FileText className="h-4 w-4 text-muted-foreground shrink-0" />
            <span className="font-medium text-sm truncate">{basename}</span>
            {file.size !== undefined && (
              <span className="text-[11px] text-muted-foreground shrink-0">{formatBytes(file.size)}</span>
            )}
          </div>
          <div className="flex items-center gap-1 shrink-0">
            <a
              href={downloadUrl}
              className="rounded-md p-1.5 text-muted-foreground hover:text-foreground hover:bg-muted/50"
              title="Download"
            >
              <Download className="h-4 w-4" />
            </a>
            <button
              onClick={onClose}
              className="rounded-md p-1.5 text-muted-foreground hover:text-foreground hover:bg-muted/50"
              title="Close"
            >
              <X className="h-4 w-4" />
            </button>
          </div>
        </div>
        <div className="flex-1 overflow-auto p-4 min-h-0">
          {preview === "image" && (
            <img src={src} alt={basename} className="max-w-full max-h-full mx-auto object-contain" />
          )}
          {preview === "pdf" && (
            <iframe src={src} className="h-full w-full border-0" title={basename} />
          )}
          {preview === "markdown" && (
            error ? <p className="text-sm text-destructive">Failed to load: {error}</p>
            : text === null ? <p className="text-sm text-muted-foreground">Loading…</p>
            : (
              <div className="prose prose-sm dark:prose-invert max-w-none">
                <ReactMarkdown remarkPlugins={[remarkGfm]}>{text}</ReactMarkdown>
              </div>
            )
          )}
          {preview === "text" && (
            error ? <p className="text-sm text-destructive">Failed to load: {error}</p>
            : text === null ? <p className="text-sm text-muted-foreground">Loading…</p>
            : (
              <pre className="text-xs font-mono whitespace-pre-wrap break-all bg-muted/30 rounded p-3">{text}</pre>
            )
          )}
          {preview === "none" && (
            <div className="flex flex-col items-center justify-center h-full gap-3 text-center">
              <File className="h-12 w-12 text-muted-foreground/50" />
              <p className="text-sm text-muted-foreground">Preview not available for this file type.</p>
              <a href={downloadUrl} className="inline-flex items-center gap-2 rounded-md bg-primary px-3 py-1.5 text-xs font-medium text-primary-foreground hover:bg-primary/90">
                <Download className="h-3.5 w-3.5" /> Download
              </a>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

function SlashMenu({
  skills,
  activeIndex,
  onHover,
  onSelect,
}: {
  skills: SkillInfo[];
  activeIndex: number;
  onHover: (i: number) => void;
  onSelect: (s: SkillInfo) => void;
}) {
  return (
    <div className="absolute bottom-full left-0 right-0 mb-2 rounded-xl border border-border bg-popover shadow-lg overflow-hidden z-20">
      <div className="max-h-[320px] overflow-y-auto py-1">
        {skills.map((s, i) => (
          <button
            key={s.name}
            // onMouseDown fires before the textarea's onBlur so the click
            // isn't swallowed by the blur-driven menu close.
            onMouseDown={(e) => {
              e.preventDefault();
              onSelect(s);
            }}
            onMouseEnter={() => onHover(i)}
            className={`w-full flex items-start gap-3 px-3 py-2 text-left transition-colors ${
              i === activeIndex ? "bg-muted/60" : "hover:bg-muted/40"
            }`}
          >
            <Puzzle className="h-4 w-4 mt-0.5 shrink-0 text-muted-foreground" />
            <div className="flex-1 min-w-0">
              <div className="flex items-center gap-2">
                <p className="text-sm font-medium truncate">{s.name}</p>
                <span className="text-[10px] uppercase tracking-wider text-muted-foreground/70">
                  {s.type || "skill"}
                </span>
              </div>
              {s.description && (
                <p className="text-xs text-muted-foreground line-clamp-1">
                  {s.description}
                </p>
              )}
            </div>
          </button>
        ))}
      </div>
      <Link
        href="/skills/"
        className="flex items-center gap-2 border-t border-border px-3 py-2 text-xs text-muted-foreground hover:text-foreground hover:bg-muted/30 transition-colors"
      >
        <SlidersHorizontal className="h-3.5 w-3.5" />
        Manage Skills
      </Link>
    </div>
  );
}
