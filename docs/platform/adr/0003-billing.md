# ADR 0003: Prepaid top-ups through one-time payments

> Amended by ADR 0012: the customer types the amount (no product per amount), because the fee is a setting staff change (ADR 0010).

Status: Accepted for the model. The payment provider is the owner's decision and still open. The purchase fee is set by staff in the admin console (ADR 0010).
Date: 2026-11-21

## Context

Customers buy credits before use, as on OpenRouter (research brief 01). TurboStarter ships three relevant pieces:

- One-time orders: the `order` table and `BillingModel.ONE_TIME` plans (`packages/billing/shared/src/config/index.ts`), with a webhook per provider (Stripe; the template's other providers were removed on 2026-11-27).
- Metered billing: `recordUsage` sends meter events (Stripe `billing.meterEvents.create`, `packages/billing/web/src/providers/stripe/usage.ts`) against a subscription. The provider bills at the end of the period.
- Organizations as billing owners (`customer.referenceId` can be an organization id).

Stripe's own guidance recommends Metronome for new usage-based billing.

## Options considered

1. **Prepaid top-ups via one-time payments.** The customer pays first. A successful-payment webhook appends a `top_up` entry to the ledger (ADR 0001). Reuses TurboStarter's order flow and webhooks.
2. **Provider meters (postpaid).** Usage is sent as meter events and invoiced monthly. Rejected: it is credit risk, not prepaid. We would serve requests before knowing the customer can pay, and the ledger would duplicate the provider's meter.
3. **Metronome.** Strong for complex contracts and commits, and it supports prepaid credits. Rejected for now: a second billing system of record next to our ledger, another vendor and cost, and the request-time balance check would still have to live in our gateway. Worth revisiting when enterprise contracts with commits appear.

## Decision

Option 1.

- Top-up amounts: a fixed list of amounts first (for example $10, $25, $50, $100), each a one-time product in the provider. Custom amounts come later, because they need provider-specific "pay what you want" support.
- The ledger entry is written only from the verified webhook, keyed by the provider's payment id as `idempotency_key`, so a redelivered webhook cannot credit twice. The checkout redirect never credits.
- Refunds append a negative `refund` entry, keyed by the provider's refund id.
- Auto top-up (charge a saved card when the balance drops below a threshold) is a later phase. It needs off-session payments, which differ per provider.
- Any purchase fee (OpenRouter charges 5.5%, minimum $0.80) is added on the payment, not taken from credits, so the ledger only holds spendable money.

## Owner decisions still open

- **Which payment provider.** The design works with any provider TurboStarter supports that can do one-time payments with webhooks. Stripe is the default assumption because the other skills and docs target it.
- Whether to charge a purchase fee, and how much.
- Top-up amounts and minimum.

## Consequences

- No postpaid exposure.
- The existing subscription and metered code stays untouched and unused by the gateway.
- Revenue recognition: top-ups are a liability until spent. Reports must read the ledger (`usage` entries), not payments.
