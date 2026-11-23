# ADR 0010: Markup per provider and the top-up fee are set by staff in the admin console

Status: Accepted
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
