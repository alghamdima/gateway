# Credits and top-ups

Sources: openrouter.ai/docs/faq.md, /docs/api_reference/limits.md, /docs/api/api-reference/credits/get-remaining-credits.md (fetched 2026-11-21).

## What the customer sees

- A prepaid balance in US dollars. Every price on the site and API is in USD.
- A Credits page: buy credits manually, or turn on auto top-up, which recharges when the balance drops below a threshold the customer sets.
- A purchase fee: 5.5% with a $0.80 minimum on card purchases, 5% on crypto (USDC). Inference itself is sold at the provider's list price with no markup. The fee is where the money is made.
- A refund button for unused credits, available for 24 hours after a purchase. After that, unused credits are not refundable.
- Credits may expire one year after purchase (the terms reserve the right).
- Card payments go through Stripe. The FAQ admits that credits can show up "up to one hour" late, which suggests the credit is applied from an asynchronous webhook rather than inline with checkout.
- Payment methods: cards, AliPay, USDC.
- New accounts get a small free allowance.

## API shape

`GET /api/v1/credits` (management key only):

```json
{ "data": { "total_credits": 100.5, "total_usage": 25.75 } }
```

The balance is `total_credits - total_usage`. The API returns two running totals, not a balance field. Values are JSON floats (`format: double`).

The deprecated `POST /api/v1/credits/coinbase` shows that purchases were once possible through the API. No current endpoint buys credits through the API.

## Observable rules

- A request is charged when it finishes (limits.md: "OpenRouter charges a request when it finishes").
- **In-flight spending budget.** Before a paid request reaches a provider, its maximum cost is estimated: input tokens plus `max_tokens`, or a fixed per-request cap when `max_tokens` is not set, priced at the endpoint's rates. Running and recently finished requests together cannot hold more than a fraction of the balance, up to a fixed ceiling. This is a reservation scheme. It applies only to accounts below a balance threshold and to newer accounts.
  - Rejection: `402` with `error.metadata.limit_source = "openrouter_in_flight_budget"`, `reason = "in_flight_budget_exhausted"`, and a `Retry-After` header.
  - A single request bigger than the whole budget gets `402` with `limit_source = "openrouter_credits"` and `reason = "weight_exceeds_budget"`.
- A negative balance is possible: "If your account has a negative credit balance, you may see 402 errors, including for free models." Final cost can therefore exceed the estimate, and the overrun is allowed to drive the balance below zero.
- Every `402` that carries `limit_source` also carries a `remedy_hint` string for humans.
- Free-model rate limits depend on lifetime purchases: under $10 purchased gives 50 requests per day, $10 or more gives 1000 per day.

## Unknown

- The fraction and ceiling of the in-flight budget, the balance threshold above which it stops applying, and the per-request cap used when `max_tokens` is missing. None are published.
- Whether auto top-up runs as a charge on a saved card (guess: Stripe off-session PaymentIntent) and how failures are retried.
- How the expiry of credits is applied: per purchase lot (FIFO) or on the whole balance.
- Internal storage of money. The API exposes floats. Whether they store integers internally is unknown (guess: no public evidence either way).
- How refunds interact with credits already spent from that purchase.
