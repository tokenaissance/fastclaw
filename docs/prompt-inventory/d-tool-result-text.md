# D. Text injected into tool results

## error suffix on every failed tool

<!-- source: internal/agent/tools/registry.go:1003 -->

````text
[Analyze the error above and try a different approach.]
````

## long foreground wait refusal

<!-- source: internal/agent/tools/exec.go:207 -->

````text
Refused: this command waits ~%ds in the foreground, and a turn can end before that arrives (the smallest turn budget is 300s — the observation dies with it, the work does not). Anything that waits belongs on run_in_background:

  exec({"command": "<the same command, waits included>", "run_in_background": true})   → returns a bash_id immediately
  bash_output({"bash_id": "<id>"})   → read its progress in a later call
  kill_shell({"bash_id": "<id>"})    → stop it

Re-issue the same command with run_in_background — do not sleep in a foreground call.
````

## exec cancelled hint

<!-- source: internal/sandbox/e2b_executor.go:1196 -->

````text
 [hint: the exec request was cancelled by the runtime, not by the sandbox — the turn's budget expired, the turn was superseded, or the caller disconnected. A process this command started may still be running inside the sandbox: check it (ps, plus whatever log file it was redirected to) and adopt that result before re-running anything. To make that check possible next time, start it with exec({"run_in_background": true}) and read it with bash_output.]
````

## exec stalled hint

<!-- source: internal/sandbox/e2b_executor.go:1211 -->

<!-- NOTE: 2 branch point(s) — literals concatenated in source order, not rendered -->

````text
deadline_exceeded [hint: the output above was delivered, but a process this command started is still holding the exec stream open — envd ended the request at its own deadline. Run it with exec({"run_in_background": true}) instead: that hands back a bash_id immediately and bash_output reads it later, so the waiting happens in the sandbox while the turn stays free. Don't read this as a failed run: check the sandbox before re-running.]
````

## sandbox-absence hint

<!-- source: internal/agent/tools/exec.go:639 -->

````text
%w
[hint: this looks like a sandbox-environment miss (binary or path not present in the container). If the command needs the user's actual host machine — e.g. `fastagent upgrade`, `~/Downloads`, host CLI tools — retry with the `host_exec` tool instead.]
````

## background job started

<!-- source: internal/agent/tools/sandbox_background.go:466 -->

````text
Started background job %s (pid %s) in the sandbox: %s
Read its output with bash_output(bash_id=%q) (returns what it printed since the last call, plus running/exited status); stop it with kill_shell(bash_id=%q).
The job keeps running after this call returns, and its output is captured at %s inside the sandbox.
````

## background poll status lines

<!-- source: internal/agent/tools/sandbox_background.go:330-360 -->

<!-- NOTE: 5 branch point(s) — literals concatenated in source order, not rendered -->

````text
fcbg1sandbox background launch: no job handle in output %q — the shell could not start a detached processsandbox background poll: no status marker in output %qsandbox background poll: unterminated status marker in output %qfcbgsandbox background poll: malformed status marker %q
````

## interrupted-call placeholder

<!-- source: internal/provider/provider.go:71 -->

````text
(stopped — execution was interrupted before the tool returned)
````
