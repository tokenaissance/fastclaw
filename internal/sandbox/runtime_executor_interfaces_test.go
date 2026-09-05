package sandbox

// Interface-contract assertions for the backend-agnostic project runtime
// added upstream in:
//
//   - ff7ed19 feat: project runtime (coding-agent preview) + dashboard UI refinements
//   - 418ce82 feat: backend-agnostic project runtime + multi-template + preview UX
//
// These lock down the optional-capability contract the runtime depends on:
// every backend implements Executor; docker / boxlite must NOT claim
// PortExposer (their dev-server preview is host-bound), while E2B must
// implement PortExposer + TemplateProvisioner + RemoteWorkspace so the
// pooled preview path can type-assert them. No real docker / E2B connection
// is made anywhere in this file.

import (
	"context"
	"strings"
	"testing"
)

// Compile-time: every concrete executor and pool satisfies the runtime's
// entry points. If any backend drops a method, this package stops compiling.
var (
	_ Executor     = (*DockerExecutor)(nil)
	_ Executor     = (*E2BExecutor)(nil)
	_ Executor     = (*BoxliteExecutor)(nil)
	_ ExecutorPool = (*DockerExecutorPool)(nil)
	_ ExecutorPool = (*E2BExecutorPool)(nil)
	_ ExecutorPool = (*BoxliteExecutorPool)(nil)
	_ PortExposer  = (*E2BExecutor)(nil)
	_ PortExposer  = (*fakePortExposer)(nil)
)

// fakePortExposer is only here to keep the PortExposer compile-time group
// honest — a minimal real implementation used by runtime_e2e_test.go lives
// in the runtime package; this one is a self-contained sandbox-side probe.
type fakePortExposer struct{}

func (fakePortExposer) ExposePort(context.Context, int) (string, error) { return "", nil }

// TestRuntimeExecutor_BackendLabels — the pool/executor Backend() labels the
// runtime uses to pick a preview path and to log which provider handled an
// exec. Zero-value structs are enough: Backend() never depends on fields.
func TestRuntimeExecutor_BackendLabels(t *testing.T) {
	cases := []struct {
		name    string
		backend func() string
		want    string
	}{
		{"DockerExecutor", (&DockerExecutor{}).Backend, "docker"},
		{"DockerExecutorPool", (&DockerExecutorPool{}).Backend, "docker"},
		{"E2BExecutor", (&E2BExecutor{}).Backend, "e2b"},
		{"E2BExecutorPool", (&E2BExecutorPool{}).Backend, "e2b"},
		{"BoxliteExecutor", (&BoxliteExecutor{}).Backend, "boxlite"},
		{"BoxliteExecutorPool", (&BoxliteExecutorPool{}).Backend, "boxlite"},
	}
	for _, tc := range cases {
		if got := tc.backend(); got != tc.want {
			t.Fatalf("%s Backend() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestE2BExecutor_OptionalCapabilities — E2B implements every capability
// the pooled preview path needs: PortExposer (mints the .e2b.app URL),
// TemplateProvisioner (uploads a local template into the remote sandbox),
// RemoteWorkspace (no host bind mount → sync-after-exec), and
// WorkspaceSnapshotter (flush-on-evict).
func TestE2BExecutor_OptionalCapabilities(t *testing.T) {
	e := &E2BExecutor{}
	if _, ok := any(e).(PortExposer); !ok {
		t.Fatal("E2BExecutor must implement PortExposer")
	}
	if _, ok := any(e).(TemplateProvisioner); !ok {
		t.Fatal("E2BExecutor must implement TemplateProvisioner")
	}
	if _, ok := any(e).(RemoteWorkspace); !ok {
		t.Fatal("E2BExecutor must implement RemoteWorkspace")
	}
	if _, ok := any(e).(WorkspaceSnapshotter); !ok {
		t.Fatal("E2BExecutor must implement WorkspaceSnapshotter")
	}
}

// TestDockerExecutor_Capabilities — docker's /workspace is a host bind
// mount, so it snapshots but is NOT remote and has no port-expose step
// (its preview is the published host port, handled by DockerSandbox).
func TestDockerExecutor_Capabilities(t *testing.T) {
	d := &DockerExecutor{}
	if _, ok := any(d).(WorkspaceSnapshotter); !ok {
		t.Fatal("DockerExecutor must implement WorkspaceSnapshotter")
	}
	if _, ok := any(d).(RemoteWorkspace); ok {
		t.Fatal("DockerExecutor must NOT implement RemoteWorkspace (bind-mounted workspace)")
	}
	if _, ok := any(d).(PortExposer); ok {
		t.Fatal("DockerExecutor must NOT implement PortExposer (host ports via DockerSandbox)")
	}
	if _, ok := any(d).(TemplateProvisioner); ok {
		t.Fatal("DockerExecutor must NOT implement TemplateProvisioner (template is bind-mounted)")
	}
}

// TestBoxliteExecutor_Capabilities — boxlite is remote (like E2B) but
// currently has no port-expose scheme, so the runtime must reject a boxlite
// preview rather than minting a dead URL.
func TestBoxliteExecutor_Capabilities(t *testing.T) {
	b := &BoxliteExecutor{}
	if _, ok := any(b).(RemoteWorkspace); !ok {
		t.Fatal("BoxliteExecutor must implement RemoteWorkspace")
	}
	if _, ok := any(b).(WorkspaceSnapshotter); !ok {
		t.Fatal("BoxliteExecutor must implement WorkspaceSnapshotter")
	}
	if _, ok := any(b).(PortExposer); ok {
		t.Fatal("BoxliteExecutor must NOT implement PortExposer (no URL scheme)")
	}
	if _, ok := any(b).(TemplateProvisioner); ok {
		t.Fatal("BoxliteExecutor must NOT implement TemplateProvisioner")
	}
}

// TestE2BExecutor_ExposePortURL — the exact URL scheme the runtime embeds
// in the preview: https://<port>-<sandboxID>.e2b.app, with a hard error
// when the sandbox hasn't been created yet.
func TestE2BExecutor_ExposePortURL(t *testing.T) {
	e := &E2BExecutor{sandboxID: "sb-test-123"}
	url, err := e.ExposePort(context.Background(), 3000)
	if err != nil {
		t.Fatalf("ExposePort: %v", err)
	}
	if url != "https://3000-sb-test-123.e2b.app" {
		t.Fatalf("ExposePort URL = %q, want https://3000-sb-test-123.e2b.app", url)
	}

	if _, err := (&E2BExecutor{}).ExposePort(context.Background(), 3000); err == nil {
		t.Fatal("ExposePort on un-created sandbox succeeded, want error")
	} else if !strings.Contains(err.Error(), "sandbox not created") {
		t.Fatalf("ExposePort error = %q, want sandbox-not-created", err.Error())
	}
}
