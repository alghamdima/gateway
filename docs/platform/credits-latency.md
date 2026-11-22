# Credits plugin: added latency

What the credits plugin (`apps/gateway/plugins/credits`) adds to one request, measured on 2026-11-22.

## What is on the request path

- `PreLLMHook`: one statement against Postgres (the hold). The request waits for it.
- `PostLLMHook`: a push onto an in-memory queue. A worker writes the settlement afterwards. The response waits for the ledger only when the queue is full (ADR 0009).

## Setup, and what it does not show

- `TestLatency_Report` in the plugin's tests, run with `CREDITS_LATENCY=1 CREDITS_TEST_DATABASE_URL=... go test -run TestLatency_Report -v`.
- The hooks are called directly, with the real store, as the `gateway_ledger` role. Not measured: the rest of the gateway, and the provider.
- Postgres 16 on the same 4-CPU container as the test, over loopback, pool of 20 connections. **A production database adds one network round trip to every `PreLLMHook`**; these numbers are a floor, not a forecast.
- The load is a closed loop with no provider time: each worker reserves and settles again immediately. Real requests spend hundreds of milliseconds at the provider between the two, so the same concurrency produces far fewer ledger writes per second in production. The rows at concurrency 10 and 50 show behaviour under saturation, not what a customer sees at that concurrency.
- 2,000 requests per row, and the table is a single run. The one-organization rows were run three times: `PreLLMHook` agreed within about 15 percent, the `PostLLMHook` tail did not (p99 at concurrency 10 ranged from 0.2 ms to 9 ms). The many-organization rows were run once.

## Results

| Concurrency | Organizations | `PreLLMHook` p50 | p95     | p99     | `PostLLMHook` p50 | p95     | p99     |
| ----------- | ------------- | ---------------- | ------- | ------- | ----------------- | ------- | ------- |
| 1           | 1             | 0.63 ms          | 1.2 ms  | 1.8 ms  | 0.02 ms           | 0.04 ms | 0.06 ms |
| 10          | 1 (shared)    | 3.5 ms           | 19.6 ms | 33.2 ms | 0.01 ms           | 0.04 ms | 0.2 ms  |
| 10          | 10            | 1.3 ms           | 2.5 ms  | 3.3 ms  | 0.01 ms           | 1.7 ms  | 2.6 ms  |
| 50          | 1 (shared)    | 21.7 ms          | 48.1 ms | 66.9 ms | 0.01 ms           | 34.7 ms | 53.2 ms |
| 50          | 50            | 4.6 ms           | 8.0 ms  | 29.2 ms | 0.01 ms           | 6.9 ms  | 29.3 ms |

## Reading

- Unloaded, the plugin adds well under 1 ms at the median and under 2 ms at p99, plus the network round trip to the database in production.
- **One organization's requests queue behind each other.** Every hold and every settlement updates that organization's single `credit_account` row, so they are serialized by its row lock. With all traffic on one organization the median hold is about five times slower than with the same traffic spread over many. This is the price of the atomic check in ADR 0001 and it is per organization: one busy customer does not slow another down.
- The `PostLLMHook` tail at concurrency 50 is the queue filling up and the caller writing its own settlement, which is the designed fallback. It appears here because the closed loop produces settlements faster than four workers write them.

## Not done

- No measurement through the whole gateway against a remote database. That belongs with the deployment (Phase 10), where the real network distance is known.
- No sustained-load run. Queue size (1024) and worker count (4) are defaults chosen without production data.
