# ADR 0016: One frontend, and the gateway code is ours

- Status: Accepted (owner, 2026-11-25)
- Date: 2026-11-25
- Replaces: ADR 0007 (the gateway admin UI is internal only), ADR 0008 (upstream tag plus patches)
- Relates to: ADR 0011 (staff roles), ADR 0014 (no content storage)

## Context

The gateway (Bifrost v2.6.0) came with its own admin UI: a React app of about 1,400 files, embedded in the binary and served on the same port as the API. ADR 0007 kept it for staff, behind the private network. ADR 0008 kept `apps/gateway` reproducible as "upstream tag plus patches", so that upgrades stayed a re-import.

Both had a cost the owner did not want. Two frontends meant two stacks, two sets of languages and two brands to rename, for a UI no customer sees. And every change to the gateway had to be written as a patch against code we were not allowed to edit.

The owner's decisions (2026-11-25): the product has one frontend; we do not stay dependent on the gateway's upstream, the owner's team develops the whole product; the repository keeps only what the product needs; the engine stays Bifrost, with LiteLLM as a reference when fixing a weakness.

## Decision

1. **One frontend: `apps/web`.** The gateway serves its API and nothing else. What staff did in the gateway's UI is done in the admin, through the gateway's management API, which stays:
   - providers and their keys: `/admin/providers` (`/api/providers`, `/api/providers/{provider}/keys`);
   - the request log: `/admin/requests` (`/api/logs`), which shows what happened to a request and never its content;
   - forcing a price sync: a button on the providers page (`/api/pricing/force-sync`);
   - everything else the UI configured is set in `apps/gateway/compose/config.json` at deploy.
2. **The gateway code is ours.** `apps/gateway` is edited directly. `MODIFICATIONS.md` lists every change to a file that came from upstream, as the Apache licence requires; `LICENSE`, `THIRD_PARTY_NOTICES.md` and `UPSTREAM` stay. There is no import script, no patch directory and no CI step that re-imports the tag.
3. **Features that are not part of the product are removed one at a time**, each with its tests, never in bulk: the Go code is interwoven and each removal has to be proven by the suite.

## Consequences

- The gateway builds without Node, and its tree is a third smaller.
- A fix from upstream, a security fix included, is picked by hand. Nobody is told when upstream publishes one: watching its releases is now a task of ours.
- A feature upstream ships with a screen has no screen here until we build one.
- The admin surface of the gateway (`/api/*`) is still powerful and must stay off the public network, as ADR 0007 required: the public load balancer forwards the inference routes only. That part of ADR 0007 holds.
- The rename to the product name (Stage B) is smaller: no UI text, no UI assets.

## What was checked

On the built gateway: `GET /` and the old UI paths answer 404, `/health` 200, `/api/providers` 200 with the admin credential and 401 without, `/v1/models` 401 without a key. The admin pages were driven in a browser against it.
