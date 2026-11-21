# BYOK (bring your own key)

Sources: /docs/guides/overview/auth/byok.md, /docs/api/api-reference/byok/*, /docs/faq.md (fetched 2026-11-21).

## What the customer sees

- The customer stores their own provider keys (OpenAI, Anthropic, Amazon Bedrock, Google Vertex, Azure and others) in workspace BYOK settings. Requests to that provider then use the customer's key, rate limits and provider bill.
- Fee: free up to $25,000 per month of list-price inference cost on pay-as-you-go, then 5% of what the same request would have cost on OpenRouter, taken from OpenRouter credits. Enterprise allowances are custom.
- Each key goes in one of two sections:
  - **Prioritized** keys are tried in order before OpenRouter's shared capacity.
  - **Fallback** keys are tried only after shared capacity.
- Per prioritized key, a **shared capacity fallback** setting: use shared capacity on failure (default), never for this key's models, or never for any model on this provider.
- Per key restrictions: allowed models, allowed API keys, allowed users. Declarations of ZDR and data region about the customer's own provider account.

## API shape

| Method and path | Purpose |
| --- | --- |
| `POST /api/v1/byok` | Create. Body: `provider`* (slug), `key`*, `name`, `workspace_id`, `allowed_models`, `allowed_api_key_hashes`, `allowed_user_ids`, `is_fallback`, `is_required`, `is_byok_only`, `declared_zdr` (true/false/null), `declared_region` (`global`/`europe`/`us`/null), `disabled` |
| `GET /api/v1/byok`, `GET/PATCH/DELETE /api/v1/byok/{id}` | List, read, update, delete |

Response fields include `id`, `label` (masked snippet), `sort_order` (tried ascending), and `workspace_id` (null means usable across all workspaces). The raw key is encrypted at rest and never returned.

On the response and in generation records: `usage.is_byok`, `byok_usage*` fields on keys, and BYOK metrics in analytics (`byok_usage`, `byok_fees`, `byok_request_count`).

## Observable rules

- Without `order`, providers with a prioritized BYOK key are tried on that key first, then shared capacity in normal router order.
- With `order`, each named provider's slot tries the customer's key first, then shared capacity for that provider. A fallback key reverses that within its slot. A "never use shared capacity" key removes the shared endpoint from the slot.
- Several keys for one provider are tried in `sort_order`. If one fails, the next matching key is tried before shared capacity.
- If every BYOK key fails, routing falls back to shared capacity (default), which spends credits.
- BYOK does not bypass data policies: `data_collection` and ZDR still filter endpoints. A key-level `declared_zdr` is trusted without verification.
- `include_byok_in_limit` on API keys and `include_byok_in_budgets` on workspaces control whether BYOK spend counts against caps.
- Regional endpoints carry a premium. A prioritized BYOK key may route to a regional endpoint only when the provider has no standard endpoint for the model.

## Unknown

- How keys are encrypted and who holds the encryption key (guess: envelope encryption with a KMS).
- Whether the 5% fee is debited per request or batched.
- How a BYOK provider error (for example, the customer's key is out of quota) is distinguished from a shared-capacity error in the breaker.
