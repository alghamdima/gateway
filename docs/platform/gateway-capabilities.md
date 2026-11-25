# What the gateway can do, and what the product uses

Inventory taken 2026-11-23 from `apps/gateway` (Bifrost `transports/v2.6.0`): every management route the gateway registers (about 360 in 30 areas) and every built-in plugin, sorted by who should use each one. Source of the counts: the `r.GET/POST/PUT/DELETE("/api/...")` registrations in `transports/bifrost-http/handlers/*.go`.

Who uses a capability:

- **Customer**: in our dashboard or `/api/v1`, scoped to their organization.
- **Staff**: in our admin console, with the staff roles, 2FA and audit log of ADR 0011.
- **Internal**: only in the gateway's own UI on the private network (ADR 0007). Not worth rebuilding.
- **Out of scope**: not part of an OpenRouter-like product; left off.

## Inventory

| Area                                                                               | What it does                                                                                                                                                  | Used today                        | Who should use it                                                                               |
| ---------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------- | ----------------------------------------------------------------------------------------------- |
| Inference (`/v1/*`, provider-compatible paths)                                     | One API for 40 providers, format translation, streaming, retries, fallbacks                                                                                   | Yes: every customer request       | Customer (through the edge, step 8.1)                                                           |
| Virtual keys (`/api/governance/virtual-keys`)                                      | Create, disable, expire, delete, rotate. Per key: allowed providers and models, model blacklist, provider weights, budgets, **request and token rate limits** | Create, disable, expire, delete   | Customer: **rate limits per key**, **allowed models per key**, rotation. Budgets: no (ADR 0013) |
| Customers (`/api/governance/customers`)                                            | One per organization (ADR 0004). Budgets and a **rate limit** applied to all its keys                                                                         | Create, delete                    | Staff: **rate limit per organization**                                                          |
| Provider governance (`/api/governance/providers`)                                  | Budget and rate limit per provider across all customers                                                                                                       | No                                | Staff: protects our quota with each provider                                                    |
| Model configs (`/api/governance/model-configs`)                                    | Budget and rate limit per model across all customers                                                                                                          | No                                | Staff, later: caps an expensive model                                                           |
| Providers and their keys (`/api/providers`)                                        | Add a provider, its API keys, key weights, network settings, refresh the model list                                                                           | Configured by hand in the gateway | Staff (admin role only): **providers page**                                                     |
| Model catalog (`/api/models/*`)                                                    | Models per provider with context, modalities and unit costs                                                                                                   | Yes: `/api/v1/models`             | Customer: catalog pages (8.3)                                                                   |
| Pricing overrides, datasheet overrides                                             | Replace catalog prices                                                                                                                                        | No                                | Rejected for margin (ADR 0005). Staff, later, only to price a model the catalog lacks           |
| Logs (`/api/logs/*`, 33 routes)                                                    | Every request with tokens, cost, latency, provider, errors; histograms by model, provider, key                                                                | Not read                          | Customer: activity and generation detail (Phase 9). Staff: platform analytics                   |
| Routing plugin (`/api/routing/model-routers`)                                      | Rules that pick a model or provider per request                                                                                                               | No                                | Internal for now                                                                                |
| Semantic cache plugin                                                              | Serves a stored answer to a similar prompt                                                                                                                    | Off                               | Off until we decide what a cached answer costs the customer                                     |
| Teams, users, business units                                                       | Bifrost's own org chart                                                                                                                                       | No                                | Out of scope: organizations are ours (ADR 0004)                                                 |
| MCP gateway, agents, skills, prompt repository, relays, Warp, RSS, OAuth, webhooks | Tooling platform features                                                                                                                                     | No                                | Out of scope; must stay unreachable from the public host (ADR 0007)                             |
| Config, plugins, feature flags, proxy config, session, telemetry, pprof            | Running the gateway                                                                                                                                           | Set at deploy time                | Internal; telemetry feeds Phase 10                                                              |

## Gaps

1. **No rate limits anywhere.** One organization can use the whole gateway's capacity, and a leaked key can send unlimited requests until the balance runs out.
2. **Staff manage providers in the gateway's internal UI**, outside our roles, 2FA and audit log.
3. **Customers cannot narrow a key**: no per-key request limit, no model allow-list, no rotation.
4. **Usage data the gateway already records is not shown** (Phase 9 covers this).

## Plan: Phase G, gateway controls

Order by risk removed. Each step follows the pattern already in use: our database holds the setting and the audit trail, the control plane pushes it to the gateway through `@workspace/gateway-client`, and an end-to-end test against the built gateway proves it is enforced.

| Step | What                                                                                                                                                                                                                                           | Who      | Gateway feature                      | Size |
| ---- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------- | ------------------------------------ | ---- |
| G1   | **Rate limit per organization**: requests per minute and tokens per minute on the admin organization page, plus a platform default for new organizations. Refusals are 429 with `Retry-After`, OpenRouter's shape.                             | Staff    | Customer `rate_limit`                | M    |
| G2   | **Rate limit per key**: requests per minute on the keys page, next to the spending limit. Can only be tighter than the organization's.                                                                                                         | Customer | Virtual key `rate_limit`             | S    |
| G3   | **Providers page** in the admin console: providers, their status and model count, keys shown masked, add or replace a key, weights, turn a provider off. Provider secrets are write-only: never returned by our API. Admin role only, audited. | Staff    | `/api/providers`                     | M    |
| G4   | **Provider and model caps**: requests and tokens per minute per provider, and per model, across all customers, to stay inside our provider quotas.                                                                                             | Staff    | Provider governance, model configs   | S    |
| G5   | **Allowed models per key** and **key rotation** on the keys page.                                                                                                                                                                              | Customer | Virtual key provider configs, rotate | M    |

Already planned elsewhere: per-request logs and usage charts (Phase 9, from the logs API scoped to the organization's keys), catalog pages (8.3), telemetry and alerts (Phase 10).

## Checks before G1

- The governance plugin applies a customer's rate limit to keys created directly under the customer (`store.go` collects `vk.CustomerID`). G1's end-to-end test must prove a 429 from the built gateway, not assume it.
- Counters live in each gateway instance's memory and are flushed to its store. With more than one instance a limit is per instance until ADR 0006's multi-instance work; G1 must state the effective limit.
- How 429s from our limits reach the customer: body shape, `Retry-After`, and whether the credits hold is released (it must be: the provider is never called).

## Owner decisions

- The platform default per organization (for example 600 requests and 1,000,000 tokens per minute), or no default.
- Whether customers may set rate limits on their keys (G2) or only staff.
- The semantic cache: off, or on with the customer charged the full price, or a discount.
