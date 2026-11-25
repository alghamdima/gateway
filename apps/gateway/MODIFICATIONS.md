# Modifications

This directory started as a copy of Bifrost (https://github.com/maximhq/bifrost), which is licensed under the Apache License, Version 2.0. The upstream licence and notices are kept unchanged in `LICENSE` and `THIRD_PARTY_NOTICES.md`. The revision the copy was taken from is recorded in `UPSTREAM`.

This file states the changes, as section 4(b) of the licence requires. It is never rewritten by the rename tool.

Since 2026-11-25 the code is developed here, by the owner's team, and no longer kept reproducible from the upstream tag: changes are made directly and listed below. A fix from upstream is picked by hand.

## Scope of the copy

Only part of the upstream repository was copied: `core`, `framework`, `plugins`, `transports`, `ui`, the build files at the root, and the paths the unit tests read (`docs/openapi`, `examples/mcps`, `examples/plugins/hello-world`, `examples/configs/withlocalpricingfiles`, `helm-charts/bifrost/values.schema.json`, `tests/docker-compose.yml`).

## Removed

- The Maxim observability plugin and its UI: `plugins/maxim/`, `ui/app/workspace/observability/views/plugins/maximView.tsx`, `ui/app/workspace/observability/fragments/maximFormFragment.tsx`, `ui/public/maxim-logo.webp`, `ui/public/maxim-logo-dark.webp`.
- Every language of the UI but English, with the two test files that only switch between languages: `ui/locales/{de,es,fr,it,ja,ko,pt,ru,zh-CN,zh-TW}/`, `ui/lib/i18n/index.test.ts`, `ui/lib/i18n/useLocale.test.tsx`.
- `ui/next-env.d.ts`, a leftover from when the UI was a Next.js app. Inside this monorepo its `/// <reference types="next" />` found the web app's `next`, pulled in a second copy of `@types/react` and failed the type check.
- `.claude/`, upstream's own agent tooling, and `recipes/` with the three `include` lines in the `Makefile`: deployment recipes (Fly, ECS, local Kubernetes) this product does not use.

## Changed

| Files | Change and reason |
| --- | --- |
| `ui/.npmrc` | `node-options` gives every npm script a 4 GB heap. The UI type check peaks near 2.9 GB and dies at the 2 GB limit Node picks on a small machine. |
| `transports/bifrost-http/server/plugins.go`, `transports/bifrost-http/lib/ctx.go`, `transports/bifrost-http/lib/config.go`, `transports/go.mod`, `transports/go.sum`, `ui/app/workspace/observability/views/observabilityView.tsx` | The Maxim plugin is taken out of the build: its registration, its `x-bf-maxim-*` header handling, its Go dependencies and its tab in the UI. |
| `transports/bifrost-http/server/plugins.go` | One import and one `case`, so the gateway loads `plugins/credits` by name from its config. If the plugin's config cannot be decoded the case returns a plugin that refuses every request, because the gateway logs a plugin that fails to load and then serves without it. |
| `ui/lib/i18n/index.ts` | `SUPPORTED_LOCALES` keeps English only: the product has one language list for the gateway and the control plane. |
| `ui/app/workspace/agent-logs/views/agentTrace.test.ts`, `ui/app/workspace/logs/views/columns.test.ts`, `ui/app/workspace/mcp-sessions/views/sessionsTable.test.tsx`, `ui/lib/i18n/resources.test.ts`, `ui/lib/i18n/validationMessage.test.ts`, `ui/lib/types/schemas.proxy.test.ts`, `ui/lib/utils/databricksMigration.test.ts`, `ui/lib/utils/mcpCredential.test.ts`, `ui/lib/utils/numbers.test.ts` | The cases that need a second language are switched off with the reason beside them, or removed where they no longer compile. |
| `Makefile` | The three `include recipes/*.mk` lines are gone with `recipes/`. |

## Added

- `UPSTREAM`, `MODIFICATIONS.md`, `go.work`
- `scripts/` (build, test and rename helpers), `compose/` (the shipped configuration)
- `plugins/credits/`: the prepaid credits plugin

## Renaming

No file has been renamed yet. The product is named Lemru, with `lm` as the short prefix; `scripts/rename` will rewrite the upstream product name, module path, wire prefixes and vendor hosts across this directory once a domain exists. That step will be recorded here when it is applied.
