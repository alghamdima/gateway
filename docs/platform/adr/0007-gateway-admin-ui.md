# ADR 0007: The gateway admin UI is internal only

Status: Accepted
Date: 2026-11-21

## Context

Bifrost ships an admin UI (built from `ui/`, embedded at `transports/bifrost-http/ui`) on the same port as the inference API. It can see and change everything: providers and their API keys, virtual keys of every customer, budgets, logs with request and response content, and plugin settings.

Customers use the TurboStarter dashboard (ADR 0004) and never need this UI.

## Options considered

1. **Expose it to customers**, scoped per customer. Rejected: it is single-tenant by design. Scoping it would be a rewrite, and it would compete with our dashboard.
2. **Expose it to our staff on the public domain behind login.** Rejected: the admin surface and the public inference API would share one hostname, so any auth bypass in the admin UI is internet-facing.
3. **Internal only.** The public load balancer forwards only the inference routes (`/v1/*` and the provider-compatible paths we support). The admin UI and the `/api/*` management routes are reachable only on the private network (VPN or bastion), with Bifrost's own admin auth also turned on.

## Decision

Option 3.

- Public ingress has an allowlist of inference paths. Everything else returns 404 at the edge.
- The control plane reaches `/api/governance/*` over the private network with an admin credential stored as a secret.
- The UI stays in the build, renamed and white-labeled (Phase 5a), because staff use it for debugging and provider setup. It is not a customer surface.

## Consequences

- Customer-facing features that Bifrost's UI already has (logs, usage charts) must be rebuilt in the dashboard against our own APIs.
- One more ingress rule to test: an end-to-end check that `GET /` and `/api/*` on the public host return 404.
