# ADR 0006: Start with one gateway instance; add instances only after a tested invalidation patch

Status: Accepted. The multi-instance part must be proven by test before it ships.
Date: 2026-11-21

## Context

Bifrost's cluster mode is enterprise-only. The OSS schema has `cluster_config` (`transports/config.schema.json`), but no OSS transport code reads it.

In the OSS build every instance loads governance state (virtual keys, customers, budgets, rate limits) from the config store into memory and serves from memory. Writes through one instance's API update that instance's memory and the database only. The OSS server code says so directly: "A clustered deployment overrides this to also notify peers so their in-memory copies stay current" (`transports/bifrost-http/server/server.go`, around line 1553).

So with two OSS instances behind a load balancer, we expect:

- A virtual key created through instance A is unknown to instance B until B restarts or reloads. Requests that land on B get 401.
- Budget and rate-limit counters are per instance, so a cap is enforced N times over.

These are expectations from reading the code. They must be confirmed by test before we design around them.

## Options considered

1. **One instance, scaled vertically, with a standby.** No consistency problem. Bifrost is a Go service with low per-request overhead, so one instance should carry a lot of traffic. To be measured.
2. **Several instances, control plane fans every governance write out to all of them.** Works without patching Bifrost, but the control plane must know every instance, and an instance that misses a write stays wrong until restart.
3. **Several instances with our own invalidation patch:** after a governance write, the instance sends Postgres `NOTIFY`. Every instance `LISTEN`s and reloads the changed row from the config store. This is a small patch in our patch set (ADR 0008).
4. **Buy the enterprise build.** Out of scope.

Money is not part of this problem: the prepaid balance is in Postgres (ADR 0001), so all instances share it no matter which option we pick. Only customer-set caps (Bifrost budgets on keys) are affected by per-instance counters.

## Decision

Option 1 at launch. Option 3 when one instance is not enough, behind this test gate:

1. Run two instances against one Postgres config store.
2. Create a virtual key through instance A. Send a request with it to instance B. Record the result (expected without the patch: 401).
3. With the patch: the same request on B succeeds within one second of creation. Disabling the key on A makes B refuse it within one second.
4. Per-key budgets: document how far over a cap two instances can go, or move per-key caps into the ledger plugin too.

## Consequences

- The PLAN has an explicit step that runs this test and records the output (PLAN phase 7).
- Until option 3 ships, availability depends on one instance plus a standby, and a deploy is a short interruption unless the load balancer drains the old instance first.
