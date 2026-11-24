import { hasStaffPermission, isStaff } from "@workspace/auth";

import type { StaffPermissions } from "@workspace/auth";

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
  /** Set on every rule that changes something: the audit log's action name. */
  audit?: { action: string; target: string };
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
    audit: { action: "user.unlink-account", target: "user" },
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
    audit: { action: "credits.adjust", target: "organization" },
  },
  {
    method: "PUT",
    path: new RegExp(`^/organizations/${ID}/rate-limit$`),
    needs: { organizations: ["update"] },
    audit: { action: "organization.rate-limit", target: "organization" },
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
    audit: { action: "organization.delete", target: "organization" },
  },
  {
    method: "PATCH",
    path: new RegExp(`^/organizations/${ID}$`),
    needs: { organizations: ["update"] },
    audit: { action: "organization.update", target: "organization" },
  },
  {
    method: "PATCH",
    path: new RegExp(`^/organizations/${ID}/members/${ID}$`),
    needs: { organizations: ["update"] },
    audit: { action: "organization.member-update", target: "organization" },
  },
  {
    method: "DELETE",
    path: new RegExp(`^/organizations/${ID}/(members|invitations)/${ID}$`),
    needs: { organizations: ["update"] },
    audit: { action: "organization.member-remove", target: "organization" },
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
    audit: { action: "pricing.update", target: "pricing" },
  },
  {
    method: "DELETE",
    path: /^\/pricing\/providers\/[^/]+$/,
    needs: { pricing: ["update"] },
    audit: { action: "pricing.remove-provider", target: "pricing" },
  },

  { method: "GET", path: /^\/audit$/, needs: { audit: ["read"] } },
  // read-only money reports: admin, finance and support (support reads everything, ADR 0011)
  {
    method: "GET",
    path: /^\/reconciliation$/,
    needs: { credits: ["read"] },
  },
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

/**
 * For the audit log: which record a path is about. /organizations/o1/credits
 * is organization o1; /pricing/providers/openai is the pricing of openai.
 */
export const auditTarget = (path: string) => {
  const segments = path.split("/").filter(Boolean);
  return segments[0] === "pricing" ? segments.at(-1) : segments[1];
};
