package agent

// e2e for commit ee806d8 "fix(security): deny admin access on IM
// channels without admins config".
//
// Cloud zero-impact rationale: isAdminChatter is an agent-runtime
// internal (slash-command admission control) invoked only from
// internal/agent/slash.go. Cloud (Next.js app) talks to FastAgent
// through the /api/fastagent proxy and posts /chat messages; the admin
// gate lives entirely behind the agent loop, never in the proxy's
// request/response surface. No endpoint, response shape, or auth
// change.
//
// The vulnerability: on IM channels (discord/telegram/slack, ...) with
// NO configured admins allowlist, isAdminChatter previously returned
// `true` unconditionally — so any anonymous chatter on a public-facing
// IM channel could run write-mode slash commands (/new /undo /retry
// /compact /model /personality) that modify SOUL.md / IDENTITY.md via
// write_file/edit_file. The fix: an unconfigured/empty allowlist now
// falls through to an OWNERSHIP check (chatter's resolved FastAgent
// user_id == agent owner), denying anonymous chatters. A configured
// allowlist still gates by membership, and the fork's extra web/api
// guard (owner check, bypassing the allowlist) is preserved.
//
// isAdminChatter is a pure read of (msg.Channel, msg.UserID,
// a.ownerUserID, a.admins) — a bare *Agent with those fields wired
// exercises the exact admission decision the loop makes for every
// slash command.

import (
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

func TestAgent_AdminGate_CloudPathE2E(t *testing.T) {
	const owner = "u_owner"
	const telegramID = "114514" // a resolved FastAgent user_id on telegram

	a := &Agent{
		ownerUserID: owner,
		admins: map[string][]string{
			// telegram has an allowlist: only these IDs are admin.
			"telegram": {telegramID},
		},
		// "discord" / "slack" have NO admins entry (unconfigured).
		// "web" / "api" channels are handled by the fork guard.
	}

	msg := func(channel, uid string) bus.InboundMessage {
		return bus.InboundMessage{Channel: channel, UserID: uid}
	}

	t.Run("configured allowlist gates by membership", func(t *testing.T) {
		if !a.isAdminChatter(msg("telegram", telegramID)) {
			t.Error("allowlisted telegram ID must be admin")
		}
		if a.isAdminChatter(msg("telegram", "u_other")) {
			t.Error("non-allowlisted telegram chatter must NOT be admin")
		}
		// The owner is NOT in the telegram allowlist → must be denied
		// (the configured list is authoritative).
		if a.isAdminChatter(msg("telegram", owner)) {
			t.Error("owner must NOT bypass a configured allowlist")
		}
	})

	t.Run("unconfigured channel falls back to ownership (the fix)", func(t *testing.T) {
		// discord has no admins entry: the owner's resolved user_id is
		// admin, but an anonymous chatter is now DENIED (was `return
		// true` — any chatter could run write-mode commands).
		if !a.isAdminChatter(msg("discord", owner)) {
			t.Error("owner on unconfigured channel must be admin (ownership fallback)")
		}
		if a.isAdminChatter(msg("discord", "anon_discord_user")) {
			t.Error("anonymous chatter on unconfigured channel must be DENIED (the fix)")
		}
	})

	t.Run("configured-but-empty allowlist = unconfigured fallback", func(t *testing.T) {
		// slack has an entry but it's an empty list — same as absent.
		a2 := &Agent{ownerUserID: owner, admins: map[string][]string{"slack": {}}}
		if !a2.isAdminChatter(msg("slack", owner)) {
			t.Error("owner on empty allowlist must be admin (ownership fallback)")
		}
		if a2.isAdminChatter(msg("slack", "anon_slack_user")) {
			t.Error("anonymous chatter on empty allowlist must be DENIED (the fix)")
		}
	})

	t.Run("web/api channels use owner check (fork guard preserved)", func(t *testing.T) {
		// The fork adds an explicit web/api guard BEFORE the allowlist:
		// web/api carry FastAgent UUIDs directly, so owner check is
		// sufficient and the IM allowlist is irrelevant there.
		if !a.isAdminChatter(msg("web", owner)) {
			t.Error("owner on web must be admin")
		}
		if a.isAdminChatter(msg("web", "u_other")) {
			t.Error("non-owner on web must NOT be admin")
		}
		if a.isAdminChatter(msg("api", "u_other")) {
			t.Error("non-owner on api must NOT be admin")
		}
		// Even if the web chatter happens to match an allowlist ID,
		// the guard short-circuits to the owner check.
		if a.isAdminChatter(msg("api", telegramID)) {
			t.Error("allowlisted telegram ID on api must NOT be admin (guard is owner-check)")
		}
	})
}
