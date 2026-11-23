import { createMiddleware } from "hono/factory";

import { hasStaffPermission, isStaff } from "@workspace/auth";
import { HttpStatusCode } from "@workspace/shared/constants";
import { HttpException } from "@workspace/shared/utils";

import type { StaffPermissions, User } from "@workspace/auth";

/**
 * Which staff permission each /api/admin route needs
 * (docs/platform/adr/0011-staff-roles.md). The first matching rule wins. A
 * route with no rule is refused, so a new route cannot ship unguarded.
 */
interface AccessRule {
  method: string;
  /** Matched against the path after /api/admin. */
  path: RegExp;
  /** "any-staff" lets every staff role through. */
  needs: StaffPermissions | "any-staff";
}

const ID = "[^/]+";

export const ADMIN_ACCESS_RULES: readonly AccessRule[] = [
  { method: "GET", path: /^\/summary$/, needs: "any-staff" },

  { method: "GET", path: /^\/users$/, needs: { user: ["list"] } },
  {
    method: "GET",
    path: new RegExp(`^/users/${ID}(/.*)?$`),
    needs: { user: ["get"] },
  },
  {
    method: "DELETE",
    path: new RegExp(`^/users/${ID}/accounts/${ID}$`),
    needs: { user: ["update"] },
  },

  {
    method: "GET",
    path: new RegExp(`^/organizations/${ID}/credits$`),
    needs: { credits: ["read"] },
  },
  {
    method: "POST",
    path: new RegExp(`^/organizations/${ID}/credits$`),
    needs: { credits: ["adjust"] },
  },
  {
    method: "GET",
    path: /^\/organizations(\/.*)?$/,
    needs: { organizations: ["read"] },
  },
  {
    method: "DELETE",
    path: new RegExp(`^/organizations/${ID}$`),
    needs: { organizations: ["delete"] },
  },
  {
    method: "PATCH",
    path: new RegExp(`^/organizations/${ID}$`),
    needs: { organizations: ["update"] },
  },
  {
    method: "PATCH",
    path: new RegExp(`^/organizations/${ID}/members/${ID}$`),
    needs: { organizations: ["update"] },
  },
  {
    method: "DELETE",
    path: new RegExp(`^/organizations/${ID}/(members|invitations)/${ID}$`),
    needs: { organizations: ["update"] },
  },

  { method: "GET", path: /^\/customers$/, needs: { customers: ["read"] } },

  {
    method: "GET",
    path: /^\/pricing(\/changes)?$/,
    needs: { pricing: ["read"] },
  },
  {
    method: "PUT",
    path: /^\/pricing\/(settings|providers\/[^/]+)$/,
    needs: { pricing: ["update"] },
  },
  {
    method: "DELETE",
    path: /^\/pricing\/providers\/[^/]+$/,
    needs: { pricing: ["update"] },
  },

  { method: "GET", path: /^\/audit$/, needs: { audit: ["read"] } },
];

/** The rule for a request, or undefined when no rule covers it. */
export const findAccessRule = (method: string, path: string) =>
  ADMIN_ACCESS_RULES.find(
    (rule) => rule.method === method && rule.path.test(path),
  );

/** Whether a user may make this request to /api/admin. */
export const canAccessAdminRoute = (
  user: { role?: string | null },
  method: string,
  path: string,
) => {
  const rule = findAccessRule(method, path);
  if (!rule) {
    return false;
  }
  if (rule.needs === "any-staff") {
    // not left to enforceAdmin alone: this function must be safe on its own
    return isStaff(user);
  }
  return hasStaffPermission(user, rule.needs);
};

/** Mounted on /api/admin after enforceAdmin, which has already required a staff role. */
export const enforceStaffAccess = createMiddleware<{
  Variables: { user: User };
}>(async (c, next) => {
  const path = c.req.path.replace(/^.*?\/api\/admin/, "") || "/";
  if (!canAccessAdminRoute(c.var.user, c.req.method, path)) {
    throw new HttpException(HttpStatusCode.FORBIDDEN, {
      code: "error.forbidden",
    });
  }
  await next();
});
