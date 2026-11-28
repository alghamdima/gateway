#!/usr/bin/env bash
# Usage: scripts/dev.sh
# Local development on Linux or macOS (needs go and gcc): runs the gateway from source on
# GATEWAY_PORT (default 8080). Restart this script after changing Go code.
# Data lives in tmp/dev, on SQLite: a quick local run only. The product runs on Postgres, and the
# gateway refuses SQLite without -allow-sqlite; `pnpm gateway:up` runs it the real way.
# Set GATEWAY_SETUP_TOKEN to change the first-admin setup token.
set -euo pipefail
cd "$(dirname "$0")/.."
port=${GATEWAY_PORT:-8080}
http=$(find transports -maxdepth 1 -type d -name '*-http')
mkdir -p tmp/dev
trap 'kill 0' EXIT
(cd "$http" && BIFROST_SETUP_TOKEN=${GATEWAY_SETUP_TOKEN:-local-setup-token} \
  go run . -allow-sqlite -app-dir "$OLDPWD/tmp/dev" -port "$port" -host 127.0.0.1 -log-style pretty) &
wait
