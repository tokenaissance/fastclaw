package tools

import "testing"

// codingRootScope must collapse the session segment so file tools address
// the project root the dev server serves; off, it preserves per-chat
// isolation. This is the invariant the coding-agent preview relies on.
func TestScopeSessionIDCodingRoot(t *testing.T) {
	r := NewRegistry(t.TempDir(), t.TempDir())
	r.SetSessionID("sess-123")

	if got := r.scopeSessionID(); got != "sess-123" {
		t.Fatalf("default mode: want session segment preserved, got %q", got)
	}

	r.SetCodingRootScope(true)
	if got := r.scopeSessionID(); got != "" {
		t.Fatalf("coding-root mode: want empty session segment, got %q", got)
	}

	r.SetCodingRootScope(false)
	if got := r.scopeSessionID(); got != "sess-123" {
		t.Fatalf("after disabling: want session segment restored, got %q", got)
	}
}

func TestWsPathSubdirRedirect(t *testing.T) {
	r := NewRegistry(t.TempDir(), t.TempDir())

	// No subdir → passthrough.
	if got := r.wsPath("src/x.tsx"); got != "src/x.tsx" {
		t.Fatalf("no subdir: want passthrough, got %q", got)
	}

	r.SetCodingSubdir("app")
	cases := map[string]string{
		"src/x.tsx":     "app/src/x.tsx", // bare path gets prefixed
		"app/src/x.tsx": "app/src/x.tsx", // already-prefixed is idempotent
		"/src/x.tsx":    "app/src/x.tsx", // leading slash tolerated
		"app":           "app",           // the subdir itself
		"package.json":  "app/package.json",
	}
	for in, want := range cases {
		if got := r.wsPath(in); got != want {
			t.Fatalf("wsPath(%q): want %q, got %q", in, want, got)
		}
	}

	r.SetCodingSubdir("")
	if got := r.wsPath("src/x.tsx"); got != "src/x.tsx" {
		t.Fatalf("after disabling: want passthrough, got %q", got)
	}
}

func TestEffectiveUserIDFallback(t *testing.T) {
	r := NewRegistry(t.TempDir(), t.TempDir())
	r.SetOwnerUserID("owner-1")
	if got := r.EffectiveUserID(); got != "owner-1" {
		t.Fatalf("no chatter: want owner fallback, got %q", got)
	}
	r.SetChatterUserID("chatter-9")
	if got := r.EffectiveUserID(); got != "chatter-9" {
		t.Fatalf("with chatter: want chatter, got %q", got)
	}
}
