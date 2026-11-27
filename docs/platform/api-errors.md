# API errors

The reference for developers calling Lemru (SPEC API-08). It covers the inference routes of the gateway (`/v1/...`, `/openai/v1/...`, `/anthropic/v1/...`) and the control plane's `/api/v1/...`. There is no customer docs site yet; when there is, this page moves there.

## The envelope

A request the product refuses before any provider is called gets an HTTP error status and this body:

```json
{
  "error": {
    "code": 402,
    "message": "This API key has reached its spending limit. Raise the limit, wait for it to reset, or lower max_tokens.",
    "type": "credits_key_limit",
    "metadata": {
      "limit_source": "key_limit",
      "reason": "key_limit_reached",
      "remedy_hint": "Raise this key's spending limit in the dashboard, or wait for it to reset.",
      "limit": 10,
      "spent": 9.9,
      "remaining": 0.1,
      "required": 0.036882,
      "limit_reset": "monthly",
      "resets_at": "2026-12-01T00:00:00Z",
      "currency": "SAR"
    }
  }
}
```

- `error.code` is the HTTP status, as a number. `error.message` is for people and may change. `error.type` is kept for OpenAI SDKs, which read `message`, `type` and `code`.
- **Branch on `metadata.limit_source` and `metadata.reason`**, never on the message.
- Amounts are SAR, as decimal numbers, with `currency: "SAR"`.
- `Retry-After` (seconds) is sent only when the same request can succeed later; `metadata.retry_after_seconds` repeats it. No `Retry-After` means retrying the same request will not help.
- On the gateway's native routes the body also keeps the gateway's own fields beside `error` (`type`, `status_code`, `extra_fields`); clients may ignore them.
- On `/anthropic/v1/...` the body is Anthropic's envelope, `{"type":"error","error":{"type":"billing_error","message":...}}`, with the same `metadata` inside `error`. Anthropic's `type` follows the status: 401 `authentication_error`, 402 `billing_error`, 403 `permission_error`, 429 `rate_limit_error`.
- An error from a provider, or one after the response has started streaming, is not a refusal and has no `metadata`.

## Codes

| HTTP | `limit_source` | `reason`                                                       | When                                                                                                 | `Retry-After`                | Other metadata                                                        |
| ---- | -------------- | -------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------- | ---------------------------- | --------------------------------------------------------------------- |
| 401  | `key`          | `key_missing`                                                  | No API key was sent                                                                                  | no                           |                                                                       |
| 401  | `key`          | `key_invalid`                                                  | The key does not exist, was deleted, or the header is malformed                                      | no                           |                                                                       |
| 401  | `key`          | `key_disabled`                                                 | The key is disabled; it can be enabled again                                                         | no                           |                                                                       |
| 401  | `key`          | `key_expired`                                                  | The key has expired; create a new one                                                                | no                           |                                                                       |
| 402  | `credits`      | `insufficient_balance`                                         | The organization has no credit left                                                                  | no                           | `balance`, `held`, `available`, `required`                            |
| 402  | `credits`      | `request_exceeds_balance`                                      | The most this one request can cost is more than the whole balance; add credits or lower `max_tokens` | no                           | as above                                                              |
| 402  | `credits`      | `balance_held_by_requests_in_flight`                           | The balance covers the request once requests still running finish                                    | yes                          | as above                                                              |
| 402  | `key_limit`    | `key_limit_reached`                                            | The key's spending limit for its window is used up                                                   | no                           | `limit`, `spent`, `remaining`, `required`, `limit_reset`, `resets_at` |
| 402  | `key_limit`    | `request_exceeds_key_limit`                                    | The most this one request can cost is more than the key's whole limit                                | no                           | as above                                                              |
| 402  | `budget`       | `budget_exceeded`                                              | A gateway budget is spent (not used for money today)                                                 | no                           |                                                                       |
| 403  | `data_policy`  | `provider_not_allowed`                                         | The organization's data policy allows no endpoint of this provider                                   | no                           |                                                                       |
| 403  | `data_policy`  | `model_not_allowed`                                            | The data policy does not allow this model                                                            | no                           |                                                                       |
| 403  | `request_type` | `request_type_not_enabled`                                     | The request type (speech, images, ...) is not switched on yet                                        | no                           | `request_type`                                                        |
| 403  | `key`          | `key_has_no_organization`                                      | The key belongs to no organization                                                                   | no                           |                                                                       |
| 400  | `credits`      | `cost_cannot_be_bounded`                                       | The request uses a feature whose cost cannot be bounded before it is sent (the message names it)     | no                           |                                                                       |
| 400  | `credits`      | `model_not_priced`                                             | The model has no price                                                                               | no                           |                                                                       |
| 400  | `request_type` | `request_empty`                                                | The request has no body                                                                              | no                           |                                                                       |
| 429  | `rate_limit`   | `rate_limited`, `request_limit_reached`, `token_limit_reached` | The organization is over its rate limit                                                              | yes, until the window resets | `limit_kind`, `limit`, `used`, `window`, `resets_at`                  |
| 503  | `service`      | `ledger_unavailable`                                           | Credits or prices could not be read; temporary                                                       | yes                          |                                                                       |
| 503  | `service`      | `credits_not_configured`                                       | The gateway is misconfigured; retrying will not fix it                                               | no                           |                                                                       |
| 503  | `service`      | `catalog_unavailable`                                          | `GET /api/v1/models` could not reach the model catalog                                               | yes                          |                                                                       |

A refusal names only the caller's own key and organization; it never carries another customer's data or a provider key.

## Checking a key before calling

`GET /api/v1/key` with the key itself (`Authorization: Bearer <key>`) returns its `label`, `name`, `limit`, `limit_remaining`, `limit_reset` (`daily`, `weekly`, `monthly` or null), `limit_resets_at`, `usage` (all time, and `usage_daily`, `usage_weekly`, `usage_monthly`), `expires_at` and `currency: "SAR"`. Windows are UTC calendar days, ISO weeks (from Monday) and calendar months. A refused key gets the 401s above.

## Where it is built

- The envelope: `apps/gateway/core/schemas/refusal.go` (`Refusal`, `NewRefusalError`), written by `ErrorField.MarshalJSON` (`core/schemas/bifrost.go`); `Retry-After` by `lib.ApplyRefusalHeaders` (`transports/bifrost-http/lib/responseheaders.go`); the Anthropic shape in `core/providers/anthropic/errors.go`.
- Credits, key limits, request types, the ledger: `apps/gateway/plugins/credits/refusals.go`.
- Keys, rate limits, data policy, budgets: `apps/gateway/plugins/governance/main.go` (`refusal`, `defaultRefusal`, `unusablePermit`) and `store.go` (`RateLimitError`).
- The control plane: `packages/api/src/modules/v1/router.ts` and `auth.ts`.
- A new `limit_source` or `reason` is added to this page in the same change.
