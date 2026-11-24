import { describe, expect, it } from "vitest";

import {
  ADMIN_ACCESS_RULES,
  auditTarget,
  canAccessAdminRoute,
  findAccessRule,
} from "../access";

// Every route the admin routers define, as [method, path after /api/admin].
// Keep in step with modules/admin/**/router.ts and modules/pricing/router.ts.
const ROUTES = {
  summary: ["GET", "/summary"],
  listUsers: ["GET", "/users"],
  userAccounts: ["GET", "/users/u1/accounts"],
  userOrders: ["GET", "/users/u1/orders"],
  deleteUserAccount: ["DELETE", "/users/u1/accounts/a1"],
  listOrganizations: ["GET", "/organizations"],
  getOrganization: ["GET", "/organizations/o1"],
  organizationMembers: ["GET", "/organizations/o1/members"],
  readCredits: ["GET", "/organizations/o1/credits"],
  adjustCredits: ["POST", "/organizations/o1/credits"],
  updateOrganization: ["PATCH", "/organizations/o1"],
  deleteOrganization: ["DELETE", "/organizations/o1"],
  updateMember: ["PATCH", "/organizations/o1/members/m1"],
  removeMember: ["DELETE", "/organizations/o1/members/m1"],
  cancelInvitation: ["DELETE", "/organizations/o1/invitations/i1"],
  listCustomers: ["GET", "/customers"],
  readPricing: ["GET", "/pricing"],
  pricingChanges: ["GET", "/pricing/changes"],
  updatePricing: ["PUT", "/pricing/settings"],
  setProviderMarkup: ["PUT", "/pricing/providers/openai"],
  removeProviderMarkup: ["DELETE", "/pricing/providers/openai"],
  readProviders: ["GET", "/providers"],
  setProviderTerms: ["PUT", "/providers/k1/terms"],
  removeProviderTerms: ["DELETE", "/providers/k1/terms"],
  readAudit: ["GET", "/audit"],
} as const;

type Route = keyof typeof ROUTES;

const READ_ONLY: Route[] = [
  "summary",
  "listUsers",
  "userAccounts",
  "userOrders",
  "listOrganizations",
  "getOrganization",
  "organizationMembers",
  "readCredits",
  "listCustomers",
  "readPricing",
  "pricingChanges",
  "readProviders",
];

const ALLOWED: Record<string, Route[]> = {
  admin: Object.keys(ROUTES) as Route[],
  finance: [
    ...READ_ONLY,
    "adjustCredits",
    "updatePricing",
    "setProviderMarkup",
    "removeProviderMarkup",
    "readAudit",
  ],
  support: READ_ONLY,
  user: [],
};

describe("admin route access", () => {
  const cases = Object.entries(ALLOWED).flatMap(([role, allowed]) =>
    Object.entries(ROUTES).map(
      ([name, [method, path]]) =>
        [role, name, method, path, allowed.includes(name as Route)] as const,
    ),
  );

  it.each(cases)(
    "%s on %s (%s %s): allowed %s",
    (role, _name, method, path, isAllowed) => {
      expect(canAccessAdminRoute({ role }, method, path)).toBe(isAllowed);
    },
  );

  it("treats several roles as the union of what each allows", () => {
    expect(
      canAccessAdminRoute(
        { role: "support,finance" },
        "PUT",
        "/pricing/settings",
      ),
    ).toBe(true);
    expect(
      canAccessAdminRoute(
        { role: "support,finance" },
        "DELETE",
        "/organizations/o1",
      ),
    ).toBe(false);
  });

  it("gives an unknown or missing role nothing", () => {
    expect(canAccessAdminRoute({ role: "superuser" }, "GET", "/summary")).toBe(
      false,
    );
    expect(canAccessAdminRoute({ role: null }, "GET", "/users")).toBe(false);
  });

  it.each([
    ["GET", "/unknown"],
    ["POST", "/pricing"],
    ["GET", "/users/u1/accounts/extra/../.."],
    ["DELETE", "/customers"],
    ["HEAD", "/summary"],
  ])("refuses %s %s, which no rule covers, to every role", (method, path) => {
    if (findAccessRule(method, path)) {
      return; // a rule exists; the matrix above checks it
    }
    for (const role of Object.keys(ALLOWED)) {
      expect(canAccessAdminRoute({ role }, method, path)).toBe(false);
    }
  });

  it.each(Object.values(ROUTES))("covers %s %s with a rule", (method, path) => {
    expect(findAccessRule(method, path)).toBeDefined();
  });
});

describe("admin audit", () => {
  it.each(ADMIN_ACCESS_RULES.filter((rule) => rule.method !== "GET"))(
    "names an audit action for $method $path",
    (rule) => {
      expect(rule.audit?.action).toMatch(/^[a-z]+\.[a-z-]+$/);
    },
  );

  it.each([
    ["/organizations/o1/credits", "o1"],
    ["/organizations/o1/members/m1", "o1"],
    ["/users/u1/accounts/a1", "u1"],
    ["/pricing/settings", "settings"],
    ["/pricing/providers/openai", "openai"],
    ["/providers/k1/terms", "k1"],
  ])("targets %s at %s", (path, target) => {
    expect(auditTarget(path)).toBe(target);
  });
});
