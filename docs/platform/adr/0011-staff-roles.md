# ADR 0011: Staff roles, and how the admin console is protected

Status: Accepted
Date: 2026-11-23

## Context

The template had one platform role besides `user`: `admin`, which could do everything in the console. That includes signing in as any customer, banning and deleting accounts, changing prices (ADR 0010) and adjusting credit balances (ADR 0001). A support agent who only needs to look at an account would hold the power to change what every customer pays.

Roles inside an organization (owner, admin, member, with the `billing` permission) are a separate matter and are unchanged.

The owner decided (2026-11-23) to split the platform role.

## Decision

Three staff roles, defined with Better Auth's access control and registered with its admin plugin (`packages/auth/src/rbac/staff.ts`):

| Permission                                           | admin | finance | support |
| ---------------------------------------------------- | :---: | :-----: | :-----: |
| See users, organizations, customers, credits, prices |   ✓   |    ✓    |    ✓    |
| Sign in as a customer (impersonate)                  |   ✓   |         |    ✓    |
| List and end a user's sessions                       |   ✓   |         |    ✓    |
| Ban, delete, set password or email, change roles     |   ✓   |         |         |
| Edit or delete an organization and its members       |   ✓   |         |         |
| Adjust a credit balance                              |   ✓   |    ✓    |         |
| Change prices and fees                               |   ✓   |    ✓    |         |
| Read the audit log                                   |   ✓   |    ✓    |         |

- `admin` keeps its name, so existing admins keep their access.
- A user may hold several roles, comma-separated; one role granting a permission is enough.
- No role may impersonate another staff member (`impersonate-admins` is granted to nobody, and all three roles are listed as admin roles to the plugin).
- **Two places enforce the table, both on the server.** Better Auth checks `user` and `session` permissions on its own `/api/auth/admin/*` endpoints. Our `/api/admin/*` routes go through one middleware with a table that maps each method and path to a permission (`packages/api/src/modules/admin/access.ts`). A route that has no entry in the table is refused, so a new route cannot ship unguarded.
- The console hides what a role cannot do. That is convenience only; the server refuses it either way.
- **Every staff change is recorded** in `admin_audit_log`: who (and their roles then), what action, on which record, the request body, the client address, when. It covers Better Auth's admin actions (ban, unban, set role, set password, impersonate and its end, end sessions, create, update and delete user) and every changing route of `/api/admin` (named in the access table, so a new changing route without an audit name fails `access.test.ts`). Only successful actions are recorded. Passwords and session tokens are never stored. The table rejects UPDATE and DELETE, has no foreign keys, and is shown at `/admin/audit` to roles with `audit:read`.
- **Staff need two-factor authentication.** Without it, a staff member is refused on every `/api/admin` route and every Better Auth `/api/auth/admin/*` endpoint, and the console sends them to their security settings. Customers are not affected.

## Consequences

- Granting a role is a change to `user.role` made by an `admin`, through the console or Better Auth's `set-role`.
- A new admin route needs an entry in the access table, or every role gets 403. `access.test.ts` lists every route and checks each role against it.
- Sessions are read from the database on every request (no cookie cache), so revoking a role or banning someone applies on their next request.
- The audit entry is written after the action succeeds. If that write fails, the action stands and the failure is logged as an error for monitoring; it is not shown to the staff member.
- What a support agent does while signed in as a customer is the customer's activity, not recorded here; the session carries `impersonatedBy` if that is needed later.
- Granting a staff role to someone without 2FA gives them nothing until they turn it on. Turning it off takes the access away on the next request.
