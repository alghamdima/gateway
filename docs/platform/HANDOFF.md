# Handoff: what is left and the plan

Written 2026-11-26 for the next agent. Read `AGENTS.md` and `STATE.md` first. This file covers the work in flight and the order of what follows. Once it is done, fold what is still true into `STATE.md` and `PLAN.md`, then delete this file.

## Where the code is

- Work branch: `feat/ai-gateway` on `origin` (`github.com/alghamdima/gateway`). GitHub reports that the repository moved to `Lem-ru-sa/Lemru-sa`, and pushes to `origin` still land there. Push only this branch. Never push to `main`, never force-push, never rewrite history. Never push to `turbostarter/core`, the template vendor.
- Last verified commit: `10e637c8`.
- The MCP removal in progress is on `claude/stage-a-gateway-foundation-xoo9f9`: one commit, `908400ab`, on top of `10e637c8`. It is unverified, so do not merge it as is (see step 1).
- Pull request `alghamdima/gateway#1`. CI on that repository fails within seconds for every job (`runner_id 0`): this is an account matter, not the code. Do not re-run it and do not comment again. On the new repository Actions shows no workflows, and the owner has to enable Actions there. Until then run every check locally.

## Owner rules learned in the last session (also in AGENTS.md)

- **Remove all or keep all:** a feature taken out goes fully, with its code, routes, schemas, migrations, tests and baseline rows. No half measures.
- **Switched off is not removed:** TTS, STT, images, video, realtime, rerank, batch and files are part of the product. The credits plugin refuses them with a 403 today (`classify` in `apps/gateway/plugins/credits/plugin.go`). Never delete their code, routes, schemas or tests.
- Talk to the owner in Arabic (Saudi dialect). Code, commits and docs are in English. Keep answers short and back every claim with a check you ran.
- Commit trailers: `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>` and the session link. Commit messages follow Conventional Commits. No model names in code or docs.

## Gateway cleanup: status

Seven features are outside the product (task #41). Six are removed, verified and pushed:

| Commit     | Removed                        |
| ---------- | ------------------------------ |
| `5a031181` | RSS feeds                      |
| `2555ab7d` | outbound webhooks              |
| `31af3f75` | Warp (internal assistant chat) |
| `0808a19e` | the prompt repository          |
| `4173100f` | the Agent Gateway (A2A)        |
| `3de89485` | relays                         |

Each one is recorded in `apps/gateway/MODIFICATIONS.md`, under "Features outside the product". The seventh, MCP, is in progress.

## The recipe for one removal (used for all six)

1. Delete the feature's own files.
2. Build every module and fix the compile errors until it builds, removing each use in shared files.
3. Delete its migrations. No deployment exists, so there is no data to keep.
4. Prune its test-baseline rows: `python3 prune-baseline.py --write` updates `docs/platform/gateway-baseline/results.tsv`. The script was in the old session's scratchpad and is lost. Rewrite it if you need it: it drops the rows of tests that no longer exist.
5. Add a sub-bullet under "Features outside the product" in `MODIFICATIONS.md`. It lists every file deleted and every change to a shared file, as the six existing bullets do.
6. Verify, all of it:
   - `go build ./... && go vet ./...` in every module of `apps/gateway/go.work` (14 modules).
   - Grep for leftovers: Go, JSON schemas (`transports/config.schema.json`, `helm-charts/bifrost/values.schema.json`), `docs/openapi`, `Makefile`, and the TypeScript side (`packages/gateway-client`, `packages/api`, `apps/web`).
   - The control-plane routes the web app needs still exist: `/api/governance/customers`, `/api/governance/virtual-keys`, `/api/keys`, `/api/logs`, `/api/logs/rankings/by-dimension`, `/api/models/details`, `/api/pricing/force-sync`, `/api/providers`.
   - The switched-off request types (TTS, STT, images, video, realtime, rerank, batch, files) still have their code and routes.
   - Credits end-to-end test with the race detector (see "Checks" below).
   - `bash apps/gateway/scripts/test.sh <module>` against the recorded baseline.

## Step 1: finish the MCP removal

State of `908400ab`, as checked on 2026-11-26:

- 368 files changed, about 142,000 lines removed. The MCP tables, OAuth tables, handlers and most tests are gone.
- All 14 modules build.
- `go vet` fails in two transports tests that still use removed types:
  - `transports/bifrost-http/lib/config_test.go:1554` uses `tables.TableOauthConfig`
  - `transports/bifrost-http/handlers/config_test.go:1694` uses `schemas.MCPCodeModeLimits`
- 94 non-test Go files still contain "mcp" (case-insensitive). Some are legitimate: the `logs_add_correlation_columns` migration adds columns to `mcp_tool_logs`, and that table goes with MCP. Decide each one.
- Not done yet: the leftover sweep, the `MODIFICATIONS.md` bullet, baseline pruning, the credits e2e run, `test.sh`.

Steps:

1. Run `git checkout feat/ai-gateway && git merge --ff-only origin/claude/stage-a-gateway-foundation-xoo9f9`. This is a fast-forward and does not rewrite history. Alternatively run `git cherry-pick 908400ab`. Either way, do not push until step 1.5 passes.
2. Fix the two vet failures by deleting the test cases that cover MCP, not by stubbing the types.
3. Sweep the leftovers listed above. Remove the TypeScript-side MCP fields, if any, in `packages/gateway-client`. The old session found no MCP strings in `packages/` or `apps/web/src` outside `node_modules`.
4. Delete `apps/gateway/docs` and `apps/gateway/examples`, except files that remaining tests still read. Run `grep -rn 'docs/\|examples/' --include=*_test.go apps/gateway` before deleting.
5. Verify with the full list in the recipe, then amend nothing. Add one commit on top, for example `feat(gateway)!: remove MCP`, then push `feat/ai-gateway`.

## Step 2: Makefile and loose ends

- Replace the roughly 3,000-line `apps/gateway/Makefile` with one that has `build` (with `LOCAL=1`) and what `scripts/` really calls. Today it still has relay and MCP e2e targets. `scripts/test-baseline.sh` calls `make setup-mcp-tests`: remove that call too.
- Remove mentions of the seven features from the changelog files and their routes and schemas from `docs/openapi` if that folder survives step 1.4. If nothing builds, runs or tests it, delete it (AGENTS.md: a folder nothing uses is deleted).
- Record every change in `MODIFICATIONS.md`.

## Step 3: full verification, once

```sh
service postgresql start            # no Docker here; cluster 16 main on 5432
node --run format && node --run lint && node --run typecheck
env -u SKIP_ENV_VALIDATION pnpm with-env pnpm -F @workspace/api exec vitest run
pnpm with-env pnpm --filter web build
(cd apps/gateway && make build LOCAL=1)
bash apps/gateway/scripts/test.sh            # every module
bash apps/gateway/scripts/verify.sh
(cd apps/gateway/transports && go build -o /tmp/gateway-e2e ./bifrost-http)
(cd apps/gateway/plugins/credits && \
  CREDITS_TEST_DATABASE_URL=postgresql://turbostarter:turbostarter@localhost:5432/core \
  CREDITS_E2E_BINARY=/tmp/gateway-e2e go test -count=1 -race ./...)
```

Rebuild the gateway binary after every engine change, so the e2e test runs the current code. Use your scratchpad instead of `/tmp` if your environment provides one.

## Step 4: update the docs

- `STATE.md`:
  - "What is done" gains the seven removals.
  - "What is next" item 1 is closed.
  - The GitHub Actions note is updated for the new repository.
  - The date is updated.
- `PLAN.md`: mark the gateway cleanup done. Add the plan for switching on request types (below).
- Task #41 is done.

## Then: the rest of the project, in order

1. **Switch on request types, one at a time.** For each one: a way to bound the worst-case cost before the call (needed for the reserve in `plugins/credits`), a price source, a settle from the real usage, an e2e test, then allow it in `classify`.
   1. First, e2e tests for embeddings and responses. Both are already billed but have no e2e test.
   2. STT and TTS.
   3. Images.
   4. Realtime, rerank, batch and files, later.
2. **Playground** like OpenRouter's chat, as a new module that calls the gateway with the organization's key, so credits and the data policy apply (`STATE.md`, "What is next" 2).
3. **Response cache, opt-in** per request. Never on in the in-Kingdom deployment, and never for an organization whose policy forbids retention.
4. **Arabic** with right-to-left layout, in the web app and in emails.
5. **Gateway rename** to Lemru (`apps/gateway/scripts/rename`, prefix `lm`), once a domain exists (task #37).
6. **Saudi payments** once the owner picks a gateway (Moyasar, Tap, HyperPay, PayTabs). It becomes a provider module beside Stripe in `packages/billing`.

## Waiting on the owner

- Enable GitHub Actions on `Lem-ru-sa/Lemru-sa`, and decide what `main` holds there. Today `feat/ai-gateway` is the default branch.
- Domain, hosting for both deployments, logo, payment gateway, chargeback rule, legal pages, PostHog consent (all in `STATE.md`).
