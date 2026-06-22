package tools

import (
	"context"
	"testing"
	"time"
)

type fakeExecutor struct{}

func (fakeExecutor) Exec(context.Context, string, time.Duration) (string, error) { return "", nil }
func (fakeExecutor) ReadFile(context.Context, string) (string, error)            { return "", nil }
func (fakeExecutor) WriteFile(context.Context, string, string) (string, error)   { return "", nil }
func (fakeExecutor) ListDir(context.Context, string) (string, error)             { return "", nil }
func (fakeExecutor) Backend() string                                             { return "test" }
func (fakeExecutor) Close() error                                                { return nil }

func TestFastAgentInternalPathsIncludeRootAndEnvHome(t *testing.T) {
	t.Setenv("FASTAGENT_HOME", "/srv/fastagent")

	cases := []string{
		"~/.fastagent",
		"~/.fastagent/workspaces",
		"/root/.fastagent",
		"/root/.fastagent/workspaces",
		"/srv/fastagent",
		"/srv/fastagent/workspaces",
	}
	for _, path := range cases {
		if !isFastAgentInternalPath(path) {
			t.Fatalf("isFastAgentInternalPath(%q) = false, want true", path)
		}
		if got, ok := hostHomePath(path); ok || got != "" {
			t.Fatalf("hostHomePath(%q) = (%q, %v), want denied", path, got, ok)
		}
	}
}

func TestRouteForDoesNotExposeFastAgentInternalsViaHostFS(t *testing.T) {
	r := &Registry{executor: fakeExecutor{}}

	if got := r.routeFor("/root/.fastagent/workspaces", OpList); got != RouteSandbox {
		t.Fatalf("routeFor FastAgent internal path = %v, want RouteSandbox", got)
	}
	if got := r.routeFor("/Users/maxwell/Documents/report.txt", OpRead); got != RouteHostFS {
		t.Fatalf("routeFor explicit host document = %v, want RouteHostFS", got)
	}
}
