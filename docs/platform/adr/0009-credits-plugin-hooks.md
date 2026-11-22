# ADR 0009: The credits plugin reserves and settles per attempt

Status: Accepted
Date: 2026-11-22

## Context

ADR 0001 says the plugin reserves "once per request" in `HTTPTransportPreHook` and settles "after the request ends". Reading the gateway's plugin contract (`apps/gateway/core/schemas/plugin.go`) and the governance plugin before writing any code showed that this does not fit how the hooks run:

- `HTTPTransportPreHook` and `HTTPTransportPostHook` run once per request. `PreLLMHook` and `PostLLMHook` run **once per attempt**: the primary provider call, then once per fallback. Retries against the same provider happen inside one pair.
- The organization is not known in `HTTPTransportPreHook`. The governance plugin sets `BifrostContextKeyGovernanceCustomerID` when it evaluates the virtual key in its own `PreLLMHook`.
- The model that will serve the request is not final until `PreRequestHook` and the routing plugins have run, and each fallback attempt has its own model and price.
- The pipeline guarantees symmetry: for every `PreLLMHook` that ran, the matching `PostLLMHook` is called, with a response or an error. For a stream, `PostLLMHook` is called for every chunk and `IsFinalChunk` marks the last one.
- `HTTPTransportPostHook` is documented as unreliable for streamed responses, so it cannot be the place that releases a hold.

## Options considered

1. **One reservation per request**, taken in `HTTPTransportPreHook` for the most expensive of the listed models, released in `HTTPTransportPostHook`. Rejected: the organization is unknown there, the release point is unreliable for streams, and the hold would have to guess across fallbacks.
2. **One reservation per attempt**, taken in `PreLLMHook` and settled in the matching `PostLLMHook`. Attempts run one after another, so an organization holds at most one reservation per request at a time, sized for the model actually being tried.
3. **No reservation**, only a balance check. Rejected by ADR 0001: concurrent requests overdraw.

## Decision

Option 2.

- The plugin is registered after governance, so the customer id is on the context when its `PreLLMHook` runs.
- **Reserve** (`PreLLMHook`): estimate the maximum cost of this attempt and run one statement that raises `held` only if `balance - held` covers it, and inserts the reservation row in the same statement. No row means the request is refused with status 402 and `AllowFallbacks = false`, before any provider call.
- **Settle** (`PostLLMHook`; for a stream, on the final chunk only): one statement marks the reservation settled, appends the `usage` entry if the attempt cost anything, and moves `balance` and `held`. The idempotency key is `billing_nonce:request_id:fallback_index:attempt_number`, the tuple governance uses. Replaying it changes nothing.
- An attempt that fails without billable usage releases its hold and writes no entry. An attempt that fails after producing billable usage (`BilledUsage` on the error) is charged for that usage. This answers the open question in ADR 0002 the way the provider bills us.
- **Estimate**: the catalog's own cost function applied to a hypothetical usage, so pricing tiers are honoured. Output tokens are the request's own limit, or the model's maximum output when the request sets none. Input tokens are estimated from the size of the request. The estimate is a ceiling for output and a guess for input, so the balance can still end slightly negative, as ADR 0001 accepts.
- **Fail closed.** No organization on the context, no price for the model, or the ledger database unreachable: the request is refused. A gateway that cannot account for a request does not serve it.
- **Reaper**: a reservation past `expires_at` is released by a periodic statement. The default lifetime is 15 minutes, longer than a normal stream. A stream that outlives it keeps running and is still charged at settlement; only its hold is gone.
- **Markup**: one global value in basis points from the plugin's config. The per-organization override in ADR 0005 is deferred until there is a place to store it.

## Consequences

- A request with fallbacks reserves again for each attempt, each time against the current balance. A fallback can therefore be refused with 402 after the primary failed, if the fallback model is more expensive than the balance allows.
- A request that sets no output limit on a model with a large maximum output needs a balance that covers that maximum. Customers with small balances will see 402 on such requests and need to set `max_tokens`. If this proves too strict, the estimate can be capped by config; that trades fewer refusals for a larger possible overdraft and is the owner's call.
- The plugin opens its own connection to the control-plane database with the `gateway_ledger` role. It needs no access to the gateway's own stores.
- Everything in ADR 0001 about the tables, the unit and the append-only ledger stands. Only where the two statements run has changed.
