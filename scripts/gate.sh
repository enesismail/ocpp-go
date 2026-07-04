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
UPSTREAM_MODULE="github.com/lorenzodonini/ocpp-go"
FORK_MODULE="$(cd "$FORK_DIR" && { go list -m 2>/dev/null || awk 'NR==1 && $1=="module"{print $2}' go.mod; })"
fail=0
skipped=0

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
    # A consumer may vendor the fork (a pinned snapshot). The gate must test the
    # consumer's SOURCE against the live fork tree, so drop the scratch copy's
    # vendor dir (disposable copy — the real project is untouched) and resolve via
    # the local `replace` in module mode.
    rm -rf vendor
    # The live fork tree declares $FORK_MODULE and self-imports under that path,
    # so a consumer can only be gated against it if the consumer imports the fork
    # under $FORK_MODULE too. A consumer still importing the pre-rename upstream
    # path cannot be served from this tree (one directory cannot satisfy two
    # module paths), and pointing the old path elsewhere would silently test
    # against the wrong tree — so flag it for migration instead. Grep runs after
    # the vendor drop so only the consumer's own source is inspected.
    if [ "$UPSTREAM_MODULE" != "$FORK_MODULE" ] && \
       grep -rqE "\"${UPSTREAM_MODULE}[/\"]" --include='*.go' .; then
      echo "!! $name imports the fork under the pre-rename path $UPSTREAM_MODULE;"
      echo "!! migrate its imports to $FORK_MODULE to gate it against this tree."
      exit 3
    fi
    # Point the fork dependency at THIS working tree. go mod edit -replace
    # overwrites any pre-existing directive (relative path or version) for the
    # same left-hand path, neutralizing a consumer's own local/pinned replace.
    go mod edit -replace "$FORK_MODULE=$FORK_DIR"
    go mod tidy   || exit 1
    echo "--- go build ./... ---";      go build ./...      || exit 1
    echo "--- go vet ./... ---";        go vet ./...        || exit 1
    echo "--- go test ./... -race ---"; go test ./... -race || exit 1
  )
  local rc=$?
  rm -rf "$scratch"
  case "$rc" in
    0) echo "== project $name OK =="; return 0 ;;
    3) echo "== project $name SKIPPED (imports pre-rename path $UPSTREAM_MODULE — migrate to $FORK_MODULE) =="; return 3 ;;
    *) echo "== project $name GATE FAILED =="; return 1 ;;
  esac
}

if [ "${GATE_SKIP_PROJECTS:-0}" = "1" ]; then
  echo "== GATE_SKIP_PROJECTS=1 — library gate only =="
elif [ -n "${GATE_PROJECT_DIRS:-}" ]; then
  IFS=':' read -ra _gate_dirs <<< "$GATE_PROJECT_DIRS"
  for _dir in "${_gate_dirs[@]}"; do
    [ -n "$_dir" ] || continue
    gate_project "$(basename "$_dir")" "$_dir"
    case $? in
      0) : ;;
      3) skipped=$((skipped + 1)) ;;
      *) fail=1 ;;
    esac
  done
else
  echo "== no consumer projects configured (set GATE_PROJECT_DIRS=/a:/b) — library gate only =="
fi

if [ "$fail" -ne 0 ]; then
  echo "GATE: FAILURES (see above)"
elif [ "$skipped" -gt 0 ]; then
  echo "GATE: GREEN — but $skipped project(s) SKIPPED (see !! warnings above)"
else
  echo "GATE: ALL GREEN"
fi
exit "$fail"
