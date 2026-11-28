# ADR 0015: Data policy per organization, enforced by the gateway's key allowlists

- Status: Accepted (owner, 2026-11-24). The default policy for new organizations is a placeholder until the owner sets it.
- Date: 2026-11-24
- Relates to: ADR 0004 (key model), ADR 0007 (gateway internal only), ADR 0014 (no content storage), research brief 10

## Context

The owner's product direction: a gateway for the Saudi market where the customer decides where their data may go (in the Kingdom, or anywhere) and whether a provider may keep it or train on it, and where we prove what happened on each request. OpenRouter does this for the EU and the US only (brief 10).

What the gateway (Bifrost v2.6.0) has, read in the code:

- A virtual key lists the provider keys it may use: `provider_configs[].key_ids`, deny by default (`framework/configstore/tables/virtualkey.go`, `handlers/governance.go`). The gateway itself refuses any other provider key.
- Each provider key has a stable `key_id` (`framework/configstore/tables/key.go`), and keys for Azure, Bedrock and Vertex carry their region or endpoint.
- A custom provider takes any OpenAI-compatible base URL (`CustomProviderConfig`): a local partner or a private deployment.
- The log row records the provider key that served the request (`selected_key_id`).
- Nothing describes a provider's data terms: no retention, training or processing-country attribute anywhere in core, framework or governance.

## Decision

1. **An endpoint is one provider key in the gateway.** Our database holds its data terms (`provider_endpoint`): processing region, retention (zero, a number of days, or unknown), training (no, yes, or unknown), the agreement it rests on, and the source and date of that knowledge. Staff maintain it in the admin console; every change is audited.
2. **Unknown is the worst case.** An endpoint whose retention or training is unknown counts as retaining and training. It can never satisfy a zero-retention or no-training policy.
3. **An organization has one data policy**: require zero retention, forbid training, allowed regions, and optionally allowed providers. The default allows everything.
4. **The gateway enforces it.** The control plane computes the endpoints that satisfy the policy and writes them to every key of the organization as `provider_configs[].key_ids`, replacing `allow_all_providers`. A new key gets them at creation. A change to the policy or to an endpoint's terms is pushed to every affected key.
5. **Fail closed.** If no endpoint satisfies the policy, the organization's keys can call nothing and the request is refused (403 `provider_blocked`, before any hold or provider call). The policy is never widened automatically. Each policy has a version and the version its keys are known to carry: a push that fails, and a change to an endpoint's terms, leave the policy marked as not applied until every key is written again. The owner sees that on the policy page, saving again retries, and a scheduled call to `GET /api/internal/data-policy` pushes whatever is still pending. Until then the keys keep the allowlist they had.
6. **Evidence per request.** The generation record shows the endpoint that served the request, its region and terms, and the policy version in force.
7. **One codebase, two deployments.** The global deployment and a Saudi deployment (in-Kingdom servers and database, in-Kingdom endpoints only, no external telemetry) differ in configuration, not in code.

## Consequences

- The data terms are only as true as the agreements behind them. Each endpoint records its source; an endpoint without one stays unknown.
- "In the Kingdom" is a claim about the whole path: the Saudi deployment must itself run in the Kingdom, including its database, logs and backups.
- The allowlist is per key in the gateway, so changing a policy touches every key of the organization. The push must be idempotent and report partial failure.
- Fallback stays inside the allowlist: the gateway only falls back to provider keys the virtual key may use.
- Request-level tightening (a customer asking for stricter terms on one request) is not built; the organization policy is the only layer for now.
