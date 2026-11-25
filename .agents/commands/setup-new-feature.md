# Setup New Feature

Set up a new feature in the Lemru monorepo following project conventions.

## Before starting

1. **Clarify scope**: Which parts need this feature? (web, API, gateway)
2. **Check existing code**: Look in `packages/*` for reusable logic before creating new code
3. **Identify shared vs app-specific**: Shared logic goes in `packages/*`, UI in app-specific modules

## Feature setup by target

### Web feature (`apps/web`)

1. **Page/route**: Create in `src/app/[locale]/{section}/`
   - Marketing pages → `(marketing)/`
   - Auth pages → `auth/`
   - Dashboard pages → `dashboard/`
   - Admin pages → `admin/`

2. **Module**: Create in `src/modules/{feature}/`
   ```
   src/modules/{feature}/
     components/     # React components
     lib/            # Hooks, utils, API calls
     index.ts        # Public exports
   ```

3. **Use RSC by default**: Only add `use client` when necessary

### Shared logic (`packages/*`)

If the feature needs shared logic across apps:

| Need                  | Package                    | Location                          |
| --------------------- | -------------------------- | --------------------------------- |
| API endpoint          | `@workspace/api`           | `packages/api/src/modules/`       |
| Database schema       | `@workspace/db`            | `packages/db/src/schema/`         |
| Auth logic            | `@workspace/auth`          | `packages/auth/src/`              |
| Shared types/utils    | `@workspace/shared`        | `packages/shared/src/`            |
| Translations          | `@workspace/i18n`          | `packages/i18n/src/translations/` |
| Email templates       | `@workspace/email`         | `packages/email/src/templates/`   |

## Checklist

- [ ] Scope defined
- [ ] Checked `packages/*` for existing helpers
- [ ] Created module structure in target app(s)
- [ ] Added shared logic to appropriate package (if needed)
- [ ] Used `@workspace/ui-web` for UI
- [ ] Added translations if user-facing text
- [ ] Types explicit, no `any`
- [ ] Zod schemas for validation

## After setup

1. Run `pnpm typecheck` to verify types
2. Run `pnpm lint` to check code style
3. Test the feature in dev: `pnpm --filter <app> dev`
