# ADR 0010: Markup per provider and the top-up fee are set by staff in the admin console

Status: Accepted. Amended by ADR 0017: money settings are micro-SAR, and `pricing_settings` also holds the USD to SAR rate and the bank's spread. **Amended 2026-11-27 (owner): there is no markup on requests**; see "Amendment: no markup, the fee at top-up is the revenue" at the end. The sections on markups below are history.
Date: 2026-11-23

## Context

ADR 0005 puts the margin in the credits plugin as a multiplier on provider cost, with the value still open. ADR 0003 adds any purchase fee on the payment, with the value still open. ADR 0009 shipped the plugin with one global `markup_bps` in the gateway's config file.

The owner decided (2026-11-23):

- Both values are set by hand from the admin console, not in a config file and not in code.
- The markup is set per provider.

A config file value can only be changed by redeploying the gateway, and it is one number for every provider.

## Options considered

1. **Keep the value in the gateway config, one per provider.** Rejected: staff would edit a file and restart the gateway to change a price, outside the admin console.
2. **Bifrost's pricing overrides.** Rejected for the reasons in ADR 0005: they hold absolute prices per model, not a percentage, and they hide the provider cost.
3. **Settings in the control-plane database, edited in the admin console, read by the plugin.** The plugin already connects to that database for the ledger.

## Decision

Option 3.

- **Storage.** `pricing_settings` is a single row: the default markup and the top-up fee in basis points, and the minimum top-up fee in micro-USD (migration `0018`). `provider_markup` has one row per provider that differs from the default. Both carry who changed them and when. Every change is also appended to `pricing_change` (who, when, the values before and after), which, like the ledger, rejects UPDATE and DELETE.
- **Bounds.** A markup is 0 to 100,000 bps (0% to 1,000%). The fee is 0 to 10,000 bps. Negative values are refused by the database, so a price below provider cost cannot be set by mistake.
- **Initial values.** OpenRouter's model (research brief 01): no markup on inference, and a top-up fee of 5.5% with a $0.80 minimum. Staff change them in the console.
- **The fee** is the larger of the percentage and the minimum, rounded half up to whole cents because a card is charged in cents (`packages/api/src/modules/pricing/fee.ts`, exact integer arithmetic).
- **Which markup applies.** The provider of the attempt, by its gateway name (`openai`, `anthropic`, ...). A provider with no row gets the default. A fallback to another provider uses that provider's markup.
- **When a change applies.** The plugin keeps the values in memory and reloads them every 30 seconds. The markup is fixed when the attempt's hold is taken and the same value is used to charge it, so a change never splits one attempt between two prices. Each usage entry records the markup it used.
- **Fail closed.** Until the plugin has loaded the values once, it refuses billable requests with 503, as for a ledger it cannot reach. A failed reload keeps the last values loaded and is logged.
- **Who can change them.** Users with the admin role only, through `/api/admin/pricing`. Organization owners cannot.
- **The fee** is read from the same row by the top-up checkout (Phase 6) and added on the payment, never taken from credits (ADR 0003).
- `markup_bps` is removed from the plugin's config. ADR 0009's "one global value from the plugin's config" is superseded.

## Consequences

- A price change takes effect within about 30 seconds on every gateway instance, without a deploy.
- The ledger shows, for every charge, the provider cost and the markup applied. `pricing_change` shows who set which value when.
- The provider list is free text checked only for format: a misspelled provider name matches nothing and the default applies. The console shows the rows that exist, and the gateway's provider names are listed beside the field.
- The public model catalog (Phase 7) must show prices with the same per-provider markup.
- Still deferred from ADR 0005: a markup per organization.
- Where we differ from OpenRouter on price: the markup per provider is ours (OpenRouter sells at the provider's price); there is no crypto payment, so no separate crypto fee (OpenRouter: 5%); refunds within 24 hours and credit expiry after a year belong to Phase 6 and are not built.

## Amendment: no markup, the fee at top-up is the revenue (2026-11-27)

Owner decision, as OpenRouter does it: the platform earns the fee taken at top-up, and a request is charged at the serving provider's rate with no markup (`SPEC.md` §2, BILL-14, CHARGE-03).

- **The charge.** `micro-SAR = provider cost USD × effective USD to SAR rate e6`, the effective rate being official × (1 + bank spread), rounded up to the millionth (ADR 0017). The hold rounds up, the charge rounds half up, and the charge uses the hold's rate (`apps/gateway/plugins/credits/money.go`).
- **What the plugin reads.** Only the rate and the spread, from `pricing_settings`, every 30 seconds (`plugins/credits/rates.go`, which replaces `markups.go`). Holds and usage entries no longer record `markup_bps`.
- **Storage.** Migration `0034` drops `pricing_settings.default_markup_bps` and the table `provider_markup` (with the gateway role's grant on it). `pricing_change` keeps its old rows, markup values included: it is append-only history, and the admin list shows a field that no longer exists as such.
- **Admin.** The markup routes (`PUT`/`DELETE /api/admin/pricing/providers/:provider`), their access rules, Zod schemas, form and translations are removed; a request to one is refused to every role. The fee, the top-up limits and the rate stay.
- **Customers.** `/api/v1/models` and `/models` show the provider's price (USD) and that price × the effective rate (SAR). An activity record's `total_cost` (SAR) equals its `upstream_cost_usd` × the rate it was converted at.
- **Fee.** Unchanged: `topup_fee_bps` with `topup_fee_min_micro_sar`, shown before payment, added to the payment, never taken from credits and never refunded (ADR 0003).
- Superseded: ADR 0005 (margin as a multiplier) and the per-organization markup it deferred; the markup parts of this ADR above; the markup in ADR 0017's formula.
