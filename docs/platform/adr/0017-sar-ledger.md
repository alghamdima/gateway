# ADR 0017: Money in SAR, converted once per request at a rate staff set

- Status: Accepted
- Date: 2026-11-27
- Amends: ADR 0001 (unit of the ledger), ADR 0010 (pricing settings), ADR 0012 (checkout currency)
- Amended by: ADR 0010's amendment of 2026-11-27. The markup is removed: the conversion below is `micro-SAR = cost USD × effective rate e6`, the plugin reads the rate and spread alone (`rates.go`, which replaces `markups.go`), and holds and entries carry no markup.
- Features: ADMIN-06, BILL-15 (`SPEC.md`)

## Context

ADR 0001 kept every amount in micro-USD. The owner decided on 2026-11-27 that customers deal in SAR end to end: balances, holds, key limits, top-ups, receipts, emails and screens. Providers bill in USD, and the gateway's model catalog prices in USD (`framework/modelcatalog`, `CalculateCost` returns a `float64` of USD). The owner also decided that the USD to SAR rate is set by hand by staff (default 3.75, the peg; never fetched), that the bank's conversion commission is a second setting added to it, and that a rate change applies to new requests only.

Gateway first: nothing in `apps/gateway` converts currencies. The governance budgets and the catalog are USD only, and the pricing overrides hold absolute USD prices (ADR 0005). The conversion is ours, in `plugins/credits`, which already reads staff prices every 30 seconds (`markups.go`).

LiteLLM keeps spend in USD and has no customer currency; the idea taken from it is to keep the provider's USD cost on each spend row so reports can be rebuilt.

## Decision

1. **Unit.** Every amount the ledger and the settings hold is `bigint` micro-SAR (1 SAR = 1,000,000): `credit_account.balance` and `held`, `credit_reservation.amount`, `credit_ledger_entry.amount`, `api_key.limit_micro_sar`, `low_balance_alert.threshold_micro_sar`, and in `pricing_settings` the fee minimum, the top-up minimum, maximum and quick picks (`*_micro_sar`). Columns that named their unit were renamed (migration `0033`); `amount`, `balance` and `held` keep their names. A halala is 10,000 micro-SAR, so payments convert exactly, as cents did.
2. **Rate.** `pricing_settings.usd_to_sar_rate_e6` is the official rate × 1,000,000 as an integer (3.75 is 3,750,000), default 3,750,000, checked between 3,000,000 and 5,000,000 by the database and the API: a ceiling against a typo, not a business rule. `pricing_settings.fx_spread_bps` is the bank's commission in basis points, default 0, checked 0 to 1,000 (10%). Both are edited in the admin pricing settings, with the effective rate shown live, and every change is written to `pricing_change` and `admin_audit_log` with the values before and after (the effective rate included).
3. **Effective rate.** What a request converts at: `effective = ceil(official × (10,000 + spread) / 10,000)`, an integer × 1,000,000, rounded up so the commission is never under-counted. With no spread it is the official rate. The plugin (`money.go`, `newFXRate`), the API (`@workspace/shared/utils`, `effectiveUsdToSarRateE6`) and a database check on every hold and charge compute the same thing.
4. **Where the rate is read.** The credits plugin loads the rate and spread with the markups, by named columns, in one statement, every 30 seconds (`markups.go`); `gateway_ledger` already has SELECT on `pricing_settings`. A rate that is not positive is refused, and the plugin keeps the last good values (or refuses requests until it has loaded once), as for markups.
5. **Conversion.** Once per attempt, exactly (big rationals on the value the float holds, never float multiplication): `micro-SAR = cost USD × effective rate e6 × (10,000 + markup bps) / 10,000`.
   - The **hold** rounds **up**, so it is never smaller than what it stands for.
   - The **charge** rounds **half up** to the nearest micro-SAR, at the **hold's** official rate, spread, effective rate and markup, whatever staff set meanwhile. A charge that rounds to zero writes no entry, as before.
   - The markup per provider stays as it is (its removal is BILL-14 and CHARGE-03, a separate step).
6. **What is stored.** `credit_reservation` stores `usd_to_sar_rate_e6`, `fx_spread_bps` and `effective_usd_to_sar_rate_e6` (not null). Each usage entry stores the same three plus `cost_micro_usd`, the provider's cost before markup, rounded half up to the micro-USD; the exact float stays in `metadata.provider_cost_usd`. A check requires all four on every new usage entry (`NOT VALID`, because the append-only ledger cannot fill in entries written before). Past entries and balances never change when the rate changes.
7. **Reconciliation** compares USD with USD: the gateway log's cost with the sum of `cost_micro_usd`, allowing half a micro-USD of rounding per entry on top of the existing tolerances. The rate cannot make the two disagree.
8. **Payments.** Stripe Checkout in currency `sar`, amounts in halalas (Stripe's minor unit for SAR has two decimals); the webhook refuses a session or refund in any other currency and credits micro-SAR. The migration seeds the SAR defaults as today's USD values × 3.75, rounded to whole riyals: top-up minimum SAR 20, maximum SAR 3,750, quick picks 40/100/200/400, fee minimum SAR 3. The default low-balance threshold is SAR 20 (a code default until ADMIN-14).
9. **Public API.** `/api/v1/key`, `/api/v1/credits` and `/api/v1/generation` answer in SAR, the account's currency, and say so with `currency: "SAR"`; a generation adds `upstream_cost_usd` (total and per attempt) and the attempt's `usd_to_sar_rate`. `/api/v1/models` keeps `pricing` in USD per token, as OpenRouter's, and adds `pricing_sar` (the same at the current effective rate, exact decimals) and `usd_to_sar_rate`. OpenRouter answers these fields in USD; a USD figure for a SAR balance would be a conversion at today's rate that changes with it, so the fields carry the ledger's own unit instead.

## Consequences

- One request is converted once, so reserve and settle agree, and a past charge can be rebuilt from its own row: `amount = round(cost × effective rate × (1 + markup))`, with the exact cost in `metadata`.
- Reports can show what the bank's spread cost: `effective − official` on each entry.
- No deployment exists, so balances were not converted: a test database keeps its numbers, read as SAR. Holds open at the migration were back-filled with a rate of exactly 1 (they were in USD); old usage entries have no USD cost column and reconciliation reads none for them.
- The global deployment may want USD later (ADMIN-06 "currency per deployment"): that is a second currency setting and is not built.
