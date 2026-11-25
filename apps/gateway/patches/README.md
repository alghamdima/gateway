# Patches to upstream files

Every edit to a file that came from upstream lives here as a patch, applied in order by `scripts/import.sh`
right after the tag is extracted (ADR 0008). The committed tree is always "upstream tag + these patches";
re-running the import must leave `git status` clean.

| Patch | Touches | Why |
| --- | --- | --- |
| `0001-ui-build-heap.patch` | `ui/.npmrc` | The UI typecheck (`tsc --noEmit`, part of `npm run build` and `build-enterprise`) peaks near 2.9 GB of heap and dies at a 2 GB limit, which is what Node picks on a small machine or container. `node-options` in `.npmrc` gives every npm script a 4 GB limit, so the Makefile and both Dockerfiles are covered without editing them. |
| `0002-ui-drop-stale-next-env.patch` | `ui/next-env.d.ts` (deleted) | A leftover from when the UI was a Next.js app (upstream's own `.gitignore` lists it). Its `/// <reference types="next" />` resolves nothing upstream, but inside this monorepo it finds `next` in the pnpm root `node_modules`, drags in a second copy of `@types/react`, and fails the typecheck with 4 `TS2322` errors. |
| `0003-remove-maxim-plugin.patch` | `transports/bifrost-http/server/plugins.go`, `lib/ctx.go`, `lib/config.go`, `transports/go.mod`, `go.sum`, the observability view in `ui/` | Takes the Maxim observability plugin out of the build: its registration, its `x-bf-maxim-*` header handling, its Go dependencies and its tab in the UI. The plugin's own directory and UI files are not deleted by this patch; `scripts/import.sh` leaves them out of the extract (its `dropped` list). `go.work` no longer lists the module. |
| `0004-register-credits-plugin.patch` | `transports/bifrost-http/server/plugins.go` | One import and one `case`, so the gateway can load our `plugins/credits` by name from its config. The plugin itself is new files only. If its config cannot be decoded the case returns a plugin that refuses every request, because the gateway logs a plugin that fails to load and then serves without it. |
| `0005-ui-english-only.patch` | `ui/lib/i18n/index.ts`, ten UI test files | The product has one language list for the gateway UI and the control plane (owner, 2026-11-25): English until Arabic is added. `SUPPORTED_LOCALES` keeps English only; `scripts/import.sh` leaves the other ten catalogs out (its `dropped` list) together with two test files that do nothing but switch languages. In the remaining test files, the cases that need a second language are switched off with the reason beside them, or removed where they no longer compile. They come back when a second language exists. |

To add one: edit the file, then `git diff --relative -- <file> > patches/NNNN-name.patch` from `apps/gateway`.
