package gateway

import (
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

// Per-source turn budgets (docs/session-turn-integrity.md, P5): the two autonomous sources that
// are deliberately long — cron ticks and goal continuations — may differ, and only when the
// operator configured a value; everything else keeps the queue default (zero = "queue default").
//
// Falsification (run 2026-09-28 when the goal row changed): restore the `SourceCron`-only guard
// and `goal continuation` reads 0 again — the production value that cut a 36-iteration goal turn
// at 300 s.
func TestTaskTimeoutForSourcePolicy(t *testing.T) {
	configured := &Gateway{}
	configured.cronTaskTimeoutNs.Store(int64(15 * time.Minute))
	cases := []struct {
		name   string
		gw     *Gateway
		source string
		want   time.Duration
	}{
		{"cron with a configured budget", configured, bus.SourceCron, 15 * time.Minute},
		{"cron without one", &Gateway{}, bus.SourceCron, 0},
		{"user turn", configured, bus.SourceUser, 0},
		{"goal continuation with the same configured budget", configured, bus.SourceGoalContext, 15 * time.Minute},
		{"goal continuation without one", &Gateway{}, bus.SourceGoalContext, 0},
		{"heartbeat", configured, bus.SourceHeartbeat, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.gw.taskTimeoutFor(bus.InboundMessage{Source: tc.source})
			if got != tc.want {
				t.Fatalf("taskTimeoutFor(%q) = %s; want %s", tc.source, got, tc.want)
			}
		})
	}
}
