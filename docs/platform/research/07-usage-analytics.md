# Usage analytics

Sources: /docs/guides/features/activity.md, /docs/api/api-reference/analytics/*, /docs/api/api-reference/analytics/get-user-activity-grouped-by-endpoint.md, `openrouter-analytics-schema` skill (fetched 2026-11-21).

## What the customer sees

An Activity page with four tabs:

- **Overview**: total spend, requests, tokens, blended cost per million tokens, cache hit rate. Top API keys, apps and users. Charts by model, token type (prompt, completion, reasoning), caching, and credits versus BYOK.
- **Trends**: time series with a "trending" card for the biggest movers versus the prior period.
- **Explore**: pick a metric, group by up to 2 dimensions, choose a time rollup, bar/line/dot chart. Configurations can be saved and shared within the organization.
- **Guardrails**: guardrail activity.

Filters live in the URL and are shared across tabs. Results export to CSV or PDF.

## API shape

- `GET /api/v1/analytics/meta` lists the available metrics, dimensions, operators and granularities. The list is live, so clients should not hardcode it.
- `POST /api/v1/analytics/query`. Body: `metrics`*, `dimensions` (max 2), `filters` (`eq`, `neq`, `in`, `not_in`, `gt`, `gte`, `lt`, `lte`), `granularity` (`minute`, `hour`, `day`, `week`, `month`), `time_range {start, end}` (ISO 8601 with seconds), `order_by`, `limit` (default 1000), `group_limit`. Response: `data[]` plus `metadata {query_time_ms, row_count, truncated}` and `warnings`.
- `GET /api/v1/activity` returns daily rows for the last 30 completed UTC days, grouped by model endpoint: `date`, `model`, `model_permaslug`, `endpoint_id`, `provider_name`, `requests`, `prompt_tokens`, `completion_tokens`, `reasoning_tokens`, `cached_tokens`, `usage`, `byok_usage_inference`. Filters: `date`, `api_key_hash`, `user_id`, `workspace_id`, `group_by=workspace`.
- All of these need a management key.

Metric families (from the schema skill):

- Volume: `request_count`, `tokens_*`, `reasoning_tokens`, `cached_tokens`, `byok_request_count`.
- Cost: `total_usage` (credits + BYOK inference), `credits_usage`, `byok_usage`, `byok_fees`, `usage_upstream`, `usage_cache`, `usage_web`, `blended_cost_per_million_tokens`.
- Performance: `avg/p50/p90/p95/p99` of latency, router latency, time to first token, generation time, inter-token latency, throughput.
- Efficiency: `cache_hit_rate`, `response_cached_rate`, `guardrail_invoked_rate`.

Dimensions include `model`, `variant`, `api_key_id`, `user`, `workspace`, `app`, `provider`, `origin`, `country`, `data_region`.

## Observable rules

- Volume and cost metrics allow ranges up to 367 days at day granularity or coarser. Latency metrics and per-generation dimensions are limited to 31 days. Exceeding a limit returns `400` with the maximum in the message.
- Some dimensions are resolved to labels (key name, app title, user email, workspace name). Sentinels: `api_key_id = -1` is "Chatroom", and `app = -1` is "Unknown".
- Rate metrics (`is_rate`) are averaged, not summed.
- The 367 and 31 day limits plus "hourly rollup only" for some metrics suggest pre-aggregated rollup tables over a raw generation store (guess).

## Unknown

- How fresh the data is (lag between a request and its appearance).
- The storage engine (guess: a columnar store such as ClickHouse, given the query shape and the rollup limits).
