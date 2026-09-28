# C. Tool descriptions (the schema the model reads)

## apply_patch

<!-- source: internal/agent/tools/apply_patch.go:444 (via applyPatchDescription) -->

````text
Apply a multi-file patch in OpenAI Codex DSL format. Use this instead of chained edit_file/write_file calls when a change touches ≥2 files or ≥2 hunks — one tool call performs every edit atomically (parse + hunk matching happens for every file before any write; if any hunk fails to anchor, NO file is modified).

Format:

  *** Begin Patch
  *** Add File: path/new.go
  +line one
  +line two
  *** Update File: path/old.go
  *** Move to: path/renamed.go    (optional rename, before any hunk)
  @@
   keep_this_line
  -drop_this
  +add_this
   keep_this_too
  @@
   second_anchor
  -bye
  +hi
  *** End of File                  (optional; pin the previous hunk to file end)
  *** Delete File: path/legacy.go
  *** End Patch

Rules:
- Hunks anchor on context lines (' ' prefix) plus '-' lines that must literally match the file. Provide enough context to make the location unambiguous; matching is in-order, first match wins.
- Pure-add hunks (only '+' lines) only work with *** End of File or at the very top of a file.
- Identity files (SOUL.md, IDENTITY.md, MEMORY.md, AGENTS.md, BOOTSTRAP.md, TOOLS.md, HEARTBEAT.md, USER.md) accept Add and Update but NOT Delete or Move.
- Path resolution matches read_file/write_file: workspace-relative paths go to the workspace store, identity-file basenames go to the system store, absolute paths go to disk.
````

## apply_patch

<!-- source: internal/agent/tools/apply_patch.go:444 (via applyPatchDescription) -->

````text
Apply a multi-file patch in OpenAI Codex DSL format. Use this instead of chained edit_file/write_file calls when a change touches ≥2 files or ≥2 hunks — one tool call performs every edit atomically (parse + hunk matching happens for every file before any write; if any hunk fails to anchor, NO file is modified).

Format:

  *** Begin Patch
  *** Add File: path/new.go
  +line one
  +line two
  *** Update File: path/old.go
  *** Move to: path/renamed.go    (optional rename, before any hunk)
  @@
   keep_this_line
  -drop_this
  +add_this
   keep_this_too
  @@
   second_anchor
  -bye
  +hi
  *** End of File                  (optional; pin the previous hunk to file end)
  *** Delete File: path/legacy.go
  *** End Patch

Rules:
- Hunks anchor on context lines (' ' prefix) plus '-' lines that must literally match the file. Provide enough context to make the location unambiguous; matching is in-order, first match wins.
- Pure-add hunks (only '+' lines) only work with *** End of File or at the very top of a file.
- Identity files (SOUL.md, IDENTITY.md, MEMORY.md, AGENTS.md, BOOTSTRAP.md, TOOLS.md, HEARTBEAT.md, USER.md) accept Add and Update but NOT Delete or Move.
- Path resolution matches read_file/write_file: workspace-relative paths go to the workspace store, identity-file basenames go to the system store, absolute paths go to disk.
````

## bash_output

<!-- source: internal/agent/tools/bash_tools.go:25 (via bashOutputDescription) -->

````text
Read new stdout/stderr from a backgrounded shell since the last call. Use this to monitor a long-running process started with exec(run_in_background=true).

Returns:
  - new output produced since the previous bash_output call on this bash_id (each call advances a per-session cursor)
  - "[status] running" or "[status] exited (code=N)" — only "exited" rows guarantee the process is done; killed processes report code=-1 with the kill reason appended
  - a "[truncated]" line prepended if the 4 MiB per-session output buffer rolled past the read cursor (oldest bytes dropped FIFO)

Notes:
  - The session keeps running across calls until kill_shell or natural exit.
  - After exit, bash_output is still callable to read any final output and confirm the exit code.
  - The optional 'filter' regex is applied per output line (lines that don't match are dropped before return) — useful for tailing a noisy log when you only care about errors.
````

## kill_shell

<!-- source: internal/agent/tools/bash_tools.go:37 (via killShellDescription) -->

````text
Terminate a backgrounded shell started by exec(run_in_background=true). Sends SIGKILL via process-group cancellation. Idempotent — calling it on an already-exited shell is a no-op and returns success.
````

## get_billing_usage

<!-- source: internal/agent/tools/billing.go:13 -->

````text
Get the current user's token usage and remaining quota for billing. Use this when the user asks how many tokens they used, how much quota remains, whether they are over limit, when quota resets, or what plan allowance they have. This is read-only and only returns the current billing account; it cannot inspect other users.
````

## create_cron_job

<!-- source: internal/agent/tools/cron.go:29 -->

````text
Create a scheduled task. Use this for any user request that names a specific time, an interval, or a recurring schedule (e.g. "5 分钟后提醒", "every Monday 9am", "each day at 8"). When the schedule fires, the agent receives `message` as a fresh inbound prompt on the same channel the request originated from. Do NOT write timed reminders into HEARTBEAT.md — that file is only for conditional self-checks reviewed at every heartbeat tick.
````

## list_cron_jobs

<!-- source: internal/agent/tools/cron.go:60 -->

````text
List all scheduled tasks for this agent.
````

## delete_cron_job

<!-- source: internal/agent/tools/cron.go:72 -->

````text
Delete a scheduled task by ID.
````

## delegate_task

<!-- source: internal/agent/tools/delegate.go:50 -->

````text
Spawn a sub-agent with its own context and its own iteration budget to run one bounded sub-task. Sub-agents run SERIALLY: five calls in one round still execute one at a time (they share your sandbox and browser daemon), so a fan-out costs N × the single-run wall time — scope each call small rather than expecting parallel throughput.

Same tools and provider as you, minus delegate_task itself (no nesting). Return: the sub-agent's text as a tool result — you assemble the user's deliverable from it.
````

## exec

<!-- source: internal/agent/tools/exec.go:48 (via execHostDescription) -->

````text
Execute a shell command and return stdout/stderr.
````

## exec

<!-- source: internal/agent/tools/exec.go:50 (via execSandboxDescription) -->

````text
Execute a shell command in the sandbox and return stdout/stderr.
````

## read_file

<!-- source: internal/agent/tools/file.go:429 -->

````text
Read the contents of a file
````

## write_file

<!-- source: internal/agent/tools/file.go:73 (via writeFileDescription) -->

````text
Write content to a file (creates directories as needed). For a long document this is one call and one set of arguments, and those arguments cannot exceed your output limit — write the first section, then append the rest with edit_file.
````

## list_dir

<!-- source: internal/agent/tools/file.go:434 -->

````text
List files and directories in a path
````

## edit_file

<!-- source: internal/agent/tools/file.go:113 (via editDescription) -->

````text
Edit a file by replacing an exact substring. Prefer this over write_file when changing only part of a file (especially identity files like SOUL.md / MEMORY.md): it's cheaper, can't drop unrelated content, and validates the replacement was applied. old_string must match a unique substring unless replace_all is true; new_string must differ from old_string. Read the file first if you're unsure of the exact text.
````

## read_file

<!-- source: internal/agent/tools/file.go:1115 -->

````text
Read the contents of a file
````

## write_file

<!-- source: internal/agent/tools/file.go:73 (via writeFileDescription) -->

````text
Write content to a file (creates directories as needed). For a long document this is one call and one set of arguments, and those arguments cannot exceed your output limit — write the first section, then append the rest with edit_file.
````

## list_dir

<!-- source: internal/agent/tools/file.go:1269 -->

````text
List files and directories in a path
````

## edit_file

<!-- source: internal/agent/tools/file.go:113 (via editDescription) -->

````text
Edit a file by replacing an exact substring. Prefer this over write_file when changing only part of a file (especially identity files like SOUL.md / MEMORY.md): it's cheaper, can't drop unrelated content, and validates the replacement was applied. old_string must match a unique substring unless replace_all is true; new_string must differ from old_string. Read the file first if you're unsure of the exact text.
````

## update_goal

<!-- source: internal/agent/tools/goal.go:17 -->

````text
Mark the active goal complete. Status is restricted to "complete"; pausing, resuming, and budget_limited transitions are controlled by the user or the runtime, not by the model. Only call this when the objective has actually been achieved and no required work remains — do not call it merely because the budget is nearly exhausted or because you want to stop.
````

## image_gen

<!-- source: internal/agent/tools/image_gen.go:24 -->

````text
Generate images from a text prompt. Uses a configurable provider chain (OpenAI gpt-image-1, fal flux, …) with automatic fallback. Returns markdown image tags that render inline in chat.
````

## knowledge_search

<!-- source: internal/agent/tools/knowledge_search.go:26 -->

````text
Search the agent's knowledge base (reference files uploaded by the agent owner) by keyword. Use focused keywords from the question; if a search misses, retry once or twice with different or broader terms.
````

## load_skill

<!-- source: internal/agent/tools/load_skill.go:18 -->

````text
Load the full content of a skill by name. Use this when you need detailed instructions for a specific skill.
````

## memory_search

<!-- source: internal/agent/tools/memory_search.go:34 -->

````text
Search through conversation history logs using keyword matching with recency weighting
````

## message

<!-- source: internal/agent/tools/message.go:31 -->

````text
Send a message to a channel
````

## set_preference

<!-- source: internal/agent/tools/preference.go:21 -->

````text
Save a personal preference or API key for the current chatter on this agent. Use this when the user wants to configure something that should persist across conversations — for example their timezone, language, an API key for image generation, drawing style, etc. The preference is scoped to this user + this agent only, not shared with other agents or users.
````

## search_skills

<!-- source: internal/agent/tools/skill_install.go:22 -->

````text
Search for skills on skills.sh (primary registry) and clawhub.ai. Returns the top matches so you can pick one to install.
````

## install_skill

<!-- source: internal/agent/tools/skill_install.go:56 -->

````text
Install a skill into THIS agent's private skills directory. Tries skills.sh first, then clawhub.ai. If neither has it, returns a not-found error — at that point ask the user whether to build a custom skill with the skill-creator skill instead of retrying. Installed skills are scoped to this agent only; they do not affect other agents.
````

## spawn_subagent

<!-- source: internal/agent/tools/subagent.go:23 -->

````text
Spawn another agent as a sub-task and return its response. Use this to delegate work to specialized agents.
````

## set_timezone

<!-- source: internal/agent/tools/timezone.go:26 -->

````text
Record the current chatter's timezone. Call this whenever the chatter tells you their timezone, city, or country (e.g. "我在北京" → Asia/Shanghai). This persists the timezone to the chatter's profile so future sessions use their local time automatically.
````

## tts

<!-- source: internal/agent/tools/tts.go:22 -->

````text
Convert text to speech. Uses a configurable provider chain (OpenAI tts-1, MiniMax speech-02, …) with automatic fallback. The audio file is attached to the chat message automatically.
````

## web_fetch

<!-- source: internal/agent/tools/web_fetch.go:113 (via webFetchDescription) -->

````text
Fetch one known URL and return its plain text. Pass the exact full URL (https://…), not a search-results URL or a path guessed from memory; which tool to reach for first is spelled out in your instructions. A URL that already returned 4xx/5xx in this turn is refused rather than retried.
````

## web_fetch

<!-- source: internal/agent/tools/web_fetch.go:113 (via webFetchDescription) -->

````text
Fetch one known URL and return its plain text. Pass the exact full URL (https://…), not a search-results URL or a path guessed from memory; which tool to reach for first is spelled out in your instructions. A URL that already returned 4xx/5xx in this turn is refused rather than retried.
````

## web_search

<!-- source: internal/agent/tools/web_search.go:26 -->

````text
Search the web and return results with titles, URLs, and snippets. Backed by a configurable provider chain (e.g. exa, brave, searxng) with automatic fallback.
````
