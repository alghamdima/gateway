# OpenRouter's features, and what we build

Compiled 2026-11-23 from the research briefs in `research/` (OpenRouter docs fetched 2026-11-21) and a web search on 2026-11-23 for what the briefs do not cover (plugins, guardrails, presets, workspaces, privacy, app attribution). openrouter.ai cannot be fetched from the build sandbox, so items marked _unverified_ rest on search summaries, not on the docs pages themselves.

Status of each feature here:

- **Done**: built and tested on this branch.
- **Planned**: a step in `PLAN.md` or Phase G (`gateway-capabilities.md`).
- **Decision**: waits on the owner.
- **Not planned**: deliberately left out for now, with the reason.

"Gateway" says whether Bifrost already does the hard part, which makes the step small.

## Core API

| OpenRouter                                                                                             | Status                                                          | Gateway                          | Notes                                                                                  |
| ------------------------------------------------------------------------------------------------------ | --------------------------------------------------------------- | -------------------------------- | -------------------------------------------------------------------------------------- |
| One OpenAI-compatible API for many providers (chat, completions, responses, messages, embeddings)      | Done in the gateway; public routing is 8.1 (Decision: hosting)  | Yes, 40 providers                | Our `/api/v1/*` path for inference waits on where we host                              |
| Streaming, tool calling, structured outputs, images in input                                           | Done (passed through to the provider)                           | Yes                              | Not tested by us per provider; checked in 8's SDK smoke test                           |
| Prompt caching passed to providers, with cache prices                                                  | Done for charging (catalog cache prices)                        | Yes                              | Shown in `/api/v1/models` as `input_cache_read` / `input_cache_write`                  |
| Model fallbacks (`models: [...]`) and provider fallbacks                                               | Partly: the gateway retries and falls back on its configuration | Yes                              | The request's `models` array in OpenRouter's shape is not mapped yet: Planned with 8.1 |
| Provider routing controls per request (`provider.order`, `only`, `ignore`, `sort`, `:nitro`, `:floor`) | Not planned yet                                                 | Partly (routing plugin, weights) | Useful once we serve a model through several providers; today one provider per model   |
| Auto router (`openrouter/auto`)                                                                        | Not planned                                                     | Routing plugin can pick a model  | Product decision; low value early                                                      |
| `:free` models with daily limits                                                                       | Not planned                                                     | No                               | We have no free models; would need the rate limits of G1                               |

## Money

| OpenRouter                                                      | Status                                                     | Notes                                                             |
| --------------------------------------------------------------- | ---------------------------------------------------------- | ----------------------------------------------------------------- |
| Prepaid credits, one balance per account                        | Done                                                       | Ledger, holds, settlement (ADR 0001, 0009)                        |
| Card top-ups with a 5.5% fee ($0.80 minimum)                    | Done; real Stripe run waits on a Stripe sandbox (Decision) | Fee and markup set by staff (ADR 0010, 0012). Limits $5 to $1,000 |
| Crypto top-ups (5% fee)                                         | Not planned                                                | Owner decided card only (ADR 0010)                                |
| Auto top-up below a threshold                                   | Not planned yet                                            | Needs off-session card payments; after the first real payments    |
| Credit expiry (one year) and refunds within 24 hours            | Decision                                                   | Policy, then a small build                                        |
| Invoices and receipts                                           | Done (Stripe invoice per top-up)                           |                                                                   |
| Price shown equals price charged                                | Done                                                       | Markup per provider in the catalog and the charge (ADR 0005)      |
| In-flight budget (402 when holds exceed a share of the balance) | Done, stricter: every request holds its maximum cost       | ADR 0009                                                          |

## Keys and spend control

| OpenRouter                                                     | Status                               | Notes                                                                                   |
| -------------------------------------------------------------- | ------------------------------------ | --------------------------------------------------------------------------------------- |
| API keys shown once, stored hashed                             | Done                                 |                                                                                         |
| Per-key credit limit with daily/weekly/monthly reset           | Done, on the charged amount          | ADR 0013                                                                                |
| Key expiry, disable, rename, delete                            | Done                                 |                                                                                         |
| `GET /api/v1/key`, `GET /api/v1/credits`, `GET /api/v1/models` | Done                                 | Step 8.2                                                                                |
| Management keys and the key management API (`/api/v1/keys`)    | Not planned yet                      | Customers manage keys in the dashboard; add when a customer needs automation            |
| Distinct 401 messages                                          | Done                                 |                                                                                         |
| GitHub secret scanning for leaked keys                         | Not planned yet                      | Needs our key prefix registered with GitHub; after the brand name                       |
| Rate limits                                                    | Planned: G1 (organization), G2 (key) | OpenRouter has none on paid models; we add them to protect capacity. Decision: defaults |

## Organizations

| OpenRouter                                                                                | Status          | Notes                                                              |
| ----------------------------------------------------------------------------------------- | --------------- | ------------------------------------------------------------------ |
| Organizations with admin and member roles, one shared balance                             | Done            | Owner, admin, member; API keys permission (ADR 0004)               |
| Workspaces (environments inside an account, each with keys, budgets, guardrails, presets) | Not planned     | One organization per environment works today; add if customers ask |
| Workspace budgets and guardrail budgets per member                                        | Not planned yet | Per-key limits cover the common case                               |
| System keys owned by the organization, not a person                                       | Done in effect  | Our keys belong to the organization                                |

## Records and analytics

| OpenRouter                                                                           | Status           | Gateway                         | Notes                                                                          |
| ------------------------------------------------------------------------------------ | ---------------- | ------------------------------- | ------------------------------------------------------------------------------ |
| Generation id per request, logs page, generation detail and `GET /api/v1/generation` | Planned: Phase 9 | Yes, logs API                   | Read through the gateway's logs API, scoped to the organization's keys         |
| Activity page (spend, requests, tokens by model, key and day; CSV export)            | Planned: Phase 9 | Yes, histograms                 | Per-key usage is now in the ledger (ADR 0013)                                  |
| Prompt and completion text not stored by default; opt-in logging for a 1% discount   | Decision         | Logging plugin can drop content | Default must be "not stored"; check the gateway's logging config before launch |
| App attribution headers and public app rankings                                      | Not planned      | No                              | Marketing feature; later                                                       |
| Broadcast traces to the customer's observability tool (_unverified_)                 | Not planned      | OpenTelemetry plugin exists     | Later                                                                          |

## Privacy and policy

| OpenRouter                                                                               | Status          | Notes                                                               |
| ---------------------------------------------------------------------------------------- | --------------- | ------------------------------------------------------------------- |
| Zero data retention routing (`provider.zdr`), `data_collection`                          | Not planned yet | Needs a per-provider policy table; matters for enterprise customers |
| Data residency / regions                                                                 | Not planned     | Depends on hosting (Decision)                                       |
| Guardrails: per-workspace rules, block/flag/log on content policy (_unverified details_) | Not planned yet | The gateway has no content guardrail in the open-source build       |

## Request extras

| OpenRouter                                                              | Status                                        | Gateway                                  | Notes                                                   |
| ----------------------------------------------------------------------- | --------------------------------------------- | ---------------------------------------- | ------------------------------------------------------- |
| Presets (saved system prompt, model and parameters, referenced by name) | Not planned yet                               | `prompts` plugin is similar              | Small once wanted                                       |
| Response healing for broken JSON (opt-in plugin)                        | Not planned yet                               | No                                       |                                                         |
| Web search as a server tool (the `:online` plugin is deprecated)        | Not planned                                   | No                                       | Needs a search provider contract and its own price      |
| PDF and file inputs                                                     | Passed through where the provider supports it | Yes                                      | No conversion of our own                                |
| Bring your own provider key (BYOK), free to $25k/month then 5%          | Not planned yet                               | Possible with per-customer provider keys | A pricing decision and an isolation design; later phase |

## Catalog and site

| OpenRouter                                                | Status          | Notes                                                                 |
| --------------------------------------------------------- | --------------- | --------------------------------------------------------------------- |
| Public model list with filters and a page per model       | Planned: 8.3    | From `/api/v1/models`                                                 |
| Per-provider endpoints with latency and uptime            | Not planned yet | Needs our own measurements from logs                                  |
| Sign in with OpenRouter (OAuth PKCE) for third-party apps | Not planned     | Later, for an app ecosystem                                           |
| Chat playground                                           | Not planned     | TurboStarter has an AI chat page that could be pointed at our gateway |

## What this means for the order of work

1. **Before the first paying customer**: G1 rate limits, 8.1 edge routing (needs hosting), Phase 9 records, the content-logging default, a real Stripe run, the brand (Phase 7).
2. **Soon after**: G2 to G5, 8.3 catalog pages, management keys, auto top-up, credit expiry and refunds.
3. **When customers ask**: workspaces, ZDR routing, guardrails, presets, BYOK, provider routing controls, web search.
