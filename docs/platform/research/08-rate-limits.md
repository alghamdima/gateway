# Rate limits

Sources: /docs/api_reference/limits.md, /docs/api_reference/errors-and-debugging.md, /docs/faq.md (fetched 2026-11-21).

## What the customer sees

- Paid models have **no published per-customer request limit**. The docs say capacity is governed globally, and making more accounts or keys does not change rate limits.
- Limits that do exist:
  1. **Free models** (`:free`): 20 requests per minute, plus 50 per day if the account has bought under $10 in total, or 1000 per day at $10 or more.
  2. **Key management**: 10,000 key writes per day per account (see the API keys brief).
  3. **Per-IP bursts** on key and credits endpoints.
  4. Cloudflare DDoS protection.
- Spend caps behave like limits but return different errors: key credit limits and budgets give `403`, and balance or in-flight budget gives `402` (see the credits brief).
- Upstream provider rate limits are absorbed by routing to other providers where possible.

## API shape

- `429` with the standard error body `{ "error": { "code": 429, "message": ..., "metadata": {...} } }`.
- `X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset` (milliseconds since epoch) appear **only** on 429s that OpenRouter itself issues for free-model and key-management limits. Successful responses carry no rate-limit headers.
- `Retry-After` (seconds) may appear on `429`, `503`, and on in-flight-budget `402`s.
- An upstream 429 that survives every fallback carries `error.metadata.provider_code`.
- `GET /api/v1/key` returns `free_model_daily_requests` (counter and ceiling). Its `rate_limit` field is legacy and always -1.
- A rate limit hit mid-stream arrives as an SSE event with `finish_reason: "error"`.

## Observable rules

- Limits are per account, not per key.
- Free-model limits depend on lifetime purchases, which ties rate limiting to the credits ledger.
- 429s count toward a provider's routing penalty but not toward its reliability score.

## Unknown

- Whether hidden per-account limits exist for paid models (the docs imply none at the platform level beyond DDoS protection).
- The per-IP burst values.
- How the in-flight budget, which is effectively a concurrency limit for low-balance accounts, interacts with these.
