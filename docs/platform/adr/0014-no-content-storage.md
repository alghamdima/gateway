# ADR 0014: Prompts and completions are not stored; request logs are kept 90 days

- Status: Accepted (90 days is a placeholder until the owner sets the retention)
- Date: 2026-11-24
- Relates to: ADR 0007 (gateway internal only), research brief 06 (generation records)

## Context

The gateway's logging plugin stores every request's prompt and completion by default (`disable_content_logging: false`) and keeps logs for 365 days. Measured on the local gateway: the log store held the user's message and the model's answer in `input_history`, `output_message` and `content_summary`. Our deployment config set none of this.

OpenRouter stores no prompt or completion text by default; customers may opt in (for a 1% discount), and zero-data-retention requests never have content (brief 06).

## Decision

1. **No content by default.** The shipped `client` block (`apps/gateway/compose/config.json`) sets `disable_content_logging: true`. The log row keeps what usage and support need: model, provider, status, tokens, cost, latency, the key and the customer. Prompts, completions, the content summary and raw bodies are not written.
2. **Customers cannot turn it back on per request.** `allow_per_request_content_storage_override` and `allow_per_request_raw_override` are `false`, so the `x-bf-disable-content-logging` and `x-bf-store-raw-request-response` headers are ignored. The public edge (step 8.1) will also strip `x-bf-*` headers.
3. **Retention: 90 days** (`log_retention_days`). The gateway's cleaner deletes older log rows. Money is not affected: charges live in our ledger, which is never deleted (ADR 0001). The owner sets the final number.
4. **Proven as shipped.** `plugins/credits/privacy_e2e_test.go` boots the built gateway with that `client` block, sends a request that asks for its content to be kept, and reads the log through the gateway's API: no prompt, completion, summary or raw body, and the retention in force is 90 days. Mutations of the three settings each fail it.

## Consequences

- Support cannot read what a customer sent; they see the request's metadata and the error.
- Phase 9 (activity and generation detail) is built on metadata only.
- An opt-in for customers who want their content stored, if the owner wants one, can use the gateway's per-key `disable_content_logging`, which overrides the global setting for that key.
- `pnpm gateway:dev` starts the gateway with no config file, so a developer's local gateway still stores content. It never sees customer traffic.
