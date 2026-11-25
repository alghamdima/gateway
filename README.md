# Lemru

One API for many AI models, with prepaid credits and control over where your data goes.

This monorepo holds the control plane (`apps/web`, `packages/*`: sign-up, organizations, credits, API keys, usage, data policy) and the gateway that serves the API (`apps/gateway`).

- Plan, architecture and decisions: `docs/platform/` (`PLAN.md`, `ARCHITECTURE.md`, `adr/`).
- Commands and conventions: `AGENTS.md`.
- Licenses: `LICENSE.md` for the SaaS template the control plane is built on; `apps/gateway/LICENSE`, `NOTICE` and `THIRD_PARTY_NOTICES.md` for the gateway.
