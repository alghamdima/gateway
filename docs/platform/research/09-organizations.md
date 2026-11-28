# Organizations

Sources: /docs/guides/features/workspaces.md, /docs/guides/features/workspaces/workspace-budgets.md, /docs/guides/best-practices/spend-controls.md, /docs/guides/overview/auth/management-api-keys.md, /docs/api/api-reference/organization/_, /docs/api/api-reference/workspaces/_ (fetched 2026-11-21).

## What the customer sees

- An account is either personal or an organization. Organizations have members with roles `org:admin` and `org:member` (`org:viewer` is announced, not live).
- **Billing is per account**: one credit balance shared by every workspace and member.
- **Workspaces** split an account into environments (teams, projects, staging and production). Each workspace has its own API keys, guardrails, BYOK keys, routing defaults, presets, plugins, observability, members and budgets. Every account starts with a Default workspace that all org members join automatically.
- Account-level only: activity and logs (filterable by workspace), credits and billing, org members, management keys, privacy, preferences.
- **Spend controls in two layers**:
  - Workspace budgets: a pooled cap per workspace, with up to four intervals (daily, weekly, monthly, lifetime). Limits must strictly decrease as the interval narrows.
  - Guardrail budgets: per member or per key, with a workspace default guardrail checked first.
- Budgets follow the **key creator**, not whoever uses the key.
- Admins can create "system keys" owned by the workspace instead of a person.
- Deleting the Default workspace disables every key not assigned to a workspace and leaves members with no landing workspace. It cannot be undone.

## API shape (management key)

- `GET /organization/members` (paginated, max 100): `id`, `email`, `first_name`, `last_name`, `role`.
- `GET/PATCH /organization/settings`: for example `is_filtered_model_catalog_enabled`.
- `POST/GET/PATCH/DELETE /workspaces[/{id}]`: `name`, `slug`, `description`, `default_text_model`, `default_image_model`, `default_provider_sort`, `disabled_server_tools`, observability flags. The response includes `default_guardrail_id` and `include_byok_in_budgets`.
- `/workspaces/{id}/members` (list, add, remove in bulk). Members added through the API inherit their org role (org admin becomes workspace admin).
- `/workspaces/{ref}/budgets[/{interval}]`: read with a workspace key, write only with an organization key.
- Guardrails and guardrail policies (`budget`, `moderation`, `model-availability`) with assignments to keys and members.

## Observable rules

- Budget check order on each paid request: guardrail budgets (workspace default, then member, then key), then workspace budgets, then the key's own limit. The first spent one blocks with `403`. `error.metadata` carries `limit_source`, `scope`, `limit_usd`, `spend_usd` and `resets_at`, and there is no `Retry-After`.
- Raising a limit does not reset its counter, and there is no manual reset.
- A member guardrail sums spend over every org-owned key that member created.
- A workspace-scoped management key can only touch its own workspace. Credits, org settings, members and workspace creation need an organization key.

## Mapping to TurboStarter (our observation, not OpenRouter's)

TurboStarter already has Better Auth organizations with members and roles. Workspaces have no equivalent today. An org-level balance with per-key caps covers the first version. Workspaces can come later as a grouping under the organization.

## Unknown

- Whether one person can belong to several organizations (guess: yes, the dashboard has an account switcher).
- Whether a workspace can ever have its own balance (the docs say billing is unified, so guess: no).
