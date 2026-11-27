# Project state

Read this first. It says what the product is, where everything lives, what is done, what is next and what waits on the owner, so that nobody has to read the whole repository to continue. Last updated 2026-11-27.

**What to build next is in `SPEC.md`**: every feature of the product, compared with OpenRouter, with its status, where it lives, when it is done and the build order; the owner's open decisions are its section 3. The long form of the history is in `PLAN.md` (steps and their status), `ARCHITECTURE.md` and `adr/` (one file per decision). When this file and the code disagree, the code is right: fix this file.

## The product

Lemru: one API for many AI models, for the Saudi market. A customer signs up, creates an organization, adds prepaid credits, creates API keys and calls models in the OpenAI request format. What sets it apart is control over data: the customer chooses the regions their requests may be processed in, the longest a provider may keep them and whether training is allowed, and every request records where it was processed.

One repository, one codebase, two deployments planned: a global one, and one inside the Kingdom that differs only in configuration.

## The repository

The shape is the TurboStarter template's and it does not change: `apps/` holds what is deployed, `packages/` holds shared modules.

```
apps/
  web/        Next.js: marketing site, customer dashboard, admin. The only frontend.
  gateway/    The engine, in Go. Started from Bifrost v2.6.0; the code is ours now.
packages/
  api/        Hono API. One folder per feature under src/modules.
  db/         Drizzle schema (one file per feature) and migrations 0000-0037.
  gateway-client/  TypeScript client for the gateway's management API.
  auth/ billing/ email/ i18n/ ui/ shared/ storage/ cms/ flags/ analytics/ monitoring/
docs/platform/  PLAN.md, ARCHITECTURE.md, adr/, research/, this file.
```

A feature has four homes and no others:

| What                   | Where                                                                  |
| ---------------------- | ---------------------------------------------------------------------- |
| Logic and tests        | `packages/api/src/modules/<feature>`                                   |
| Tables                 | `packages/db/src/schema/<feature>.ts`                                  |
| Screens                | `apps/web/src/modules/<feature>`, pages in `apps/web/src/app/[locale]` |
| Work inside the engine | `apps/gateway/plugins/<feature>`                                       |

Our features today: `keys`, `credits`, `activity`, `data-policy`, `pricing`, `model-prices`, `payments`, `promo-codes`, `reconciliation`, `v1` (the public API under `/api/v1`), and under `admin`: `organizations`, `providers`, `requests`, `audit`, `users`, `customers`. In the gateway: `plugins/credits`.

The mobile app and the browser extension of the template were removed on 2026-11-23 (commit `2089e54`). If a mobile app is built, `apps/mobile` comes back from git history and uses the same `packages/`.

## How a request is paid for

1. The customer's key is a gateway virtual key; the organization is a gateway customer.
2. `plugins/credits` reserves the worst-case cost of the request before it reaches the provider, and settles the real cost after. The ledger is in our Postgres (`credit_*` tables); the gateway connects as the role `gateway_ledger`, which may only do what migration 0026 grants.
3. Money is SAR (ADR 0017). Every amount is micro-SAR (1 SAR = 1,000,000). Provider costs are USD; the plugin converts each attempt once, at the effective rate: the official USD→SAR rate staff set (`pricing_settings.usd_to_sar_rate_e6`, default 3.75) plus the bank's spread (`fx_spread_bps`, default 0), rounded up to the millionth, read every 30 s (`plugins/credits/rates.go`). The hold rounds up; the charge rounds half up at the hold's rate. Each hold and usage entry stores the official rate, the spread and the effective rate, and each usage entry the provider's cost in `cost_micro_usd`, so a rate change touches new requests only and reconciliation compares USD with USD.
4. No markup on requests (owner, 2026-11-27; ADR 0010 amended, BILL-14, CHARGE-03, migration `0034`): a request is charged its serving provider's cost in USD × the effective rate, and nothing else. The provider's cost is our own price (ADMIN-09, ADR 0018): the version in force in `model_price` (per provider and model, or per provider key for a negotiated rate) is pushed to the gateway's pricing overrides, which both the hold and the charge use; each usage entry names the version in `metadata.price_override`. The platform's revenue is the top-up fee (`pricing_settings.topup_fee_bps`, at least `topup_fee_min_micro_sar`), shown before payment and never refunded. `/api/v1/models` and `/models` show our price in force × rate; an activity record's SAR charge equals its upstream USD cost × the rate it was converted at.
5. The gateway's own budgets are not used for money: they count recorded spend only and lag by seconds.
6. Top-ups are a Stripe checkout in SAR (halalas) and a signed webhook (ADR 0012), with the provider, its mode and the on/off switch chosen by staff (ADMIN-03). Stripe is not settled for Saudi Arabia: see "Waiting on the owner".

## What is done

- Credits ledger with reserve and settle, key spending limits, low-balance handling, reconciliation against the gateway's log.
- Keys, credits, activity and data-policy pages for the customer; public model catalog at `/models`.
- Data policy (ADR 0015): provider terms per gateway key, a policy per organization pushed to the gateway as key allowlists, evidence per request as of the time it was made.
- Admin: users, organizations (credits, rate limit, data policy), pricing (the top-up fee, the top-up limits and quick picks, ADMIN-05, and the USD→SAR rate with the bank's spread, ADMIN-06, all in SAR), providers and their keys, the request log, the audit log, reconciliation. Staff roles admin, finance and support, all with two-factor sign-in.
- Brand: the control plane says Lemru everywhere; CI fails if the template's name comes back outside an allowlist.
- Money in SAR (ADMIN-06, BILL-15, ADR 0017, migration `0033`): ledger, holds, key limits, low-balance thresholds, top-ups and the money settings are micro-SAR; seeded SAR defaults are top-ups of SAR 20 to SAR 3,750, quick picks 40/100/200/400, fee minimum SAR 3, low-balance default SAR 20 (the owner adjusts them in the admin). Screens and emails format with `formatMicroSar` (`@workspace/shared/utils`); the model catalog shows SAR per 1M with USD beside it. The public API answers in SAR with `currency: "SAR"` (SPEC section 2). The admin form was not checked in a browser.
- One frontend (ADR 0016): the gateway's own UI is deleted. `GET /` on the gateway is a 404.
- Top-up fee per organization (ADMIN-24, migration `0035`): staff set a fee percentage and/or minimum for one organization, with an optional expiry and a note, on its admin page; `resolveTopupFee` (`pricing/fee.ts`) picks it while valid, else the default, and each top-up entry records the rate, minimum and source it paid. Not yet checked in a browser.
- Promo codes (BILL-08, migration `0036`, `packages/api/src/modules/promo-codes`). There are three kinds: a fee discount on a top-up, bonus credit on a top-up, and free credit. Each code can limit total uses, uses per organization, dates, the minimum top-up and which organizations may use it. Every use locks the code's row. Bonus and free credit are `promo` ledger entries that name the code. A discounted top-up records fee source `promo`. Staff manage codes at `/admin/promo-codes`. Customers enter a code in the top-up form or redeem free credit on the credits page. The decisions are under SPEC BILL-08. Not yet checked in a browser.
- Model price table (ADMIN-09, ADR 0018, migration `0037`, `packages/api/src/modules/model-prices`, `/admin/model-prices`): append-only price versions in USD per provider and model or per provider key, with an effective time (never backdated, one per instant), five typed token rates and the gateway's other units in validated `other_rates`. The version in force is pushed to the gateway's pricing overrides (`gateway-client` `*PricingOverride*`), idempotently; `checkModelPrices` compares overrides and the gateway's catalog with ours. The datasheet only proposes (`model_price_suggestion`); staff approve or reject with a reason, and a price staff set after a suggestion wins. Jobs to schedule: `/api/internal/model-prices` every minute (pushes due and future prices, mends drift) and `/api/internal/model-price-suggestions` daily. **After deploying, click "Import current prices" once** (or POST `/api/admin/model-prices/import`): it copies what the gateway charges today, so charges do not change. Gateway change: `ModelCatalog.PricingOverrideName` and the credits plugin's `metadata.price_override` (`MODIFICATIONS.md`). Proven against the real binary (`model-prices.e2e.test.ts`). Not yet checked in a browser.
- Payment gateway management (ADMIN-03): staff switch top-ups on or off, pick the active provider and its mode (test or live) at `/admin/payments` (`packages/api/src/modules/payments`, tables in `schema/payments.ts`, migration `0032` seeds today's behaviour: Stripe, live, top-ups on). Secrets stay in env, one pair per mode (`STRIPE_TEST_*` added); the page shows only set or missing. Checkouts use the chosen mode; the webhook verifies each event with its own mode's secret and always credits live payments; test payments credit only while in test mode. Not yet checked in a browser.
- Payment providers: Stripe only. The template's other providers were removed (owner, 2026-11-27) with their code, env, dependencies and translations; a provider implements `BillingProviderStrategy` (`packages/billing/web/src/providers/types.ts`), which is where the Saudi gateway goes.
- The gateway features outside the product are removed, each with its code, routes, schemas, migrations, tests and baseline rows, and listed in `apps/gateway/MODIFICATIONS.md` ("Features outside the product"): RSS feeds, outbound webhooks, Warp (the internal chat), the prompt repository, the Agent Gateway (A2A), relays, MCP (with its OAuth2 server, the Claude Code gateway sign-in and temporary tokens), and the providers that resell a personal subscription or another gateway (GitHub Copilot, Opencode Zen and Go, the ChatGPT/Codex sign-in passthrough, the Claude Code subscription passthrough that forwarded a caller's own Anthropic token upstream). Also gone (2026-11-26): the `jsonparser` plugin, `framework/queue`, `helm-charts/`, the settings of upstream's paid edition that no code reads (alerting, guardrails, audit logs, cluster, SCIM and others) and every trace of its observability connectors; and the dead code: every function the gateway binary cannot reach, found with `deadcode`, except code for the switched-off request types, test support, `plugins/mocker` (used by the semantic-cache tests) and the plugin SDK's guardrail and redaction helpers. The `Makefile` has `build` only.
- Switched off, not removed: speech, transcription, images, video, realtime, rerank, batch and files keep their code and routes; `plugins/credits` refuses them with a 403 until each is switched on.
- English only. Spanish was removed.
- The template's leftovers are gone: seven analytics vendors (PostHog stays), demo charts, demo blog posts, the chat demo.

## What is next, in order

1. **Switch on the request types, one at a time** (`PLAN.md`, "Switching on request types"): first end-to-end tests for embeddings and responses, which are billed already; then speech and transcription; then images; realtime, rerank, batch and files later.
2. **The playground.** The template's chat demo is removed (page, API route and the `ai` packages): it called OpenAI directly for any signed-in user, outside credits and outside the data policy. The owner wants a playground like OpenRouter's chat. Build it as a new module that calls the gateway with the organization's key, so credits and the data policy apply.
3. **Response cache, opt-in.** The owner keeps the gateway's semantic cache and routing between providers. The cache must be off unless the customer asks for it per request (OpenRouter does it with a header), and never on in the in-Kingdom deployment or for an organization whose policy forbids retention.
4. **Arabic**, with right-to-left layout, in the web app and in emails.
5. **The gateway rename** to Lemru (`apps/gateway/scripts/rename`): name `Lemru`, prefix `lm`. It needs a registered domain that resolves. Until then one upstream name reaches the customer: the activity page mentions the header `x-bifrost-request-id`.
6. **Payments for Saudi Arabia**, once the owner picks a gateway: a new provider module beside Stripe in `packages/billing`.

## Waiting on the owner

- A domain, hosting for both deployments, a logo (the current mark is a placeholder).
- The payment gateway. Stripe's availability to a Saudi-registered business is disputed between sources and Stripe does not route mada; the local options are Moyasar, Tap, HyperPay and PayTabs, all quoted on request.
- The chargeback rule (proposal: suspend the organization's keys when a dispute opens; deduct the amount if it is lost).
- Legal pages: the three under `packages/cms` are the template's text.
- Analytics: PostHog is the one provider left. As configured it contacts its host before the visitor consents.
- GitHub: the repository moved to `Lem-ru-sa/Lemru-sa` (pushes to `origin` still land there) and `feat/ai-gateway` is its default branch. On the old name every Actions job has ended within seconds without running a step since 2026-11-25 09:34 UTC (an account matter, not the code); on the new one Actions shows no workflows. The owner enables Actions there and decides what `main` holds. Until then every check runs locally (below), and CI is not re-run.

## How to check your work

CI cannot be relied on right now, so run the checks yourself before you push, and push once per piece of work: every push to the pull request starts the half-hour gateway job.

```sh
node --run format && node --run lint && node --run typecheck
# database tests need real Postgres; without DATABASE_URL they are skipped, not passed
env -u SKIP_ENV_VALIDATION pnpm with-env pnpm -F @workspace/api exec vitest run
pnpm with-env pnpm --filter web build      # catches what the type check does not, such as a missing Suspense
# gateway
(cd apps/gateway && make build LOCAL=1)
bash apps/gateway/scripts/test.sh                     # every module against the recorded baseline (about an hour)
(cd apps/gateway/plugins/credits && CREDITS_TEST_DATABASE_URL=... CREDITS_E2E_BINARY=$PWD/../../tmp/bifrost-http go test -count=1 -race ./...)
bash apps/gateway/scripts/verify.sh brand.json        # rename acceptance; the brand's domain must resolve
```

In a fresh cloud container: `service postgresql start`, create a login role and database (`turbostarter`/`core` matches the credits test), copy `.env.example` to `.env.local` with that `DATABASE_URL`, run the migrations, and give the web build placeholder values for the variables it asks for (PostHog, Better Auth, Stripe, email, S3); `.env.local` is ignored by git. `verify.sh` needs a domain that resolves: until the owner registers one, pass a copy of `scripts/rename/brand.example.json` with a domain that does. After deleting gateway tests, `python3 apps/gateway/scripts/prune-baseline.py --show` lists the baseline rows they leave behind, and `--write` drops them.

House rules that have caught real bugs here:

- Every new rule gets a test, and the test is proven by breaking the rule once and seeing it fail.
- A screen is checked in a browser on the production build, not in dev mode: two hydration bugs only showed there.
- Before writing gateway, money, usage or provider logic, read what `apps/gateway` already does (`AGENTS.md`, "Gateway first").
- Anything a subagent or a review reports is a claim until it is checked in the code.

## Known weak spots

- The gateway accepts a loopback address as a provider's base URL (it refuses link-local ones). Only an admin can add a provider.
- A request cut off before its first chunk is charged nothing.
- `x-request-id` and `x-bf-*` headers from the client are not stripped yet; that belongs to the edge (PLAN 8.1).
- Subscription plans from the template still exist in `packages/billing` and the dashboard billing pages; the product sells credits only.
- No browser test runs in CI.
- `keys.e2e.test.ts` (run with `GATEWAY_E2E_BINARY`) still expects a charge in micro-USD (450) from before the SAR ledger; the ledger charges 1,687 micro-SAR, so three of its tests fail. The credits plugin's own e2e checks the SAR charge.
- Model prices: a push landing between an attempt's hold and its charge charges the new price (as a datasheet sync did); a model the gateway's catalog does not list cannot be priced (ADR 0018, out of scope).
