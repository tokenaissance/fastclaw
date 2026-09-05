package tools

import (
	"strings"
	"testing"
)

func TestIdentityFileBlockedRespectsCallerFlag(t *testing.T) {
	cases := []struct {
		path  string
		admin bool
		want  bool
	}{
		// Identity files: admin OK, chatter blocked.
		{"SOUL.md", false, true},
		{"SOUL.md", true, false},
		{"IDENTITY.md", false, true},
		{"BOOTSTRAP.md", false, true},
		{"AGENTS.md", false, true},
		{"TOOLS.md", false, true},
		{"HEARTBEAT.md", false, true},

		// Absolute path with identity basename still gets caught — model
		// often produces "/data/.fastagent/agents/<id>/SOUL.md" from the
		// Working Directory hint in the system prompt.
		{"/var/lib/fastagent/agents/xyz/SOUL.md", false, true},
		{"./SOUL.md", false, true},

		// Per-chatter files: NOT gated even for non-admin — they're the
		// chatter's own state and they should be able to inspect them.
		{"MEMORY.md", false, false},
		{"USER.md", false, false},
		{"KNOWLEDGE.md", false, false},

		// Workspace artifacts: never gated.
		{"report.md", false, false},
		{"notes/SOUL.md", false, false}, // nested, not the identity file
		{"output.png", false, false},
		{"", false, false},
	}
	for _, c := range cases {
		r := &Registry{callerIsAdmin: c.admin}
		got := r.identityFileBlocked(c.path)
		if got != c.want {
			t.Errorf("identityFileBlocked(%q) admin=%v = %v, want %v",
				c.path, c.admin, got, c.want)
		}
	}
}

func TestOwnerManagedFileWriteBlocked(t *testing.T) {
	regular := &Registry{callerIsAdmin: false}
	if !regular.ownerManagedFileWriteBlocked("KNOWLEDGE.md") {
		t.Fatalf("regular chatter should not be allowed to write KNOWLEDGE.md")
	}
	if regular.ownerManagedFileWriteBlocked("notes/KNOWLEDGE.md") {
		t.Fatalf("nested workspace file named KNOWLEDGE.md should not be owner-managed")
	}
	admin := &Registry{callerIsAdmin: true}
	if admin.ownerManagedFileWriteBlocked("KNOWLEDGE.md") {
		t.Fatalf("agent owner/admin should be allowed to write KNOWLEDGE.md")
	}
}

func TestIdentityFileRefusalMessageShape(t *testing.T) {
	// Lock the contract: the refusal must (1) be tool-output safe (no
	// outer quotes) and (2) tell the model NOT to paraphrase — without
	// that, a chatter who asks "summarize your SOUL" still wins.
	if !strings.HasPrefix(IdentityFileRefusal, "[refused:") {
		t.Errorf("refusal should be a [refused: …] tool message; got %q", IdentityFileRefusal)
	}
	if !strings.Contains(IdentityFileRefusal, "paraphrase") {
		t.Errorf("refusal must instruct the model not to paraphrase; got %q", IdentityFileRefusal)
	}
	if !strings.Contains(IdentityFileRefusal, "stay in character") {
		t.Errorf("refusal must tell the model to stay in character; got %q", IdentityFileRefusal)
	}
}

func TestNestedIdentityNameIsNotBlocked(t *testing.T) {
	// "notes/SOUL.md" is a workspace artifact named like an identity
	// file — it's the chatter's own note, not the agent's persona. The
	// gate must not block it.
	r := &Registry{callerIsAdmin: false}
	if r.identityFileBlocked("notes/SOUL.md") {
		t.Errorf("nested SOUL.md must not be gated as the agent's identity")
	}
}
