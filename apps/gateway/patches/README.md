# Patches to upstream files

Every edit to a file that came from upstream lives here as a patch, applied in order by `scripts/import.sh`
right after the tag is extracted (ADR 0008). The committed tree is always "upstream tag + these patches";
re-running the import must leave `git status` clean.

| Patch | Touches | Why |
| --- | --- | --- |
| `0001-ui-build-heap.patch` | `ui/.npmrc` | The UI typecheck (`tsc --noEmit`, part of `npm run build` and `build-enterprise`) peaks near 2.9 GB of heap and dies at a 2 GB limit, which is what Node picks on a small machine or container. `node-options` in `.npmrc` gives every npm script a 4 GB limit, so the Makefile and both Dockerfiles are covered without editing them. |
| `0002-ui-drop-stale-next-env.patch` | `ui/next-env.d.ts` (deleted) | A leftover from when the UI was a Next.js app (upstream's own `.gitignore` lists it). Its `/// <reference types="next" />` resolves nothing upstream, but inside this monorepo it finds `next` in the pnpm root `node_modules`, drags in a second copy of `@types/react`, and fails the typecheck with 4 `TS2322` errors. |
| `0003-remove-maxim-plugin.patch` | `transports/bifrost-http/server/plugins.go`, `lib/ctx.go`, `lib/config.go`, `transports/go.mod`, `go.sum`, the observability view in `ui/` | Takes the Maxim observability plugin out of the build: its registration, its `x-bf-maxim-*` header handling, its Go dependencies and its tab in the UI. The plugin's own directory and UI files are not deleted by this patch; `scripts/import.sh` leaves them out of the extract (its `dropped` list). `go.work` no longer lists the module. |

To add one: edit the file, then `git diff --relative -- <file> > patches/NNNN-name.patch` from `apps/gateway`.
