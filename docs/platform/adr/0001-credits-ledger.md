# ADR 0001: Credits ledger in Postgres, enforced by a gateway plugin with reservations

Status: Accepted. Where the reserve and settle statements run is revised by ADR 0009. The unit is revised by ADR 0017: the ledger is in micro-SAR, and each hold and charge stores the USD to SAR rate it was converted at and the provider's USD cost.
Date: 2026-11-21

## Context

Customers prepay. A request must be refused when the organization cannot pay for it, and every charge must be traceable.

Neither codebase has a prepaid balance:

- TurboStarter has subscriptions and one-time orders (`packages/db/src/schema/billing.ts`) but no balance.
- Bifrost budgets (`framework/configstore/tables/budget.go`) store `max_limit` and `current_usage` as `float64` dollars with a required `reset_duration` (`not null`). They cap spend per window. They do not hold a balance that top-ups increase. Their counters live in each instance's memory (`plugins/governance/store.go`) and are flushed to the config store, so two instances enforce independently (see ADR 0006).

Constraints from the brief: money is an integer in the smallest unit, the ledger is append-only, every debit is idempotent.

Per-token prices go down to 1.014e-9 USD (research brief 03), so cents cannot hold a per-request cost.

## Options considered

1. **Bifrost budgets with a very long reset period** (for example `100Y`), with top-ups raising `max_limit`. Rejected: floats, no history of individual charges, per-instance counters, and a top-up becomes an in-place edit instead of an appended entry.
2. **Ledger in the control-plane Postgres, enforced in the gateway by a plugin.** The plugin reserves an estimated maximum before the provider call and settles the actual cost afterwards.
3. **A separate ledger service** (its own process and API, called by the gateway on each request). Rejected for now: one more network hop and deployable for no gain at our size. The table design in option 2 can move behind a service later without a data migration.
4. **Check the balance, then debit afterwards, with no reservation.** Simpler, but N concurrent requests can each pass the check and overdraw the balance by N times the largest request. OpenRouter added its in-flight spending budget for exactly this reason (research brief 01).

## Decision

Option 2.

**Unit.** All money in the ledger is `bigint` **micro-USD** (1 USD = 1,000,000). One cent is 10,000 units, so payments convert exactly. The per-request cost from Bifrost (`float64` USD) is converted once, at settlement, rounding half up to the nearest micro-USD. Unit prices are never stored in this unit (they stay as Bifrost's catalog values).

**Tables** (Drizzle schema in `packages/db`, owned by the control plane):

- `credit_ledger_entry`: append-only. Columns: `id`, `organization_id`, `amount` (signed micro-USD; credits positive, debits negative), `kind` (`top_up`, `usage`, `refund`, `adjustment`, `promo`), `idempotency_key` (unique), `reference` (order id, request id), `metadata` (jsonb), `created_at`. No `UPDATE` or `DELETE` grants for any application role.
- `credit_account`: one row per organization. `balance` (micro-USD, the sum of entries), `held` (sum of open reservations), `updated_at`. Updated in the same transaction as each entry insert. It is a cache of the ledger and can be rebuilt from it.
- `credit_reservation`: one row per in-flight request. `id`, `organization_id`, `amount`, `expires_at`, `settled_at`.

**Enforcement** (gateway plugin `credits`, Go, compiled into our gateway build):

1. Once per request, after authentication (`HTTPTransportPreHook`, where the virtual key and its customer are known), estimate the maximum cost: input tokens are not known yet, so use the request's `max_tokens` (or the model's max output when unset) at the model's output price, plus a fixed allowance for input. When the request lists fallback models, use the most expensive of them, because any of them may end up serving it. Then run, in one statement:
   `UPDATE credit_account SET held = held + $est WHERE organization_id = $org AND balance - held >= $est`.
   Zero rows updated means the request is refused with `402` before any provider call.
2. After the request ends (see ADR 0002), one transaction inserts the `usage` entry with its idempotency key, subtracts the actual cost from `balance`, releases the hold, and marks the reservation settled.
3. A reaper releases reservations past `expires_at` (crashed gateway, lost settlement). Each release is logged for reconciliation (ADR 0002).

The actual cost can exceed the estimate (the input allowance is a guess). The balance may then go slightly negative, as OpenRouter's does. The next request is refused until a top-up.

## Consequences

- The ledger is the single source of truth for money. Bifrost budgets stay available for customer-set spend caps on keys, but nothing financial depends on them.
- One hot row per organization (`credit_account`). Shortcut: fine for hundreds of requests per second per organization. When one organization exceeds that, split the hold into several sub-accounts.
- The gateway needs a database role that can only insert ledger entries and update `credit_account` and `credit_reservation`. The control plane owns the migrations.
- Each request pays two short Postgres round trips (reserve, settle). This must be measured with governance and Postgres on before any latency number is quoted (working rule from the brief).
