import { createAccessControl } from "better-auth/plugins/access";
import { defaultStatements } from "better-auth/plugins/admin/access";

/**
 * Roles of the people who run the platform, as opposed to the roles members
 * have inside an organization (./roles.ts). See
 * docs/platform/adr/0011-staff-roles.md.
 *
 * `user` and `session` are Better Auth's admin statements, checked by its own
 * /api/auth/admin/* endpoints. The others guard our /api/admin routes.
 */
export const staffAc = createAccessControl({
  ...defaultStatements,
  organizations: ["read", "update", "delete"],
  customers: ["read"],
  credits: ["read", "adjust"],
  pricing: ["read", "update"],
  audit: ["read"],
});

/** Sees everything and changes everything. Kept as "admin" so existing admins keep their access. */
const admin = staffAc.newRole({
  user: [
    "create",
    "list",
    "set-role",
    "ban",
    "impersonate",
    "delete",
    "set-password",
    "set-email",
    "get",
    "update",
  ],
  session: ["list", "revoke", "delete"],
  organizations: ["read", "update", "delete"],
  customers: ["read"],
  credits: ["read", "adjust"],
  pricing: ["read", "update"],
  audit: ["read"],
});

/** Money: prices and credit adjustments. Can look at accounts, cannot act on them. */
const finance = staffAc.newRole({
  user: ["list", "get"],
  session: [],
  organizations: ["read"],
  customers: ["read"],
  credits: ["read", "adjust"],
  pricing: ["read", "update"],
  audit: ["read"],
});

/** Helping customers: reads everything, signs in as a customer, ends their sessions. No money. */
const support = staffAc.newRole({
  user: ["list", "get", "impersonate"],
  session: ["list", "revoke"],
  organizations: ["read"],
  customers: ["read"],
  credits: ["read"],
  pricing: ["read"],
  audit: [],
});

/** Everyone else. */
const user = staffAc.newRole({
  user: [],
  session: [],
  organizations: [],
  customers: [],
  credits: [],
  pricing: [],
  audit: [],
});

export const staffRoles = { admin, finance, support, user } as const;

export type StaffPermissions = Parameters<
  (typeof staffRoles)["admin"]["authorize"]
>[0];

/** The roles that open the admin console. */
export const STAFF_ROLE_NAMES = ["admin", "finance", "support"] as const;

const rolesOf = (role: string | null | undefined) =>
  (role ?? "user").split(",").map((name) => name.trim());

/** Whether a user holds any staff role. */
export const isStaff = (user: { role?: string | null }) =>
  rolesOf(user.role).some((name) =>
    (STAFF_ROLE_NAMES as readonly string[]).includes(name),
  );

/**
 * Whether one of the user's roles grants every permission asked for. Same
 * rule as Better Auth's admin plugin: roles are comma-separated, any one may
 * grant the whole request, unknown roles grant nothing.
 */
export const hasStaffPermission = (
  user: { role?: string | null },
  permissions: StaffPermissions,
) =>
  rolesOf(user.role).some((name) => {
    const role = staffRoles[name as keyof typeof staffRoles];
    return role?.authorize(permissions).success ?? false;
  });
