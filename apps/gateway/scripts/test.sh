#!/usr/bin/env bash
# Usage: scripts/test.sh [module-dir...]
# Runs the unit tests (see test-baseline.sh) and fails if a test that passes in the recorded baseline
# (docs/platform/gateway-baseline/results.tsv) fails, skips or is missing now. Tests that fail in the
# baseline are not checked: most of them depend on the machine (DNS, IPv6 loopback, backing services).
# A test that regresses in the full run is run once more on its own, because a few upstream tests time
# out when the whole suite loads the machine; it counts only if it fails again.
# RESULTS=<dir> reuses the output of an earlier run instead of running the suite.
set -euo pipefail
cd "$(dirname "$0")/.."
baseline=$PWD/../../docs/platform/gateway-baseline/results.tsv
# test-baseline.sh runs offline, so fetch every module the tests need first
go mod download
for mod in $(go work edit -json | jq -r '.Use[].DiskPath'); do
  (cd "$mod" && go list -test -deps ./... > /dev/null)
done
out=${RESULTS:-}
if [ -z "$out" ]; then
  out=$(mktemp -d)
  scripts/test-baseline.sh "$out" "$@"
fi
regressions=$(awk -F'\t' -v mods="$*" '
  BEGIN { n = split(mods, m, " "); for (i = 1; i <= n; i++) want[m[i]] = 1 }
  NR == FNR { now[$1 FS $2 FS $3] = $4; next }
  $4 == "pass" && (n == 0 || $1 in want) {
    key = $1 FS $2 FS $3
    if (!(key in now)) print key FS "missing"
    else if (now[key] != "pass") print key FS now[key]
  }' "$out/results.tsv" "$baseline")
confirmed=
while IFS=$'\t' read -r mod pkg test result; do
  [ -n "$mod" ] || continue
  if [ "$test" != "-" ] && [ "$result" = "fail" ] &&
    (cd "$mod" && env -i PATH="$PATH" HOME="$HOME" TMPDIR="${TMPDIR:-/tmp}" \
      GOPATH="$(go env GOPATH)" GOCACHE="$(go env GOCACHE)" GOMODCACHE="$(go env GOMODCACHE)" \
      GOTOOLCHAIN="${GOTOOLCHAIN:-auto}" GOPROXY=off CGO_ENABLED=1 \
      go test -count=1 -run "^$test\$" "$pkg" > /dev/null 2>&1); then
    echo "flaky, passed on its own: $pkg $test" >&2
  else
    confirmed+="$mod"$'\t'"$pkg"$'\t'"$test"$'\t'"$result"$'\n'
  fi
done <<< "$regressions"
if [ -n "$confirmed" ]; then
  echo "Regressions against the baseline (module, package, test, result now):" >&2
  printf '%s' "$confirmed" >&2
  echo "Raw output: $out/raw" >&2
  exit 1
fi
echo "No regressions against the baseline. Results: $out/results.tsv"
