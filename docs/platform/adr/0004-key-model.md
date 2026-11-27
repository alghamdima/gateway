# ADR 0004: One organization is one gateway customer, one dashboard key is one virtual key

Status: Accepted
Date: 2026-11-21

## Context

The dashboard (TurboStarter) has users, organizations and members (Better Auth `organization` plugin, tables `organization` and `member` in `packages/db/src/schema/auth.ts`). The gateway (Bifrost governance) has customers, teams and virtual keys (`framework/configstore/tables/{customer,team,virtualkey}.go`). A virtual key belongs to at most one team and one customer. Teams can belong to a customer.

The virtual key's value is the bearer token customers send. Bifrost stores it in `governance_virtual_keys.value` (plaintext unless a config-store encryption key is set, `EncryptionStatus` defaults to `plain_text`) plus a `value_hash` for lookup. Values are generated as prefix + UUIDv4 (`plugins/governance/main.go`, line 2523).

## Options considered

1. **Organization to customer, dashboard key to virtual key, no teams.** Each organization gets one Bifrost customer. Every key created in the dashboard creates one virtual key under that customer.
2. **Organization to team, with one global customer.** Rejected: teams have no business meaning for us, and Bifrost's per-customer views (logs, budgets) line up with organizations naturally.
3. **One shared virtual key per organization, with our own keys mapped in a proxy.** Rejected: needs an extra hop or a custom auth layer, and loses Bifrost's per-key logs and limits.

## Decision

Option 1.

- `organization.id` maps to a Bifrost customer created on demand with `id = organization.id`, so no mapping table is needed for it.
- A new table `api_key` in the control plane: `id`, `organization_id`, `created_by` (member), `name`, `virtual_key_id` (Bifrost), `prefix` (first characters, for display), `value_hash` (SHA-256, to look up a key a customer pastes), `limit` (micro-USD, optional), `limit_reset`, `expires_at`, `disabled_at`, `last_used_at`, `created_at`. The plaintext key is shown once and never stored by the control plane.
- Creating a key: the control plane calls Bifrost's governance API to create the virtual key under the customer, receives the value, stores the hash and prefix, and shows the value once. The Bifrost call happens first. If our insert then fails, the control plane deletes the virtual key, so no unknown keys stay alive.
- Bifrost's config store runs with an encryption key, so virtual key values are encrypted at rest there too.
- Per-key spend caps (OpenRouter's `limit` and `limit_reset`) map to a Bifrost budget on the virtual key. **Superseded by ADR 0013:** the credits plugin enforces them on the charged amount, because the gateway budget counted cost without our markup. They are customer-chosen caps, separate from the prepaid balance, which only the ledger enforces (ADR 0001).
- Bifrost teams are unused for now. If we add workspaces later (research brief 09), a workspace maps to a Bifrost team under the customer.
- Key prefix: the brand's customer key prefix (brief input, for example `sk-acme-`) replaces `sk-bf-` in the rename (ADR 0008). Keys keep the UUIDv4 body (122 random bits), which is enough entropy.
- Only members with the organization's admin or owner role can create, disable or delete keys.

## Amendment (2026-11-23): customer ids come from the gateway

The gateway's governance API does not accept a customer id: it generates one (a UUID). So `organization.id` cannot be the customer id, and a mapping table replaces that part of the decision:

- `gateway_customer (organization_id primary key, customer_id unique)` in the control plane, created when an organization's first key is made.
- The credits plugin reads it as `gateway_ledger` to turn the customer id the governance plugin puts on the context into the organization that pays. It caches hits in memory; a miss is asked again, so a new organization works at once.
- A customer with no row is refused with 403 `credits_no_organization`, before any provider call.
- Deleting an organization deletes its row (foreign key), so its keys can no longer be charged even if removing them from the gateway failed: they are refused.
- Disabling a key in the gateway returns 403 `policy_access_denied` to its holder, not 401; a deleted or unknown key returns 401.

## Amendment (2026-11-27): expiry, maximum lifetime, last used (KEYS-03, KEYS-11, ADMIN-12)

- **Expiry is required and fixed.** A key is created with a lifetime (1 hour to 1 year, or no expiration), turned into a moment at creation. That moment is sent to the gateway's virtual key and stored in `api_key.expires_at`: one value in two places, and the gateway enforces it (401 `key_expired`). The customer can never change it; a request that tries is refused.
- **Maximum key lifetime** (staff, `key_settings`, migration `0040`). A new key may not outlive it, and "no expiration" is refused while one is set. When staff shorten it, every existing key that would outlive it is cut to `created_at + maximum`, in the gateway first and then in `api_key`, the same moment in both (`packages/api/src/modules/keys/lifetime.ts`). The gateway stores expiry per virtual key and only accepts a future one, so a key already past its cut-off gets an expiry 5 seconds ahead. The cut is idempotent, only ever shortens, locks the key's row while the gateway is told, and is retried hourly (`/api/internal/key-lifetime`). OpenRouter refuses such keys; computing the cut at check time instead would mean the gateway reading our settings on every request. Raising the maximum later extends no key.
- **No `last_used_at` column.** Last used is the time of the key's latest `usage` ledger entry, read at display time through the partial index on `(virtual_key_id, created_at)` that ADR 0013 added. A column would be an update of `api_key` on every request: a hot, update-heavy row on the gateway's request path, and `gateway_ledger` would need write access to `api_key`. A request that was refused or charged nothing does not count as a use.

## Consequences

- The control plane is the only writer of customers and virtual keys. The Bifrost admin UI is not exposed to customers (ADR 0007), so there is one source of truth.
- Deleting an organization must delete its virtual keys and customer in Bifrost. This is part of the organization deletion hook.
- Multi-instance consistency of newly created keys depends on ADR 0006.
