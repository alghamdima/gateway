# Build plan

Ordered phases from the current state (TurboStarter only, gateway not yet in the repo) to a working prepaid gateway. Each phase has a scope, its dependencies, and an exit check that can be run and recorded. Architecture is in `ARCHITECTURE.md`, decisions in `adr/`.

"Brief phase" refers to the numbering in the original build brief.

## P1. Gateway baseline on the pristine tag (brief phase 4, part 1)

- **Scope:** In a scratch directory outside the repo, build Bifrost `transports/v2.6.0` with a local `go.work` (`make build LOCAL=1`) and the embedded UI. Run every unit test that needs no provider keys, per module (`core`, `framework`, each plugin, `transports`). Point `framework.pricing.pricing_url` at a local file so startup does not depend on getbifrost.ai.
- **Depends on:** Go 1.27.4 and Node (both installed).
- **Exit check:** `docs/platform/gateway-baseline.md` lists the exact commands, the commit, the Go and Node versions, and per module the passing, failing and skipped tests (names, not counts). The binary starts with a local pricing file and answers `GET /health`.

## P2. Rename script and gateway import (brief phases 4 part 2 and 5a)

- **Scope:** `services/gateway/scripts/rename` (one script, deterministic and idempotent) that turns a pristine tree at a given tag into ours, applying the mappings most-specific first: Go module paths, npm package, wire names (headers, key prefix, env vars, metrics, config keys), identifiers, files and directories, database names, UI text and assets. The script's own tests cover `maximum`/`maximize`, network module imports, and URLs. Remove `plugins/maxim` if nothing requires it. Point getbifrost.ai and getmaxim.ai URLs at our host or local files, or remove the feature. Keep LICENSE, THIRD_PARTY_NOTICES.md, NOTICE files and every license header. Add `services/gateway/UPSTREAM` (repo, tag, commit), the scan allowlist with a reason per entry, and a modification notice as Apache-2.0 requires. Exclude `services/gateway/` from the pnpm workspace globs, oxlint and oxfmt. Keep the upstream skills reachable and renamed.
- **Depends on:** P1, and the **brand inputs** (code name, short prefix, key prefix, Go module path, product name, domain). The script can be built and tested with placeholder values before then, but nothing is committed until the real values exist, because the first commit of the gateway must already be renamed.
- **Exit check:** all of brief phase 5a's done conditions:
  1. Every Go module builds and `go vet` is clean.
  2. The unit-test pass/fail set equals the P1 baseline.
  3. The UI builds and the gateway boots against Postgres.
  4. A virtual key created through the governance API completes a chat request against a mock upstream.
  5. A case-insensitive scan for `bifrost`, `maximhq`, `getmaxim`, `x-bf-`, `sk-bf-` returns only allowlisted entries.
  6. Real 200, 400, 401 and `/v1/models` responses carry none of those strings in headers or body.
  7. Running the script on a fresh checkout of the tag reproduces the committed tree byte for byte.

## P3. TurboStarter white-label (brief phase 5b)

- **Scope:** Replace the 153 `turbostarter` occurrences: `PRODUCT_NAME`, auth `appName`, `cookiePrefix`, `emailDomainName`, the `turbostarter://` scheme and its test, extension symbols, e2e constants and seed emails, i18n (`en`, `es`), email header, footer and logo, marketing links and buy CTAs, docker-compose project name and default credentials, `turbo/` generator templates, README, AGENTS.md and `.agents/` text. Delete `apps/mobile/google-services.json`. Mobile config (`app.config.ts` name, slug, scheme, bundle ids, EAS project id, Expo updates URL) is left alone while mobile is skipped, and listed in the allowlist with that reason. Lemon Squeezy buy links are removed. Keep `LICENSE.md` and copyright notices.
- **Depends on:** brand inputs (product name, domain).
- **Exit check:** the `turbostarter` scan returns only the committed allowlist. `pnpm lint`, `pnpm format`, `pnpm test` and the web build pass. Sign-in works end to end and the session cookie carries the new prefix.

## P4. Credits ledger in the control plane (ADR 0001, 0003)

- **Scope:** Drizzle schema and migration for `credit_ledger_entry`, `credit_account`, `credit_reservation`. A small ledger module in `packages/api` with `credit(...)`, `getBalance(...)`, and a rebuild that recomputes `credit_account` from entries. A Postgres role for the gateway with only `INSERT` on entries and `UPDATE` on accounts and reservations. An admin endpoint to add a manual `adjustment` (staff only), so the system is usable before payments exist.
- **Depends on:** nothing above. Can start right after approval, in parallel with P2/P3.
- **Exit check:** tests show that (a) inserting the same idempotency key twice changes the balance once, (b) the gateway role cannot `UPDATE` or `DELETE` ledger entries (a real Postgres error), (c) the rebuild equals the cached balance after a random sequence of entries, (d) an admin adjustment appears in both ledger and balance.

## P5. Credits plugin in the gateway (ADR 0001, 0002, 0005)

- **Scope:** A new Go plugin `credits` in its own directory under `services/gateway/plugins/`, registered in the built-in plugin list by a small patch. Reserve in `HTTPTransportPreHook`, settle in `PostLLMHook` (final chunk for streams), the attempt-tuple idempotency key, the bounded settlement queue that falls back to synchronous writes when full, markup in basis points, and the reservation reaper.
- **Depends on:** P2 (renamed tree to patch), P4 (tables).
- **Exit check:** against a mock upstream and real Postgres:
  1. An organization with zero balance gets `402` and the mock sees no call.
  2. 50 concurrent requests against a balance that covers 10 never take the balance below minus one request's overrun.
  3. A replayed settlement (same tuple) writes one entry.
  4. The entry's amount equals provider cost × markup, rounded half up to micro-USD.
  5. A streamed request settles once, on the final chunk.
  6. Added latency per request is measured with governance and Postgres on and recorded with the method (p50 and p99).

## P6. Customer API keys end to end (ADR 0004)

- **Scope:** `api_key` table. A gateway governance client in `packages/api` (private network URL and admin credential from env). Create, list, disable and delete key routes with organization role checks. Dashboard pages for keys and balance. Organization deletion removes its customer and virtual keys in the gateway.
- **Depends on:** P2, P4, P5.
- **Exit check:** an e2e test signs in, creates an organization, gets credit through an admin adjustment, creates a key in the dashboard, calls the gateway's chat route with it against a mock upstream, and sees the balance drop by the expected amount. Disabling the key makes the next call return 401.

## P7. Multi-instance test (ADR 0006)

- **Scope:** Run two gateway instances on one config store and run the test in ADR 0006 as written. If it fails as expected, write the `LISTEN/NOTIFY` invalidation patch and run it again.
- **Depends on:** P6.
- **Exit check:** the ADR's test results recorded in `docs/platform/scaling-test.md`, first without the patch, then with it. A key created on A works on B within one second, and a key disabled on A is refused on B within one second.

## P8. Top-ups with real payments (ADR 0003)

- **Scope:** Top-up products with fixed amounts, checkout from the dashboard, the webhook that appends `top_up` entries keyed by payment id, refunds as negative entries.
- **Depends on:** P4, and the owner's choice of **payment provider**, **top-up amounts** and **purchase fee**.
- **Exit check:** in the provider's test mode: a test payment credits the balance exactly once even when the webhook is redelivered. A refund debits it exactly once. The checkout redirect alone never credits.

## P9. Catalog and usage views

- **Scope:** A public model catalog page with prices from the gateway catalog times the markup (shown per million tokens). A usage page per organization (spend, requests, tokens by model and by key, daily) from the ledger and the gateway log store. A generation detail view (OpenRouter-style record, research brief 06).
- **Depends on:** P5, P6.
- **Exit check:** for a fixed set of seeded requests, the usage page totals equal the ledger sums, and the catalog's displayed price for a model equals catalog price × markup.

## P10. Reconciliation and operations (ADR 0002, 0007)

- **Scope:** The daily job comparing log-store cost with ledger usage per organization and listing reaped reservations, with an alert on differences. The public ingress allowlist. Runbooks for upgrades (ADR 0008) and incidents.
- **Depends on:** P5.
- **Exit check:** a deliberately dropped settlement shows up in the next reconciliation report. On the public host, `GET /` and `/api/*` return 404 while `/v1/chat/completions` works.

## Order and parallel work

```
P1 ──> P2 ──> P5 ──> P6 ──> P7
        │      ^      │
        │      │      ├──> P9
P3      │     P4 ─────┼──> P8
(brand) │             └──> P10
```

- P1 and P4 need no owner input and can start immediately after approval.
- P2 and P3 wait for brand inputs. P8 waits for the payment provider choice.
- Per the brief, the gateway's first commit in this repo is the renamed tree (P2). Nothing from P1 is committed except the baseline report.
