# Project state

Read this first. It says what the product is, where everything lives, what is done, what is next and what waits on the owner, so that nobody has to read the whole repository to continue. Last updated 2026-11-25.

The long form is in `PLAN.md` (steps and their status), `ARCHITECTURE.md` and `adr/` (one file per decision). When this file and the code disagree, the code is right: fix this file.

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
  db/         Drizzle schema (one file per feature) and migrations 0000-0030.
  gateway/    TypeScript client for the gateway's management API.
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

Our features today: `keys`, `credits`, `activity`, `data-policy`, `pricing`, `reconciliation`, `v1` (the public API under `/api/v1`), and under `admin`: `organizations`, `providers`, `requests`, `audit`, `users`, `customers`. In the gateway: `plugins/credits`.

The mobile app and the browser extension of the template were removed on 2026-11-23 (commit `2089e54`). If a mobile app is built, `apps/mobile` comes back from git history and uses the same `packages/`.

## How a request is paid for

1. The customer's key is a gateway virtual key; the organization is a gateway customer.
2. `plugins/credits` reserves the worst-case cost of the request before it reaches the provider, and settles the real cost after. The ledger is in our Postgres (`credit_*` tables); the gateway connects as the role `gateway_ledger`, which may only do what migration 0026 grants.
3. The gateway's own budgets are not used for money: they count recorded spend only and lag by seconds.
4. Top-ups are a Stripe checkout and a signed webhook (ADR 0012). Stripe is not settled for Saudi Arabia: see "Waiting on the owner".

## What is done

- Credits ledger with reserve and settle, key spending limits, low-balance handling, reconciliation against the gateway's log.
- Keys, credits, activity and data-policy pages for the customer; public model catalog at `/models`.
- Data policy (ADR 0015): provider terms per gateway key, a policy per organization pushed to the gateway as key allowlists, evidence per request as of the time it was made.
- Admin: users, organizations (credits, rate limit, data policy), pricing (our fee per provider and the top-up fee), providers and their keys, the request log, the audit log, reconciliation. Staff roles admin, finance and support, all with two-factor sign-in.
- Brand: the control plane says Lemru everywhere; CI fails if the template's name comes back outside an allowlist.
- One frontend (ADR 0016): the gateway's own UI is deleted. `GET /` on the gateway is a 404.
- English only. Spanish was removed.

## What is next, in order

1. **Remove gateway features that are not part of the product**, one at a time, each with its tests: MCP, agents, relays, the internal chat (`framework/warp`), RSS, webhooks, the prompt repository. `docs/` and `examples/` under `apps/gateway` go with MCP, whose tests are what reads them. After MCP, replace the 3,000-line `Makefile` with one that has `build` only: `scripts/test-baseline.sh` still calls `make setup-mcp-tests`.
2. **The playground.** `POST /api/ai/chat` and the page `/dashboard/ai` are the template's demo: they call OpenAI directly with `OPENAI_API_KEY`, for any signed-in user, outside credits and outside the data policy. The owner wants a playground like OpenRouter's chat. It has to go through the gateway with the organization's key and credits. Until then, do not set `OPENAI_API_KEY` in a deployment.
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
- GitHub Actions: since 2026-11-25 09:34 UTC every job on the repository ends within seconds without running a step. It is an account matter, not the code.

## How to check your work

CI cannot be relied on right now, so run the checks yourself before you push, and push once per piece of work: every push to the pull request starts the half-hour gateway job.

```sh
node --run format && node --run lint && node --run typecheck
# database tests need real Postgres; without DATABASE_URL they are skipped, not passed
env -u SKIP_ENV_VALIDATION pnpm with-env pnpm -F @workspace/api exec vitest run
pnpm with-env pnpm --filter web build      # catches what the type check does not, such as a missing Suspense
# gateway
(cd apps/gateway && make build LOCAL=1)
bash apps/gateway/scripts/test.sh transports          # one module against the recorded baseline
(cd apps/gateway/plugins/credits && CREDITS_TEST_DATABASE_URL=... CREDITS_E2E_BINARY=$PWD/../../tmp/bifrost-http go test -count=1 -race ./...)
```

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
