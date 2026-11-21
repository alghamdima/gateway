# Model catalog and pricing display

Sources: live `GET https://openrouter.ai/api/v1/models` (fetched 2026-11-21), /docs/guides/overview/models.md, /docs/api/api-reference/endpoints/list-all-endpoints-for-a-model.md.

## What the customer sees

- A models browser listing each model with name, author, context length, modalities, and prices shown per million tokens (images, requests and web search per unit).
- A per-model page listing every provider endpoint serving it, with that endpoint's price, context, max output, quantization, latency and throughput over the last 30 minutes, and uptime.
- Sorting by price, and filtering by modality and supported parameter.
- Model variants as suffixes: `:free` and `:batch` are separate catalog entries with their own prices. `:nitro`, `:floor` and `:exacto` change routing only. `:online` is deprecated.
- Router pseudo-models (`openrouter/auto`, `openrouter/auto-code`, and others) that pick a model for you.

## API shape

`GET /api/v1/models` is public, with no key needed. Snapshot on 2026-11-21:

- 477 models from 70 authors (openai 106, google 49, qwen 43, anthropic 32, mistralai 22).
- Fields per model: `id`, `canonical_slug`, `name`, `created`, `description`, `context_length`, `architecture {modality, input_modalities, output_modalities, tokenizer, instruct_type}`, `pricing`, `top_provider {context_length, max_completion_tokens, is_moderated}`, `per_request_limits`, `supported_parameters`, `default_parameters`, `reasoning {mandatory, default_enabled, supported_efforts, default_effort}`, `endpoints` (which API surfaces accept it), `expiration_date`, `knowledge_cutoff`, `hugging_face_id`, `links.details`, `supported_voices`.
- `pricing` keys and how many models have each: `prompt` 477, `completion` 477, `input_cache_read` 340, `web_search` 198, `input_cache_write` 117, `overrides` 88, `audio` 38, `internal_reasoning` 36, `input_cache_write_1h` 36, `image` 35, `input_audio_cache` 33, `image_output` 10, `audio_output` 2.
- **Prices are decimal strings in USD per single token or unit**, for example `"0.00000015"`. The smallest non-zero unit price seen is `1.014E-9` USD (10 decimal places). The largest is `0.018`.
- `"-1"` marks variable pricing on router pseudo-models (5 of them).
- `"0"` means free. 15 models are fully free.
- `pricing.overrides[]` holds conditional prices, for example `{"min_prompt_tokens": 272000, "prompt": "0.000016", ...}` for long context, plus peak and off-peak time windows. Later entries win per key, and missing keys inherit the base price. Clients are told to skip entries containing condition fields they do not recognize.
- The default response contains only text-output models. Other modalities need an explicit `output_modalities` filter or `all`.
- `GET /api/v1/models/{author}/{slug}/endpoints` lists endpoints. Each has `name`, `model_id` (permaslug), `context_length`, `max_completion_tokens`, `max_prompt_tokens`, `pricing`, `latency_last_30m {p50,p75,p90,p99}` (authenticated only), `perf_last_30m_by_workload`, and `native_tools`.
- `GET /api/v1/models/count` and a user-filtered list (filtered by the account's provider preferences, privacy settings and guardrails) also exist.

## Observable rules

- The catalog price is the price of the **top provider**. Per-endpoint prices differ and live on the endpoints route.
- The charge for a request uses the price of the endpoint that actually served it (see the routing brief).
- `id` is a stable slug. `canonical_slug` is dated (`...-20261120`) and pins a snapshot.

## Consequences for our design (not OpenRouter facts)

- A per-token price needs at least 10 decimal places of USD precision. An integer money unit of cents, or even micro-dollars (1e-6), cannot hold a unit price. Prices must be stored as exact decimals (or integers of a much smaller unit), and only the per-request total gets rounded to the ledger unit.

## Unknown

- How they reconcile provider price changes with requests already in flight.
- Where the per-endpoint latency and uptime figures come from (guess: their own request logs, over the 30-minute windows shown).
