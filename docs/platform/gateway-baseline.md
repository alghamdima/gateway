# Gateway baseline: upstream `transports/v2.6.0` in this repo

Recorded 2026-11-21 for Phase 1 of `PLAN.md`. This is what the imported gateway does before any rename, so Stage B can prove the rename changed nothing (Step 2.4, check 2).

The build and boot checks were taken on commit `85447a0` (upstream at `172617bb` plus the two patches in `apps/gateway/patches/`, neither of which touches Go code). The unit-test baseline was last retaken after the import scope was widened and the Maxim plugin was removed (patch `0003`), which leaves 14 Go modules.

## Environment

| Item                    | Value                                                                                              |
| ----------------------- | -------------------------------------------------------------------------------------------------- |
| Machine                 | Linux x86-64 cloud container, 4 cores, 15 GB RAM                                                   |
| Go                      | 1.27.4 (toolchain download), CGO on, gcc                                                           |
| Node / npm              | 22.23.5 / npm from that release (upstream pins Node 26 in `.nvmrc` and Node 25 in its Dockerfiles) |
| Provider credentials    | none; the test environment is built with `env -i`                                                  |
| Network during tests    | none; `GOPROXY=off`, no proxy variables, and the container has no direct egress                    |
| DNS                     | does not resolve public names                                                                      |
| IPv6 loopback (`[::1]`) | not available                                                                                      |
| Backing services        | none running (no Postgres, Redis, Weaviate, Qdrant, ClickHouse)                                    |

These limits matter for reading the failures below. A machine with DNS, IPv6 loopback and the backing services will see fewer.

## Build and boot

| Check                                                         | Command                                                   | Result                                                                                                    |
| ------------------------------------------------------------- | --------------------------------------------------------- | --------------------------------------------------------------------------------------------------------- |
| UI typecheck at a 2 GB heap, pristine UI outside the monorepo | `NODE_OPTIONS=--max-old-space-size=2048 npx tsc --noEmit` | fails: `JavaScript heap out of memory`                                                                    |
| Same at 4 GB                                                  | `NODE_OPTIONS=--max-old-space-size=4096 npx tsc --noEmit` | exit 0, 5942 files, 2.89 GB used, 87 s                                                                    |
| UI build inside the monorepo, before patch 0002               | `npm run build-enterprise`                                | exit 2: 4 `TS2322` errors from a second `@types/react`                                                    |
| Full build with both patches                                  | `make build LOCAL=1`                                      | exit 0 in 284 s, `tmp/bifrost-http` 175 MB, static, UI embedded                                           |
| Boot with default config                                      | `tmp/bifrost-http -app-dir <empty dir>`                   | fatal after 7 s: cannot fetch `https://getbifrost.ai/datasheet` and has no cached pricing                 |
| Boot with a local pricing file                                | `config.json` below                                       | ready in about 9 s                                                                                        |
| Health                                                        | `curl -i localhost:8080/health`                           | `200`, `{"components":{"db_pings":"ok"},"status":"ok"}`                                                   |
| UI                                                            | `curl localhost:8080/` and headless Chromium              | `200 text/html`, 11 kB; JS bundle `200`; browser lands on `/login` and renders the first-run setup screen |

The config used for the boot, with SQLite stores created in the app directory:

```json
{
  "framework": {
    "pricing": {
      "pricing_url": "file://<repo>/apps/gateway/framework/modelcatalog/datasheet/testdata/pricing.json",
      "model_parameters_url": "file://<repo>/apps/gateway/framework/modelcatalog/datasheet/testdata/model-parameters.json"
    }
  }
}
```

### Added in the second pass (same day, tree at `709c53f`)

| Check                                                                                                    | Command                                                               | Result                                                                                                     |
| -------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------- |
| `go vet`, all 15 modules (14 after patch `0003`)                                                         | `go vet ./...` per module                                             | exit 0 everywhere, no output                                                                               |
| Native boot on Postgres 16 with the real datasheet (5.0 MB, fetched once from `getbifrost.ai/datasheet`) | `config.json` with `config_store` and `logs_store` of type `postgres` | ready in 32 s, `/health` 200, 91 tables, 8,108 rows in `governance_model_pricing`, no SQLite files written |
| Docker image                                                                                             | `docker build -f transports/Dockerfile.local` (see the caveat below)  | exit 0 in 378 s, 496 MB image; the UI stage runs `npm run build-enterprise` on `node:25-alpine` and passes |
| Compose stack                                                                                            | `docker compose --profile gateway up -d --wait`                       | exit 0 in 47 s; `db`, `mailpit`, `gateway` healthy, `gateway-db` exited 0                                  |
| Database separation                                                                                      | `psql` in the `db` container                                          | databases `core` and `gateway`; `gateway` has 91 tables, `core` has 0                                      |
| Re-run                                                                                                   | the same `up` again                                                   | exit 0, `gateway-db` exits 0 without creating anything                                                     |
| UI under compose                                                                                         | headless Chromium                                                     | setup screen asks for the setup token, as configured                                                       |

Caveat on the Docker rows: this container reaches the internet only through a TLS-inspecting proxy, so the image was built from a copy of `Dockerfile.local` with two extra lines after each `FROM` (copy the proxy CA, set `SSL_CERT_FILE` and `NODE_EXTRA_CA_CERTS`) and with `--network host`. The gateway container also cannot resolve DNS here, so the compose run mounted a local pricing file. The Dockerfile and compose file in the repo were not run exactly as committed. That still has to be done once on a normal machine with `pnpm gateway:up`.

Still not covered:

- **`pnpm gateway:up` exactly as committed**, for the reason above.
- **First boot with default pricing.** It needs `getbifrost.ai` over the gateway's own DNS lookup, which this container cannot do. Verified here only that the boot is fatal without it.
- **A real model request.** No provider key. Inference also requires a virtual key by default (`401 virtual_key_required`), and creating one needs an admin account.
- **CI.** `.github/workflows/gateway.yml` is written but has never run: it triggers on pull requests and on `main`.

## Outbound calls (Step 1.3)

Method: the gateway ran in a mount namespace whose `/etc/resolv.conf` pointed at a logging DNS stub that answers every name with an unroutable address, with no proxy variables set. Every hostname the process tried to resolve was recorded. This shows which hosts are contacted, not the full URLs; URLs are from the gateway's own log lines and the source.

| Host                                                   | When                                   | Seen at runtime                        | Source                             | Recommended disposition                                 |
| ------------------------------------------------------ | -------------------------------------- | -------------------------------------- | ---------------------------------- | ------------------------------------------------------- |
| `getbifrost.ai/datasheet`                              | boot, then every 24 h                  | yes; fatal on first boot if it fails   | `framework/modelcatalog/datasheet` | our host or a file shipped in the image (`pricing_url`) |
| `getbifrost.ai/datasheet/model-parameters`             | boot                                   | yes (same host)                        | same                               | our host or a file (`model_parameters_url`)             |
| each configured provider, for example `api.openai.com` | boot (model listing) and every request | yes, with a fake OpenAI key configured | provider config                    | expected; this is the product                           |
| `www.getbifrost.ai/announcements/rss.xml`              | not seen before login                  | no                                     | Go source                          | off                                                     |
| `www.getbifrost.ai/schema`                             | never fetched; only named in a warning | no                                     | config validator                   | rename only                                             |

Limits of this inventory: 45 s of a default boot, then a configured boot plus 75 s idle and the UI login page in a browser (which made no external request). Not exercised: the UI after login, a successful inference request, any plugin that is off by default (OTel, semantic cache, Maxim), and `framework/warp`. Those need an admin account and provider keys.

## Unit tests

Run with `apps/gateway/scripts/test-baseline.sh` on the tree with patch `0003` applied. It executes `go test -json -count=1 -timeout 20m ./...` in each of the 14 `go.work` modules with no credentials and no network. The run took 11 minutes.

Every top-level test and its result is in [`gateway-baseline/results.tsv`](gateway-baseline/results.tsv) (module, package, test, result; 15,547 test rows and 122 package rows). Subtests are not listed separately: a parent fails if any subtest fails.

### Counts per module

| Module                         | Tests pass | Tests fail | Tests skip | Packages pass | Packages fail | Packages without tests |
| ------------------------------ | ---------- | ---------- | ---------- | ------------- | ------------- | ---------------------- |
| `core`                         | 6413       | 12         | 85         | 51            | 5             | 4                      |
| `framework`                    | 4012       | 48         | 291        | 30            | 5             | 4                      |
| `plugins/compat`               | 26         | 0          | 0          | 1             | 0             | 0                      |
| `plugins/credits`              | 32         | 0          | 20         | 1             | 0             | 0                      |
| `plugins/governance`           | 506        | 0          | 1          | 1             | 0             | 0                      |
| `plugins/jsonparser`           | 5          | 0          | 3          | 1             | 0             | 0                      |
| `plugins/logging`              | 374        | 0          | 2          | 1             | 0             | 0                      |
| `plugins/mocker`               | 8          | 0          | 0          | 1             | 0             | 0                      |
| `plugins/modelcatalogresolver` | 4          | 0          | 0          | 1             | 0             | 0                      |
| `plugins/otel`                 | 85         | 2          | 0          | 0             | 1             | 0                      |
| `plugins/prompts`              | 42         | 0          | 0          | 1             | 0             | 0                      |
| `plugins/routing`              | 601        | 0          | 0          | 5             | 0             | 0                      |
| `plugins/semanticcache`        | 88         | 0          | 71         | 1             | 0             | 0                      |
| `plugins/telemetry`            | 58         | 0          | 0          | 1             | 0             | 0                      |
| `transports`                   | 2808       | 1          | 1          | 5             | 1             | 2                      |
| **Total**                      | **15062**  | **63**     | **474**    | **101**       | **12**        | **10**                 |

### Failing tests by name

**`core`** (1)

| Test                      | Cause |
| ------------------------- | ----- |
| `TestValidateExternalURL` | dns   |

**`core/mcp`** (1)

| Test                                    | Cause         |
| --------------------------------------- | ------------- |
| `TestBuildTLSHTTPClientUsesGlobalProxy` | ipv6-loopback |

**`core/network`** (7)

| Test                                                            | Cause         |
| --------------------------------------------------------------- | ------------- |
| `TestDefaultHTTPClientFactoryRouting`                           | ipv6-loopback |
| `TestFactoryFasthttpProxyReachesIPv6ProxyAndHonoursNoProxyList` | ipv6-loopback |
| `TestGRPCDialersConnectUnixSockets`                             | ipv6-loopback |
| `TestHTTPClientFactoryClientsAreLive`                           | ipv6-loopback |
| `TestHTTPClientFactoryNeverProxiesLocalOrMetadata`              | ipv6-loopback |
| `TestHTTPClientFactoryProxyMatrix`                              | ipv6-loopback |
| `TestPolicyTransport_ProxiedTunnelIsBoundToTheCheckedAddress`   | ipv6-loopback |

**`core/providers/bedrock`** (1)

| Test                              | Cause         |
| --------------------------------- | ------------- |
| `TestBedrockTransportProxyMatrix` | ipv6-loopback |

**`core/providers/utils`** (2)

| Test                       | Cause         |
| -------------------------- | ------------- |
| `TestProxyRoutingMatrix`   | ipv6-loopback |
| `TestWebSocketProxyMatrix` | ipv6-loopback |

**`framework/modelcatalog`** (1)

| Test                                 | Cause         |
| ------------------------------------ | ------------- |
| `TestFetchMCPLibraryUsesGlobalProxy` | ipv6-loopback |

**`framework/oauth2`** (1)

| Test                                       | Cause         |
| ------------------------------------------ | ------------- |
| `TestDiscoverOAuthMetadataUsesGlobalProxy` | ipv6-loopback |

**`framework/plugins`** (1)

| Test                                                | Cause         |
| --------------------------------------------------- | ------------- |
| `TestProxiedPluginDownloadClientHonoursGlobalProxy` | ipv6-loopback |

**`framework/vectorstore`** (44)

| Test                                                      | Cause           |
| --------------------------------------------------------- | --------------- |
| `TestNewRedisStore_ConfiguresClusterTLSClient`            | service-refused |
| `TestNewRedisStore_ConfiguresStandaloneTLSClient`         | service-refused |
| `TestPineconeStore_CreateNamespaceRejectsDimensionChange` | service-refused |
| `TestPineconeStore_Delete`                                | service-refused |
| `TestPineconeStore_DeleteMissingIDSucceeds`               | service-refused |
| `TestPineconeStore_ErrorHandling`                         | service-refused |
| `TestPineconeStore_GetNearestScoresCosineSimilarity`      | service-refused |
| `TestPineconeStore_Integration`                           | service-refused |
| `TestPineconeStore_SemanticCacheWorkflow`                 | service-refused |
| `TestPineconeStore_VectorSearch`                          | service-refused |
| `TestQdrantStore_CreateNamespaceRejectsDimensionChange`   | service-refused |
| `TestQdrantStore_DeleteFromMissingCollectionSucceeds`     | service-refused |
| `TestQdrantStore_DeleteMissingIDSucceeds`                 | service-refused |
| `TestQdrantStore_DimensionHandling`                       | service-refused |
| `TestQdrantStore_ErrorHandling`                           | service-refused |
| `TestQdrantStore_Filtering`                               | service-refused |
| `TestQdrantStore_GetNearestScoresCosineSimilarity`        | service-refused |
| `TestQdrantStore_Integration`                             | service-refused |
| `TestQdrantStore_VectorSearch`                            | service-refused |
| `TestRedisConfig_Validation`                              | service-refused |
| `TestRedisStore_CompleteUseCases`                         | service-refused |
| `TestRedisStore_CreateNamespaceRejectsDimensionChange`    | service-refused |
| `TestRedisStore_DeleteOperations`                         | service-refused |
| `TestRedisStore_ErrorHandling`                            | service-refused |
| `TestRedisStore_FilteringScenarios`                       | service-refused |
| `TestRedisStore_GetNearestScoresCosineSimilarity`         | service-refused |
| `TestRedisStore_Integration`                              | service-refused |
| `TestRedisStore_NamespaceDimensionHandling`               | service-refused |
| `TestRedisStore_VectorReadGoesPastTheSearchLimit`         | service-refused |
| `TestRedisStore_VectorSearch`                             | service-refused |
| `TestVectorStoreAddReplacesExistingID`                    | service-refused |
| `TestVectorStoreDeleteAllHonoursItsFilter`                | service-refused |
| `TestVectorStoreGetAllPagesAFilteredReadWithVectors`      | service-refused |
| `TestVectorStoreGetAllReturnsVectorsOnRequest`            | service-refused |
| `TestVectorStoreListNamespaces`                           | service-refused |
| `TestWeaviateStore_CompleteUseCases`                      | service-refused |
| `TestWeaviateStore_CreateNamespaceRejectsDimensionChange` | service-refused |
| `TestWeaviateStore_DeleteAllPastTheBatchLimit`            | service-refused |
| `TestWeaviateStore_DeleteMissingIDSucceeds`               | service-refused |
| `TestWeaviateStore_FilteringScenarios`                    | service-refused |
| `TestWeaviateStore_GetNearestScoresCosineSimilarity`      | service-refused |
| `TestWeaviateStore_Integration`                           | service-refused |
| `TestWeaviateStore_MissingClassIsNotFound`                | service-refused |
| `TestWeaviateStore_NamespaceDimensionHandling`            | service-refused |

**`framework/webhooks`** (1)

| Test                                             | Cause         |
| ------------------------------------------------ | ------------- |
| `TestDeliverGoesThroughGlobalProxyKeepingPolicy` | ipv6-loopback |

**`plugins/otel`** (2)

| Test                                | Cause         |
| ----------------------------------- | ------------- |
| `TestOtelClientGRPCUsesGlobalProxy` | ipv6-loopback |
| `TestOtelClientHTTPUsesGlobalProxy` | ipv6-loopback |

**`transports/v2/bifrost-http/handlers`** (1)

| Test                                     | Cause         |
| ---------------------------------------- | ------------- |
| `TestFetchURLSafeGoesThroughGlobalProxy` | ipv6-loopback |

### What the causes mean

All 63 failures come from this container, not from the tree. Each group was confirmed from its error output.

| Cause             | Tests | Meaning                                                                                         | Expected elsewhere                             |
| ----------------- | ----- | ----------------------------------------------------------------------------------------------- | ---------------------------------------------- |
| `service-refused` | 44    | `framework/vectorstore` cannot reach Redis, Weaviate, Qdrant or Pinecone on localhost           | Passes with `tests/docker-compose.yml` running |
| `ipv6-loopback`   | 18    | The proxy test harness listens on `[::1]` (`listen tcp6 [::1]:0: address family not supported`) | Should pass on a normal host                   |
| `dns`             | 1     | `TestValidateExternalURL` resolves `api.openai.com`                                             | Should pass on a normal host                   |

None of the 63 has been re-run on a host without these limits.

### This baseline depends on one DNS name

`transports/.../lib` `TestLoadConfigServesCatalogFeedsFromFixture` and six `plugins/governance` tests need the default pricing host, `getbifrost.ai`, to resolve (the governance tests also download the datasheet from it). The cloud container answers DNS only for hosts on its network allowlist, and that host was added before this run, so they pass here. On a machine where it does not resolve, the first fails, its package runs into the 20 minute timeout, and the six skip. That is what the first baseline recorded as "unexplained".

### History of this baseline

The first run, before the import scope was widened (commit `85447a0`), had 14,234 passing, 166 failing and 584 skipped. 103 of those failures were tests reading upstream paths we had not imported. Importing `docs/openapi`, `examples/mcps`, `examples/plugins/hello-world`, `examples/configs/withlocalpricingfiles`, `helm-charts/bifrost/values.schema.json` and `tests/docker-compose.yml` (282 files, commit `d043173`) and building the MCP test servers removed all 103. No test that passed in the first run fails in this one.

Two things the first version of this report got wrong: it attributed 10 `transports/.../lib` failures to DNS and left 2 unexplained, when all 12 were the missing OpenAPI spec; and the commit message of `d043173` says 336 files were added, when the number is 282.

### After removing the Maxim plugin

Patch `0003` and the import script drop `plugins/maxim` and the UI that configured it. The run after that differs from the one before in exactly the plugin's own rows (20 passing tests, 3 skipped, 1 package): 15,030 pass, 63 fail, 454 skip. Nothing else moved.

### Our own module

`plugins/credits` is ours, not upstream's. In this baseline it is recorded without a database: 32 tests pass and 20 skip: 19 need Postgres or the built gateway, and one is the latency report, which runs only on request. Those run in CI in their own step, with both.

### Stability

On the first tree the whole suite was run twice. The two result sets differed in one test: `core` `TestListAllModelsIncludesEveryPageOfASlowLargeProvider` passed, then failed with `context deadline exceeded`, then passed 3 times out of 3 alone. It is timing-sensitive under load. `scripts/test.sh` therefore re-runs a regressed test on its own before counting it: once at first, up to 3 times since 2026-11-23, when the same test failed the full run and one solo re-run in CI. Measured alone on this container it fails 3 times in 50 (each page sleeps 300 ms against a 500 ms deadline). The current tree has been run once.

### Skips

454 tests skip. The reasons printed most often: Postgres not available, ClickHouse not available, a provider key such as `OPENAI_API_KEY` not set, and a few that upstream skips unconditionally. Names are in `results.tsv`.

| Package                                     | Skipped |
| ------------------------------------------- | ------- |
| `core`                                      | 1       |
| `core/internal/llmtests`                    | 11      |
| `core/internal/mcptests`                    | 29      |
| `core/network`                              | 1       |
| `core/providers/anthropic`                  | 1       |
| `core/providers/azure`                      | 1       |
| `core/providers/baseten`                    | 1       |
| `core/providers/bedrock`                    | 1       |
| `core/providers/bedrockmantle`              | 1       |
| `core/providers/byteplus`                   | 1       |
| `core/providers/cerebras`                   | 1       |
| `core/providers/cloudflare`                 | 1       |
| `core/providers/cohere`                     | 1       |
| `core/providers/databricks`                 | 1       |
| `core/providers/deepseek`                   | 1       |
| `core/providers/elevenlabs`                 | 1       |
| `core/providers/fireworks`                  | 1       |
| `core/providers/gemini`                     | 1       |
| `core/providers/githubcopilot`              | 1       |
| `core/providers/groq`                       | 1       |
| `core/providers/huggingface`                | 1       |
| `core/providers/kimchi`                     | 1       |
| `core/providers/machgen`                    | 1       |
| `core/providers/minimax`                    | 1       |
| `core/providers/mistral`                    | 4       |
| `core/providers/nebius`                     | 1       |
| `core/providers/ollama`                     | 1       |
| `core/providers/openai`                     | 1       |
| `core/providers/openrouter`                 | 1       |
| `core/providers/parasail`                   | 1       |
| `core/providers/perplexity`                 | 1       |
| `core/providers/replicate`                  | 1       |
| `core/providers/runware`                    | 1       |
| `core/providers/runway`                     | 1       |
| `core/providers/sagemaker`                  | 1       |
| `core/providers/sarvam`                     | 1       |
| `core/providers/sgl`                        | 1       |
| `core/providers/soniox`                     | 2       |
| `core/providers/typesafe`                   | 1       |
| `core/providers/vertex`                     | 1       |
| `core/providers/vllm`                       | 1       |
| `core/providers/wafer`                      | 1       |
| `core/providers/xai`                        | 1       |
| `framework/configstore`                     | 34      |
| `framework/configstore/tables`              | 1       |
| `framework/logstore`                        | 179     |
| `framework/postgresconn`                    | 14      |
| `framework/queue`                           | 56      |
| `framework/vectorstore`                     | 6       |
| `framework/warp`                            | 1       |
| `plugins/governance`                        | 1       |
| `plugins/jsonparser`                        | 3       |
| `plugins/logging`                           | 2       |
| `plugins/semanticcache`                     | 71      |
| `transports/v2/bifrost-http/tests/realtime` | 1       |

### Backing services

Upstream starts them with `tests/docker-compose.yml`, now imported: Postgres 16, Redis Stack 7.4 plus TLS, cluster and sentinel variants, Weaviate 1.32, Qdrant 1.16, a Pinecone local index and ClickHouse 24.8. This baseline was taken without them, so the `vectorstore` tests fail and the Postgres and ClickHouse tests skip. A run with the services up has not been done.
