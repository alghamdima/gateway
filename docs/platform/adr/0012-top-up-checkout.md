# ADR 0012: Top-up checkout and the payment webhook

- Status: Accepted (amount limits confirmed by the owner on 2026-11-23)
- Date: 2026-11-23
- Builds on: ADR 0001 (ledger), ADR 0003 (prepaid top-ups), ADR 0010 (fee set by staff)

## Context

ADR 0003 planned a fixed list of amounts, each a one-time product in Stripe. Since then the fee became a setting staff change in the admin console (ADR 0010), so the total a customer pays is only known when the checkout starts. A product per amount cannot carry a fee that changes, and it would need products created in every Stripe account (sandbox, live) before the page works.

## Decision

1. **Amounts.** The customer types an amount in whole cents, with quick picks of $10, $25, $50 and $100. Limits: at least $5 and at most $1,000 per payment (constants in `packages/api/src/modules/credits/top-up.ts`). This replaces the fixed list in ADR 0003.
2. **Checkout.** A Stripe Checkout Session in `payment` mode with two inline lines (`price_data`, USD): the credits, and the fee from `computeTopupFee` with the settings read at that moment. No promotion codes. The session and its payment intent carry metadata `purpose=credits_top_up`, `organizationId`, `creditMicroUsd`, `feeMicroUsd`. An invoice is created for the receipt. The organization's Stripe customer is reused (the same one the subscription code uses).
3. **Who may pay.** Members with `billing:create` (organization admins and owners).
4. **Crediting.** Only the signed webhook credits, never the redirect. `checkout.session.completed` with `payment_status=paid`, and `checkout.session.async_payment_succeeded`, append one `top_up` entry of `creditMicroUsd`, keyed `top_up:stripe:<payment intent id>`, so both events and every redelivery credit once. If the amount Stripe collected is not exactly credit plus fee, nothing is credited and the event is logged as an error for staff to resolve with an adjustment: that can only mean a bug or a change made in the Stripe dashboard.
5. **Refunds.** `refund.created` and `refund.updated` with `status=succeeded` append one `refund` entry keyed `refund:stripe:<refund id>`. The credit removed is the refunded share of the payment: `floor(credit × refunded / total)`, so a full refund removes exactly what was credited and the fee share is never removed as credit. Refunds of payments that were not top-ups are ignored. The balance can go below zero when the credits were already spent; the next request is then refused like any empty balance.
6. **TurboStarter's order handler** is skipped for top-up sessions. It looks for the first line's price among the configured plans and would fail on an inline price.
7. **Deleted organizations.** A payment or refund for an organization that no longer exists is logged and acknowledged, so Stripe does not retry it for days. The payment record stays in Stripe.

## Consequences

- No Stripe products to create per environment; a new Stripe account only needs the webhook endpoint and its events: `checkout.session.completed`, `checkout.session.async_payment_succeeded`, `refund.created`, `refund.updated`.
- A fee change applies to checkouts started after it; a checkout already open keeps the fee it showed.
- Not built: refunds within 24 hours on request, credit expiry, auto top-up, tax. Tax needs Stripe Tax registrations first (see the Stripe skill's tax reference).
