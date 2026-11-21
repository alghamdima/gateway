# ADR 0002: Usage reaches the ledger through a gateway plugin, with log reconciliation as a backstop

Status: Accepted (charging for failed attempts needs the owner's input, see Open question)
Date: 2026-11-21

## Context

Each request's cost must reach the ledger exactly once. Bifrost computes cost per attempt (`framework/modelcatalog/pricing.go`, `CalculateCost` returns `float64` USD) and exposes it in three places: inside the plugin pipeline, through the logging plugin's `logs` table (`framework/logstore/tables.go`, column `cost`), and through `GET /api/logs`.

## Options considered

1. **Gateway plugin on `PostLLMHook`.** Sees every attempt's response and error as it happens. Bifrost's own governance plugin bills this way (`plugins/governance/tracker.go`).
2. **Poll `GET /api/logs`.** Rejected as the primary path. Logs are written asynchronously, and the logging plugin drops deferred usage updates when its queue or semaphore is full (`plugins/logging/main.go`, lines 1105, 1134, 1169). Polling also adds delay between spend and balance, which lets a customer overspend.
3. **Read the log store tables directly.** Same loss problem as option 2, and it couples the control plane to Bifrost's internal schema, which our rename script rewrites (ADR 0008).

## Decision

Option 1 as the primary path. Options 2 and 3 are used only for reconciliation.

- The `credits` plugin settles in `PostLLMHook`. For streams it settles on the final chunk, which is when Bifrost has the usage (`IsFinalChunk` in the governance tracker).
- **Idempotency key:** `billing_nonce:request_id:fallback_index:attempt_number`, the same tuple governance uses, because the attempt number restarts for each fallback provider and `request_id` can be caller-supplied. Governance keeps this key in an in-memory map for 5 minutes. We store it in the unique `idempotency_key` column of `credit_ledger_entry`, so a retry after a crash or on another instance cannot charge twice.
- Settlement writes are taken off the response path: the hook pushes the settlement onto a bounded in-process queue, and a worker writes them in small batches. The reservation (ADR 0001) keeps the balance safe while a settlement waits. On shutdown the queue is drained. If the queue is full, the hook writes synchronously instead of dropping.
- **Reconciliation job** (control plane, daily): sums `cost` from the gateway log store per organization and compares it with the ledger's `usage` entries for the same window, and lists reservations the reaper expired. Differences raise an alert. They are never auto-corrected, because adjustments to a customer balance are a human decision.

## Open question for the owner

When a provider call fails partway and the provider still charges for tokens it processed, Bifrost governance bills that attempt (`tracker.go`: "Bill for tokens the provider actually processed, even when the request ultimately failed"). OpenRouter appears to charge only the model that served the final response (research brief 04).

Until the owner decides, the plugin follows OpenRouter: only the attempt whose response is returned to the customer is charged. Failed attempts are still recorded as zero-amount entries with their provider cost in `metadata`, so the cost to us stays visible in margin reports.

## Consequences

- One code path for every endpoint type, because it lives in the plugin pipeline rather than in handlers.
- The plugin depends on Bifrost's hook contract, which is stable across the 2.x tags we read. Upgrades must run the plugin's tests (ADR 0008).
