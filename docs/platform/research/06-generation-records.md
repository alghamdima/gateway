# Per-request generation records

Sources: /docs/api/api-reference/generations/*, /docs/api/api-reference/chat/create-a-chat-completion.md (usage schema), `openrouter-generations` skill (fetched 2026-11-21).

## What the customer sees

- Every completion has an id (`gen-...`). A Logs page lists individual generations. The Activity page aggregates them.
- For each generation: model, provider, tokens, cost, latency, finish reason, whether it streamed, whether BYOK was used, and the full chain of provider attempts.
- Prompt and completion text are not stored by default. The customer can opt in to logging, which earns a 1% usage discount. ZDR requests never have content.
- Feedback can be submitted on a generation.

## API shape

Inline in every completion response, `usage`:

```
prompt_tokens, completion_tokens, total_tokens,
prompt_tokens_details { cached_tokens, cache_write_tokens, audio_tokens, video_tokens },
completion_tokens_details { reasoning_tokens, audio_tokens, accepted_prediction_tokens, rejected_prediction_tokens },
cost,                       // USD, number, what the customer is charged
is_byok,
cost_details { upstream_inference_cost, upstream_inference_prompt_cost, upstream_inference_completions_cost, server_tool_cost },
server_tool_use_details { web_search_requests, tool_calls_requested, tool_calls_executed }
```

`GET /api/v1/generation?id=` (any API key) returns the stored record. Main fields:

- Identity: `id`, `request_id` (groups all generations of one API request), `session_id`, `upstream_id`, `created_at`.
- Routing: `model`, `provider_name`, `router`, `service_tier`, `data_region`, `provider_responses[]` (every attempt, including fallbacks), `is_byok`.
- Tokens: `tokens_prompt`, `tokens_completion` (normalized) and `native_tokens_prompt`, `native_tokens_completion`, `native_tokens_reasoning`, `native_tokens_cached`, `native_tokens_completion_images` (as the provider reported them).
- Money: `total_cost`, `usage`, `upstream_inference_cost`, `cache_discount`.
- Timing: `latency`, `generation_time`, `moderation_latency` (ms).
- Outcome: `finish_reason`, `native_finish_reason`, `cancelled`, `streamed`.
- Attribution: `app_id`, `origin`, `http_referer`, `user_agent`, `external_user`, `workspace_id`, `preset_id`, `api_type` (`completions`, `embeddings`, `image`, `tts` and others).

`GET /api/v1/generation/content?id=` returns stored prompt and completion. `POST` feedback exists.

## Observable rules

- Cost is returned inline, so a client needs no second call to know what it paid.
- Two token counts coexist: normalized (OpenRouter's count) and native (the provider's). Native counts are what the provider bills on (guess, based on the naming and on "we receive the total number of tokens processed by the provider" in the FAQ).
- The record exists for failed fallback attempts too, inside `provider_responses`.
- A response served from the response cache points at the original generation via `response_cache_source_id`.

## Unknown

- How long records are kept.
- How soon after a response the record is queryable (guess: seconds, since cost is written at the end of the request).
- Whether `usage` and `total_cost` ever differ, and why both exist.
