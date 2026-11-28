# Plan: the model page as OpenRouter's, with everything behind it

Status: **in progress**. The owner approved starting with stage 1 on 2026-11-28. Stage 1 is on staging; stage 2 is next.

Source: 23 screenshots of OpenRouter's model page the owner took on 2026-11-28 (`docs/platform/research/openrouter/model-page-2026-11-28/`), and the earlier captures (`research/openrouter/openrouter-flows.md` §2 and §3, `openrouter-flows-personal-2.md` §6).

Rule for this work (owner, 2026-11-28): no temporary solutions. Each stage is built on the structure the later stages need, so nothing is thrown away.

## What the page shows, and what each part needs

| Part of the page                                                                                           | Needs                                                                                         | SPEC                       |
| ---------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------- | -------------------------- |
| Header: name, copyable id, description, modalities, price, context, released                               | Catalog fields the gateway has and does not return yet                                        | CAT-08, CAT-10             |
| Section menu (Providers, Pricing, Performance, Uptime, Apps, Activity, API)                                | UI only                                                                                       | CAT-08                     |
| Providers: one row per endpoint with prices, latency, throughput, uptime                                   | Endpoints as a concept; hourly statistics                                                     | CAT-09, CAT-11             |
| Routing mode selector: Standard, Nitro, Floor, Batch                                                       | Several endpoints per model; routing modes; the batch API                                     | ROUTE-01, ROUTE-02, batch  |
| Service tiers (Fast, Ultrafast) and regional endpoints                                                     | Endpoints keyed by tier and region; `service_tier` passed through                             | ROUTE-01                   |
| Pricing: weighted average actually paid, listed and effective price history, token share per endpoint      | Price versions (have), per-request input and output cost and tokens (have in the gateway log) | CAT-12                     |
| Performance: throughput, latency to first token, end-to-end latency, cache hit rate, by endpoint over time | Hourly statistics from the gateway log                                                        | CAT-11                     |
| Tool call error rate, structured output error rate                                                         | The gateway must classify these two outcomes per request                                      | new                        |
| Location filter                                                                                            | The gateway must record the caller's region                                                   | new                        |
| Uptime and availability (3 days)                                                                           | Hourly statistics                                                                             | CAT-11                     |
| Apps using the model                                                                                       | App attribution from request headers                                                          | new                        |
| Activity: prompt, reasoning and completion tokens per day                                                  | Hourly statistics; a staff switch for showing volume publicly                                 | new                        |
| API: key, first request in Python, TypeScript and cURL, parameters                                         | Have the snippets (onboarding); `supported_parameters` from the catalog                       | CAT-08, CAT-10             |
| Try (opens the playground with the model)                                                                  | The playground                                                                                | PLAY-01 to PLAY-06, CAT-13 |
| Compare                                                                                                    | The compare page                                                                              | CAT-07                     |
| Inline playground on the page                                                                              | The playground                                                                                | PLAY-01                    |

## Stages

Each stage is usable on its own and ends on staging, checked in a browser. Sizes: S a day or less, M a few days, L a week or more.

### Stage 1: the data underneath (M)

Nothing visible on its own; every later stage reads from it.

1. **Endpoint as a first-class thing.** An endpoint is one way a model is served: provider account, the provider's model name, region, service tier, its own price. Today every model has exactly one. The table is built for many (`model_endpoint`), keyed so that stage 5 adds rows and changes nothing else.
2. **Hourly statistics per endpoint** (`model_endpoint_stat_hourly`): requests, successes, failures by class, prompt, completion, reasoning and cached tokens, input and output cost, and latency, time to first token and throughput as percentile sketches. A job fills it from the gateway log, idempotent per hour, so a page never scans the log. The gateway log's retention no longer limits how far back a chart goes.
3. **Catalog fields** the gateway already holds: description, supported parameters, expiration date, default parameters, returned by `/api/models/details` and by `GET /api/v1/models` (CAT-10, API-02).
4. **Staff settings** (rule 6): the least number of requests before a figure is shown (start: 20), and whether traffic volume is public.

Done when: the job has filled a week of hours on staging and re-running an hour changes nothing; the API returns the new fields; both settings are in the admin with history.

**Built 2026-11-28, on staging (migration 0046).** What changed from the list above while building:

- No `model_endpoint` table. An endpoint is the tuple (provider, model, provider key, service tier): the key's region and data terms are already in `provider_endpoint` and its price in `model_price`, so a table of its own would only repeat them. Stage 5 adds the table that maps a canonical model to its endpoints; nothing built here changes for it.
- The gateway counts and the control plane keeps: `GET /api/logs/endpoints/hourly` (gateway, `framework/logstore/endpointstats.go`) answers per hour and endpoint with attempts, answers, tokens, cost split into input and output, and three histograms on 64 fixed buckets; the job (`modules/catalog/stats-job.ts`, timer `endpoint-stats` every 5 minutes) replaces whole hours in `model_endpoint_stat_hourly`. Histograms, not percentiles, because the counts of two hours or two endpoints add up.
- Found in real data and handled in the gateway: a failed request has no selected key (core keeps it for answers only), so it is counted on the key of its attempt trail; and the provider's standard tier arrives as nothing, `default` or `auto`, which are one endpoint.
- Item 3 moved to stage 2 and changed: **the gateway has no description, supported parameters or modalities for a model served by its own provider** (checked on staging: OpenAI's model list gives only id, creation time and owner; the datasheet adds context and prices). These come from us: a staff-edited record per model (display name, description, modalities, release date), built with the page.
- Settings: `/admin/catalog-settings` (least answered requests for a figure, 20 to start; traffic volume public, off to start), with history and audit.
- The price unit (tokens, characters, seconds, items) is not built yet: nothing shows a non-token price until the page lists such a model. It is part of stage 2's header and price card.
- Owner's part: staging has no data-terms row for the OpenAI key (`/admin/providers`), so the page cannot show its region until one is entered.

### Stage 2: the model page (M)

The page from the screenshots, from stage 1's data: header with description, section menu, providers table, pricing (weighted average, listed and effective history, token share), performance charts, cache hit rate, uptime and availability, API section in three languages with the parameters. Prices in SAR per 1M with USD beside (CAT-08). A figure with too little data says so, as OpenRouter does. `GET /api/v1/models/{author}/{slug}/endpoints` returns the same figures (API-03).

Left out until their stage: the routing mode selector, service tiers, regional endpoints, Try, Compare, the inline playground, Apps, the two error-rate charts, the location filter. No dead buttons.

Done when: the page matches the screenshots section by section for one endpoint, on a phone and a desktop width, and every number on it can be traced to the statistics table.

### Deferred by the owner (2026-11-28): the playground with Try, and compare

Try (PLAY-01 to PLAY-06, CAT-13), the inline playground on the page, and the compare page (CAT-07) come later, in an order the owner will give. Their buttons are not on the page until then.

### Stage 5: one model, several providers (L)

The routing project (ROUTE-01, -02, -03, -05, API-07). A canonical model id maps to several endpoints; a request is served by one of them, chosen by mode, within the customer's data policy, and falls back to the next on failure. Each endpoint keeps its own price; the ledger and the log record which one served. Modes: Standard (balanced), Nitro (fastest), Floor (cheapest), per request and as an organization default (ROUTE-06). The page gains the mode selector, more rows, and one line per endpoint in every chart.

Needs from the owner: a second provider account that serves a model we already have (for example Azure OpenAI), or there is nothing to route between.

Done when: the same model id answers from either endpoint; each mode picks the endpoint its rule names (test); a failed endpoint falls back; the charge is the serving endpoint's price.

### Stage 6: service tiers and regional endpoints (M)

Endpoints by tier (`service_tier`: fast, ultrafast, at their own prices) and by region, shown as their own groups on the page. Region ties into the data policy we already have.

### Request types: what is built and what each needs (checked 2026-11-28)

The catalog's modality tabs at OpenRouter (screenshot 24) are Text, Image, Video, Speech, Decisions, Embeddings, Rerank, Transcription and Audio. For each, four things must be true before a customer can use it here: the engine has the route, the credits plugin can bound its cost before the call and charge it after (`classify` in `apps/gateway/plugins/credits/plugin.go`), the public edge sends its path to the gateway (`DEPLOY.md`), and the catalog lists its models with their prices.

| Type                           | Engine route                                                                                                                                                                                                                                                                                                                                                      | Charged by the credits plugin | Public path | In the catalog                                                      | Verdict                                                                                               |
| ------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------- | ----------- | ------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------- |
| Text: chat                     | `POST /v1/chat/completions`                                                                                                                                                                                                                                                                                                                                       | yes                           | yes         | yes                                                                 | Built. A successful answer not yet seen on staging (the provider account has no credit)               |
| Text: responses                | `POST /v1/responses`                                                                                                                                                                                                                                                                                                                                              | yes                           | yes         | yes                                                                 | Built, same note                                                                                      |
| Text: completions              | `POST /v1/completions`                                                                                                                                                                                                                                                                                                                                            | yes                           | yes         | yes                                                                 | Built, not tried live                                                                                 |
| Embeddings                     | `POST /v1/embeddings`                                                                                                                                                                                                                                                                                                                                             | yes                           | yes         | no: the catalog keeps only models with an input and an output price | Charging built; the catalog and the model page need it                                                |
| Image                          | `/v1/images/generations`, `/edits`, `/variations`                                                                                                                                                                                                                                                                                                                 | no, refused 403               | no          | no                                                                  | Needs cost bounding (per image, by size and quality), prices, catalog                                 |
| Video                          | `/v1/videos` and its sub-routes                                                                                                                                                                                                                                                                                                                                   | no                            | no          | no                                                                  | Needs cost bounding (per second, by resolution), a job that is paid when it finishes, prices, catalog |
| Speech (text to speech)        | `POST /v1/audio/speech`                                                                                                                                                                                                                                                                                                                                           | no                            | no          | no                                                                  | Needs cost bounding (per character), prices, catalog                                                  |
| Transcription (speech to text) | `POST /v1/audio/transcriptions`                                                                                                                                                                                                                                                                                                                                   | no                            | no          | no                                                                  | Needs cost bounding (per second of audio, known only after reading the file), prices, catalog         |
| Rerank                         | `POST /v1/rerank`                                                                                                                                                                                                                                                                                                                                                 | no                            | no          | no                                                                  | Needs cost bounding (per search or per token), prices, catalog                                        |
| Decisions                      | `POST /v1/decisions`                                                                                                                                                                                                                                                                                                                                              | no                            | no          | no                                                                  | Needs cost bounding (token priced, as text), prices, catalog                                          |
| Audio                          | Two kinds at OpenRouter (screenshot 30): music generation, priced per song or clip (Lyria), and chat models that take and return audio, priced per token (GPT Audio). The second is the chat route with audio content, which the credits plugin refuses today as content it cannot bound; the first has no route of its own in the engine that we have identified | no                            | no          | no                                                                  | Audio chat needs bounding of audio tokens; music needs the route confirmed, then a per-item bound     |
| Batch                          | `/v1/batches` and its sub-routes                                                                                                                                                                                                                                                                                                                                  | no                            | no          | no                                                                  | Needs a hold for the whole batch and settlement when it finishes                                      |

What the engine already does for all of them: it serves the route, and it can work out the cost of a finished request of every type (`framework/modelcatalog/pricing.go`). What is missing is ours: the hold before the call, which is why the plugin refuses them, and the price table and catalog, which know text prices only (`model_price` has typed columns for the five text rates and a free-form `other_rates` for the rest).

The units OpenRouter prices each type in (screenshots 25 to 30), which the price table and the catalog must be able to hold and show: embeddings and rerank per 1M tokens (one rate); decisions per 1M input tokens (output free); speech per 1M characters, or per 1M input and output tokens for token-priced models; transcription per second of audio; music per song or clip; audio chat per 1M input and output tokens.

Switching these on is one piece of work with one design (a bound per type, one settlement path), not one patch per type. It is outside this plan's stages; the model page is built so a type's models appear on it once the type is on.

### Batch: not part of this plan (owner, 2026-11-28)

Batch is not a mode of the chat route: it has its own endpoints (`POST /v1/batches`, `GET /v1/batches/{id}`, `.../results`, `.../cancel`), which the engine already has and the credits plugin refuses today. It is switched on with the other request types that are off (speech, transcription, images, video, rerank, files), as one piece of work on bounding their cost before the call. The Batch entry of the routing selector is added to the page then.

### Stage 8: the remaining charts (M)

- Tool call and structured output error rates: the gateway classifies both per request; the statistics job counts them.
- Location filter: the gateway records the caller's region (not the address).
- Apps: attribution from `HTTP-Referer` and `X-Title`, an opt-in public list.
- Activity volume public, when the owner switches it on.

### Stage 9: the catalog list (M)

Tabs with counts per modality, list and table views, search, filters, pins (CAT-01 to CAT-06).

## Order and why

1, then 2: the page the owner asked for, real and complete for what exists today. 5 is the largest change to the product and unlocks most of what is still missing on the page; it waits on a second provider. 6 builds on 5. 8 and 9 are independent and can move up. The playground, compare and batch are outside this order (see above).

## Never in this work

- Figures that are estimated or filled in to look complete: a number comes from the statistics table or is not shown.
- A button for a feature that is not built.
- OpenRouter's name, logo, colours or text: the structure and the features are matched, the page is Lemru's.
