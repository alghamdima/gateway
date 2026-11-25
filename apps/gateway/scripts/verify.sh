#!/usr/bin/env bash
# Usage: scripts/verify.sh [brand.json]
# Rename acceptance (PLAN.md Step 2.4) on a scratch copy of the committed tree; nothing in the repo is
# changed. With no argument it uses scripts/rename/brand.example.json. The checks, in order:
#   1. the rename is deterministic (two fresh copies come out identical) and idempotent
#   2. no upstream name is left outside the allowed list
#   3. the renamed tree builds, UI included, and `go vet` is clean in every module
#   4. the binary boots, /health is 200, and 400/401/403 responses and the boot log carry no upstream name
#   5. the unit tests match the recorded baseline, with the baseline's names put through the same rename
# Before them it checks that the brand's domain resolves, and whether it serves the pricing datasheet.
# Not covered yet: a chat request through a virtual key against a mock upstream (needs an admin session).
# KEEP=1 leaves the scratch directory in place. The whole run takes about 40 minutes.
set -euo pipefail
here=$(cd "$(dirname "$0")/.." && pwd)
repo=$(git -C "$here" rev-parse --show-toplevel)
brand=$(realpath "${1:-$here/scripts/rename/brand.example.json}")
work=$(mktemp -d)
[ -n "${KEEP:-}" ] || trap 'rm -rf "$work"' EXIT
step() { printf '\n== %s\n' "$*"; }
upstream_names='bifrost|maximhq|getmaxim|x-bf-|sk-bf-'

step "0. the brand's domain"
domain=$(jq -r .domain "$brand")
# the catalog loader resolves the default pricing host before anything else; if it does not resolve,
# one fixture test fails and its whole package runs into the test timeout
getent hosts "$domain" > /dev/null || { echo "FAIL: $domain does not resolve in DNS; register it (or point it anywhere) first"; exit 1; }
skip=
if ! curl -fsS -m 20 -o /dev/null "https://$domain/datasheet"; then
  skip=$here/scripts/rename/needs-datasheet-host.txt
  echo "WARNING: https://$domain/datasheet is not served, so $(wc -l < "$skip") governance tests that download it are left out of check 5"
fi

step "1. rename twice from fresh copies"
(cd "$here/scripts/rename" && GOWORK=off go build -o "$work/rename" .)
for copy in a b; do
  mkdir -p "$work/$copy"
  git -C "$repo" archive HEAD apps/gateway docs/platform/gateway-baseline/results.tsv | tar -x -C "$work/$copy"
  # the baseline rides along inside the root so its package paths and test names get the same rename
  mv "$work/$copy/docs/platform/gateway-baseline/results.tsv" "$work/$copy/apps/gateway/baseline.tsv"
  "$work/rename" -brand "$brand" -root "$work/$copy/apps/gateway"
done
diff -r "$work/a" "$work/b" > /dev/null || { echo "FAIL: two renames of the same tree differ"; exit 1; }
tree=$work/a/apps/gateway
again=$("$work/rename" -brand "$brand" -root "$tree")
[ "$again" = "files rewritten: 0, paths renamed: 0, go.work pinned: false" ] || { echo "FAIL: second run changed something: $again"; exit 1; }

step "2. scan for upstream names"
"$work/rename" -scan -root "$tree"

step "3. build and vet"
cd "$tree"
make build LOCAL=1 > "$work/build.log" 2>&1 || { tail -30 "$work/build.log"; exit 1; }
for mod in $(go work edit -json | jq -r '.Use[].DiskPath'); do
  (cd "$mod" && go vet ./...)
done
binary=$(find tmp -maxdepth 1 -type f -name '*-http')

step "4. boot $binary"
port=${VERIFY_PORT:-18080}
app=$work/app && mkdir -p "$app"
pricing=$tree/examples/configs/withlocalpricingfiles
printf '{"framework":{"pricing":{"pricing_url":"file://%s/pricing.json","model_parameters_url":"file://%s/model-parameters.json"}}}\n' "$pricing" "$pricing" > "$app/config.json"
"$binary" -app-dir "$app" -port "$port" -host 127.0.0.1 > "$work/boot.log" 2>&1 &
pid=$!
trap 'kill $pid 2>/dev/null || true; [ -n "${KEEP:-}" ] || rm -rf "$work"' EXIT
for _ in $(seq 60); do curl -fsS -o /dev/null "http://127.0.0.1:$port/health" 2>/dev/null && break; sleep 1; done
curl -fsS "http://127.0.0.1:$port/health"; echo
for path in /health /v1/models /api/config '/v1/chat/completions -d {"model":"openai/gpt-4o-mini","messages":[]}'; do
  # shellcheck disable=SC2086
  curl -sS -i -m 20 -H 'content-type: application/json' http://127.0.0.1:$port$path > "$work/resp.txt"
  echo "$(head -1 "$work/resp.txt" | tr -d '\r')  ${path%% *}"
  ! grep -qiE "$upstream_names" "$work/resp.txt" || { echo "FAIL: upstream name in the response to ${path%% *}"; exit 1; }
done
# the gateway serves no dashboard: the product's only frontend is the web app
[ "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/")" = 404 ] || { echo "FAIL: / is not a 404"; exit 1; }
! grep -qiE "$upstream_names" "$work/boot.log" || { echo "FAIL: upstream name in the boot log"; grep -iE "$upstream_names" "$work/boot.log" | head -3; exit 1; }
kill $pid

step "5. unit tests against the renamed baseline"
if [ -n "$skip" ]; then
  awk -F'\t' 'NR == FNR { drop[$1] = 1; next } !($3 in drop && $1 == "plugins/governance")' "$skip" baseline.tsv > "$work/baseline.tsv"
else
  cp baseline.tsv "$work/baseline.tsv"
fi
BASELINE=$work/baseline.tsv scripts/test.sh

printf '\nAll rename acceptance checks passed for %s\n' "$brand"
