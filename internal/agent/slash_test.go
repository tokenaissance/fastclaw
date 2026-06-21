package agent

import (
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

func TestSlashRequiresAdmin(t *testing.T) {
	tests := []struct {
		name     string
		cmd      string
		peerKind string
		want     bool
	}{
		{name: "new in dm", cmd: "/new", peerKind: "dm", want: false},
		{name: "reset in dm", cmd: "/reset", peerKind: "dm", want: false},
		{name: "new with legacy empty peer kind", cmd: "/new", want: false},
		{name: "new in group", cmd: "/new", peerKind: "group", want: true},
		{name: "reset in group", cmd: "/reset", peerKind: "group", want: true},
		{name: "model in dm", cmd: "/model", peerKind: "dm", want: true},
		{name: "read command", cmd: "/status", peerKind: "group", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := bus.InboundMessage{PeerKind: tt.peerKind}
			if got := slashRequiresAdmin(tt.cmd, msg); got != tt.want {
				t.Fatalf("slashRequiresAdmin(%q, peer=%q) = %v, want %v", tt.cmd, tt.peerKind, got, tt.want)
			}
		})
	}
}
