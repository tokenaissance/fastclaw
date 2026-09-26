package agent

import (
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

// Per-chatter state (USER.md / MEMORY.md rows, per-user skills dir, the
// chatter_user_id stamped on sessions) must never be keyed on the
// sentinel UserID a scheduled turn carries.
//
// Production repro (2026-09-14, agent agt_cda27bb…): the agent's own
// `*/5` cron job re-entered a web session as UserID="cron". The gateway
// minted the synthetic chatter u_cd824b9bc93943e40f84 (external id
// "web:cron"), so edit_file('MEMORY.md') looked up
// (agent, u_cd824…, MEMORY.md) — a row that does not exist — and the
// turn failed with "system file get: store: not found" while the very
// same edit succeeded in the operator's own turn in that same session.
func TestChatterUserID_AutonomousTurnsActAsOwner(t *testing.T) {
	a := &Agent{ownerUserID: "u_owner"}

	cases := []struct {
		name string
		msg  bus.InboundMessage
		want string
	}{
		{
			name: "cron fire uses the job owner",
			msg:  bus.InboundMessage{Channel: "web", UserID: "cron", OwnerUserID: "u_owner", Source: bus.SourceCron},
			want: "u_owner",
		},
		{
			name: "cron fire without an owner falls back to the agent owner",
			msg:  bus.InboundMessage{Channel: "web", UserID: "cron", Source: bus.SourceCron},
			want: "u_owner",
		},
		{
			name: "cron job owned by another account keeps that account",
			msg:  bus.InboundMessage{Channel: "web", UserID: "cron", OwnerUserID: "u_binder", Source: bus.SourceCron},
			want: "u_binder",
		},
		{
			// The job knows who scheduled it. Routing still follows
			// OwnerUserID; the per-chatter state the tick touches must
			// not — otherwise a visitor's reminder edits the owner's
			// MEMORY.md.
			name: "cron job acts for the chatter who created it",
			msg: bus.InboundMessage{
				Channel: "web", UserID: "cron",
				OwnerUserID: "u_owner", CreatorUserID: "u_visitor",
				Source: bus.SourceCron,
			},
			want: "u_visitor",
		},
		{
			name: "heartbeat tick has no owner field at all",
			msg:  bus.InboundMessage{Channel: "heartbeat", ChatID: "heartbeat_sakurain", UserID: "system", Source: bus.SourceHeartbeat},
			want: "u_owner",
		},
		{
			name: "goal continuation acts for the goal owner",
			msg:  bus.InboundMessage{Channel: "web", UserID: "goal", OwnerUserID: "u_owner", Source: bus.SourceGoalContext},
			want: "u_owner",
		},
		{
			name: "real user turn keeps its own identity",
			msg:  bus.InboundMessage{Channel: "web", UserID: "u_human", OwnerUserID: "u_owner"},
			want: "u_human",
		},
		{
			name: "IM chatter keeps its minted u_xxx",
			msg:  bus.InboundMessage{Channel: "telegram", UserID: "u_chatter", OwnerUserID: "u_owner"},
			want: "u_chatter",
		},
		{
			name: "legacy turn without a user falls back to the owner",
			msg:  bus.InboundMessage{Channel: "telegram"},
			want: "u_owner",
		},
		{
			name: "sub-agent spawn inherits the parent chatter",
			msg:  bus.InboundMessage{Channel: "web", UserID: "u_parent", OwnerUserID: "u_owner", Source: bus.SourceSubAgent},
			want: "u_parent",
		},
		{
			// The webhook server stamps UserID "webhook" and never sets
			// Source, so the sentinel list has to catch it too.
			name: "webhook post acts for the token's owner",
			msg:  bus.InboundMessage{Channel: "webhook", ChatID: "webhook-default", UserID: "webhook", OwnerUserID: "u_owner"},
			want: "u_owner",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := a.chatterUserID(tc.msg); got != tc.want {
				t.Fatalf("chatterUserID(%+v) = %q, want %q", tc.msg, got, tc.want)
			}
		})
	}
}
