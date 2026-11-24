# Brief 10: the Saudi market and data policy

Source: a deep-research report the owner commissioned on 2026-11-24 ("OpenRouter and the Saudi AI Gateway Opportunity", about 10,000 words, 100+ cited sources read that day). This brief keeps what the plan rests on. Claims here are the report's unless marked **checked**: those we read ourselves.

## What OpenRouter does

- **Regional gateways are EU and US only** (`eu.openrouter.ai`; Business and Enterprise plans). Regional traffic is decrypted and handled in-region and only reaches endpoints onboarded for that geography. No Saudi or Gulf region.
- **Every endpoint has a data policy**: zero data retention (ZDR), safety retention, or retention that may allow training. **An endpoint whose policy cannot be established is treated as retaining and training.**
- **Policy layers** (account, workspace, key, request) combine by intersection for allowlists and union for ZDR requirements. A request can tighten the policy, never loosen it.
- **No eligible endpoint means the request fails** (503 "no provider meets routing requirements"; 403 for a region or allowlist guardrail). The policy is never widened automatically.
- **Evidence per request**: `GET /api/v1/generation` returns `provider_name`, `model`, `data_region`, `is_byok`.
- **BYOK**: the customer declares ZDR and region for their own key; shared capacity can be allowed or denied per key.
- **Chargebacks**: the process is not documented. **Checked** against the official terms (updated 2026-08-31): no chargeback clause. Section 3.2 lets OpenRouter suspend or limit API credentials to protect the service; section 9 ends access on a terms violation without refunding unused credits; section 4.1 gives refunds of unused credits within 24 hours, platform fees excluded.

## Saudi supply (dates matter; all as of 2026-11-24)

- HUMAIN and Groq announced in-Kingdom deployment of gpt-oss-20b and gpt-oss-120b. HUMAIN says its AI cloud has been available since August 2026 and that ALLaM 34B is hosted end to end in the Kingdom. Commercial API access, failover and retention terms must be confirmed per deployment.
- Cloud regions live: Google Cloud Dammam, Oracle Jeddah and Riyadh, Azure Saudi Arabia East (Dammam). AWS describes its Saudi region as planned for December 2026.
- **HUMAIN Node** (limited preview) advertises one API with global, in-Kingdom or sovereign hosting: a possible supplier and a competitor.

## Saudi rules

- PDPL Article 29 and the SDAIA transfer regulation allow transfers outside the Kingdom under conditions and safeguards.
- The NCA cloud controls (CCC 2:2024) apply to government and critical-infrastructure tenants and the cloud providers serving them.
- So: agree the customer's data classification and residency boundary before quoting a sovereign service, and include support access, telemetry, backups and subprocessors in that boundary.

## The report's launch scope

- **Developers**: OpenAI-compatible API, a small catalog with prices, fallback, uniform errors, self-serve keys, prepaid billing, quotas and key budgets, request logs with provider attribution, a status page.
- **Saudi enterprises**: a dedicated Saudi hostname on an isolated in-Kingdom path; at least one contracted Arabic-capable model and a second endpoint or an explicit fail-closed policy; tenant allowlists for region, provider and model; strict ZDR and no training; payload-free logs; a DPA, subprocessor list, retention schedule and incident plan; invoice and purchase-order billing; a per-request evidence record (policy version, route, region, attempts, outcome); SSO when the first procurement asks.
- **Before the first sovereign workload**: walk the whole request path with the customer (ingress, inference, logs, metrics, support, backups), turn off payload logging, persistent caches, uploads and external telemetry on that path, and test a local success, region and model denials, a provider outage, an attempted global fallback and a mid-stream error.
