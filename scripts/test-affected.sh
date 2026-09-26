#!/usr/bin/env bash
#
# Run only the Go packages a change can reach — locally, while you iterate.
#
# Why: `go test ./...` is 393 test files across ~33 packages (measured 2026-09-26: 49s warm,
# ~100s with -race in CI). Running it after every edit is 90% waiting. `go test` is already
# package-scoped; what is missing is the *selection* — "which packages can this diff reach?"
# — and that question has an exact answer here, because Go's import graph is in the module.
#
# What this is NOT: a gate. CI runs the whole tree (`go test ./... -race`, .github/workflows/
# go-test.yml) on every push and PR, and that is the only place "everything passes" is
# claimed. This script prints exactly what it ran and what it skipped, and never says more.
#
# Usage:
#   scripts/test-affected.sh [base-ref] [--list] [--race] [--all]
#     base-ref   what to diff against (default: origin/fastagent; falls back to HEAD~1)
#     --list     print the selection and the reason, run nothing
#     --race     same flags CI uses (slower; worth it before pushing)
#     --all      skip the selection and run the whole tree
#
# The selection is the *reverse dependency closure* of the changed packages, not the changed
# directories. That distinction is the whole point in this repository: coupling here is by
# fact, not by folder — the turnId work of 2026-09-26 touched internal/agent, internal/setup
# and web/, and a change to internal/agent/sessionlease.go is exercised by
# internal/gateway, internal/setup and internal/store tests as much as by its own.
set -uo pipefail

cd "$(git rev-parse --show-toplevel)" || exit 1

BASE=""
MODE="run"
RACE=""
for arg in "$@"; do
  case "$arg" in
    --list) MODE="list" ;;
    --race) RACE="-race" ;;
    --all) MODE="all" ;;
    *) BASE="$arg" ;;
  esac
done
if [ -z "$BASE" ]; then
  if git rev-parse --verify --quiet origin/fastagent >/dev/null; then BASE="origin/fastagent"; else BASE="HEAD~1"; fi
fi

# Files whose *readers* are wider than the import graph — the same idea as the "one fact, has
# one expression" family in docs/fs-formal-proof: a change here is a contract change, and the
# cheap honest answer is the whole tree. Each line names why, because a list like this decays
# into superstition otherwise.
CONTRACT=(
  "internal/agent/sessionlease.go" # the lease port; its contract is docs/fs-formal-proof/12
  "internal/agent/admission.go"    # TurnMode: decides queue-vs-refuse for every caller
  "internal/session/manager.go"    # the session's key, gate and history — clause W/P/O/T
  "internal/provider/"             # every wire builder inherits normalizeForPrompt
  "internal/bus/"                  # the transport under every entry point
  "go.mod"                         # a dependency change is not a package change
  "go.sum"
  ".github/"
  "Makefile"
  "Dockerfile"
)

changed=$( { git diff --name-only "$BASE"...HEAD 2>/dev/null
             git diff --name-only
             git diff --name-only --cached; } | sort -u | grep -v '^$' )

if [ -z "$changed" ]; then
  echo "no changes against $BASE — nothing to select"
  exit 0
fi

reason=""
for file in $changed; do
  for hit in "${CONTRACT[@]}"; do
    # Prefix match without `case`: bash 3.2 (macOS's /bin/bash) misparses a `case` whose
    # pattern list starts with a quoted word, which is exactly this shape.
    if [ -z "$reason" ] && [ "${file#"$hit"}" != "$file" ]; then
      reason="$file (contract: $hit)"
    fi
  done
  [ -n "$reason" ] && break
done

web_changed=$(echo "$changed" | grep -c '^web/' || true)

run_all() {
  echo "selection: THE WHOLE TREE"
  [ -n "$reason" ] && echo "reason:     $reason"
  [ "$MODE" = "list" ] && return 0
  echo
  # shellcheck disable=SC2086
  go test ./... -count=1 $RACE
}

if [ "$MODE" = "all" ] || [ -n "$reason" ]; then
  run_all
  exit $?
fi

# Changed .go files → their packages. A deleted file still names the package that lost it.
roots=$(echo "$changed" | grep '\.go$' | while read -r f; do dirname "$f"; done | sort -u)

if [ -z "$roots" ]; then
  echo "no Go files changed."
  [ "$web_changed" -gt 0 ] && echo "web/ changed ($web_changed files) — this script only drives Go: cd web && pnpm test"
  exit 0
fi

deps=$(mktemp)
tests=$(mktemp)
trap 'rm -f "$deps" "$tests"' EXIT

# `go list ./...` cannot compile internal/setup on a clean checkout: embed.go does
# `//go:embed all:web` and that directory is produced by `make build-web`. A failure here is
# not a reason to test less — it is a reason to test everything, loudly.
if ! go list -f '{{.ImportPath}}{{range .Deps}} {{.}}{{end}}' ./... >"$deps" 2>/dev/null; then
  reason="go list could not resolve the tree (missing internal/setup/web? run 'make build-web')"
  run_all
  exit $?
fi
go list -f '{{.ImportPath}} {{len .TestGoFiles}} {{len .XTestGoFiles}}' ./... >"$tests" 2>/dev/null

seeds=$(for d in $roots; do
          if [ "$d" = "." ]; then
            echo "github.com/fastclaw-ai/fastclaw"
          else
            echo "github.com/fastclaw-ai/fastclaw/$d"
          fi
        done | sort -u)

selection=$(python3 - "$deps" "$tests" $seeds <<'PY'
import sys

deps_path, tests_path = sys.argv[1], sys.argv[2]
seeds = set(sys.argv[3:])

reverse = {}
for line in open(deps_path):
    parts = line.split()
    if not parts:
        continue
    for dep in parts[1:]:
        reverse.setdefault(dep, set()).add(parts[0])

with_tests = set()
for line in open(tests_path):
    parts = line.split()
    if len(parts) == 3 and (int(parts[1]) or int(parts[2])):
        with_tests.add(parts[0])

seen, stack = set(seeds), list(seeds)
while stack:
    pkg = stack.pop()
    for up in reverse.get(pkg, ()):
        if up not in seen:
            seen.add(up)
            stack.append(up)

for pkg in sorted(seen & with_tests):
    print(pkg)
PY
)

if [ -z "$selection" ]; then
  echo "selection: no package with tests is reachable from this diff"
  [ "$web_changed" -gt 0 ] && echo "web/ changed ($web_changed files) — this script only drives Go: cd web && pnpm test"
  exit 0
fi

count=$(echo "$selection" | wc -l | tr -d ' ')
echo "selection: $count package(s) with tests, reversed from:"
for d in $roots; do echo "           $d"; done
if [ "$web_changed" -gt 0 ]; then
  echo "note:      web/ changed ($web_changed files) — this script only drives Go: cd web && pnpm test"
fi
if [ "$MODE" = "list" ]; then
  echo "$selection" | sed 's/^/  /'
  exit 0
fi

echo
# shellcheck disable=SC2086
go test -count=1 $RACE $selection
