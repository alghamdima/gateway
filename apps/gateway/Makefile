# Makefile for the gateway engine.
#
# Only the build lives here. Tests, the baseline and the rename have their own scripts under
# scripts/ (test.sh, test-baseline.sh, verify.sh, dev.sh, rename/), and images are built from
# transports/Dockerfile.local.
#
#   make build LOCAL=1      build tmp/bifrost-http from the sources in this directory (go.work)
#   make build              build against the published modules named in transports/go.mod
#
# Variables: VERSION (default dev-build), BUILD_TAGS (extra Go build tags), DYNAMIC=1 (dynamic
# linking on Linux; the default is a static binary), GOOS/GOARCH (must match the host: cross
# builds go through the Dockerfile).

VERSION ?= dev-build
LOCAL ?=
DYNAMIC ?=
BUILD_TAGS ?=

SHELL := /usr/bin/env bash

.DEFAULT_GOAL := build
.PHONY: build help

help: ## Show the targets
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-8s %s\n", $$1, $$2}'

build: ## Build tmp/bifrost-http (LOCAL=1 builds from the sources here)
	@set -euo pipefail; \
	host_os=$$(uname -s | tr '[:upper:]' '[:lower:]'); \
	host_arch=$$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/'); \
	target_os=$${GOOS:-$$host_os}; \
	target_arch=$${GOARCH:-$$host_arch}; \
	if [ "$$target_os/$$target_arch" != "$$host_os/$$host_arch" ]; then \
		echo "Cross builds are not supported here ($$host_os/$$host_arch -> $$target_os/$$target_arch): use transports/Dockerfile.local." >&2; \
		exit 1; \
	fi; \
	ldflags="-w -s -X main.Version=v$(VERSION)"; \
	tags="sqlite_static $(BUILD_TAGS)"; \
	if [ "$$target_os" = "linux" ]; then \
		if [ -n "$(DYNAMIC)" ]; then tags="$(BUILD_TAGS)"; else ldflags="-w -s -extldflags \"-static\" -X main.Version=v$(VERSION)"; fi; \
	fi; \
	mkdir -p tmp; \
	echo "Building tmp/bifrost-http for $$target_os/$$target_arch$(if $(LOCAL), from local sources (go.work),)..."; \
	cd transports/bifrost-http; \
	CGO_ENABLED=1 $(if $(LOCAL),,GOWORK=off) go build -ldflags="$$ldflags" -trimpath -tags "$$tags" -o ../../tmp/bifrost-http .; \
	echo "Built: tmp/bifrost-http (version: v$(VERSION))"
