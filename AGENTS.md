# AGENTS.md

<!-- BEGIN:nextjs-agent-rules -->

# This is NOT the Next.js you know

This version has breaking changes — APIs, conventions, and file structure may all differ from your training data. Read the relevant guide in `node_modules/next/dist/docs/` (resolved from this file's directory; in monorepos the `next` package may not be visible from the repo root — prefer `apps/web/node_modules/next/dist/docs/` or the workspace hoist) before writing any Next.js code. Heed deprecation notices.

This block is written for Next.js 16.4. Verify patterns against bundled docs when upgrading.

<!-- END:nextjs-agent-rules -->

Guide for AI agents working on the Lemru monorepo.

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

| Task            | Start here                                 |
| --------------- | ------------------------------------------ |
| Auth logic      | `packages/auth/` → then app-specific usage |
| API endpoints   | `packages/api/src/modules/`                |
| Database schema | `packages/db/src/schema/`                  |
| UI components   | `packages/ui/web/`                         |
| Billing         | `packages/billing/{shared,web}/`           |
| Feature flags   | `packages/flags/` → `flags-web`            |
| Translations    | `packages/i18n/src/translations/`          |
| Email templates | `packages/email/src/templates/`            |
| Web pages       | `apps/web/src/app/[locale]/`               |

## Project structure

```
apps/
  web/         Next.js App Router (src/app/[locale]/{(marketing),auth,dashboard,admin})
  gateway/     Go AI gateway (vendored from upstream, see apps/gateway/UPSTREAM). Not a pnpm package:
               own Go modules (go.work) and npm UI. Read apps/gateway/AGENTS.md before editing it

packages/
  api/         @workspace/api       Hono modules (admin,ai,auth,billing,organization,storage)
  auth/        @workspace/auth      Better Auth server + web client (src/client/web.ts)
  billing/     @workspace/billing   Shared + web (Stripe,LemonSqueezy,Polar)
  flags/       @workspace/flags     OpenFeature helpers + flags-web
  cms/         @workspace/cms       MDX content (blog, legal)
  db/          @workspace/db        Drizzle schema, migrations, scripts
  email/       @workspace/email     React Email templates + providers
  i18n/        @workspace/i18n      Translations + server/client utils
  shared/      @workspace/shared    Constants, schema, logger, hooks, utils
  storage/     @workspace/storage   S3 storage providers
  analytics/   @workspace/analytics[-web]
  monitoring/  @workspace/monitoring[-web]
  ui/          @workspace/ui[-web]

tooling/       oxfmt, oxlint, typescript, vitest, github, scripts
```

## Commands

| Task               | Command                                                      |
| ------------------ | ------------------------------------------------------------ |
| Install deps       | `pnpm install`                                               |
| Dev (all/specific) | `pnpm dev` / `pnpm --filter <app> dev`                       |
| Build              | `pnpm build` / `pnpm --filter <app> build`                   |
| Lint/Format        | `pnpm lint` / `pnpm format`                                  |
| Fix lint/format    | `pnpm lint:fix` / `pnpm format:fix`                          |
| Test               | `pnpm test`                                                  |
| Clean              | `pnpm clean`                                                 |
| Gateway            | `pnpm gateway:up` (Docker build + run, `localhost:8080`)     |
| Gateway build/test | `pnpm gateway:build` (image) / `gateway:test` (Go, native)   |
| Gateway dev/verify | `pnpm gateway:dev` (from source) / `gateway:verify` (rename) |
| Services           | `pnpm services:setup` / `services:start` / `services:stop`   |
| DB migrate         | `pnpm with-env pnpm -F @workspace/db db:migrate`             |
| DB generate        | `pnpm with-env pnpm -F @workspace/db db:generate`            |
| DB studio          | `pnpm with-env pnpm -F @workspace/db db:studio`              |

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
