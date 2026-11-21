#!/usr/bin/env bash
# Usage: scripts/test-baseline.sh <out-dir> [module-dir...]
# Runs `go test -json ./...` in each go.work module with no provider credentials and no network egress,
# keeps the raw event stream in <out-dir>/raw/, and writes <out-dir>/results.tsv:
#   module <TAB> package <TAB> test <TAB> pass|fail|skip
# Top-level tests only; a package that fails to build or has no test files gets one row with test "-".
# Needs go, gcc, node and jq. Modules must already be in the module cache (run `go mod download` first).
# The MCP test servers under examples/mcps are built first (that step uses the network); the MCP tests
# start them over STDIO.
set -uo pipefail
out=$(mkdir -p "$1" && cd "$1" && pwd)
shift
cd "$(dirname "$0")/.."
modules=("$@")
[ ${#modules[@]} -gt 0 ] || mapfile -t modules < <(go work edit -json | jq -r '.Use[].DiskPath | ltrimstr("./")')
mkdir -p "$out/raw"
make setup-mcp-tests > "$out/setup-mcp-tests.log" 2>&1 || { echo "setup-mcp-tests failed, see $out/setup-mcp-tests.log" >&2; exit 1; }
: > "$out/results.tsv"
for mod in "${modules[@]}"; do
  raw="$out/raw/${mod//\//-}.jsonl"
  echo "== $mod" >&2
  (cd "$mod" && env -i PATH="$PATH" HOME="$HOME" TMPDIR="${TMPDIR:-/tmp}" \
    GOPATH="$(go env GOPATH)" GOCACHE="$(go env GOCACHE)" GOMODCACHE="$(go env GOMODCACHE)" \
    GOTOOLCHAIN="${GOTOOLCHAIN:-auto}" GOPROXY=off CGO_ENABLED=1 \
    go test -json -count=1 -timeout "${TEST_TIMEOUT:-20m}" ./... > "$raw" 2> "${raw%.jsonl}.stderr")
  echo "   exit $?" >&2
  jq -r --arg mod "$mod" '
    select(.Action == "pass" or .Action == "fail" or .Action == "skip")
    | select(.Package != null)
    | select(.Test == null or (.Test | contains("/") | not))
    | [$mod, .Package, (.Test // "-"), .Action] | @tsv' "$raw" >> "$out/results.tsv"
done
LC_ALL=C sort -o "$out/results.tsv" "$out/results.tsv"
