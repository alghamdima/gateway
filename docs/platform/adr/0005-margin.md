# ADR 0005: Margin is applied in the ledger plugin as a multiplier on provider cost

Status: Accepted for the mechanism. The markup value is the owner's decision and still open.
Date: 2026-11-21

## Context

We sell inference above our provider cost. Bifrost knows the provider cost of each request: it computes it from its model catalog (`framework/modelcatalog/pricing.go`). It also supports scoped pricing overrides (`governance_pricing_overrides`, scopes `global`, `provider`, `virtual_key`, `user` and combinations, `framework/modelcatalog/datasheet/types.go`). An override holds **absolute** per-token prices (`input_cost_per_token`, `output_cost_per_token` and about 40 other fields). There is no percentage or multiplier field.

## Options considered

1. **Gateway pricing overrides.** Write overrides whose prices are provider price times (1 + markup). Rejected: an override holds absolute prices, so every model and every price field would need an override, regenerated whenever upstream pricing changes. Bifrost would also report our sale price as "cost", and the provider cost would disappear from the logs, which breaks margin reporting.
2. **Computed in the ledger.** Bifrost keeps reporting the true provider cost. The `credits` plugin multiplies it by the markup when it settles: `charge = round(provider_cost × (10000 + markup_bps) / 10000)`. Both numbers go into the entry: `amount` is the charge, and `metadata.provider_cost` is the cost to us.

## Decision

Option 2.

- The markup is stored in basis points (integer). A global default plus an optional per-organization override, read by the plugin and cached with a short TTL.
- The public model catalog in the dashboard shows prices multiplied by the same markup, so the displayed price is the charged price.
- BYOK requests (later phase) are charged a fee instead of a markup, matching OpenRouter (research brief 05). That fee is the owner's decision.

## Owner decisions still open

- The default markup (OpenRouter's model is 0% on inference with a fee on top-ups, see ADR 0003).
- Whether some organizations get a different markup.

## Consequences

- Margin per request is exact and auditable from the ledger alone.
- Bifrost's own logs and UI show provider cost, which is right for internal use.
- Pricing overrides stay available for their real purpose: correcting a wrong or missing catalog price.
