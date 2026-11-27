# ADR 0013: Per-key spending limits are enforced on what the ledger charges

- Status: Accepted
- Date: 2026-11-23
- Amends: ADR 0004 (per-key caps as gateway budgets)
- Builds on: ADR 0001 (ledger), ADR 0005 (markup in the ledger), ADR 0009 (credits plugin hooks)
- Note (2026-11-27): the markup is removed (ADR 0010 amendment). The limit still counts what the ledger charges, now provider cost × the USD to SAR rate.

## Context

ADR 0004 mapped a key's spending limit (OpenRouter's `limit` and `limit_reset`) to a budget on the gateway's virtual key, and Phase 5 built it that way. The gateway's governance plugin charges a budget with the provider's cost from its own price catalog. Our markup is applied only in the ledger (ADR 0005), so a key with a $5 limit and a 5% markup let the customer spend about $5.25 before it stopped. The governance plugin takes its cost from the catalog directly; changing that means patching upstream code.

## Decision

1. **The credits plugin enforces the limit** when it takes the hold for an attempt, on the amounts the ledger charges, markup included. The gateway's virtual keys get no budget; the control plane no longer sends one.
2. **What counts.** A key's spend is the sum of its `usage` entries in the current window, plus the amounts of its open holds. A hold is refused when spend plus the new hold would pass the limit. Holds are the most an attempt can cost, so a key cannot overshoot its limit by in-flight requests.
3. **Windows** are UTC calendar periods, as OpenRouter's: the day, the ISO week (from Monday) and the month. A limit without a reset counts all time.
4. **Recording the key.** `credit_reservation` and `credit_ledger_entry` get a nullable `virtual_key_id` (migration `0022`). The hold records the key from the governance context; the usage entry copies it from the hold in the settlement statement. Partial indexes cover the two sums. This column also gives Phase 9 its per-key usage.
5. **Concurrency.** The check runs in a transaction that first takes `pg_advisory_xact_lock` on the key, then reads the sums, then takes the hold, so two requests on one key cannot both use the last of the limit. Keys without a limit skip the transaction and the lock: the common path costs what it did.
6. **Where the limit is read.** `api_key.limit_micro_usd` and `limit_reset`, by `virtual_key_id`, on every hold. The `gateway_ledger` role gets SELECT on those three columns only, not on the key's name or hash. A virtual key the control plane did not create has no row and only the balance applies.
7. **Refusal.** `403` with type `credits_key_limit`, as OpenRouter answers a key over its limit with 403.

## Amendment (2026-11-27): one definition of the windows

The windows were written twice, as `date_trunc` in the plugin's SQL and in TypeScript for `GET /api/v1/key`, with `windowEnd` again in Go and TypeScript. They are now the SQL functions `key_limit_window_start(reset, at)` and `key_limit_window_end(reset, at)` (migration `0040`, `IMMUTABLE`): the plugin counts a key's spend from the first and says `resets_at` from the second; the dashboard and `GET /api/v1/key` count usage and show the reset with the same two (`packages/api/src/modules/keys/usage.ts`). Their boundaries are tested in Go, and through the gateway binary a key at its limit gets 402 `key_limit` until its window has turned, then works again.

## Consequences

- The limit and the charge are the same number, whatever the markup or a change to it.
- A key that reaches its limit gets 403 before the provider is called, like an empty balance gets 402.
- The spend sum reads the key's entries in the window. For a key with very many requests a month this grows; a per-key counter maintained in the settlement statement is the change to make if measurements show it.
- Virtual keys created before this change keep the gateway budget they were given. There is no production data yet; in a running system they would be cleared with one update per key.
