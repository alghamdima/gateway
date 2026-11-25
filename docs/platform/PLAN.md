# Build plan: Lemru AI gateway platform

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
- The mobile and extension apps were removed on the owner's decision (2026-11-23), with the `turbostarter://` and `chrome-extension://` trusted origins.
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
  - The caller's `x-request-id` and every `x-bf-*` header are dropped before the gateway (ADR 0014; request ids must not be chosen by callers).
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

### Phase G: Gateway controls (inventory in `gateway-capabilities.md`)

Verified when: each limit set in our console is refused by the built gateway with the documented status, the change is in the audit log, and provider secrets never leave our API.

#### Step G1: Rate limit per organization (Size: M)

- Acceptance criteria:
  - Staff set requests per minute and tokens per minute on the admin organization page; a platform default applies to new organizations (owner decision; none until decided).
  - Stored in our database with who and when, audited, and pushed to the gateway customer's `rate_limit`.
  - A request over the limit gets 429 in OpenRouter's shape with `Retry-After`, the provider is never called, and the credits hold is released.
- Verification: an end-to-end test against the built gateway (the PreLLMHook order of governance and credits decides whether a hold is taken first; the test proves no hold is left).
- Blocked by: nothing. The default waits on the owner.

#### Step G2: Rate limit per key (Size: S)

- Requests per minute on the keys page next to the spending limit, pushed to the virtual key's `rate_limit`; refused if above the organization's.
- Blocked by: G1; owner decision whether customers set it.

#### Step G3: Providers page (Size: M)

- Admin role only: providers, status, model count, keys masked, add or replace a key, weights, turn a provider off. Secrets are write-only through our API. Every change audited.
- Blocked by: nothing.

#### Step G4: Provider and model caps (Size: S)

- Requests and tokens per minute per provider and per model across all customers, from the admin console.
- Blocked by: G3.

#### Step G5: Allowed models per key, and key rotation (Size: M)

- Blocked by: G2.

### Phase 9 additions

- 9.0 (Size: S), before 9.1: prove the gateway stores no prompt or completion text by default (logging plugin configuration plus a test that reads a log entry), and set the log retention window. Owner decision: whether customers can opt in to content logging.

### Phase D: Data policy (ADR 0015, research brief 10). The product's differentiator

Verified when: an organization that requires in-Kingdom processing with zero retention can only reach endpoints with those terms, proven end to end against the built gateway; a request no endpoint can serve is refused, never rerouted; and the generation record shows the endpoint, its region and terms, and the policy version.

#### Step D1: Provider catalog with data terms (Size: M)

- Acceptance criteria:
  - Admin page listing every provider key in the gateway (read through the gateway's API, secrets never shown) with its data terms: processing region, retention, training, agreement, source and date.
  - Terms are stored in our database, edited by the admin role only, and every change is in the audit log.
  - A key with no terms shows as unknown, and unknown counts as retaining and training.
- Blocked by: nothing.

#### Step D2: Organization data policy, enforced (Size: M)

- Acceptance criteria:
  - A policy page for the organization: require zero retention, forbid training, allowed regions, allowed providers. Default: everything allowed (owner decision).
  - The control plane computes the matching endpoints and writes them to every key of the organization as `provider_configs[].key_ids`; new keys get them at creation; a change to a policy or to an endpoint's terms is pushed to every affected key.
  - No matching endpoint: the keys can call nothing. A failed push never leaves a wider allowlist than before.
- Verification: end to end against the built gateway with two mock providers of different terms: the strict organization reaches only the matching one, is refused when it is removed, and never falls back to the other.
- Blocked by: D1.

#### Step D3: Evidence per request (Size: S)

- The generation detail and `GET /api/v1/generation` show the endpoint that served the request, its region and terms, and the policy version in force.
- Blocked by: D2.

#### Step D4: Models by policy (Size: S)

- `GET /api/v1/models` and the catalog page list only the models the organization's policy can reach.
- Blocked by: D2.

#### Later in this phase

- BYOK: the customer's own provider key, stored encrypted in the gateway, write-only through our API, usable only by their keys, charged a platform fee instead of the provider cost (the credits plugin must know the key is theirs). Owner decision: the fee.
- The Saudi deployment: in-Kingdom servers and database on its own hostname, in-Kingdom endpoints only, external telemetry off (Sentry and PostHog in the web app send data abroad today), the legal pack (DPA, subprocessor list, retention schedule, incident plan). Needs: in-Kingdom hosting and a contracted in-Kingdom model provider.
- Enterprise: invoice and purchase-order billing, SSO, SCIM, audit exports.

## Order and parallel work

### Direction (owner, 2026-11-24)

The product is a gateway for the Saudi market: one API for many models, where the customer controls where their data goes and we prove it per request. Four stages:

| Stage | What                                                                                                        | Waits on                                             |
| ----- | ----------------------------------------------------------------------------------------------------------- | ---------------------------------------------------- |
| 1     | Phase D steps D1 to D4: the data policy                                                                     | nothing                                              |
| 2     | Launch the global deployment: 8.1 edge, 8.3 catalog page, Phase 7 brand, a real Stripe run, 10.3 deployment | a domain, hosting, a Stripe sandbox                  |
| 3     | The Saudi deployment (same code, in-Kingdom configuration)                                                  | in-Kingdom hosting, a contracted in-Kingdom provider |
| 4     | Enterprise: invoices and purchase orders, SSO, BYOK                                                         | the first enterprise customer                        |

Stages 1 and 2 run in parallel. The milestones below are the earlier ordering and still hold for stage 2.

Stages A to C are history now (see Progress). What remains, in the order to build it. Steps marked with a decision wait on the owner; everything else can start at once.

### Milestone 1: ready for the first paying customer

| #   | Step                                                                            | Needs from the owner                   |
| --- | ------------------------------------------------------------------------------- | -------------------------------------- |
| 1   | G1 rate limit per organization                                                  | default limit (can ship with none)     |
| 2   | 9.0 no prompt or completion text stored by default, retention window            | opt-in content logging, retention days |
| 3   | 9.1 activity page                                                               |                                        |
| 4   | 9.2 generation detail and `GET /api/v1/generation`                              |                                        |
| 5   | 10.1 daily reconciliation with alert                                            |                                        |
| 6   | 10.2 low-balance email                                                          |                                        |
| 7   | 10.4 security pass over the new code                                            |                                        |
| 8   | 6.3 a real top-up and refund in a Stripe sandbox, plus `stripe trigger` replays | a Stripe sandbox                       |
| 9   | Refund and credit-expiry policy built and shown on the site                     | the policy                             |
| 10  | The gateway rename (Stage B); Phase 7 is done except the real logo              | a domain that resolves; a logo         |
| 11  | 8.1 edge routing and the `openai` SDK smoke test                                | where we host                          |
| 12  | 10.3 deployment and runbooks; `pnpm gateway:up` on a normal machine             | where we host                          |

Steps 1 to 7 need nothing and go in that order. Steps 8 to 12 start the day their input arrives.

### Milestone 2: soon after launch

G2, G3, G4, G5; 8.3 catalog pages; management keys and `/api/v1/keys`; auto top-up; leaked-key scanning with GitHub (needs the key prefix from the brand).

### Milestone 3: when customers ask

Workspaces, guardrails, presets, provider routing controls per request, web search. Reasons in `openrouter-comparison.md`. Zero data retention routing and BYOK moved to Phase D (owner direction, 2026-11-24).

## Decision log

| Decision                   | Chosen option                                                               | Why                                                                                                                                                                           |
| -------------------------- | --------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| One repo, two applications | Gateway in `apps/gateway/` next to `apps/web`                               | Owner decision. It has no root `package.json`, so the npm UI and Go modules stay separate from pnpm                                                                           |
| Order of work              | Foundation, then rename, then integration                                   | Owner decision, 2026-11-21. Replaces the earlier rule that the gateway's first commit must already be renamed, so the upstream names will appear in git history until Stage B |
| Brand                      | **Lemru**; `lm` replaces `bf` in key and header names (`sk-lm-…`, `x-lm-*`) | Owner decision, 2026-11-25. Domain and logo not chosen; the Go module path is ours to pick when the rename runs                                                               |
| White-label depth          | Full, including Go identifiers and module paths                             | Owner decision. Done by script so upgrades stay possible (ADR 0008)                                                                                                           |
| How upstream is taken      | Re-import the pristine tag, rename, apply `patches/`                        | A normal merge would conflict on every renamed line                                                                                                                           |
| Our gateway code           | New files (`plugins/credits`) plus tiny ordered patches                     | Keeps each upgrade's manual work small                                                                                                                                        |
| Money                      | Integer micro-USD ledger, append-only, idempotent, reserve then settle      | ADR 0001. Prevents overdraft under concurrency                                                                                                                                |
| Key model                  | Organization = gateway customer, dashboard key = virtual key                | ADR 0004                                                                                                                                                                      |
| Margin                     | Applied at settlement in basis points; provider cost kept                   | ADR 0005                                                                                                                                                                      |
| Admin UI                   | Gateway UI internal only                                                    | ADR 0007                                                                                                                                                                      |
| Launch scale               | One gateway instance plus a standby                                         | ADR 0006                                                                                                                                                                      |

## Not yet specified

- Phase 4: the per-model max-output fallback when a request sets no `max_tokens` and the catalog lacks the value. Settled in 4.1 from catalog coverage data.
- Phase 8: which provider-compatible paths (`/anthropic/*`, `/genai/*`) are public. Recommend `/api/v1/*` only at launch.
- Phase 9: log retention window and whether prompts and completions are stored at all (privacy). Fog until we pick a launch market.
- Later: see Milestones 2 and 3 above and `openrouter-comparison.md`.

## Always / Never

- Always: every money write idempotent; license notices kept; each phase lands green with its named check; no commit or push without the owner asking.
- Never in this plan: mobile or extension releases, subscriptions, Metronome, BYOK, enterprise cluster mode, exposing the gateway admin UI publicly, hand edits to `apps/gateway/` outside `patches/` and `plugins/credits/`.

## Progress

Updated 2026-11-21 after two Stage A verification passes in a Linux cloud container. Evidence is in `gateway-baseline.md`.

| Phase                      | Status                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| -------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 0 Prepare                  | done in the cloud container: Go 1.27.4, gcc, Node, Docker and Postgres through `docker compose` all work there, and skills load (38 links resolve). Step 0.1 is still open on the Windows machine                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| 1 Baseline                 | done, with limits. 1.1: native build, boot on SQLite and on Postgres, `/health` and UI verified. 1.2: 14 modules, 15,030 pass, 63 fail, 454 skip, by name; all 63 failures come from the cloud container (no backing services, no IPv6 loopback, no DNS). 1.3: hosts recorded with a disposition each, but only for boot and the login page                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| 2 Rename + import          | import (2.2a) done, re-verified against upstream by blob hash and file mode. Wiring (2.3) done, `gateway:dev` included; CI / Gateway passes on GitHub. Rename tool (2.1) done. Cleanup (2.2b) done as far as it can go without a brand: Maxim plugin removed by patch `0003`, vendor hosts mapped to the brand domain, `MODIFICATIONS.md` written. Acceptance (2.4): `pnpm gateway:verify` passes end to end with a placeholder brand (checks 1, 2, 3, 5, 6, 7); check 4, a chat through a virtual key against a mock upstream, is not scripted. No brand is chosen, so the committed tree is not renamed                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| 3 Ledger                   | 3.1 and 3.2 done: three tables in migration `0016` (up and down both run), append-only trigger, `gateway_ledger` role, `packages/api/src/modules/credits`, and 11 tests on real Postgres covering the four checks. 3.3 done: the admin organization page shows the balance and has the adjustment field (signed USD amount, required reason, one idempotency key per submission), shown only to staff with `credits:adjust`; checked in the browser as admin (ledger entry and audit row written) and as support (balance only, API refuses with 403)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| 4 Credits plugin           | 4.1 and 4.2 done (design revised by ADR 0009): `apps/gateway/plugins/credits` holds the maximum cost per attempt, refuses with 402 before the provider, and settles exactly once; registered by patch `0004`. Verified by 52 tests, including the built gateway end to end against a mock provider and the real ledger. 4.3 done: settlements go through a bounded queue with retries, a full queue falls back to the caller, shutdown drains within a grace period, the reaper runs every minute, and 50 concurrent requests on a balance for 10 admit exactly 10. Added latency is in `credits-latency.md` (local Postgres only; under 1 ms median unloaded). Not durable across a crash: see ADR 0009. The markup is read from the database per provider (ADR 0010): loaded at start, reloaded every 30 seconds, fixed per attempt, and the plugin refuses billable requests until it has loaded it once                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| Pricing (ADR 0010)         | done: `/admin/pricing` sets the default markup, a markup per provider, and the top-up fee with its minimum (OpenRouter: 5.5%, at least $0.80). Stored in `pricing_settings`, `provider_markup` and the append-only `pricing_change` (migrations `0017`, `0018`); the fee calculation is in `pricing/fee.ts`; admin API and fee with 40 tests; checked in a browser as admin (save, per-provider add, edit, remove, validation) and refused for a member (403) and anonymous (401). The fee is stored for Phase 6, which has not built the checkout yet                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| Staff and auth (ADR 0011)  | done: staff roles admin, finance and support enforced by Better Auth and by a route table that refuses unknown routes; 2FA required for staff; every staff change in an append-only audit log shown at `/admin/audit`; anonymous sign-in refused; no session cookie cache, so bans and role changes apply at once; rate limits shared in the database. Checked on the running app per role. Not done: organization roles for API keys (Phase 5)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| 5 Keys e2e                 | done. 5.1: `@workspace/gateway` client, `gateway_customer` mapping (the gateway generates customer ids, ADR 0004 amended) read by the credits plugin, `api_key` (hash and prefix only), `/api/keys/:organizationId` with an `apiKey` permission (owners and admins manage, members read). 5.2: dashboard pages for keys (create shown once, edit name and spending limit, disable, delete) and credits (balance, held, history). 5.3: deleting an organization removes its keys and customer from the gateway first. Verified with 15 API tests (3 end to end against the built gateway) and in a browser on the production build: key created on the page calls a model, credits drop by 450 micro-USD, disabled key 403, a member sees keys but cannot manage them (UI and API 403). Disabling gives 403, not the 401 the step assumed. The API end-to-end tests do not run in CI yet                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| 6 Top-ups (ADR 0012)       | built, not yet run against Stripe: the credits page quotes the fee from the admin settings and starts a Stripe Checkout payment with a credits line and a fee line (owners and admins only); the signed webhook credits `top_up` once per payment intent (completed and paid, or async payment succeeded) and a succeeded refund removes the refunded share of the credit once per refund id. 15 tests on Postgres with webhooks signed the way Stripe signs them, mutation-checked; in the browser on the production build the page polled and showed the credit after a signed webhook to the real route, a forged one got 400, a member saw no box and got 403. Stripe is unreachable from the build sandbox, so the checkout redirect and `stripe trigger` replays wait for a Stripe sandbox. Amount limits $5 to $1,000 per payment, confirmed by the owner                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| Key limits (ADR 0013)      | fixed: a key's spending limit was a gateway budget, which counted provider cost without the markup, so a $5 key could spend about $5.25 at a 5% markup. The credits plugin now refuses the hold (403 `credits_key_limit`) when the key's spend in its UTC window plus open holds would pass the limit; holds and usage entries record the key (migration `0022`). 5 store tests on Postgres (window boundaries, 40 concurrent holds on one key, the gateway role cannot read key hashes), mutation-checked; end to end against the built gateway                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| 8 API surface              | 8.2 done: `GET /api/v1/key` (usage all time and per UTC day, week, month; limit_remaining net of open holds), `GET /api/v1/credits` (total_credits - total_usage = balance) with the organization key as Bearer or `x-api-key`, and a distinct 401 message for a missing, malformed, unknown, disabled or expired key; `GET /api/v1/models` public, from the gateway's catalog with our markup per provider as exact decimal strings, cached one minute. 23 tests on Postgres, mutation-checked, and the catalog checked against the built gateway. Not done: 8.1 edge routing (waits on where we host), 8.3 catalog pages. Management keys are not offered: `/credits` answers to the organization's own keys 8.3 done: `/models` lists every model on offer with its price per million tokens (search and provider filter as a plain GET form, so it works without scripts) and `/models/<provider>/<name>` shows context length, maximum output, modalities, cache prices and a curl example. Both are rendered on the server from `GET /api/v1/models`, the same list and prices an API client gets, validated with Zod; a catalog that cannot be read says so instead of failing, and an unknown model is a 404. Checked on the production build: the model and its price are in the HTML the server sends. The example calls `<site>/api/v1/chat/completions`, which answers only once the edge (8.1) routes inference. Not on the page yet: which regions and data terms each model is available under. Found on the way and fixed: pages of the production build (never development mode) logged React hydration error #418 once on load and re-rendered on the client. Two causes, each found by rewriting React's "hydration failed" function in the served bundle so it names the component. (1) Every page: `NODE_ENV` reached the browser only through `...process.env`, which is empty there, so the browser took the schema default ("development") and rendered the React Query devtools slot the server had not; it is now named in `apps/web/env.config.ts`. (2) Dashboard pages that read a query the sidebar also reads (the active organization, the billing summary): the page rendered first on the server, React Query fills an existing query only in an effect, so the sidebar was sent without its data, and the browser had fetched it by the time it hydrated the sidebar; both dashboard layouts now give the page the same queries as the sidebar. Checked on the production build: 32 pages as visitor, member, owner and admin without an error; before each fix the pages it covers still had it. No automated check guards this: CI has no browser run.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| G Gateway controls         | G1 done: admins set requests and tokens per minute on the admin organization page; stored in `organization_rate_limit` (migration `0023`), audited as `organization.rate-limit`, pushed to the gateway customer's rate limit. Against the built gateway a limit of 2 a minute answers 200, 200, 429 (`policy_rate_limited`), leaves no hold, and charges only the 2 that ran; removing it lets requests through at once. Checked in the browser as admin and as finance (no form, API 403). No platform default yet (owner decision). Found and fixed while testing: the gateway's key names are unique across all customers, so a second key named like another organization's failed with 503; keys are now named by our id in the gateway                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| 9 Usage                    | 9.0 done (ADR 0014): the gateway stored every prompt and completion and kept logs 365 days by default; the shipped config now stores no content (`disable_content_logging`, per-request overrides off) and keeps logs 90 days (placeholder). Proven against the built gateway with the shipped config: a request asking to keep its content leaves no prompt, completion, summary or raw body in the log, while model, tokens and cost remain; mutating any of the three settings fails the test. 9.1 done: `/dashboard/<org>/activity` and `GET /api/activity/:organizationId?days=` (billing:read) show spend, requests and tokens over 7, 30 or 90 days, by UTC day, by model and by key, read from the ledger (kept for good) rather than the gateway logs (90 days); the credits plugin now records prompt and completion tokens on each usage entry. Totals equal the ledger sums in tests and in the browser. Entries charged before this change have no token counts. 9.2 done: `GET /api/v1/generation?id=` (any key of the organization) and a "Recent requests" list with a record view on the activity page. The id is the gateway's `x-bifrost-request-id`; the charge, tokens and attempts come from the ledger, latency, finish reason and streaming from the gateway log while it is kept, and only when the log belongs to the organization's own gateway customer, because a caller can choose the request id. Checked against the built gateway and in the browser. The log appears a few seconds after the answer. For 8.1: the edge should drop the caller's `x-request-id` so ids cannot collide                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| 10 Operations              | 10.1 done: a daily reconciliation compares, per organization and UTC day, the provider cost the gateway logged (all customers, not the default 100) with the provider cost the ledger recorded, lists cost no organization owns and charges lost to reaped holds, stores each run (`reconciliation_run`, migration `0024`), shows them at `/admin/reconciliation`, and emails `RECONCILIATION_ALERT_EMAIL` on a difference or an error. Any scheduler calls `GET /api/internal/reconciliation` with `Bearer CRON_SECRET`. A deliberately dropped settlement appears in the report (test), and against the built gateway the day's costs agree. Requests that start before midnight and settle after it can show as a small difference. 10.2 done: an hourly job (`GET /api/internal/low-balance`, same secret) emails an organization's owners and admins when available credits drop below its threshold ($5 by default, changeable or off on the credits page by owners and admins), once per dip; organizations that never had credit are skipped. It marks the organization before sending, so a failed send is not retried every hour. Both new emails render; checked in tests (mutation-checked) and in the browser                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| 7 TurboStarter white-label | The owner named the product **Lemru** on 2026-11-25, with `lm` as the short prefix for keys and headers; no domain is registered yet. 7.1 done: product name, auth `appName` and cookie prefix (`lemru.`), email layout, every translated string, the local and CI database user and the compose project name, generators, README. Removed, because they advertised the template to our customers: the purchase dialog that opened on every page after 15 seconds, the buy card in the dashboard sidebar, the "source code" link in the user menu, the "built with" badge, the template's Discord, app-store, extension, docs and roadmap links, and the feature-flag demo banner. 7.2 as far as the brand needs it: the home page (hero, six features, five questions) and `/pricing` now describe this product and claim only what is built; the template showcase, its invented testimonials and sign-up count, the newsletter block and the subscription plans are gone from the marketing pages. The logo, icons, social image and email images are a temporary mark (the letter L and the name set in Inter) until the owner supplies artwork. CI fails if the template's name appears outside an allowlist (its license, these engineering notes, its docs server in `.mcp.json`, its version check, one test). Checked on the production build: 20 pages as visitor, owner and admin carry no template name and no untranslated key, and the session cookie is `__Secure-lemru.session_token`. Found on the way and fixed: the translation instance for client components was one per server process, so pages in any language but the first one requested were sent with their client components in the wrong language, then corrected in the browser with a hydration error; the server now makes one per render (checked by alternating English and Spanish requests). Not done: the gateway rename (needs a domain that resolves); subscription plans still exist in the billing config and the dashboard billing pages; the two blog posts and the legal pages are the template's placeholder text; the personal dashboard still has the template's demo charts and AI chat page; Spanish, which the template shipped, was removed on the owner's word (2026-11-25): English is the only locale until Arabic is added. The same list holds for the gateway UI, which shipped eleven languages: patch `0005` and the import's `dropped` list leave it with English, because the owner wants one product with no differences between its parts.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| 10.4 Security pass         | done for what three reviews found, each finding checked in the code before any fix. Fixed: staff could skip the second factor through magic link, email OTP or social sign-in (`bd5d78a`); impersonated sessions could act as the customer, now read-only (`4ddf4ac`); the hold was not a ceiling for `n`, `best_of`, images, priced tiers and server tools, now multiplied or refused (`a6d6588`); the shipped gateway config had no credits plugin, no admin and a port on every interface (`2ac4591`); a stream longer than its hold lost it, now renewed while it runs (`7d586bb`); the gateway's database role could mint credit, now limited to usage charges (migration `0026`, `c6483ba`). One finding was wrong: a cancelled stream is not charged $0, the gateway estimates its usage (`core/streamusage`); an end-to-end test now guards that. Open: chargebacks (`charge.dispute.*`) and a refund that arrives before its credit, both waiting on the owner's chargeback policy; `x-request-id` and `x-bf-*` header stripping, with the edge (8.1); an attempt cut off before its first chunk, or a non-streamed timeout, is charged nothing though the provider may bill it (owner decision); the encryption key is configured but no test can observe it; production must connect to the ledger as `gateway_ledger` itself.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| D Data policy (ADR 0015)   | D1 done: `/admin/providers` lists every provider key the gateway holds (read through `GET /api/keys`; only id, name, provider and state are passed on) with its data terms from `provider_endpoint` (migration `0027`): processing region, retention in days, training, agreement, source and the day it was verified. The admin role edits them; finance and support read; each change is in the audit log with its values. A key with no terms, or with unknown retention or training, guarantees nothing. Checked in tests (mutation-checked) and on the production build against the built gateway: an admin recorded terms and saw them listed, the secret never appeared in the response, support could read but got 403 on a change. D2 done: owners and admins set the organization's policy at `/dashboard/<org>/data-policy` (zero retention, no training, allowed regions, allowed providers; members read it). It is stored in `organization_data_policy` (migration `0028`) and written to every key of the organization as the gateway's `provider_configs[].key_ids`; new keys get it at creation; a change to an endpoint's terms re-pushes every policy, and `GET /api/internal/data-policy` (same secret as the other jobs, every few minutes) retries what a gateway failure left pending. End to end against the built gateway with two provider keys of different terms: an in-Kingdom organization was served only by the in-Kingdom key (24 of 24, for keys made before and after the policy), refused with 403 `provider_blocked` and never rerouted when that endpoint's terms got worse, with no hold left. On the production build: the owner saved a strict policy and the gateway held only the in-Kingdom key on all 8 of the organization's keys; a policy nothing satisfies is warned about before saving; a member read the page and got 403 on a change. Tests mutation-checked. The admin organization page shows the policy read-only (requirements, version, whether it is on every key, how many endpoints satisfy it), so support can see why an organization is refused. Not built: a request-level override. D3 done: the credits plugin writes the provider key that served each attempt on its ledger entry (`provider_key_id`), and every policy version is kept in an append-only history (`organization_data_policy_change`, migration `0029`). The generation record (request details in the dashboard and `GET /api/v1/generation`) shows `data_region`, the endpoint's terms and the day they were verified, and the policy version in force when the request was made; the terms shown are the ones recorded when the request was made, read from an append-only history of every change to a key's terms (`provider_endpoint_change`, migration `0030`), and it says so when they were edited afterwards. Checked against the built gateway and on the production build: a real request under an in-Kingdom policy showed "Saudi Arabia, not kept, never trained on, policy version 3" in its details. D4 done: `GET /api/v1/models` stays public with no key; with a key it lists only the models the organization's policy can reach. The gateway does the narrowing (`/api/models/details?keys=`); because it returns a provider that has none of the keys unfiltered, a model is listed only when the gateway names an allowed key that serves it, and a policy that allows no endpoint lists nothing. A key that is not valid gets 401, not the whole catalog. Against the built gateway: the in-Kingdom organization was offered its model, then nothing once its endpoint stopped satisfying the policy, while the public list still had it. The catalog page (8.3) is not built yet and will use the same function. Phase D steps D1 to D4 are done. |
| 11 Multi-instance          | deferred                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |

### Found during Stage A

- The root `ignorePatterns` did not cover `apps/gateway/ui`, because that directory has its own oxlint and oxfmt configs. The ignores now live in `.eslintignore` and `.prettierignore`.
- The UI heap failure is in `tsc --noEmit`, not Vite. Patch `0001` raises the limit through `ui/.npmrc`.
- Inside the monorepo the UI typecheck also failed on a stale `next-env.d.ts`. Patch `0002` deletes it.
- A first boot with no cached pricing is fatal when `getbifrost.ai` is unreachable. Any environment without that host needs `pricing_url` pointed at a file, which Stage B has to solve for real (Step 2.2).
- 103 of the 166 failing tests in the first baseline read upstream paths outside the import scope. The owner approved widening it; with `docs/openapi`, `examples/mcps`, the example plugin, the local pricing example, the Helm values schema and `tests/docker-compose.yml` imported, all 103 pass.
- Three upstream files had lost their executable bit in the import. Restored.
- The pre-commit hook failed on any commit that staged only gateway files. `lefthook.yml` now skips the ignored paths.
- Inference needs a virtual key by default in this version (`401 virtual_key_required`), so `enforce_auth_on_inference` is already the default behaviour the plan asks for.

### Open before Stage A can be called closed

- Run `pnpm gateway:up` once on a normal machine. In the cloud container the image was built from a copy of the Dockerfile with a proxy CA added, and the stack ran with a local pricing file.
