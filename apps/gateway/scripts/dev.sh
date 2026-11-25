#!/usr/bin/env bash
# Usage: scripts/dev.sh
# Local development on Linux or macOS (needs go, gcc and node): runs the gateway from source on
# GATEWAY_PORT (default 8080) and the UI dev server on http://localhost:3000, which proxies /api to
# the gateway. Edit the UI and it reloads; restart this script after changing Go code.
# Data lives in tmp/dev (SQLite). Set GATEWAY_SETUP_TOKEN to change the first-admin setup token.
set -euo pipefail
cd "$(dirname "$0")/.."
port=${GATEWAY_PORT:-8080}
http=$(find transports -maxdepth 1 -type d -name '*-http')
# the server embeds the built UI directory, so it has to exist even when the dev server is used
mkdir -p tmp/dev
trap 'kill 0' EXIT
(cd "$http" && BIFROST_SETUP_TOKEN=${GATEWAY_SETUP_TOKEN:-local-setup-token} \
  go run . -app-dir "$OLDPWD/tmp/dev" -port "$port" -host 127.0.0.1 -log-style pretty) &
wait
