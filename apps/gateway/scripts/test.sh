#!/usr/bin/env bash
# Usage: scripts/test.sh [module-dir...]
# Runs the unit tests (see test-baseline.sh) and fails if a test that passes in the recorded baseline
# (docs/platform/gateway-baseline/results.tsv) fails, skips or is missing now. Tests that fail in the
# baseline are not checked: most of them depend on the machine (DNS, IPv6 loopback, backing services).
set -euo pipefail
cd "$(dirname "$0")/.."
baseline=../../docs/platform/gateway-baseline/results.tsv
out=$(mktemp -d)
# test-baseline.sh runs offline, so fetch every module the tests need first
go mod download
for mod in $(go work edit -json | jq -r '.Use[].DiskPath'); do
  (cd "$mod" && go list -test -deps ./... > /dev/null)
done
scripts/test-baseline.sh "$out" "$@"
regressions=$(awk -F'\t' -v mods="$*" '
  BEGIN { n = split(mods, m, " "); for (i = 1; i <= n; i++) want[m[i]] = 1 }
  NR == FNR { now[$1 FS $2 FS $3] = $4; next }
  $4 == "pass" && (n == 0 || $1 in want) {
    key = $1 FS $2 FS $3
    if (now[key] != "pass") print key FS (key in now ? now[key] : "missing")
  }' "$out/results.tsv" "$baseline")
if [ -n "$regressions" ]; then
  echo "Regressions against the baseline (module, package, test, result now):" >&2
  echo "$regressions" >&2
  echo "Raw output: $out/raw" >&2
  exit 1
fi
echo "No regressions against the baseline. Results: $out/results.tsv"
