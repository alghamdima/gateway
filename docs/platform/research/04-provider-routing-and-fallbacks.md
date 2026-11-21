# Provider routing and fallbacks

Sources: /docs/guides/routing/provider-selection.md, /docs/guides/providers/traffic-routing.md, /docs/guides/routing/model-fallbacks.md (fetched 2026-11-21).

## What the customer sees

- Most models are served by several providers. By default OpenRouter picks one for each request and silently retries another provider on failure.
- The customer can steer this per request with a `provider` object, per workspace with a routing default, or with model suffixes (`:nitro` sorts by throughput, `:floor` by price).
- Model-level fallback: send `models: [...]` and OpenRouter tries the next model when one fails.

## API shape

`provider` object on `/chat/completions` and `/responses`:

| Field                                                | Type                                   | Default | Meaning                                                                                   |
| ---------------------------------------------------- | -------------------------------------- | ------- | ----------------------------------------------------------------------------------------- |
| `order`                                              | string[]                               | -       | Provider slugs to try in order                                                            |
| `allow_fallbacks`                                    | bool                                   | true    | Allow providers outside `order`                                                           |
| `require_parameters`                                 | bool                                   | false   | Only providers that support every request parameter                                       |
| `data_collection`                                    | `allow`/`deny`                         | allow   | Exclude providers that may store data                                                     |
| `zdr`                                                | bool                                   | -       | Only zero-data-retention endpoints                                                        |
| `enforce_distillable_text`                           | bool                                   | -       | Only models whose licence allows distillation                                             |
| `only` / `ignore`                                    | string[]                               | -       | Allowlist / denylist of providers                                                         |
| `quantizations`                                      | string[]                               | -       | e.g. `["fp8"]`                                                                            |
| `sort`                                               | string or `{by, partition}`            | -       | `price`, `throughput`, `latency`. `partition: "none"` sorts across all models in `models` |
| `preferred_min_throughput` / `preferred_max_latency` | number or `{p50,p75,p90,p99}`          | -       | Soft performance thresholds                                                               |
| `max_price`                                          | `{prompt, completion, request, image}` | -       | USD per million tokens. Endpoints above it are excluded                                   |

Model fallback: `models: ["a", "b"]`. On `/api/v1/messages`, `fallbacks: [{model}]` (at most 3, cannot be combined with `models`, per-entry overrides rejected with 400).

The response `model` field names the model that actually served the request. Opt-in headers (`X-OpenRouter-Metadata`) return routing details such as the selected provider and fallback attempts. The generation record keeps `provider_responses[]` with every attempt.

## Observable rules

- **Default routing** weights providers by effective price (cache-adjusted, over the last 30 minutes), end-to-end throughput, time to first token, 429 rate, error rate and tool-call quality. The weighted pick is random. Providers far behind still get a small exploration share.
- A **breaker** stops traffic to a provider whose error or 429 rate is far worse than its peers until it recovers. Bursts of 429s or latency spikes briefly lower a provider's share.
- Errors that count against a provider: 5xx, timeouts and dropped connections, 401/402/404/405/408/410/421/423/424/426/498, and failures partway through. Errors that do not count: 400/413/422, 403/451, 499, and 429 (tracked separately).
- Follow-up requests stick to the same provider while its prompt cache is warm.
- Requests with `tools` go to providers known to support tools. `max_tokens` excludes providers that cannot output that much.
- Default routing is bypassed by `sort`, `order`, the `:nitro`/`:floor` suffixes, an account default sort, or BYOK.
- Model fallback triggers on any error, including context-length errors, moderation refusals, rate limits and downtime. If the last fallback fails, that error is returned.
- **Billing follows the model that actually served the request.** A `:free` model followed by a paid model bills at the paid price when it falls back.
- A failure after streaming has started arrives as an SSE event with `finish_reason: "error"`, because the HTTP status was already sent as 200.
- Status codes: `502` when the chosen model is down, `503` when no provider meets the routing requirements.

## Unknown

- The exact weights, the exploration share and the breaker thresholds.
- How many provider retries happen per request before giving up.
- Whether a failed provider attempt ever incurs a charge to the customer (the docs only say billing follows the serving model, guess: failed attempts are free).
