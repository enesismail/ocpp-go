#!/usr/bin/env bash
#
# Validation gate for the ocpp-go friendly fork.
#
# Runs the library checks (build / vet / race tests) and, when one or more
# downstream consumer projects are configured, builds + tests each of them
# against THIS fork via a disposable scratch copy plus a local `replace`
# directive.
#
# Consumer projects are supplied via the GATE_PROJECT_DIRS environment variable
# (a colon-separated list of absolute paths), so this script names no specific
# repositories and can live in a public fork unchanged. Example:
#   GATE_PROJECT_DIRS=/path/to/projectA:/path/to/projectB scripts/gate.sh
#
# IMPORTANT: a consumer project's real working tree is never modified. The
# `replace` is added only inside a throwaway scratch copy under $TMPDIR, so no
# local-path replace is ever left in any consumer project's go.mod file.
#
# Usage:
#   GATE_PROJECT_DIRS=/a:/b scripts/gate.sh   # library + the listed projects
#   scripts/gate.sh                           # library only (no projects set)
#   GATE_SKIP_PROJECTS=1 scripts/gate.sh      # force library only
#
set -uo pipefail

FORK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FORK_MODULE="github.com/lorenzodonini/ocpp-go"
fail=0

hr() { printf '================= %s =================\n' "$1"; }

hr "LIBRARY ($FORK_MODULE)"
(
  cd "$FORK_DIR"
  echo "--- go build ./... ---";      go build ./...      || exit 1
  echo "--- go vet ./... ---";        go vet ./...        || exit 1
  echo "--- go test ./... -race ---"; go test ./... -race || exit 1
) || { echo "LIBRARY GATE FAILED"; fail=1; }

gate_project() {
  local name="$1" src="$2"
  if [ ! -d "$src" ]; then
    echo "== project $name: not found at $src — skipping =="
    return 0
  fi
  hr "PROJECT: $name (replace -> $FORK_DIR)"
  local scratch
  scratch="$(mktemp -d "${TMPDIR:-/tmp}/gate-${name}-XXXXXX")" || { echo "mktemp failed"; return 1; }
  # Copy sources only (skip VCS metadata); the scratch dir is disposable.
  if command -v rsync >/dev/null 2>&1; then
    rsync -a --exclude='.git' "$src"/ "$scratch"/
  else
    cp -R "$src"/. "$scratch"/ && rm -rf "$scratch/.git"
  fi
  (
    cd "$scratch"
    go mod edit -replace "$FORK_MODULE=$FORK_DIR"
    go mod tidy   || exit 1
    echo "--- go build ./... ---";      go build ./...      || exit 1
    echo "--- go vet ./... ---";        go vet ./...        || exit 1
    echo "--- go test ./... -race ---"; go test ./... -race || exit 1
  )
  local rc=$?
  rm -rf "$scratch"
  if [ "$rc" -ne 0 ]; then echo "== project $name GATE FAILED =="; return 1; fi
  echo "== project $name OK =="
}

if [ "${GATE_SKIP_PROJECTS:-0}" = "1" ]; then
  echo "== GATE_SKIP_PROJECTS=1 — library gate only =="
elif [ -n "${GATE_PROJECT_DIRS:-}" ]; then
  IFS=':' read -ra _gate_dirs <<< "$GATE_PROJECT_DIRS"
  for _dir in "${_gate_dirs[@]}"; do
    [ -n "$_dir" ] || continue
    gate_project "$(basename "$_dir")" "$_dir" || fail=1
  done
else
  echo "== no consumer projects configured (set GATE_PROJECT_DIRS=/a:/b) — library gate only =="
fi

if [ "$fail" -eq 0 ]; then echo "GATE: ALL GREEN"; else echo "GATE: FAILURES (see above)"; fi
exit "$fail"
