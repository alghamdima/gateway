#!/usr/bin/env bash
# Usage: scripts/import.sh <path-to-upstream-clone> <tag>
# Extracts the product parts of upstream at <tag> over this directory, applies patches/*.patch in order,
# and records the source in UPSTREAM. The paths outside core/framework/plugins/transports/ui are the ones
# the unit tests read (OpenAPI spec, MCP test servers, the example plugin, local pricing files, the Helm
# values schema) plus the compose file for the tests' backing services.
# shortcut: files upstream deleted between tags are left behind; wipe upstream paths first when doing the first upgrade.
set -euo pipefail
src=$1 tag=$2
cd "$(dirname "$0")/.."
git -C "$src" -c core.autocrlf=false -c core.eol=lf archive "$tag" -- \
  .claude .dockerignore .editorconfig .gitattributes .nvmrc \
  AGENTS.md LICENSE THIRD_PARTY_NOTICES.md Makefile recipes \
  core framework plugins transports ui \
  docs/openapi examples/mcps examples/plugins/hello-world examples/configs/withlocalpricingfiles \
  helm-charts/bifrost/values.schema.json tests/docker-compose.yml | tar -x
for p in patches/*.patch; do
  git apply --directory="$(git rev-parse --show-prefix)" "$p"
done
printf 'repo: %s\ntag: %s\ncommit: %s\n' \
  "$(git -C "$src" remote get-url origin)" "$tag" "$(git -C "$src" rev-parse "$tag^{commit}")" > UPSTREAM
