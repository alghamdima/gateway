# ADR 0018: Our own model price table, pushed to the gateway's pricing overrides

- Status: Accepted
- Date: 2026-11-27
- Feature: ADMIN-09 (`SPEC.md`), owner decision D9 (section 2)
- Builds on: ADR 0010 (amended: no markup), ADR 0017 (SAR ledger, effective rate)

## Context

The owner decided (D9) that prices are ours: a table per provider and model, in USD (providers price in USD; customers see SAR at the effective rate), with input, output, cache read, cache write (5 minutes and 1 hour where the provider has them) and other units where they exist (per image, per second of audio or video, per character, per request or search). Every price has an effective time and a full history; it is never edited, only superseded. A provider key may carry its own negotiated price. A sync from a source arrives as a suggestion that staff approve or reject, and a price staff set always wins. Every request stores the price it was charged at.

### What the gateway already does (gateway first)

- **Base prices.** `framework/modelcatalog/datasheet` loads the upstream datasheet (`store.go:20`, `getbifrost.ai/datasheet`, every 24 h) into `governance_model_pricing`. `GET /api/models/datasheet` (`transports/bifrost-http/handlers/providers.go:1870`) lists every entry with its `upstream` row and its `effective` row (after the gateway's own datasheet edits), every cost field keyed by the datasheet's names.
- **Scoped pricing overrides.** `framework/configstore/tables/pricingoverride.go`, routes `/api/governance/pricing-overrides` (`handlers/governance.go:1842-1845`). An override has a scope (global, provider, provider key, virtual key, user, and combinations), an exact or trailing-wildcard model pattern, the request types it applies to, and a patch of `datasheet.Options` (`types.go:103`): 138 cost fields, every one of them applied by `patchPricing` (`overrides.go`). The handler writes the configstore and the in-memory catalog in the same call (`governanceManager.UpsertPricingOverride`), so a pushed price is in force when the call returns. `resolveEntry` walks the scopes most specific first and returns **one** patch: a provider-key override does not merge with a provider override, so it must carry every rate.
- **Who uses them.** `CalculateCost` and `CalculateCostForUsage` resolve overrides with the request's scopes (`PricingLookupScopesFromContext`). The credits plugin prices the hold (`plugins/credits/plugin.go`, `estimate`) and the charge (`PostLLMHook`) through those two functions with the same scopes, so a pushed price applies to the reserve and the settle alike. `GET /api/models/details` reports, per model, the provider-scope override's effect on the four displayed costs (`overridden_pricing`, `handlers/providers.go:1088`).
- **What they cannot express.** No effective time (an override is in force from the moment it is written until it is changed). No unit beyond `Options`' 138 fields. A model with no base entry is refused by the credits plugin (`estimate`: `GetPricingEntryForModel == nil`) even when an override exists.

LiteLLM keeps its own model cost map (`model_prices_and_context_window.json`) with per-deployment `input_cost_per_token` / `output_cost_per_token` overrides in the router config, and records `response_cost` on each spend log. The ideas taken: overrides per deployment (our per-key price) and the cost kept on each spend row (`cost_micro_usd`, ADR 0017).

## Decision

**The control plane owns the price table; the gateway charges with it through its own pricing overrides. There is no second pricing engine.**

### 1. Schema (`packages/db/src/schema/model-prices.ts`, migration `0037`)

`model_price`: one immutable version of a price.

| Column                                                                               | Type                                  | Why                                                                                         |
| ------------------------------------------------------------------------------------ | ------------------------------------- | ------------------------------------------------------------------------------------------- |
| `id`                                                                                 | `bigint generated always as identity` | A fact log with no need for opaque ids; short enough to put in the gateway override's name. |
| `provider`, `model`                                                                  | `text`                                | The gateway's names, as its catalog lists them. Length-checked.                             |
| `provider_key_id`                                                                    | `text` null                           | The gateway's `key_id` for a negotiated price; null is the provider-wide price.             |
| `effective_from`                                                                     | `timestamptz`                         | In force from this instant until the next version of the same target.                       |
| `is_removed`                                                                         | `boolean`                             | A key's own price ends here and the provider-wide price applies again. Key versions only.   |
| `input_usd`, `output_usd`, `cache_read_usd`, `cache_write_usd`, `cache_write_1h_usd` | `numeric(36,30)`                      | USD per token: the common rates, typed. `cache_write_usd` is the 5-minute (default) rate.   |
| `other_rates`                                                                        | `jsonb`                               | Every other unit, `{"<gateway field>": "<decimal>"}`.                                       |
| `source`                                                                             | `text`                                | `manual`, `suggestion` or `import`.                                                         |
| `suggestion_id`                                                                      | `bigint` null                         | The suggestion approved into this version.                                                  |
| `note`, `created_by`, `created_at`                                                   |                                       | Who and why.                                                                                |

- **Rates are `numeric`, not scaled integers.** The ledger keeps amounts as integer micro-SAR and micro-USD (ADR 0017); a unit rate is not an amount. The datasheet has rates such as `2.86102294921875e-9` USD per pixel and repeating thirds (`8.33333333333333e-8`), which need up to 24 decimal places; a micro- or pico-USD integer would round them and change what is charged. `numeric(36,30)` holds every rate exactly (the API refuses more than 30 decimals so Postgres never rounds silently), and Drizzle reads it as a string, so no rate passes through floating point until it is sent to the gateway, which prices in `float64`.
- **Typed columns for the common rates, validated `jsonb` for the rest**, so a new unit needs no migration. The keys of `other_rates` are exactly the gateway's `Options` JSON names (133 once the five typed ones are taken out), allowlisted in the API (`modules/model-prices/rates.ts`); a test reads the gateway's `types.go` and fails if the two lists part. A child table (one row per unit) was rejected: a version is read, compared, pushed and audited as one whole, never by unit, and a child table would make every read a join and every version N rows without a query that needs them. The database checks the shape: an object whose keys are snake_case names other than the typed five, whose values are decimal strings between 0 and 1,000 (`jsonb_path_exists`). No GIN index: nothing queries by unit.
- **Bounds against a typo:** a token rate is at most 1 USD (the datasheet's highest is 0.36), any other unit at most 1,000 USD (highest 1.5).
- **Temporal model: valid-from only, never updated.** A trigger rejects UPDATE and DELETE, as for the ledger. The price at time T for a target is the row with the greatest `effective_from <= T`. `UNIQUE NULLS NOT DISTINCT (provider, model, provider_key_id, effective_from)` makes that row unique, so two prices can never be in force at once: intervals are derived from the next version and cannot overlap by construction. An exclusion constraint on a stored `tstzrange` was rejected: closing the previous range would update a row, which the append-only rule forbids, and the derived intervals give the same guarantee. A check refuses `effective_from` more than 5 minutes before `created_at`, so history already charged cannot be rewritten; the future is allowed.
- **Indexes.** The unique index (`provider, model, provider_key_id, effective_from`) serves both access paths: "price at T" is a backward index scan with `LIMIT 1`, "current price of every target" is `DISTINCT ON (provider, model, provider_key_id) ... ORDER BY ..., effective_from DESC`, and history per model is a range scan on its prefix. A partial index on `provider_key_id` (where not null) serves the list of a key's overrides; a partial index on `suggestion_id` covers the foreign key.

`model_price_suggestion`: a price a source proposes. `status` is `pending`, `approved`, `rejected` or `superseded`; `base_price_id` is the provider-wide version in force when it was made (null for a model with no price yet); the same rate columns. Checks: a decided suggestion has `decided_at`, a staff decision has `decided_by`, a rejection has a reason. `UNIQUE (provider, model) WHERE status = 'pending'`: one open suggestion per model. Index `(status, created_at)` for the inbox. Suggestions are a workflow, so `status` changes once (pending to decided); the decision is also in the audit log.

`model_price_push`: what the gateway holds for each target (mutable state, kept apart from the immutable prices). `UNIQUE NULLS NOT DISTINCT (provider, model, provider_key_id)`, the gateway override's id, the version last pushed, when, and the last error.

Naming follows the repository: snake_case, singular table names, `*_usd` for USD values, check constraints named `<table>_<rule>`, the "ceiling against a typo" constants exported from the schema file and used by the API. A down script is in `migrations/down/0037_model_prices.sql`.

### 2. The push

The version in force for each target is sent to the gateway as one pricing override:

- scope `provider` (`provider_id` = provider) for the provider-wide price, `provider_key` (`provider_key_id`) for a key's price;
- `match_type: exact`, `pattern` = the model;
- `request_types`: the request types the credits plugin charges today (`chat_completion`, `text_completion`, `responses`, `embedding`); a type is added when it is switched on;
- `patch`: every rate of the version (`Number(decimal)`, the nearest float, which reads back as the same decimal);
- `name`: `lemru-price:<version id>`.

It is idempotent: `model_price_push` keeps the override id; a target already carrying the version in force is skipped, an override the gateway lost (404) is recreated, and before creating one the push looks for an override with our name prefix for the same target, so a lost state row never makes a duplicate. A removed key price deletes its override. A version effective in the future is pushed by the job `/api/internal/model-prices` (every minute) once its time has come; between its time and the job it is up to one interval late, and the gateway charges the version before it. Saving a version effective now pushes at once; a failed push leaves the error on the state row and the job retries.

### 3. Reconciliation

`checkModelPrices` reads `GET /api/governance/pricing-overrides` and reports drift: a target in force with no override, an override with the wrong scope, pattern, request types or any rate (float equality with our decimal), an override of ours for a target we do not have (orphan), and an override of someone else's at the global or provider scope for a model we price (it would win or confuse). It also reads `GET /api/models/details` and compares the gateway's effective input, output and cache prices per provider and model with ours. The job re-pushes what drifted; the admin page shows the report.

### 4. Suggestions

The job `/api/internal/model-price-suggestions` (daily, after the gateway's own 24 h sync) reads the datasheet's **upstream** rows for the providers the gateway has, and for each model compares them with our provider-wide price in force. Equal: nothing. Different, or no price yet: a pending suggestion, unless the same rates are already pending, or were rejected against the same base version. A pending suggestion with other rates is superseded by the new one. The sync never writes `model_price`: **a price staff set always wins.**

- **Approve** (in one transaction, the suggestion row locked): refused with 409 unless the suggestion is pending and its base version is still the one in force; otherwise inserts a version with `source = 'suggestion'` and the suggestion's id, marks it approved, and pushes it.
- **Reject**: needs a reason; the price is unchanged.

### 5. Every request stores the price it was charged at

Each usage entry already stores what was charged: `cost_micro_usd`, the exact float in `metadata.provider_cost_usd`, and the official, spread and effective rates (ADR 0017). It now also stores, in `metadata.price_override`, the name of the gateway override that priced the attempt (`lemru-price:<version id>`), which names our immutable `model_price` row. The credits plugin reads it from the catalog after computing the cost, through a new `ModelCatalog.PricingOverrideName` (upstream file change, `MODIFICATIONS.md`). A foreign key column was rejected: the ledger is written by the `gateway_ledger` role, which would then need SELECT on `model_price` and the plugin would need our ids; the name keeps the plugin generic, and the version row it names is immutable, so the unit prices need not be copied onto every entry.

### 6. Customers

`/api/v1/models` and `/models` show our provider-wide price in force (`pricing`, USD per token as before) and the same × the current effective rate (`pricing_sar`), computed exactly in decimals. A model the gateway lists without a price of ours is not offered. A request served by a key with its own price is charged that price (no markup: the customer pays the serving key's cost); the catalog shows the provider-wide price.

### 7. Seed

`importFromGateway` (`POST /api/admin/model-prices/import`, the admin's "Import current prices" button) inserts, for every model of every provider the gateway has and that has no price yet, a version with `source = 'import'` holding the datasheet's **effective** values (the price charged today), then pushes. Run once after deploying this change, so charges do not change on day one. It is idempotent: a model with a price is skipped.

### 8. Access and audit

Staff permission `pricing`: `read` (admin, finance, support) to see; `update` (admin, finance) to create versions, key prices, approve, reject, import and push. Every change sets the audit log's before and after.

## Out of scope

- Prices per organization (no markup, ADR 0010) and per virtual key.
- Models absent from the gateway's catalog: the credits plugin refuses them whatever we push. A custom datasheet entry is a later step.
- Provider APIs as a suggestion source; only the datasheet today.
- Propagation to more than one gateway instance (one instance today; reconciliation would show the drift).
- A price pinned between hold and settle: a push landing between the two charges the new price, as a datasheet sync does today. The window is the length of one attempt.
- Request types other than text: their rates are stored and pushed with the version; the override's request types grow when each type is switched on (`PUSHED_REQUEST_TYPES`).

## Consequences

- Prices change without a deploy and with a history; a datasheet change becomes a suggestion instead of a silent price change.
- The gateway's datasheet keeps every field we do not set; the import copies every field the datasheet has, so on day one nothing is left to it.
- A pushed override is in force within the gateway call; a future price starts within one job interval of its time.
- The rate allowlist follows the gateway's `Options`; the cross-check test fails the build when they part.
