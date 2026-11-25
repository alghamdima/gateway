# Modifications

This directory started as a copy of Bifrost (https://github.com/maximhq/bifrost), which is licensed under the Apache License, Version 2.0. The upstream licence and notices are kept unchanged in `LICENSE` and `THIRD_PARTY_NOTICES.md`. The revision the copy was taken from is recorded in `UPSTREAM`.

This file states the changes, as section 4(b) of the licence requires. It is never rewritten by the rename tool.

Since 2026-11-25 the code is developed here, by the owner's team, and no longer kept reproducible from the upstream tag: changes are made directly and listed below. A fix from upstream is picked by hand.

## Scope of the copy

Only part of the upstream repository was copied: `core`, `framework`, `plugins`, `transports`, `ui` (since removed), the build files at the root, and the paths the unit tests read (`docs/openapi`, `examples/mcps`, `examples/plugins/hello-world`, `examples/configs/withlocalpricingfiles`, `helm-charts/bifrost/values.schema.json`, `tests/docker-compose.yml`).

## Removed

- The whole admin UI, `ui/` (1,458 files at the time), with what served it: `transports/bifrost-http/handlers/ui.go` and its test, the `//go:embed all:ui` in `transports/bifrost-http/main.go`, `RegisterUIRoutes`, `UIContent` and `ShellRewriter` in `transports/bifrost-http/server/server.go`, the UI part of `CheckBasePathCollision` in `transports/bifrost-http/lib/basepath.go` with its two tests, the `install-ui`, `build-ui`, `dev` and `dev-pulse` targets of the `Makefile`, and the Node stage of `transports/Dockerfile.local`. The product has one frontend, the web app, which calls the management API; a path no route claims answers 404.
- `transports/Dockerfile` and `transports/Dockerfile.redhat`: they build from published upstream modules, which this copy no longer is. `transports/Dockerfile.local` builds from the sources here.
- The Maxim observability plugin: `plugins/maxim/`.
- Features outside the product (owner, 2026-11-25), each with its tests, its routes under `/api`, its tables' migrations and its rows in the test baseline:
  - RSS feeds: `framework/rssfeed/`, `framework/configstore/rssfeeds.go`, `framework/configstore/tables/rssfeed.go`, `transports/bifrost-http/handlers/rssfeeds.go`, the two `rss_feeds` migrations in `framework/configstore/migrations.go` and the handler's wiring in `transports/bifrost-http/server/server.go`.
  - Outbound webhooks: `framework/webhooks/`, `framework/configstore/tables/webhooks.go`, `transports/bifrost-http/handlers/webhooks.go` and the routes under `/api/webhooks`. In shared files: the endpoint and job store methods (`framework/configstore/store.go`, `rdb.go`), `GenerateWebhookEndpointHash` and the client config's `webhook_config` (`framework/configstore/clientconfig.go`, `tables/clientconfig.go`), three migrations in `framework/configstore/migrations.go`; the delivery history table, its store methods and four migrations in `framework/logstore` (`tables.go`, `store.go`, `rdb.go`, `hybrid.go`, `clickhousestore.go`, `clickhousemigrate.go`, `migrations.go`, `errors.go`); the dispatcher, the reload callbacks and the handler's wiring in `transports/bifrost-http/server/server.go`; the `webhooks` section of `config.json` and the in-memory endpoint store in `transports/bifrost-http/lib/config.go`; `webhook_config` and `webhooks` in `transports/config.schema.json` and `helm-charts/bifrost/values.schema.json`; the webhook routes, schemas and parameters in `docs/openapi`. Async jobs stay, without the notification: `NewAsyncJobExecutor` no longer takes a dispatcher and a manager, an async job has no `webhook_endpoint_id`, the header `x-bf-async-webhook` and the context key `BifrostContextKeyAsyncWebhookEndpoint` are gone (the header is ignored: reserved `x-bf-*` headers are never forwarded to a provider), and a failed submit is always a 500. `golang.org/x/net` became an indirect dependency of `framework`.
- `.claude/`, upstream's own agent tooling, and `recipes/` with the three `include` lines in the `Makefile`: deployment recipes (Fly, ECS, local Kubernetes) this product does not use.

## Changed

| Files | Change and reason |
| --- | --- |
| `transports/bifrost-http/server/plugins.go`, `transports/bifrost-http/lib/ctx.go`, `transports/bifrost-http/lib/config.go`, `transports/go.mod`, `transports/go.sum` | The Maxim plugin is taken out of the build: its registration, its `x-bf-maxim-*` header handling and its Go dependencies. |
| `transports/bifrost-http/server/plugins.go` | One import and one `case`, so the gateway loads `plugins/credits` by name from its config. If the plugin's config cannot be decoded the case returns a plugin that refuses every request, because the gateway logs a plugin that fails to load and then serves without it. |
| `transports/bifrost-http/main.go`, `transports/bifrost-http/server/server.go`, `transports/bifrost-http/lib/basepath.go`, `transports/bifrost-http/lib/basepath_test.go` | No embedded UI and no UI routes (see Removed). `NewBifrostHTTPServer` takes the version only; `CheckBasePathCollision` checks registered routes only. |
| `Makefile` | `build` no longer builds a UI; the UI and recipe targets are gone. |
| `transports/Dockerfile.local` | No Node stage and no UI copied into the Go stage. |

## Added

- `UPSTREAM`, `MODIFICATIONS.md`, `go.work`
- `scripts/` (build, test and rename helpers), `compose/` (the shipped configuration)
- `plugins/credits/`: the prepaid credits plugin

## Renaming

No file has been renamed yet. The product is named Lemru, with `lm` as the short prefix; `scripts/rename` will rewrite the upstream product name, module path, wire prefixes and vendor hosts across this directory once a domain exists. That step will be recorded here when it is applied.
