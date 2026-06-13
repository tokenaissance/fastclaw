package runtime

import (
	"path/filepath"
	"testing"
)

func TestScopeFor(t *testing.T) {
	m := NewManager(nil, "/home", "img", nil, "")
	const agent = "agt_1"

	t.Run("project wins", func(t *testing.T) {
		id, dir, err := m.scopeFor(agent, "proj_x", "sess_y")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if id != "proj_x" {
			t.Fatalf("scopeID: want proj_x, got %q", id)
		}
		if want := filepath.Join("/home", "workspaces", agent, "projects", "proj_x"); dir != want {
			t.Fatalf("dir: want %q, got %q", want, dir)
		}
	})

	t.Run("session fallback", func(t *testing.T) {
		id, dir, err := m.scopeFor(agent, "", "sess_y")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if id != "sess:sess_y" {
			t.Fatalf("scopeID: want namespaced sess:sess_y, got %q", id)
		}
		if want := filepath.Join("/home", "workspaces", agent, "sessions", "sess_y"); dir != want {
			t.Fatalf("dir: want %q, got %q", want, dir)
		}
	})

	t.Run("neither errors", func(t *testing.T) {
		if _, _, err := m.scopeFor(agent, "", ""); err == nil {
			t.Fatal("want error when neither project nor session given")
		}
	})
}

func TestPreviewURLHostSanitizesSessionScope(t *testing.T) {
	// Local mode: scope id never appears in the URL.
	local := NewManager(nil, "/home", "img", nil, "")
	if got := local.previewURL("sess:abc", 4321); got != "http://127.0.0.1:4321" {
		t.Fatalf("local previewURL: got %q", got)
	}
	// Gateway mode: the "sess:" colon must not leak into a hostname.
	gw := NewManager(nil, "/home", "img", nil, "https://{project}.preview.example.com")
	if got := gw.previewURL("sess:abc", 0); got != "https://sess-abc.preview.example.com" {
		t.Fatalf("gateway previewURL: want colon sanitized to dash, got %q", got)
	}
}
