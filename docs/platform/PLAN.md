# Build plan: <<BRAND_NAME>> AI gateway platform

Status: **In progress.** The owner approved Stage A (foundation) on 2026-11-21 and set the order of work: foundation, then rename, then integration (see Stages).
Replaces the earlier draft of this file (still in git at `4f99cbdd`).

One product in one repo: this TurboStarter monorepo is the control plane (sign-up, organizations, top-ups, keys, usage), and Bifrost, fully white-labeled, lives in `apps/gateway/` as the data plane that serves the API. Behavior is modeled on OpenRouter (briefs in `research/`). The architecture is in `ARCHITECTURE.md` and the decisions are in `adr/`. This file is the order of work.

## What the research found (facts the plan rests on)

### TurboStarter (`core`)

- pnpm workspace globs are `apps/*`, `packages/**` and `tooling/*`. `apps/gateway/` has no `package.json` of its own, so pnpm skips it and the gateway UI's npm `package.json` and `package-lock.json` don't join the pnpm workspace. `apps/gateway/**` is ignored by oxlint, oxfmt and the root vitest projects.
- API: Hono, mounted at `/api`, with one router per module in `packages/api/src/modules/` (admin, ai, auth, billing, organization, user). New modules: `credits`, `keys`, `usage`.
- DB: Drizzle with `packages/db/src/schema/{auth,billing}.ts` and 16 migrations. Billing already has `customer.referenceId`, which can be a user or an organization, plus `order` for one-time payments.
- Billing: the active web provider is **Stripe** (`packages/billing/web/src/providers/index.ts`). The webhook handler takes `WebhookCallbacks`, but the router passes none (`packages/api/src/modules/billing/router.ts:125`), so that's a clean hook for crediting the ledger.
  - It only handles `checkout.session.completed`.
  - It doesn't handle `checkout.session.async_payment_succeeded` or `charge.refunded`.
- Auth: Better Auth with the `admin()` and `organization()` plugins. RBAC already has a `billing` permission per role (member read, admin create/read, owner all). We add a `keys` permission the same way.
- Brand surface: 172 `turbostarter` hits in 61 files (i18n, auth `appName`/`cookiePrefix`, email layout, marketing, mobile e2e, generators). Package scope is `@workspace/*`, which has no brand in it.
- **Project skills are broken on Windows:** every entry in `.claude/skills/` and `.cursor/skills/` is a 38-byte stub (a symlink that didn't survive checkout). The real files are in `.agents/skills/`. Claude Code doesn't load them until this is fixed (Step 0.1).

### Bifrost (`bifrost`, checked out at `transports/v2.6.0`, Go 1.27.4)

- It's 58 Go modules. The product needs 15 of them: `core`, `framework`, `transports`, and 12 plugins (`maxim` is dropped). The rest are `examples/`, `tests/`, `cli/`, `scripts/` and `docs/` helpers.
- Brand occurrences inside the product areas (`core framework transports plugins ui`):

  | Kind                           | Hits   | Example                                                                    |
  | ------------------------------ | ------ | -------------------------------------------------------------------------- |
  | Go identifiers `Bifrost[A-Z]…` | 52,092 | `schemas.BifrostRequest`, `BifrostContext`, `BifrostError`                 |
  | Module path                    | 4,840  | `github.com/maximhq/bifrost/core`                                          |
  | Wire headers                   | 1,232  | `x-bf-vk` and other `x-bf-*`                                               |
  | Key prefix                     | 995    | `sk-bf-`                                                                   |
  | Metric and DB names            | 512    | `bifrost_upstream_latency_seconds`                                         |
  | Env vars                       | 469    | `BIFROST_ENCRYPTION_KEY`                                                   |
  | Vendor URLs                    | 178    | `getbifrost.ai` (pricing datasheet, MCP library, OpenAPI, RSS), `getmaxim` |
  | File and directory names       | 141    | `bifrost-http/`, `bifrostmcp.go`                                           |

  Brand occurrences outside the product: `docs/` 23k, `tests/` 5.9k, `helm-charts/` 2.8k, `cli/` 1k, `examples/` 1.2k.

- Outbound calls to vendor hosts: pricing datasheet (`framework/modelcatalog/datasheet/store.go`), MCP library sync, the announcements RSS feed, and `framework/warp`. Each one gets pointed at our host or switched off.
- Governance API (`/api/governance/customers`, `/virtual-keys`, `/budgets`, `/rate-limits`, `/pricing-overrides`) covers everything the dashboard needs. Two details matter:
  - A virtual key with empty `provider_configs` is **deny-all**, so every key we create must list providers with `allowed_models: ["*"]`.
  - The key value can't be chosen by the caller. The rename turns its prefix into ours.
- Inference auth: `client_config.enforce_auth_on_inference` must be on, so no request runs without a key.
- 40 providers are built in, OpenRouter among them. Fallbacks are supported. `plugins/compat` normalizes params across providers. Model ids are `provider/model`, the same shape as OpenRouter's.
- Bifrost's own skills (`bifrost/.claude/skills/`) are used where they fit: `add-pricing-field` (P5), `harness-test-writer` and `e2e-test` (gateway tests), `security-audit` (P10), `release-checklist` (upgrades).

### OpenRouter

The nine briefs in `research/` (fetched 2026-11-21) cover credits, keys, catalog and pricing, routing and fallbacks, BYOK, generation records, usage analytics, rate limits, and organizations. They are detailed enough to build from. Two things the briefs mention that the old plan didn't place:

- The **OpenRouter-compatible URL surface** (`/api/v1/chat/completions`, `/api/v1/models`, `/api/v1/key`, `/api/v1/credits`, `/api/v1/generation`). With it, a customer switches from OpenRouter by changing only the base URL. Placed in P8.
- **Specific error contracts:**
  - `402` when credits run out.
  - `403` with `limit_source` when a key's limit is hit.
  - Distinct `401` messages for each auth failure.
  - In-stream errors sent as `finish_reason: "error"`.

  Placed in P4 and P8.

### Machine (re-checked 2026-11-21; an earlier version of this section was wrong)

- Node 24.22 and pnpm 12 are present.
- **Go 1.27.4 is installed** on Windows (`%LOCALAPPDATA%\Programs\Go`). There is no gcc or `make`, and the gateway needs CGO for SQLite, so a native Windows build isn't possible.
- **Kiri 0.2.2 provides Docker**: a Docker-compatible engine (27.5.1) in the WSL2 distro `kiri`, with the `docker` CLI, Compose 5.6 and buildx in `%LOCALAPPDATA%\Kiriin`. The gateway is built and run through it (`pnpm gateway:up`). The same engine holds other projects' images and containers, so nothing is pruned or removed there.
- Symlink creation is not allowed for this user (Windows Developer Mode is off). That is why the project skills are stubs (Step 0.1).

## Target layout

```
core/                              (this repo)
  apps/web                         dashboard, admin, public catalog
  packages/api/src/modules/
    credits/  keys/  usage/        new
  packages/db/src/schema/
    credits.ts  keys.ts            new
  apps/gateway/                Bifrost (pristine in Stage A, renamed in Stage B), not a pnpm package
    core/ framework/ transports/ plugins/ ui/
    plugins/credits/               ours, new files only
    patches/                       our small edits to upstream files, ordered
    scripts/rename/                deterministic rename + its tests
    UPSTREAM                       repo, tag, commit
    LICENSE NOTICE THIRD_PARTY_NOTICES.md MODIFICATIONS.md
  docker-compose.yml               + gateway service
```

## Stages (owner decision, 2026-11-21)

The end result is one monorepo with two applications: `apps/web` (dashboard) and `apps/gateway` (API gateway). The work runs in three stages, and the phases below keep their numbers:

| Stage          | Goal                                                                                       | Phases and steps                                          |
| -------------- | ------------------------------------------------------------------------------------------ | --------------------------------------------------------- |
| A. Foundation  | Both applications live in this repo, untouched upstream gateway builds and boots from here | Phase 0, Phase 1, the import half of Step 2.2, Step 2.3   |
| B. Rename      | Full white-label of both codebases                                                         | Step 2.1, the cleanup half of Step 2.2, Step 2.4, Phase 7 |
| C. Integration | Credits, keys, payments, API surface, usage, operations                                    | Phases 3, 4, 5, 6, 8, 9, 10 (11 later)                    |

Stage A imports the gateway with its upstream names. Stage B then replaces that tree with the output of the rename script, so every later upgrade is "import the new tag, rename, apply patches" (ADR 0008).

## Phases

Order: risk first, one vertical slice early, and brand-dependent work last where possible. Sizes: S = 1–3 files, M = 4–10, L+ = split further.

### Phase 0: Prepare (no owner input needed)

Verified when: Go builds a hello-world, `docker compose up` starts Postgres, and project skills load.

#### Step 0.1: Fix project skill links (Size: S)

- Acceptance criteria: each `.claude/skills/<name>` is a real directory whose `SKILL.md` matches `.agents/skills/<name>/SKILL.md`. Same for `.cursor/skills`.
- Verification command: `find .claude/skills -name SKILL.md | wc -l` equals the number of skills in `skills-lock.json`.
- Files likely touched: `.claude/skills/*`, `.cursor/skills/*`, `.gitattributes` or git `core.symlinks` config.
- Test seams: file listing.
- Blocked by: none.

#### Step 0.2: Toolchain (Size: S)

- Acceptance criteria: Go 1.27.4 and the Kiri `docker` CLI are on PATH, and TurboStarter `pnpm services:setup` runs green. Nothing new is installed: both are already on the machine.
- Verification command: `go version && docker compose ps`.
- Files likely touched: none in the repo.
- Test seams: commands.
- Blocked by: none.

#### Step 0.3: Work branch (Size: XS)

- Acceptance criteria: branch `feat/gateway-platform` off `main`, with the uncommitted `PLAN.md` change carried over.
- Verification command: `git status`.
- Blocked by: approval.

### Phase 1: Pristine gateway baseline (nothing committed except the report)

Verified when: `docs/platform/gateway-baseline.md` lists, per module, passing, failing and skipped test **names**, and the pristine binary answers `GET /health` with a local pricing file.

#### Step 1.1: Build and boot pristine `transports/v2.6.0` in a scratch dir (Size: S)

- Acceptance criteria: `make build LOCAL=1` succeeds with the UI embedded. It boots against Postgres. The pricing URL points at a local file, so startup makes no call to `getbifrost.ai`.
- Verification command: `curl -fsS localhost:8080/health`.
- Files likely touched: scratch only.
- Test seams: HTTP health.
- Blocked by: 0.2.

#### Step 1.2: Unit-test baseline per module (Size: S)

- Acceptance criteria: `go test ./...` has been run in each of the 15 product modules with no provider keys, and the results are recorded by name.
- Verification command: `go test -json ./... | tee` per module, summarized into `gateway-baseline.md`.
- Files likely touched: `docs/platform/gateway-baseline.md`.
- Test seams: Go test JSON.
- Blocked by: 1.1.

#### Step 1.3: Outbound-call inventory (Size: S)

- Acceptance criteria: every host the pristine gateway contacts at boot and in one request is listed, each with a disposition (point to our host, local file, or off).
- Verification command: run with egress logged (Docker network with a logging proxy) and diff against the list.
- Files likely touched: `gateway-baseline.md`.
- Blocked by: 1.1.

### Phase 2: Import, then rename script (the pristine tree lands first, in Stage A)

Verified when: all seven checks in Step 2.4 pass.

#### Step 2.1: Rename script with its own tests (Size: M)

- Acceptance criteria:
  - The script is deterministic and idempotent: run twice, the second run changes nothing.
  - Mappings run most-specific first: module path, wire names (headers, key prefix, env vars, metrics, config keys, DB names), Go identifiers, file and directory names, UI text and assets.
  - Its tests cover the traps: `maximum`/`maximize`, `Maxim` inside unrelated words, URLs, and license headers left untouched.
- Verification command: `go test ./apps/gateway/scripts/rename/...`.
- Files likely touched: `apps/gateway/scripts/rename/{main.go,mappings.go,rename_test.go}`.
- Test seams: table tests on string and file fixtures.
- Blocked by: 1.2. Can be built with placeholder brand values; final values from question 1.

#### Step 2.2: Import scope and vendor cleanup (Size: M)

- Acceptance criteria:
  - Only the product areas are imported (question 7). `plugins/maxim` is removed along with its registration.
  - Vendor URLs point to our host or are switched off, per the 1.3 inventory.
  - `LICENSE`, `NOTICE`, `THIRD_PARTY_NOTICES.md` and every license header are kept, and `MODIFICATIONS.md` states the changes, as Apache-2.0 §4 requires.
- Verification command: `apps/gateway/scripts/import.sh <tag>` followed by the 2.4 checks.
- Files likely touched: `apps/gateway/scripts/import.sh`, `apps/gateway/patches/0001-remove-maxim.patch`, `0002-vendor-urls.patch`, `UPSTREAM`, `MODIFICATIONS.md`.
- Blocked by: 2.1.

#### Step 2.3: Monorepo wiring (Size: S)

- Acceptance criteria:
  - `apps/gateway/**` is ignored by oxlint, oxfmt and the root vitest projects.
  - `docker-compose.yml` gains the gateway and a second Postgres database.
  - Root scripts `gateway:build`, `gateway:test` and `gateway:dev` exist.
  - CI runs Go build, vet and test plus the reproduction check.
- Verification command: `pnpm lint && pnpm gateway:test`.
- Files likely touched: `oxlint.config.ts`, `oxfmt.config.ts`, `docker-compose.yml`, `package.json`, `.github/workflows/gateway.yml`.
- Blocked by: 2.2.

#### Step 2.4: Rename acceptance (Size: S)

- Acceptance criteria:
  1. All 15 modules build, and `go vet` is clean.
  2. The unit-test pass/fail set equals the P1 baseline.
  3. The UI builds, and the gateway boots on Postgres.
  4. A virtual key created through the governance API completes a chat request against a mock upstream.
  5. A case-insensitive scan for `bifrost|maximhq|getmaxim|x-bf-|sk-bf-` returns only allowlisted lines, each with a reason.
  6. Real 200, 400, 401 and `/v1/models` responses contain none of those strings in headers or body.
  7. Re-running import plus rename on a fresh tag checkout reproduces the committed tree byte for byte.
- Verification command: `pnpm gateway:verify` (script runs all 7).
- Blocked by: 2.3.

### Phase 3: Credits ledger in the control plane (ADR 0001, 0003). Starts in parallel with Phase 1

Verified when: the four ledger tests below pass on real Postgres.

#### Step 3.1: Schema and migration (Size: S)

- Acceptance criteria:
  - New tables:
    - `credit_ledger_entry`: append-only, `bigint` micro-USD, unique `idempotency_key`, `kind` in `top_up | usage | refund | adjustment | promo`.
    - `credit_account`: cached balance per organization.
    - `credit_reservation`.
  - The migration has a down path.
- Verification command: `pnpm with-env pnpm -F @workspace/db db:generate && db:migrate`.
- Files likely touched: `packages/db/src/schema/credits.ts`, `schema/index.ts`, `migrations/0016_*.sql`.
- Test seams: migration on a fresh database.
- Blocked by: 0.2.

#### Step 3.2: Ledger module and gateway DB role (Size: M)

- Acceptance criteria:
  - `credit()`, `getBalance()` and `rebuildBalance()` exist in `packages/api/src/modules/credits`.
  - A Postgres role `gateway_ledger` has only INSERT on entries and UPDATE on accounts and reservations.
  - Tests prove all four of these:
    - (a) The same idempotency key applied twice changes the balance once.
    - (b) The role's UPDATE or DELETE on entries fails with a real Postgres error.
    - (c) A rebuild equals the cache after a random sequence of entries.
    - (d) A staff adjustment shows in both the ledger and the balance.
- Verification command: `pnpm -F @workspace/api test credits`.
- Files likely touched: `packages/api/src/modules/credits/{router.ts,ledger.ts,ledger.test.ts}`, `packages/db/migrations/0017_gateway_role.sql`, `packages/api/src/index.ts`.
- Blocked by: 3.1.

#### Step 3.3: Admin adjustment endpoint and admin UI field (Size: S)

- Acceptance criteria: staff can add or remove credit for an organization, with a required reason.
- Verification command: API test plus manual check on `/admin/organizations/<id>`.
- Files likely touched: `packages/api/src/modules/admin/organizations/*`, `apps/web/src/app/[locale]/admin/organizations/*`.
- Blocked by: 3.2.

### Phase 4: Credits plugin in the gateway (ADR 0001, 0002, 0005). The riskiest piece

Verified when: the six plugin checks pass against a mock upstream and real Postgres.

#### Step 4.1: Reserve and refuse (Size: M)

- Acceptance criteria:
  - The plugin lives in `apps/gateway/plugins/credits` and is registered by `patches/0003-register-credits.patch`. That patch is one `case` in `transports/.../server/plugins.go`.
  - Pre-hook: estimate the maximum cost (`max_tokens`, or the model's max output), then atomically reserve it.
  - With zero balance: `402` with OpenRouter's error shape, and the mock upstream sees no call.
- Verification command: `go test ./plugins/credits/... -run Reserve`.
- Test seams: plugin hooks with a mock upstream and testcontainers Postgres.
- Blocked by: 2.4, 3.2.

#### Step 4.2: Settle exactly once (Size: M)

- Acceptance criteria:
  - `PostLLMHook` settles, on the final chunk for streams, with the key `nonce:request_id:fallback_index:attempt`.
  - A replay writes one entry.
  - `amount = round_half_up(provider_cost × (10000 + markup_bps) / 10000)` in micro-USD, and the provider cost is kept in metadata.
- Verification command: `go test ./plugins/credits/... -run Settle`.
- Blocked by: 4.1.

#### Step 4.3: Concurrency, queue, reaper, latency (Size: M)

- Acceptance criteria:
  - 50 concurrent requests on a balance that covers 10 never overdraw by more than one request's overrun.
  - The bounded settlement queue falls back to synchronous writes when full, and drains on shutdown.
  - The reaper releases stale holds.
  - Added p50 and p99 latency is recorded.
- Verification command: `go test ./plugins/credits/... -run 'Concurrent|Queue|Reaper' -race`, plus a benchmark written into `docs/platform/credits-latency.md`.
- Blocked by: 4.2.

### Phase 5: API keys end to end (ADR 0004). First full vertical slice

Verified when: an e2e test signs up, creates an organization, gets a staff credit, creates a key in the dashboard, calls `/api/v1/chat/completions` against the mock upstream, and sees the balance drop by the expected amount. Disabling the key gives `401`.

#### Step 5.1: Gateway governance client and `api_key` table (Size: M)

- Acceptance criteria:
  - The client uses the private gateway URL and admin credential from env.
  - An organization is created as a gateway customer on demand (`id = organization.id`).
  - Creating a key calls the gateway first, then inserts the hash and prefix. If the insert fails, the virtual key is deleted.
  - The key's `provider_configs` allow all enabled providers.
- Verification command: `pnpm -F @workspace/api test keys`.
- Files likely touched: `packages/db/src/schema/keys.ts`, `packages/api/src/lib/gateway.ts`, `packages/api/src/modules/keys/*`, `packages/auth/src/rbac/roles.ts` (new `keys` permission).
- Blocked by: 4.1.

#### Step 5.2: Dashboard pages for keys and credits (Size: M)

- Acceptance criteria:
  - Keys page: create (value shown once), list, rename, disable, delete, and an optional limit with daily/weekly/monthly reset. The limit maps to a gateway budget.
  - Credits page: balance and ledger history.
- Verification command: Playwright e2e `keys.spec.ts`.
- Files likely touched: `apps/web/src/app/[locale]/dashboard/[organization]/{keys,credits}/*`, `apps/web/src/modules/{keys,credits}/*`, i18n `en`/`es`.
- Blocked by: 5.1.

#### Step 5.3: Organization deletion cleans the gateway (Size: S)

- Acceptance criteria: deleting an organization deletes its gateway customer and virtual keys, and the old keys get `401`.
- Verification command: API test.
- Blocked by: 5.1.

### Phase 6: Top-ups with real payments (ADR 0003)

Verified when, in Stripe test mode: one payment credits once even when the webhook is redelivered, a refund debits once, and the checkout redirect alone never credits.

#### Step 6.1: Top-up products and checkout (Size: M)

- Acceptance criteria:
  - Fixed amounts (question 3) as one-time Stripe prices.
  - Checkout runs from the credits page with `referenceId = organization.id`.
  - The purchase fee is added on the payment, never taken from credits.
- Files likely touched: `packages/billing/shared/src/config/index.ts`, `packages/billing/web/src/providers/stripe/checkout.ts`, `apps/web/.../credits/*`.
- Blocked by: 3.2.

#### Step 6.2: Webhook to ledger (Size: M)

- Acceptance criteria:
  - `checkout.session.completed` with `payment_status=paid`, and `checkout.session.async_payment_succeeded`, append `top_up` keyed by payment intent id.
  - `charge.refunded` appends `refund` keyed by refund id.
  - This is wired through the existing `WebhookCallbacks`.
- Verification command: `stripe trigger` replays plus `pnpm -F @workspace/api test billing-credits`.
- Files likely touched: `packages/billing/web/src/providers/stripe/webhook/index.ts`, `packages/api/src/modules/billing/router.ts`, `packages/api/src/modules/credits/topup.ts`.
- Blocked by: 6.1.

### Phase 7: TurboStarter white-label (small)

Verified when: the `turbostarter` scan returns only the allowlist (LICENSE and copyright), `pnpm lint && pnpm format && pnpm test` and the web build pass, and the session cookie carries the new prefix.

#### Step 7.1: Replace the 172 hits (Size: M)

- Covers `PRODUCT_NAME`, auth `appName` and `cookiePrefix`, email layout and logo, i18n, marketing links and buy CTAs, generator templates, the docker-compose project name, README and AGENTS.md.
- Mobile config and the `turbostarter://` scheme follow question 6.
- Blocked by: question 1.

#### Step 7.2: Marketing site rewritten for the product (Size: M)

- Hero, pricing (credits, no subscription plans), docs link.
- Subscription plans are removed from the billing config.
- Blocked by: 6.1, 7.1.

### Phase 8: OpenRouter-compatible API surface and catalog

Verified when: the official `openai` SDK with `baseURL=https://<domain>/api/v1` passes a chat, stream, tools and `/models` smoke test, and catalog prices equal catalog price × markup for a seeded set.

#### Step 8.1: Edge routing (Size: S)

- Acceptance criteria:
  - `/api/v1/{chat/completions,completions,responses,messages,embeddings,models}` go to the gateway's `/v1/*`.
  - `/api/v1/{key,credits,generation,keys}` go to the control plane.
  - Everything else on the gateway returns 404 publicly (ADR 0007).
- Files likely touched: `apps/gateway/edge/Caddyfile` (or the host's routing config, per question 8).
- Blocked by: 5.1.

#### Step 8.2: Self-service endpoints (Size: M)

- Acceptance criteria:
  - `GET /api/v1/key` returns limits and usage.
  - `GET /api/v1/credits` returns total purchased and used.
  - `GET /api/v1/models` returns OpenRouter's shape with our prices.
  - Each has an error-contract test (402/403/401 messages).
- Files likely touched: `packages/api/src/modules/v1/*`.
- Blocked by: 8.1.

#### Step 8.3: Public model catalog pages (Size: M)

- A list with filters plus a page per model: context length, modalities, price per 1M tokens incl. markup, and a code snippet.
- Server-rendered for SEO.
- Blocked by: 8.2.

### Phase 9: Usage and generation records

Verified when: for a fixed set of seeded requests, usage page totals equal ledger sums.

#### Step 9.1: Activity page (Size: M)

- Spend, requests and tokens by model, key and day, from the ledger plus the gateway log store (read through the gateway's logs API, not its tables). Blocked by: 5.2.

#### Step 9.2: Generation detail and `GET /api/v1/generation?id=` (Size: M)

- Brief 06 shape: model, provider, tokens, cost, latency, finish reason, attempts. Blocked by: 9.1.

### Phase 10: Operations

Verified when: a deliberately dropped settlement appears in the next reconciliation report, `GET /` and `/api/*` on the public gateway host return 404, and the security audit has no open high findings.

- 10.1 Daily reconciliation job (log-store cost vs ledger usage per organization, plus reaped holds), with an alert. (M)
- 10.2 Low-balance email through `@workspace/email`. (S)
- 10.3 Deployment per question 8, with runbooks for an upstream upgrade (ADR 0008) and an incident. (M)
- 10.4 Security pass with Bifrost's `security-audit` skill and the `secure-code-review` skill over the new code. (S)

### Phase 11: Multi-instance (ADR 0006). Only when one instance isn't enough

Run the ADR 0006 test without the patch, then write the `LISTEN/NOTIFY` patch and run it again. Results go in `docs/platform/scaling-test.md`.

## Order and parallel work

```
Stage A:  P0 ──> import (2.2a) ──> wiring (2.3) ──> P1 baseline
Stage B:  rename script (2.1) ──> cleanup (2.2b) ──> acceptance (2.4) ──> P7
Stage C:  P3 ──> P4 ──> P5 ──┬─> P8 ──> P9
                 P6 (after P3) ├─> P10
                               └─> P11 (later)
```

Stage B needs the brand inputs (question 1). Stage C needs the payment and pricing answers (questions 2 to 4).

## Decision log

| Decision                   | Chosen option                                                          | Why                                                                                                                                                                           |
| -------------------------- | ---------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| One repo, two applications | Gateway in `apps/gateway/` next to `apps/web`                          | Owner decision. It has no root `package.json`, so the npm UI and Go modules stay separate from pnpm                                                                           |
| Order of work              | Foundation, then rename, then integration                              | Owner decision, 2026-11-21. Replaces the earlier rule that the gateway's first commit must already be renamed, so the upstream names will appear in git history until Stage B |
| White-label depth          | Full, including Go identifiers and module paths                        | Owner decision. Done by script so upgrades stay possible (ADR 0008)                                                                                                           |
| How upstream is taken      | Re-import the pristine tag, rename, apply `patches/`                   | A normal merge would conflict on every renamed line                                                                                                                           |
| Our gateway code           | New files (`plugins/credits`) plus tiny ordered patches                | Keeps each upgrade's manual work small                                                                                                                                        |
| Money                      | Integer micro-USD ledger, append-only, idempotent, reserve then settle | ADR 0001. Prevents overdraft under concurrency                                                                                                                                |
| Key model                  | Organization = gateway customer, dashboard key = virtual key           | ADR 0004                                                                                                                                                                      |
| Margin                     | Applied at settlement in basis points; provider cost kept              | ADR 0005                                                                                                                                                                      |
| Admin UI                   | Gateway UI internal only                                               | ADR 0007                                                                                                                                                                      |
| Launch scale               | One gateway instance plus a standby                                    | ADR 0006                                                                                                                                                                      |

## Not yet specified

- Phase 4: the per-model max-output fallback when a request sets no `max_tokens` and the catalog lacks the value. Settled in 4.1 from catalog coverage data.
- Phase 8: which provider-compatible paths (`/anthropic/*`, `/genai/*`) are public. Recommend `/api/v1/*` only at launch.
- Phase 9: log retention window and whether prompts and completions are stored at all (privacy). Fog until we pick a launch market.
- Later, not planned: BYOK, management keys, OAuth PKCE for apps, presets, auto top-up, `:free` models, app rankings, workspaces.

## Always / Never

- Always: every money write idempotent; license notices kept; each phase lands green with its named check; no commit or push without the owner asking.
- Never in this plan: mobile or extension releases, subscriptions, Metronome, BYOK, enterprise cluster mode, exposing the gateway admin UI publicly, hand edits to `apps/gateway/` outside `patches/` and `plugins/credits/`.

## Progress

Updated 2026-11-21 after two Stage A verification passes in a Linux cloud container. Evidence is in `gateway-baseline.md`.

| Phase                      | Status                                                                                                                                                                                                                                                                                                                                                                                               |
| -------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 0 Prepare                  | done in the cloud container: Go 1.27.4, gcc, Node, Docker and Postgres through `docker compose` all work there, and skills load (38 links resolve). Step 0.1 is still open on the Windows machine                                                                                                                                                                                                    |
| 1 Baseline                 | done, with limits. 1.1: native build, boot on SQLite and on Postgres, `/health` and UI verified. 1.2: 15 modules, 14,234 pass, 166 fail, 584 skip, by name; two full runs differ in one flaky test. 1.3: hosts recorded with a disposition each, but only for boot and the login page                                                                                                                |
| 2 Rename + import          | import (2.2a) done and re-verified against upstream by blob hash and file mode. Wiring (2.3) done except `gateway:dev`: lint, format, test and the pre-commit hook skip the gateway; compose runs it on its own `gateway` database; `gateway:build`, `gateway:test` and `gateway:up` exist; `go vet` is clean; the CI workflow is written but has never run. 2.1, 2.2b and 2.4 not started (Stage B) |
| 3 Ledger                   | not started                                                                                                                                                                                                                                                                                                                                                                                          |
| 4 Credits plugin           | not started                                                                                                                                                                                                                                                                                                                                                                                          |
| 5 Keys e2e                 | not started                                                                                                                                                                                                                                                                                                                                                                                          |
| 6 Top-ups                  | not started                                                                                                                                                                                                                                                                                                                                                                                          |
| 7 TurboStarter white-label | not started                                                                                                                                                                                                                                                                                                                                                                                          |
| 8 API surface + catalog    | not started                                                                                                                                                                                                                                                                                                                                                                                          |
| 9 Usage                    | not started                                                                                                                                                                                                                                                                                                                                                                                          |
| 10 Operations              | not started                                                                                                                                                                                                                                                                                                                                                                                          |
| 11 Multi-instance          | deferred                                                                                                                                                                                                                                                                                                                                                                                             |

### Found during Stage A

- The root `ignorePatterns` did not cover `apps/gateway/ui`, because that directory has its own oxlint and oxfmt configs. The ignores now live in `.eslintignore` and `.prettierignore`.
- The UI heap failure is in `tsc --noEmit`, not Vite. Patch `0001` raises the limit through `ui/.npmrc`.
- Inside the monorepo the UI typecheck also failed on a stale `next-env.d.ts`. Patch `0002` deletes it.
- A first boot with no cached pricing is fatal when `getbifrost.ai` is unreachable. Any environment without that host needs `pricing_url` pointed at a file, which Stage B has to solve for real (Step 2.2).
- 91 of the 166 failing tests read upstream paths outside the import scope (`docs/openapi`, `examples/mcps`, an example plugin, `helm-charts`). Whether to widen the import is an owner decision; until then those tests cannot be part of the rename acceptance set.
- Three upstream files had lost their executable bit in the import. Restored.
- The pre-commit hook failed on any commit that staged only gateway files. `lefthook.yml` now skips the ignored paths.
- Inference needs a virtual key by default in this version (`401 virtual_key_required`), so `enforce_auth_on_inference` is already the default behaviour the plan asks for.

### Open before Stage A can be called closed

- Run `pnpm gateway:up` once on a normal machine. In the cloud container the image was built from a copy of the Dockerfile with a proxy CA added, and the stack ran with a local pricing file.
- Let the CI workflow run once (first pull request).
- `gateway:dev` is not wired: upstream's `make dev` rewrites `go.work` and expects `./cli`, which is outside the import scope.
