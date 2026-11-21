# API keys and the key-management API

Sources: /docs/api_reference/authentication.md, /docs/guides/overview/auth/management-api-keys.md, /docs/api/api-reference/api-keys/* (fetched 2026-11-21).

## What the customer sees

- Two kinds of key:
  - **API keys (inference keys)** call the model endpoints.
  - **Management keys** call only administrative routes. They cannot call completion endpoints.
- Both use one format: `sk-or-v1-` followed by 64 lowercase hex characters (73 characters in total).
- The key is shown once at creation and can never be retrieved again.
- Each key can have an optional USD credit limit that resets daily, weekly, monthly, or never. The docs strongly recommend setting one. Keys created during onboarding get a $100 limit and expire after 180 days.
- Keys can expire (`expires_at`), be disabled, renamed, and moved between workspaces.
- Leaked keys: OpenRouter is a GitHub secret-scanning partner and emails the owner when a key is found.
- Management keys are scoped to the whole organization or to one workspace, with an access level of read/write, read-only, or custom (a per-resource permission list). Scope and permissions are fixed at creation. To change them, create a new key.

## API shape

All under `https://openrouter.ai/api/v1`, Bearer auth.

| Method and path       | Auth           | Purpose                                                                                                                                                                                                                                     |
| --------------------- | -------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `POST /keys`          | management     | Create. Body: `name`*, `limit`, `limit_reset` (`daily`/`weekly`/`monthly`/null), `include_byok_in_limit`, `expires_at` (ISO 8601 with seconds), `workspace_id`, `creator_user_id`, `external {user, api_key}`. Returns `201 { data, key }`. |
| `GET /keys`           | management     | List. Query: `include_disabled`, `include_expired`, `offset`, `workspace_id`.                                                                                                                                                               |
| `GET /keys/{hash}`    | management     | Read one.                                                                                                                                                                                                                                   |
| `PATCH /keys/{hash}`  | management     | Update `name`, `disabled`, `limit`, `limit_reset`, `include_byok_in_limit`, `workspace_id`. An empty body returns `400 No update fields provided`.                                                                                          |
| `DELETE /keys/{hash}` | management     | Delete.                                                                                                                                                                                                                                     |
| `GET /key`            | the key itself | Self-inspection: limits, usage, budgets, free-model quota.                                                                                                                                                                                  |

The key object, identified by `hash` (SHA-256 hex):

`name`, `label` (masked), `disabled`, `created_at`, `updated_at`, `expires_at`, `last_used_at`, `workspace_id`, `creator_user_id`, `external_user`, `limit`, `limit_remaining`, `limit_reset`, `include_byok_in_limit`, `usage`, `usage_daily`, `usage_weekly`, `usage_monthly`, `byok_usage` (and daily/weekly/monthly).

`GET /key` adds `is_management_key`, `organization_id`, `budgets[]` (each with `scope`, `limit_usd`, `spend_usd`, `remaining_usd`, `reset_interval`, `resets_at`, `include_byok`), `effective_budget`, `free_model_daily_requests`, `allowed_data_regions`, and a legacy `rate_limit` that is always -1.

## Observable rules

- Usage windows are UTC. Weeks run Monday to Sunday. Resets happen at midnight UTC.
- Key writes (create, update including disable, delete) count against a daily quota of 10,000 per account, reset at 00:00 UTC. A request counts even when it fails. The docs advise setting `expires_at` on short-lived keys instead of deleting them.
- Separate per-IP burst limits apply to key writes, key reads, and `GET /key` / `GET /credits`. These responses have no `X-RateLimit-*` headers.
- Exceeding a key's limit: `403` with `limit_source = "openrouter_key_limit"` and `scope = "key_limit"`.
- 401 messages are specific: a non-OpenRouter `sk-` key, an empty Bearer, a malformed header, an expired key, and a deleted or disabled key each get their own message.
- A bearer starting with `sk-or-v1-` is treated as an API key. A JWT is accepted only from OpenRouter's own issuers. Anthropic SDK clients can send the key in `x-api-key`.
- `external.api_key` lets a partner supply its own key value (minimum 32 characters, stored as SHA-256). Keys are therefore stored hashed (stated for this field, implied for all because `hash` is the identifier).

## Unknown

- How the 64 hex characters are generated, and whether a checksum is embedded (guess: random, no checksum).
- Whether the limit check reads a cached counter or the ledger (guess: cached counter, given the in-flight budget design).
- The per-IP burst numbers.
