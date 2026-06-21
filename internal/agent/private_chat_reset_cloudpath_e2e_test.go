package agent

// e2e for commit f3f51a6 "fix(agent): allow private chat session reset".
//
// Cloud zero-impact rationale: the slash-command admission gate lives in
// internal/agent/slash.go behind the agent loop. Cloud (Next.js app)
// talks to FastAgent through the /api/fastagent proxy and posts /chat
// messages; /new and /reset arrive as ordinary message text and are
// resolved entirely by the Go runtime. No endpoint, response shape, or
// auth change.
//
// The change: write-mode slash commands were previously owner/admin-only
// on EVERY channel. A chatter in a private DM could never clear their own
// session. The new slashRequiresAdmin helper keeps agent-wide mutations
// (/undo /retry /compact /model /personality) admin-gated, but lets a
// NON-group chatter run /new and /reset — those mint a fresh session
// scoped to their own (channel, account, chat) triple, so they can't
// affect anyone else. Group-chat /new and /reset stay admin-gated
// (shared history).
//
// This test drives handleSlashCommand directly (bare *Agent) to pin the
// combined gate decision: slashRequiresAdmin AND the fork's isAdminChatter
// (incl. its web/api owner-check guard). The web /new path returns early
// ("__NEW_SESSION__") without touching sessions, so no session manager is
// needed for the allowed-DM assertions; the group-admin path wires a real
// session.Manager to prove a group admin still mints a fresh session.

import (
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/session"
)

// The admin denial is a distinct string ("🔒 `cmd` 只有 agent owner /
// admin 能用…") — NOT the help footer which also contains 🔒.
const adminDenialMarker = "只有 agent owner / admin 能用"

func denied(t *testing.T, r slashResult, what string) {
	t.Helper()
	if !r.handled || !strings.Contains(r.reply, adminDenialMarker) {
		t.Fatalf("%s: expected admin denial, got handled=%v reply=%q", what, r.handled, r.reply)
	}
}

func TestAgent_PrivateChatReset_CloudPathE2E(t *testing.T) {
	// Owner u_owner; telegram has an allowlist [telegram_admin].
	a := &Agent{
		name:        "拽姐",
		ownerUserID: "u_owner",
		admins:      map[string][]string{"telegram": {"telegram_admin"}},
	}

	// ── Private chat /new and /reset are now allowed (the fix): web
	// carries FastAgent UUIDs (PeerKind "dm") and the web path returns
	// the frontend signal immediately, proving the gate let it through.
	for _, cmd := range []string{"/new", "/reset"} {
		r := a.handleSlashCommand(bus.InboundMessage{Channel: "web", UserID: "u_other", PeerKind: "dm", Text: cmd})
		if !r.handled || r.reply != "__NEW_SESSION__" {
			t.Errorf("%s on web as non-owner: got handled=%v reply=%q; want __NEW_SESSION__ (private chat allowed)",
				cmd, r.handled, r.reply)
		}
	}

	// Owner is unaffected on the same path.
	r := a.handleSlashCommand(bus.InboundMessage{Channel: "web", UserID: "u_owner", PeerKind: "dm", Text: "/new"})
	if r.reply != "__NEW_SESSION__" {
		t.Errorf("owner /new on web: reply=%q, want __NEW_SESSION__", r.reply)
	}

	// ── Group chat /new /reset stay admin-gated: a non-admin group
	// member is denied (shared history protection).
	denied(t, a.handleSlashCommand(bus.InboundMessage{Channel: "telegram", UserID: "anon_grp", PeerKind: "group", Text: "/new"}), "group /new non-admin")
	denied(t, a.handleSlashCommand(bus.InboundMessage{Channel: "telegram", UserID: "anon_grp", PeerKind: "group", Text: "/reset"}), "group /reset non-admin")

	// ── Agent-wide mutations stay admin-gated even in private chat:
	// a non-owner web chatter cannot /model the shared agent.
	denied(t, a.handleSlashCommand(bus.InboundMessage{Channel: "web", UserID: "u_other", PeerKind: "dm", Text: "/model gpt-4o-mini"}), "web /model non-owner")

	// ── Read-only commands stay open for any chatter (help mentions 🔒
	// in its footer but must NOT be the admin-denial reply).
	r = a.handleSlashCommand(bus.InboundMessage{Channel: "telegram", UserID: "anon_grp", PeerKind: "group", Text: "/help"})
	if !r.handled || strings.Contains(r.reply, adminDenialMarker) {
		t.Errorf("group /help non-admin: got handled=%v reply=%q; want open help", r.handled, r.reply)
	}
	if !strings.HasPrefix(r.reply, "⚡ FastAgent Commands") {
		t.Errorf("group /help non-admin: reply should be the help text, got %q", r.reply)
	}

	// ── Group admin still mints a fresh session: wire a real session
	// manager so the non-web path completes.
	a2 := &Agent{
		name:        "拽姐",
		ownerUserID: "u_owner",
		admins:      map[string][]string{"telegram": {"telegram_admin"}},
		sessions:    session.NewManager(t.TempDir()),
	}
	r = a2.handleSlashCommand(bus.InboundMessage{Channel: "telegram", UserID: "telegram_admin", PeerKind: "group", Text: "/new"})
	if !r.handled || r.reply == "" || strings.Contains(r.reply, "🔒") {
		t.Fatalf("group /new as admin: got handled=%v reply=%q; want fresh-session reply (not 🔒)", r.handled, r.reply)
	}
	if !strings.Contains(r.reply, "New session") {
		t.Errorf("group /new as admin reply=%q; want 'New session started'", r.reply)
	}
}
