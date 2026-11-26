# AGENTS.md

<!-- BEGIN:nextjs-agent-rules -->

# This is NOT the Next.js you know

This version has breaking changes — APIs, conventions, and file structure may all differ from your training data. Read the relevant guide in `node_modules/next/dist/docs/` (resolved from this file's directory; in monorepos the `next` package may not be visible from the repo root — prefer `apps/web/node_modules/next/dist/docs/` or the workspace hoist) before writing any Next.js code. Heed deprecation notices.

This block is written for Next.js 16.4. Verify patterns against bundled docs when upgrading.

<!-- END:nextjs-agent-rules -->

Guide for AI agents working on the Lemru monorepo.

**Start here:** `docs/platform/STATE.md` says what the product is, where everything lives, what is done, what is next and what waits on the owner. Read it before anything else, and update it when you finish a piece of work, so the next agent does not have to read the whole repository.

## Agent rules

**DO:**

- Read existing files before editing; understand imports and structure first
- Keep diffs minimal and scoped to the request
- Use `pnpm --filter <pkg>` for package-specific commands
- Use `pnpm with-env` for commands needing environment variables
- Add dependencies via `pnpm add -F <pkg>` in the appropriate package
- Follow existing code style, naming, and file layout
- Use explicit types; prefer `interface` over `type` for objects
- Use Zod for validation
- Check `packages/*` for shared helpers before duplicating logic
- Import from package entry points (`@workspace/ui-web`, not deep paths)

**Gateway first:** `apps/gateway` (Bifrost) is the engine; TurboStarter is the SaaS shell around it. Before writing any gateway, money, usage, limit, logging or provider logic:

1. Search `apps/gateway` (core, framework, plugins, transports) for an existing feature and read it.
2. State what you found, with file paths, or that you found nothing and where you looked.
3. Use or configure it. Write our own code only for what it does not do, and say why.

Findings from reviews or subagents are claims: verify them in the code before building on them.

**One product:** the gateway and the control plane are one product. A product-level change (a language added or removed, the brand, a feature taken out) is made in both in the same piece of work, so no difference is left between them.

**Our own product (owner, 2026-11-25):** we do not stay dependent on the gateway's upstream; the owner's team develops the whole product, and `apps/gateway` is edited directly. Every change to a file that came from upstream is listed in `apps/gateway/MODIFICATIONS.md` (the Apache licence requires it); `LICENSE`, `THIRD_PARTY_NOTICES.md` and `UPSTREAM` stay. One frontend only: `apps/web` (customer dashboard and admin); the gateway's own UI is being removed (`docs/platform/PLAN.md`, "Gateway UI removal"). The repo keeps only what the product needs: a folder that nothing builds, runs or tests is deleted.

**Switched off is not removed (owner, 2026-11-26):** the credits plugin charges text only today (chat, responses, text completions, embeddings) and refuses every other request type with a 403 (`classify` in `apps/gateway/plugins/credits/plugin.go`), because their cost cannot be bounded before the call yet. Speech (TTS), transcription (STT), images, video, realtime, rerank, batch and files are part of the product and will be switched on one by one. Their code, routes, schemas and tests in the engine stay: do not delete them as unused. The features taken out for good are listed in `apps/gateway/MODIFICATIONS.md` ("Features outside the product").

**Learn from LiteLLM:** when fixing a weakness in the gateway, look first at how LiteLLM (github.com/BerriAI/litellm) solved it. Take the idea, not the code, and never from its `enterprise/` directory, which is under a commercial licence.

**Modular monolith:** follow the project's shape. A feature is a module with its own folder and a narrow surface: `packages/api/src/modules/<feature>` (router, logic, tests), `apps/web/src/modules/<feature>` (UI), schema in `packages/db/src/schema`, and in the gateway a plugin under `apps/gateway/plugins/<name>`. No feature reaches into another module's internals.

**DON'T:**

- Commit, push, or modify git state unless explicitly asked
- Run destructive commands (`reset --hard`, force-push) without permission
- Invent ad-hoc scripts; use repo commands from the table below
- Reformat unrelated code or make drive-by changes
- Delete or rewrite unrelated code
- Use `any` or unsafe type casts
- Use enums (prefer maps/objects)
- Use classes (prefer functions)

## Where to look first

| Task                  | Start here                                                                                               |
| --------------------- | -------------------------------------------------------------------------------------------------------- |
| Where things stand    | `docs/platform/STATE.md`, then `PLAN.md` and `adr/`                                                      |
| What to build next    | `docs/platform/SPEC.md` (features, status, done-when, build order, open decisions)                       |
| A product feature     | `packages/api/src/modules/<feature>` (keys, credits, activity, data-policy, pricing, reconciliation, v1) |
| Admin (staff) feature | `packages/api/src/modules/admin/<feature>` and `apps/web/src/modules/admin/<feature>`                    |
| Access of staff roles | `packages/api/src/modules/admin/access.ts` (a route with no rule is refused)                             |
| Talking to the engine | `packages/gateway-client/src/client.ts`                                                                  |
| Engine behaviour      | `apps/gateway` (core, framework, transports, plugins/credits)                                            |
| Auth logic            | `packages/auth/` → then app-specific usage                                                               |
| Database schema       | `packages/db/src/schema/`                                                                                |
| UI components         | `packages/ui/web/`                                                                                       |
| Payments              | `packages/billing/{shared,web}/`, top-ups in `modules/credits`                                           |
| Translations          | `packages/i18n/src/translations/` (English only for now)                                                 |
| Email templates       | `packages/email/src/templates/`                                                                          |
| Web pages             | `apps/web/src/app/[locale]/`                                                                             |

## Project structure

```
apps/
  web/         Next.js App Router (src/app/[locale]/{(marketing),auth,dashboard,admin}). The only frontend
  gateway/     The engine, in Go (ours; started from Bifrost, see apps/gateway/MODIFICATIONS.md). Not a pnpm
               package: own Go modules (go.work), no UI. Read apps/gateway/AGENTS.md before editing it

packages/
  api/         @workspace/api       Hono modules: the product's features, admin, auth, billing, organization
  gateway-client/  @workspace/gateway-client   TypeScript client for the engine's management API
  auth/        @workspace/auth      Better Auth server + web client (src/client/web.ts), staff roles
  billing/     @workspace/billing   Shared + web (Stripe; the template's other providers are still there)
  db/          @workspace/db        Drizzle schema, migrations, scripts
  email/       @workspace/email     React Email templates + providers
  i18n/        @workspace/i18n      Translations + server/client utils
  shared/      @workspace/shared    Constants, schema, logger, hooks, utils
  storage/     @workspace/storage   S3 storage providers
  cms/         @workspace/cms       MDX content (legal)
  flags/       @workspace/flags     OpenFeature helpers + flags-web
  analytics/   @workspace/analytics[-web]   PostHog only
  monitoring/  @workspace/monitoring[-web]
  ui/          @workspace/ui[-web]

tooling/       oxfmt, oxlint, typescript, vitest, github, scripts
```

## Commands

| Task               | Command                                                                                                                     |
| ------------------ | --------------------------------------------------------------------------------------------------------------------------- |
| Install deps       | `pnpm install`                                                                                                              |
| Dev (all/specific) | `pnpm dev` / `pnpm --filter <app> dev`                                                                                      |
| Build              | `pnpm build` / `pnpm --filter <app> build`                                                                                  |
| Lint/Format        | `pnpm lint` / `pnpm format`                                                                                                 |
| Fix lint/format    | `pnpm lint:fix` / `pnpm format:fix`                                                                                         |
| Test               | `pnpm test`                                                                                                                 |
| Clean              | `pnpm clean`                                                                                                                |
| Gateway            | `pnpm gateway:up` (Docker build + run, `localhost:8080`)                                                                    |
| Gateway build/test | `pnpm gateway:build` (image) / `gateway:test` (Go, native)                                                                  |
| Gateway dev/verify | `pnpm gateway:dev` (from source) / `gateway:verify` (rename)                                                                |
| Checks before push | `node --run format && node --run lint && node --run typecheck`, then the tests and the web build (`docs/platform/STATE.md`) |
| Services           | `pnpm services:setup` / `services:start` / `services:stop`                                                                  |
| DB migrate         | `pnpm with-env pnpm -F @workspace/db db:migrate`                                                                            |
| DB generate        | `pnpm with-env pnpm -F @workspace/db db:generate`                                                                           |
| DB studio          | `pnpm with-env pnpm -F @workspace/db db:studio`                                                                             |

Environment: create `.env.local` at repo root with `DATABASE_URL`, `PRODUCT_NAME`, `URL`, `DEFAULT_LOCALE`.

## Code conventions

- TypeScript: functional, declarative; no classes
- Naming: `isLoading`, `hasError`, `handleClick`
- File layout: exported component → subcomponents → helpers → types
- Error handling: guard clauses, early returns
- JSX: declarative, minimal curly braces
- Commits: Conventional Commits

## App-specific

**Web:** RSC by default; `use client` only when needed; `Suspense` for client components; `nuqs` for URL state; `@workspace/ui-web`. Next.js 16.4: `proxy.ts` (not middleware); error boundaries use `retry` (not `reset`); TypeScript 7 via project-local `tsc`; locale via `next/root-params` (`~/lib/i18n`).

## Troubleshooting

| Issue            | Fix                                          |
| ---------------- | -------------------------------------------- |
| Version mismatch | Check `package.json#packageManager`          |
| Services refused | `pnpm services:start`, verify Docker running |
| Env not loaded   | Create `.env` at root; use `pnpm with-env`   |
| Module issues    | `pnpm clean && pnpm install`                 |
