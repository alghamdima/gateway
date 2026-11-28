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
  /**
   * Set on every rule that changes something: the audit log's action name.
   * `secrets` names the body fields the log must not keep.
   */
  audit?: { action: string; target: string; secrets?: readonly string[] };
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
  // an organization's own top-up fee (ADMIN-24): the same permission as the default fee
  {
    method: "GET",
    path: new RegExp(`^/organizations/${ID}/top-up-fee(/changes)?$`),
    needs: { pricing: ["read"] },
  },
  {
    method: "PUT",
    path: new RegExp(`^/organizations/${ID}/top-up-fee$`),
    needs: { pricing: ["update"] },
    audit: { action: "pricing.organization-fee", target: "organization" },
  },
  {
    method: "DELETE",
    path: new RegExp(`^/organizations/${ID}/top-up-fee$`),
    needs: { pricing: ["update"] },
    audit: {
      action: "pricing.organization-fee-clear",
      target: "organization",
    },
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
    path: /^\/pricing\/settings$/,
    needs: { pricing: ["update"] },
    audit: { action: "pricing.update", target: "pricing" },
  },
  // the refund window and the credit expiry (ADMIN-23)
  {
    method: "PUT",
    path: /^\/pricing\/credit-terms$/,
    needs: { pricing: ["update"] },
    audit: { action: "pricing.credit-terms", target: "pricing" },
  },

  // the key defaults and the maximum key lifetime (ADMIN-12): values staff set for customers, as prices
  {
    method: "GET",
    path: /^\/key-settings(\/changes)?$/,
    needs: { pricing: ["read"] },
  },
  {
    method: "PUT",
    path: /^\/key-settings$/,
    needs: { pricing: ["update"] },
    audit: { action: "keys.defaults", target: "key-settings" },
  },

  // how many organizations a user may own, for everyone and for one user (ADR 0020)
  {
    method: "GET",
    path: /^\/account-settings(\/changes|\/users\/[^/]+)?$/,
    needs: { organizations: ["read"] },
  },
  {
    method: "PUT",
    path: /^\/account-settings$/,
    needs: { organizations: ["update"] },
    audit: { action: "accounts.settings", target: "account-settings" },
  },
  {
    method: "PUT",
    path: /^\/account-settings\/users\/[^/]+$/,
    needs: { organizations: ["update"] },
    audit: { action: "accounts.organization-limit", target: "user" },
  },

  // the model price table (ADMIN-09, ADR 0018): the same permission as the other prices
  {
    method: "GET",
    path: /^\/model-prices(\/(history|suggestions|check))?$/,
    needs: { pricing: ["read"] },
  },
  {
    method: "POST",
    path: /^\/model-prices$/,
    needs: { pricing: ["update"] },
    audit: { action: "pricing.model-create", target: "model-price" },
  },
  {
    method: "POST",
    path: /^\/model-prices\/import$/,
    needs: { pricing: ["update"] },
    audit: { action: "pricing.model-import", target: "model-price" },
  },
  {
    method: "POST",
    path: /^\/model-prices\/push$/,
    needs: { pricing: ["update"] },
    audit: { action: "pricing.model-push", target: "model-price" },
  },
  {
    method: "POST",
    path: /^\/model-prices\/suggestions\/sync$/,
    needs: { pricing: ["update"] },
    audit: { action: "pricing.suggestions-sync", target: "model-price" },
  },
  {
    method: "POST",
    path: new RegExp(`^/model-prices/suggestions/${ID}/approve$`),
    needs: { pricing: ["update"] },
    audit: {
      action: "pricing.suggestion-approve",
      target: "model-price-suggestion",
    },
  },
  {
    method: "POST",
    path: new RegExp(`^/model-prices/suggestions/${ID}/reject$`),
    needs: { pricing: ["update"] },
    audit: {
      action: "pricing.suggestion-reject",
      target: "model-price-suggestion",
    },
  },

  // payment gateways (ADMIN-03): the secrets stay in env and never pass through here
  {
    method: "GET",
    path: /^\/payments(\/changes)?$/,
    needs: { payments: ["read"] },
  },
  {
    method: "PUT",
    path: /^\/payments\/(settings|providers\/[^/]+)$/,
    needs: { payments: ["update"] },
    audit: { action: "payments.update", target: "payments" },
  },

  // promo codes (BILL-08): who may see and change them, as for pricing
  {
    method: "GET",
    path: new RegExp(`^/promo-codes(/${ID}(/report|/changes)?)?$`),
    needs: { promoCodes: ["read"] },
  },
  {
    method: "POST",
    path: /^\/promo-codes$/,
    needs: { promoCodes: ["update"] },
    audit: { action: "promo.create", target: "promo-code" },
  },
  {
    method: "PUT",
    path: new RegExp(`^/promo-codes/${ID}$`),
    needs: { promoCodes: ["update"] },
    audit: { action: "promo.update", target: "promo-code" },
  },
  {
    method: "PUT",
    path: new RegExp(`^/promo-codes/${ID}/paused$`),
    needs: { promoCodes: ["update"] },
    audit: { action: "promo.pause", target: "promo-code" },
  },

  // the request log: what happened to each call, never its content
  { method: "GET", path: /^\/requests$/, needs: "any-staff" },

  // the provider catalog and each key's data terms (ADR 0015)
  { method: "GET", path: /^\/providers$/, needs: { providers: ["read"] } },
  {
    method: "POST",
    path: /^\/providers\/price-sync$/,
    needs: { providers: ["update"] },
    audit: { action: "provider.price-sync", target: "provider" },
  },
  // the providers and their keys in the gateway; a key's value is a secret
  {
    method: "GET",
    path: /^\/providers\/accounts$/,
    needs: { providers: ["read"] },
  },
  {
    method: "POST",
    path: /^\/providers\/accounts$/,
    needs: { providers: ["update"] },
    audit: { action: "provider.create", target: "provider" },
  },
  {
    method: "DELETE",
    path: new RegExp(`^/providers/accounts/${ID}$`),
    needs: { providers: ["update"] },
    audit: { action: "provider.delete", target: "provider" },
  },
  {
    method: "POST",
    path: new RegExp(`^/providers/accounts/${ID}/refresh-models$`),
    needs: { providers: ["update"] },
    audit: { action: "provider.refresh-models", target: "provider" },
  },
  {
    method: "POST",
    path: new RegExp(`^/providers/accounts/${ID}/keys$`),
    needs: { providers: ["update"] },
    audit: {
      action: "provider.key-create",
      target: "provider",
      secrets: ["value"],
    },
  },
  {
    method: "PUT",
    path: new RegExp(`^/providers/accounts/${ID}/keys/${ID}$`),
    needs: { providers: ["update"] },
    audit: {
      action: "provider.key-update",
      target: "provider",
      secrets: ["value"],
    },
  },
  {
    method: "DELETE",
    path: new RegExp(`^/providers/accounts/${ID}/keys/${ID}$`),
    needs: { providers: ["update"] },
    audit: { action: "provider.key-delete", target: "provider" },
  },
  {
    method: "PUT",
    path: new RegExp(`^/providers/${ID}/terms$`),
    needs: { providers: ["update"] },
    audit: { action: "provider.terms-update", target: "provider-key" },
  },
  {
    method: "DELETE",
    path: new RegExp(`^/providers/${ID}/terms$`),
    needs: { providers: ["update"] },
    audit: { action: "provider.terms-remove", target: "provider-key" },
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
 * is organization o1; /pricing/settings is the pricing settings,
 * /payments/providers/stripe the payment settings of stripe.
 */
export const auditTarget = (path: string) => {
  const segments = path.split("/").filter(Boolean);
  // /model-prices/suggestions/7/approve is suggestion 7; a new version names its target in the body
  if (segments[0] === "model-prices") {
    return segments[1] === "suggestions" && segments.length === 4
      ? segments[2]
      : (segments[1] ?? "model-prices");
  }
  if (
    segments[0] === "pricing" ||
    segments[0] === "payments" ||
    segments[0] === "key-settings" ||
    // /account-settings/users/u1 is about user u1
    segments[0] === "account-settings"
  ) {
    return segments.at(-1);
  }
  // /providers/accounts/openai/keys/k1 is about provider openai
  return segments[0] === "providers" && segments[1] === "accounts"
    ? segments[2]
    : segments[1];
};

/** The body as the audit log may keep it: the named fields are blanked. */
export const withoutSecrets = (body: unknown, secrets: readonly string[]) => {
  if (typeof body !== "object" || body === null || Array.isArray(body)) {
    return body;
  }
  return Object.fromEntries(
    Object.entries(body).map(([field, value]) => [
      field,
      secrets.includes(field) ? "[redacted]" : value,
    ]),
  );
};
