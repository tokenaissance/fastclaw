package tools

// A foreground `sleep` is a bet that the turn outlives the wait. r39–r45 lost
// that bet five times: the turn's clock ended the call mid-wait, the model got
// "context canceled" instead of a number, and the work it was watching had to be
// re-derived by hand. The guard refuses that shape up front and points at the one
// supported way to wait (run_in_background), so the failure costs a second
// instead of a minute.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLongForegroundWaitPredicate(t *testing.T) {
	long := []struct {
		command string
		seconds int
	}{
		{"sleep 175", 175},
		{"sleep 45; ls", 45},
		{"cd /workspace && sleep 45; tail -2 log", 45},
		{"ls out | wc -l; setsid nohup bash job.sh > log 2>&1 < /dev/null & sleep 175; tail -3 log", 175},
		{"sleep 1m", 60},
		{"sleep 90s", 90},
		{"sleep 30", 30},
		{"(sleep 40)", 40},
		{"sleep 45.5", 45},
	}
	for _, tc := range long {
		secs, ok := longForegroundWait(tc.command)
		if !ok || secs != tc.seconds {
			t.Errorf("longForegroundWait(%q) = %d, %v; want %d, true", tc.command, secs, ok, tc.seconds)
		}
	}

	short := []string{
		"",                                       // nothing to judge
		"echo hi",                                // no sleep
		"sleep 5",                                // below the threshold
		"sleep 0.5",                              //
		"sleep 29",                               // just under
		"grep -c \"sleep 175\" /workspace/x.log", // quoted text, not a command
		"cat sleepy.txt",                         // not the sleep builtin
		"sleep $DELAY",                           // not judgeable
		"python3 -c 'import time; time.sleep(175)'", // not the shell's sleep
	}
	for _, cmd := range short {
		if secs, ok := longForegroundWait(cmd); ok {
			t.Errorf("longForegroundWait(%q) = %d, true; want no match", cmd, secs)
		}
	}
}

// The refusal must teach exactly one way to wait: run_in_background. No nohup,
// no </dev/null, no hand-rolled recipe — those are what produced the five
// incidents.
func TestLongWaitRefusalNamesOnlyTheBackgroundPrimitive(t *testing.T) {
	msg := longWaitRefusal(175).Error()
	for _, want := range []string{"run_in_background", "bash_output", "kill_shell", "175s"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal is missing %q:\n%s", want, msg)
		}
	}
	for _, unwanted := range []string{"nohup", "/dev/null", "tmux", "setsid"} {
		if strings.Contains(msg, unwanted) {
			t.Fatalf("refusal still teaches a hand-rolled alternative (%q):\n%s", unwanted, msg)
		}
	}
}

func execArgsJSON(t *testing.T, command string, extra map[string]any) string {
	t.Helper()
	args := map[string]any{"command": command}
	for k, v := range extra {
		args[k] = v
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return string(raw)
}

// A refused call must not reach the sandbox: refusing after firing the command
// would still burn the wait.
func TestExecRefusesLongForegroundSleepBeforeReachingTheSandbox(t *testing.T) {
	ctx := context.Background()
	ex := &bgRecordingExecutor{replies: []string{"fcbg 4242 1\n"}}
	r := NewRegistry(t.TempDir(), t.TempDir())
	defer r.Close()
	r.SetExecutor(ex)

	_, err := r.Execute(ctx, "exec", execArgsJSON(t,
		"cd /workspace && setsid nohup bash job.sh > log 2>&1 </dev/null & sleep 175; tail -3 log", nil))
	if err == nil {
		t.Fatal("a 175s foreground wait must be refused")
	}
	if !strings.Contains(err.Error(), "run_in_background") {
		t.Fatalf("refusal must name the supported primitive: %v", err)
	}
	if got := ex.commands(); len(got) != 0 {
		t.Fatalf("the command reached the sandbox despite being refused: %v", got)
	}
}

// run_in_background is the supported way to wait — the same command is accepted
// there, waits and all.
func TestExecAcceptsTheLongWaitOnRunInBackground(t *testing.T) {
	ctx := context.Background()
	ex := &bgRecordingExecutor{replies: []string{"fcbg 4242 1\n"}}
	r := NewRegistry(t.TempDir(), t.TempDir())
	defer r.Close()
	r.SetExecutor(ex)

	out, err := r.Execute(ctx, "exec", execArgsJSON(t,
		"sleep 175; tail -3 log", map[string]any{"run_in_background": true}))
	if err != nil {
		t.Fatalf("run_in_background must accept a waiting command: %v (out=%q)", err, out)
	}
	if got := ex.commands(); len(got) != 1 || !strings.Contains(got[0], "sleep 175") {
		t.Fatalf("the job did not reach the sandbox: %v", got)
	}
}

// The escape hatch: a wait the guard cannot judge still has a door. It is
// advertised in the schema, never in the refusal text — the default answer
// stays run_in_background.
func TestExecAcceptsTheLongWaitWithTheOverride(t *testing.T) {
	ctx := context.Background()
	ex := &bgRecordingExecutor{replies: []string{"one"}}
	r := NewRegistry(t.TempDir(), t.TempDir())
	defer r.Close()
	r.SetExecutor(ex)

	out, err := r.Execute(ctx, "exec", execArgsJSON(t,
		"sleep 175; tail -3 log", map[string]any{"allow_long_wait": true}))
	if err != nil {
		t.Fatalf("allow_long_wait must let a foreground wait through: %v (out=%q)", err, out)
	}
	if got := ex.commands(); len(got) != 1 || !strings.Contains(got[0], "sleep 175") {
		t.Fatalf("the overridden command did not run: %v", got)
	}
}

// The override must not leak into the refusal: the model's default path stays
// run_in_background, so the guidance cannot become "just set the flag".
func TestLongWaitRefusalDoesNotAdvertiseTheOverride(t *testing.T) {
	msg := longWaitRefusal(175).Error()
	if strings.Contains(msg, "allow_long_wait") {
		t.Fatalf("refusal advertises the override instead of the supported primitive:\n%s", msg)
	}
}

// Short waits stay allowed: a one-second readiness probe is a legitimate
// foreground call and must not grow a refusal path.
func TestExecAllowsShortForegroundSleep(t *testing.T) {
	ctx := context.Background()
	ex := &bgRecordingExecutor{replies: []string{"ok"}}
	r := NewRegistry(t.TempDir(), t.TempDir())
	defer r.Close()
	r.SetExecutor(ex)

	if _, err := r.Execute(ctx, "exec", execArgsJSON(t, "sleep 5; echo done", nil)); err != nil {
		t.Fatalf("short sleep refused: %v", err)
	}
	if got := ex.commands(); len(got) != 1 {
		t.Fatalf("short sleep did not run: %v", got)
	}
}

// The host path carries the same turn clock, so it gets the same guard — and
// the proof that nothing ran is a file the command would have created.
func TestExecRefusesLongForegroundSleepOnTheHostPath(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry(t.TempDir(), t.TempDir())
	defer r.Close()

	marker := filepath.Join(t.TempDir(), "marker")
	_, err := r.Execute(ctx, "exec", execArgsJSON(t, "sleep 40; touch "+marker, nil))
	if err == nil {
		t.Fatal("a 40s foreground wait must be refused on the host path too")
	}
	if !strings.Contains(err.Error(), "run_in_background") {
		t.Fatalf("host refusal must name the supported primitive: %v", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("the refused command ran on the host")
	}
}

// The exec tool is registered twice (host closure + sandbox closure). Its two
// descriptions must stay one sentence each and differ only in where the command
// runs — any added sentence is either the delivery rule again (owned by the
// system prompt) or a new promise nobody made twice.
func TestExecDescriptionsStayInSync(t *testing.T) {
	const wantHost = "Execute a shell command and return stdout/stderr."
	const wantSandbox = "Execute a shell command in the sandbox and return stdout/stderr."
	if execHostDescription != wantHost {
		t.Fatalf("host exec description = %q, want %q", execHostDescription, wantHost)
	}
	if execSandboxDescription != wantSandbox {
		t.Fatalf("sandbox exec description = %q, want %q", execSandboxDescription, wantSandbox)
	}
}

// The file-delivery rule (write binary output into the workspace, reference it by
// path, never inline base64) has exactly one owner: the sandbox module of the
// system prompt. The exec description used to restate it — 300 characters sent on
// every request, saying what the model already read — so it must not come back.
// The system-prompt half of this contract is TestFileDeliveryRuleHasOneOwner.
func TestExecDescriptionDoesNotRestateTheDeliveryRule(t *testing.T) {
	for _, desc := range []string{execHostDescription, execSandboxDescription} {
		for _, restated := range []string{"base64", "Files panel", "workspace file"} {
			if strings.Contains(strings.ToLower(desc), strings.ToLower(restated)) {
				t.Fatalf("exec description restates the delivery rule (%q): %q", restated, desc)
			}
		}
		if len(desc) > 120 {
			t.Fatalf("exec description grew back to %d chars — the schema is sent on every request: %q", len(desc), desc)
		}
	}
}
