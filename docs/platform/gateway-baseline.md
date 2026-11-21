# Gateway baseline: upstream `transports/v2.6.0` in this repo

Recorded 2026-11-21 for Phase 1 of `PLAN.md`. This is what the imported gateway does before any rename, so Stage B can prove the rename changed nothing (Step 2.4, check 2).

The tree measured is commit `85447a0`: upstream at `172617bb` plus the two patches in `apps/gateway/patches/`. Neither patch touches Go code.

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

Not covered by this baseline:

- **Docker build (`pnpm gateway:up`).** The container this ran in cannot run `apk` inside a build, so the image was not built. Base images pulled fine.
- **Boot on Postgres.** Step 1.1 asks for it; only the SQLite default was booted.
- **A real model request.** No provider key, and the governance API is locked until a setup token is configured.
- **Outbound-call inventory (Step 1.3).** Two hosts were seen in the boot log and are listed below; the full inventory needs an egress-logging proxy and has not been done.

Vendor hosts seen at boot: `getbifrost.ai/datasheet` (fatal on first boot if unreachable) and `getbifrost.ai/mcp-library` (warning, retried in the background).

## Unit tests

Run with `apps/gateway/scripts/test-baseline.sh`, which executes `go test -json -count=1 -timeout 20m ./...` in each of the 15 `go.work` modules. The whole run took 30 minutes.

Every top-level test and its result is in [`gateway-baseline/results.tsv`](gateway-baseline/results.tsv) (module, package, test, result; 14,984 test rows and 123 package rows). The tables below summarise it. Subtests are not listed separately: a parent fails if any subtest fails.

### Counts per module

| Module                         | Tests pass | Tests fail | Tests skip | Packages pass | Packages fail | Packages without tests |
| ------------------------------ | ---------- | ---------- | ---------- | ------------- | ------------- | ---------------------- |
| `core`                         | 6253       | 51         | 206        | 50            | 6             | 4                      |
| `framework`                    | 3999       | 61         | 291        | 29            | 6             | 4                      |
| `plugins/compat`               | 26         | 0          | 0          | 1             | 0             | 0                      |
| `plugins/governance`           | 500        | 0          | 7          | 1             | 0             | 0                      |
| `plugins/jsonparser`           | 5          | 0          | 3          | 1             | 0             | 0                      |
| `plugins/logging`              | 374        | 0          | 2          | 1             | 0             | 0                      |
| `plugins/maxim`                | 20         | 0          | 3          | 1             | 0             | 0                      |
| `plugins/mocker`               | 8          | 0          | 0          | 1             | 0             | 0                      |
| `plugins/modelcatalogresolver` | 4          | 0          | 0          | 1             | 0             | 0                      |
| `plugins/otel`                 | 85         | 2          | 0          | 0             | 1             | 0                      |
| `plugins/prompts`              | 42         | 0          | 0          | 1             | 0             | 0                      |
| `plugins/routing`              | 601        | 0          | 0          | 5             | 0             | 0                      |
| `plugins/semanticcache`        | 88         | 0          | 71         | 1             | 0             | 0                      |
| `plugins/telemetry`            | 58         | 0          | 0          | 1             | 0             | 0                      |
| `transports`                   | 2171       | 52         | 1          | 3             | 3             | 2                      |
| **Total**                      | **14234**  | **166**    | **584**    | **97**        | **16**        | **10**                 |

### Failing tests by name

**`core`** (1)

| Test                      | Cause |
| ------------------------- | ----- |
| `TestValidateExternalURL` | dns   |

**`core/internal/mcptests`** (39)

| Test                                                         | Cause        |
| ------------------------------------------------------------ | ------------ |
| `TestAgent_Adapter_ResponsesFormat_ErrorHandling`            | mcp-stdio    |
| `TestAgent_Adapter_ResponsesFormat_STDIO`                    | mcp-stdio    |
| `TestAgent_MixedPermissions_AllAutoExecute`                  | mcp-stdio    |
| `TestAgent_MixedPermissions_MultipleSTDIOSamePermissions`    | mcp-stdio    |
| `TestAgent_MixedPermissions_PartialExecution`                | mcp-stdio    |
| `TestAgent_MixedPermissions_SpecificToolNames`               | mcp-stdio    |
| `TestAgent_MixedPermissions_WildcardAutoExecute`             | mcp-stdio    |
| `TestAgent_MultiConnection_AllTypes`                         | mcp-stdio    |
| `TestAgent_MultiConnection_ErrorInSTDIO`                     | mcp-stdio    |
| `TestAgent_MultiConnection_LargeParallelBatch`               | mcp-stdio    |
| `TestAgent_MultiConnection_SequentialAfterParallel`          | mcp-stdio    |
| `TestCodeMode_STDIO_ApprovedCallIgnoresAutoExecuteList`      | missing-file |
| `TestCodeMode_STDIO_CombinedFiltering`                       | missing-file |
| `TestCodeMode_STDIO_ComplexCodePatterns`                     | missing-file |
| `TestCodeMode_STDIO_EdgeCaseServer_BinaryAndEncoding`        | mcp-stdio    |
| `TestCodeMode_STDIO_EdgeCaseServer_EmptyAndNull`             | mcp-stdio    |
| `TestCodeMode_STDIO_EdgeCaseServer_ExtremeSizes`             | mcp-stdio    |
| `TestCodeMode_STDIO_EdgeCaseServer_NestedAndSpecialChars`    | mcp-stdio    |
| `TestCodeMode_STDIO_EdgeCaseServer_Unicode`                  | mcp-stdio    |
| `TestCodeMode_STDIO_ErrorTestServer_IntermittentAndHandling` | mcp-stdio    |
| `TestCodeMode_STDIO_ErrorTestServer_LargePayload`            | mcp-stdio    |
| `TestCodeMode_STDIO_ErrorTestServer_MalformedAndPartial`     | mcp-stdio    |
| `TestCodeMode_STDIO_ErrorTestServer_NetworkErrors`           | mcp-stdio    |
| `TestCodeMode_STDIO_MultiServer_AllServers`                  | mcp-stdio    |
| `TestCodeMode_STDIO_MultiServer_FilteringAcrossServers`      | mcp-stdio    |
| `TestCodeMode_STDIO_MultipleServers`                         | missing-file |
| `TestCodeMode_STDIO_ParallelTestServer_Concurrent`           | mcp-stdio    |
| `TestCodeMode_STDIO_ParallelTestServer_Sequential`           | mcp-stdio    |
| `TestCodeMode_STDIO_PreHookRenameIsAuthorized`               | missing-file |
| `TestCodeMode_STDIO_ServerFiltering`                         | missing-file |
| `TestCodeMode_STDIO_SingleServerBasicExecution`              | missing-file |
| `TestCodeMode_STDIO_ToolCallSingleServer`                    | missing-file |
| `TestCodeMode_STDIO_ToolFiltering`                           | missing-file |
| `TestCodeMode_STDIO_UnattendedCallEnforcesAutoExecuteList`   | missing-file |
| `TestHealthCheckSTDIOServerDropAndRecoverIn20Seconds`        | mcp-stdio    |
| `TestRefreshClientTools_WorksOverSTDIO`                      | mcp-stdio    |
| `TestToolsToExecute_EmptyArray`                              | mcp-stdio    |
| `TestToolsToExecute_Nil`                                     | mcp-stdio    |
| `TestToolsToExecute_Wildcard`                                | mcp-stdio    |

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

**`framework/openapimcp`** (1)

| Test                                      | Cause        |
| ----------------------------------------- | ------------ |
| `TestSynthesize_RepoSpecArgNamesAreValid` | missing-file |

**`framework/plugins`** (13)

| Test                                                | Cause         |
| --------------------------------------------------- | ------------- |
| `TestDynamicPluginLifecycle`                        | missing-file  |
| `TestDynamicPlugin_ConcurrentCalls`                 | missing-file  |
| `TestDynamicPlugin_ContextPropagation`              | missing-file  |
| `TestDynamicPlugin_GetNameNotEmpty`                 | missing-file  |
| `TestDynamicPlugin_NilConfig`                       | missing-file  |
| `TestDynamicPlugin_ShortCircuitNil`                 | missing-file  |
| `TestFilterPlugins`                                 | missing-file  |
| `TestLoadDynamicPlugin_DirectCall`                  | missing-file  |
| `TestLoadPluginWithOptionalHooks`                   | missing-file  |
| `TestLoadPlugins`                                   | missing-file  |
| `TestLoadPlugins_DisabledPlugin`                    | missing-file  |
| `TestLoadPlugins_MultiplePlugins`                   | missing-file  |
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

**`transports/v2/bifrost-http/handlers`** (37)

| Test                                                          | Cause         |
| ------------------------------------------------------------- | ------------- |
| `TestBifrostMCPAnnotations`                                   | missing-file  |
| `TestBifrostMCPBaseURL`                                       | missing-file  |
| `TestBifrostMCPDefaultSpecURL`                                | missing-file  |
| `TestBifrostMCPDeniedRoutesMatchSpec`                         | missing-file  |
| `TestBifrostMCPDestructiveMarkersSurviveBundling`             | missing-file  |
| `TestBifrostMCPEndpoint_BifrostURLSetsHostAndScheme`          | missing-file  |
| `TestBifrostMCPEndpoint_CancellationIsNotSharedAcrossCallers` | missing-file  |
| `TestBifrostMCPEndpoint_ForwardsCallerButNotCookies`          | missing-file  |
| `TestBifrostMCPEndpoint_ForwardsVirtualKeys`                  | missing-file  |
| `TestBifrostMCPEndpoint_Protocol`                             | missing-file  |
| `TestBifrostMCPEndpoint_RealAPIChain`                         | missing-file  |
| `TestBifrostMCPEndpoint_Routes`                               | missing-file  |
| `TestBifrostMCPEndpoint_RoutesTakeTheAPIChain`                | missing-file  |
| `TestBifrostMCPExpandsMetadataTemplates`                      | missing-file  |
| `TestBifrostMCPExposesEveryOperation`                         | missing-file  |
| `TestBifrostMCPFailedRestrictiveReloadUnloads`                | missing-file  |
| `TestBifrostMCPInferenceTools`                                | missing-file  |
| `TestBifrostMCPLoadFailureThenRecovery`                       | missing-file  |
| `TestBifrostMCPLoadFromFile`                                  | missing-file  |
| `TestBifrostMCPLoadsUppercaseSchemeURL`                       | missing-file  |
| `TestBifrostMCPReload`                                        | missing-file  |
| `TestBifrostMCPRequiresBifrostURL`                            | missing-file  |
| `TestBifrostMCPServesOnlyRoutedOperations`                    | missing-file  |
| `TestBifrostMCPStartDoesNotBlockServer`                       | missing-file  |
| `TestBifrostMCPToolsListAsksToolAccess`                       | missing-file  |
| `TestBifrostMCPToolsNeverStream`                              | missing-file  |
| `TestBifrostMCPUnroutedEnterpriseOperations`                  | missing-file  |
| `TestBifrostMCPWritesSwitch`                                  | missing-file  |
| `TestFetchURLSafeGoesThroughGlobalProxy`                      | ipv6-loopback |
| `TestWarpManagementCallIsBoundToItsCatalog`                   | missing-file  |
| `TestWarpManagementCallRunsAsTheDashboardUser`                | missing-file  |
| `TestWarpManagementCallUsesPublicBaseURL`                     | missing-file  |
| `TestWarpManagementCatalogOffersWhatTheBifrostMCPServes`      | missing-file  |
| `TestWarpManagementClientsCloseOnceNoTurnUsesThem`            | missing-file  |
| `TestWarpManagementLoadsSpecInBackground`                     | unexplained   |
| `TestWarpManagementStaleLoadDoesNotRetireANewerBinding`       | missing-file  |
| `TestWarpManagementUnloadClosesTheClient`                     | missing-file  |

**`transports/v2/bifrost-http/lib`** (12)

| Test                                                                          | Cause        |
| ----------------------------------------------------------------------------- | ------------ |
| `TestBasePathHelmSchemaPatternMatchesNormalize`                               | missing-file |
| `TestLoadConfigServesCatalogFeedsFromFixture`                                 | unexplained  |
| `TestSQLite_SourceOfTruthConfigJSON_KeepsInlineLimitsAcrossRestarts`          | dns          |
| `TestSQLite_VirtualKey_OwnerGovernedByProfile_EditedEntryKeepsTheKeysOwnLink` | dns          |
| `TestSQLite_VirtualKey_OwnerGovernedByProfile_ModelConfigBudgetNotRecreated`  | dns          |
| `TestSQLite_VirtualKey_OwnerGovernedByProfile_NestedRateLimitsNotRecreated`   | dns          |
| `TestSQLite_VirtualKey_OwnerGovernedByProfile_UnresolvedNewKeySkipsItsBudget` | dns          |
| `TestSQLite_VirtualKey_OwnerGuardFails_EditedEntryLeftAsStored`               | dns          |
| `TestSQLite_VirtualKey_OwnerMovedThroughAPI`                                  | dns          |
| `TestSQLite_VirtualKey_ProviderConfigBudgetByID`                              | dns          |
| `TestSQLite_VirtualKey_ProviderConfigBudgetByStoreAssignedID`                 | dns          |
| `TestSQLite_VirtualKey_RateLimitSharedWithAnotherKey`                         | dns          |

**`transports/v2/bifrost-http/server`** (3)

| Test                                                      | Cause        |
| --------------------------------------------------------- | ------------ |
| `TestReloadBifrostMCPClearedBifrostURLUnloads`            | missing-file |
| `TestReloadBifrostMCPOnSettingsChange`                    | missing-file |
| `TestStartBifrostMCPResolvesRelativeSpecAgainstConfigDir` | missing-file |

### What the causes mean

Causes were assigned from the first matching error line in each failing test's output, then spot-checked per group. They are not individually confirmed.

| Cause             | Tests | Meaning                                                                                                                                                                                                                                                                | Expected elsewhere                                                                |
| ----------------- | ----- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------- |
| `missing-file`    | 62    | The test reads an upstream path we did not import: `docs/openapi/openapi.json` (most of them), `examples/mcps/test-tools-server`, the example plugin source that `framework/plugins` compiles, `helm-charts/bifrost/values.schema.json`                                | Fails everywhere until the import scope changes (owner question)                  |
| `service-refused` | 44    | `framework/vectorstore` cannot reach Redis, Weaviate, Qdrant or Pinecone on localhost                                                                                                                                                                                  | Passes with upstream's `tests/docker-compose.yml` running, which was not imported |
| `mcp-stdio`       | 29    | `core/internal/mcptests` cases that start MCP servers over STDIO and get no tools back. Ten sibling tests in the same package say outright that `examples/mcps/test-tools-server` is missing, so this is most likely the same gap, but these 29 do not print that line | Probably fails everywhere until `examples/mcps` is imported and built             |
| `ipv6-loopback`   | 18    | The proxy test harness listens on `[::1]`, which this container does not have                                                                                                                                                                                          | Should pass on a normal host                                                      |
| `dns`             | 11    | The test resolves a public hostname (`api.openai.com`)                                                                                                                                                                                                                 | Should pass on a normal host                                                      |
| `unexplained`     | 2     | `TestWarpManagementLoadsSpecInBackground` (handlers) and `TestLoadConfigServesCatalogFeedsFromFixture` (lib). Not diagnosed                                                                                                                                            | Unknown                                                                           |

So of 166 failures, 91 point at files outside the import scope, 73 at this container, and 2 are open. None has been traced to a defect in the gateway itself, and none has been re-run on a host without these limits to confirm.

### Skips

584 tests skip. The reasons printed most often: Postgres not available (100), ClickHouse not available (92), a provider key such as `OPENAI_API_KEY` or `MISTRAL_API_KEY` not set, and a few that upstream skips unconditionally. Names are in `results.tsv`.

### Framework tests need backing services

Upstream starts them with `tests/docker-compose.yml`, which is outside the import scope: Postgres 16, Redis Stack 7.4 plus TLS, cluster and sentinel variants, Weaviate 1.32, Qdrant 1.16, a Pinecone local index and ClickHouse 24.8. Without them the `vectorstore` tests fail and the Postgres and ClickHouse tests skip, as recorded here.

### Skipped tests per package

| Package                                     | Skipped |
| ------------------------------------------- | ------- |
| `core`                                      | 1       |
| `core/internal/llmtests`                    | 11      |
| `core/internal/mcptests`                    | 150     |
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
| `plugins/governance`                        | 7       |
| `plugins/jsonparser`                        | 3       |
| `plugins/logging`                           | 2       |
| `plugins/maxim`                             | 3       |
| `plugins/semanticcache`                     | 71      |
| `transports/v2/bifrost-http/tests/realtime` | 1       |
