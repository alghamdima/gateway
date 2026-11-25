# Modifications

This directory is a modified copy of Bifrost (https://github.com/maximhq/bifrost), which is licensed under the Apache License, Version 2.0. The upstream licence and notices are kept unchanged in `LICENSE` and `THIRD_PARTY_NOTICES.md`. The exact upstream revision is recorded in `UPSTREAM`.

This file states the changes, as section 4(b) of the licence requires. It is never rewritten by the rename tool.

## Scope of the copy

Only part of the upstream repository is copied: `core`, `framework`, `plugins`, `transports`, `ui`, the build files at the root, and the paths the unit tests read (`docs/openapi`, `examples/mcps`, `examples/plugins/hello-world`, `examples/configs/withlocalpricingfiles`, `helm-charts/bifrost/values.schema.json`, `tests/docker-compose.yml`). The list is in `scripts/import.sh`.

## Files left out

`scripts/import.sh` does not copy the Maxim observability plugin or its UI:

- `plugins/maxim/`
- `ui/app/workspace/observability/views/plugins/maximView.tsx`
- `ui/app/workspace/observability/fragments/maximFormFragment.tsx`
- `ui/public/maxim-logo.webp`, `ui/public/maxim-logo-dark.webp`

Nor every language of the UI but English, with the two test files that only switch between languages:

- `ui/locales/{de,es,fr,it,ja,ko,pt,ru,zh-CN,zh-TW}/`
- `ui/lib/i18n/index.test.ts`, `ui/lib/i18n/useLocale.test.tsx`

## Files changed

Every change to an upstream file is a patch in `patches/`, applied in order after the copy. `patches/README.md` gives the reason for each.

| Patch | Files |
| --- | --- |
| `0001-ui-build-heap.patch` | `ui/.npmrc` |
| `0002-ui-drop-stale-next-env.patch` | `ui/next-env.d.ts` (deleted) |
| `0003-remove-maxim-plugin.patch` | `transports/bifrost-http/server/plugins.go`, `transports/bifrost-http/lib/ctx.go`, `transports/bifrost-http/lib/config.go`, `transports/go.mod`, `transports/go.sum`, `ui/app/workspace/observability/views/observabilityView.tsx` |
| `0004-register-credits-plugin.patch` | `transports/bifrost-http/server/plugins.go` |
| `0005-ui-english-only.patch` | `ui/lib/i18n/index.ts`; tests: `ui/app/workspace/agent-logs/views/agentTrace.test.ts`, `ui/app/workspace/logs/views/columns.test.ts`, `ui/app/workspace/mcp-sessions/views/sessionsTable.test.tsx`, `ui/lib/i18n/resources.test.ts`, `ui/lib/i18n/validationMessage.test.ts`, `ui/lib/types/schemas.proxy.test.ts`, `ui/lib/utils/databricksMigration.test.ts`, `ui/lib/utils/mcpCredential.test.ts`, `ui/lib/utils/numbers.test.ts` |

## Files added

- `UPSTREAM`, `MODIFICATIONS.md`, `go.work`
- `patches/`, `scripts/`, `compose/`
- `plugins/credits/`: the prepaid credits plugin

## Renaming

No file has been renamed yet. When a product name is chosen, `scripts/rename` will rewrite the upstream product name, module path, wire prefixes and vendor hosts across this directory, and rename the files and directories that carry the name. That step will be recorded here when it is applied.
