import { describe, expect, it } from "vitest";

import {
  ADMIN_ACCESS_RULES,
  auditTarget,
  canAccessAdminRoute,
  findAccessRule,
  withoutSecrets,
} from "../access";

// Every route the admin routers define, as [method, path after /api/admin].
// Keep in step with modules/admin/**/router.ts, modules/pricing/router.ts,
// modules/payments/router.ts and modules/keys/admin-router.ts.
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
  readDataPolicy: ["GET", "/organizations/o1/data-policy"],
  adjustCredits: ["POST", "/organizations/o1/credits"],
  updateOrganization: ["PATCH", "/organizations/o1"],
  deleteOrganization: ["DELETE", "/organizations/o1"],
  updateMember: ["PATCH", "/organizations/o1/members/m1"],
  removeMember: ["DELETE", "/organizations/o1/members/m1"],
  cancelInvitation: ["DELETE", "/organizations/o1/invitations/i1"],
  readTopupFee: ["GET", "/organizations/o1/top-up-fee"],
  topupFeeChanges: ["GET", "/organizations/o1/top-up-fee/changes"],
  setTopupFee: ["PUT", "/organizations/o1/top-up-fee"],
  clearTopupFee: ["DELETE", "/organizations/o1/top-up-fee"],
  listCustomers: ["GET", "/customers"],
  readPricing: ["GET", "/pricing"],
  pricingChanges: ["GET", "/pricing/changes"],
  updatePricing: ["PUT", "/pricing/settings"],
  readKeySettings: ["GET", "/key-settings"],
  keySettingsChanges: ["GET", "/key-settings/changes"],
  updateKeySettings: ["PUT", "/key-settings"],
  readCatalogSettings: ["GET", "/catalog-settings"],
  catalogSettingsChanges: ["GET", "/catalog-settings/changes"],
  updateCatalogSettings: ["PUT", "/catalog-settings"],
  readAccountSettings: ["GET", "/account-settings"],
  accountSettingsChanges: ["GET", "/account-settings/changes"],
  updateAccountSettings: ["PUT", "/account-settings"],
  readUserOrganizationLimit: ["GET", "/account-settings/users/u1"],
  setUserOrganizationLimit: ["PUT", "/account-settings/users/u1"],
  readPayments: ["GET", "/payments"],
  paymentChanges: ["GET", "/payments/changes"],
  updatePaymentSettings: ["PUT", "/payments/settings"],
  updatePaymentProvider: ["PUT", "/payments/providers/stripe"],
  listPromoCodes: ["GET", "/promo-codes"],
  readPromoCode: ["GET", "/promo-codes/p1"],
  promoCodeReport: ["GET", "/promo-codes/p1/report"],
  promoCodeChanges: ["GET", "/promo-codes/p1/changes"],
  createPromoCode: ["POST", "/promo-codes"],
  updatePromoCode: ["PUT", "/promo-codes/p1"],
  pausePromoCode: ["PUT", "/promo-codes/p1/paused"],
  readProviders: ["GET", "/providers"],
  setProviderTerms: ["PUT", "/providers/k1/terms"],
  removeProviderTerms: ["DELETE", "/providers/k1/terms"],
  syncProviderPrices: ["POST", "/providers/price-sync"],
  readProviderAccounts: ["GET", "/providers/accounts"],
  createProvider: ["POST", "/providers/accounts"],
  deleteProvider: ["DELETE", "/providers/accounts/openai"],
  refreshProviderModels: ["POST", "/providers/accounts/openai/refresh-models"],
  createProviderKey: ["POST", "/providers/accounts/openai/keys"],
  updateProviderKey: ["PUT", "/providers/accounts/openai/keys/k1"],
  deleteProviderKey: ["DELETE", "/providers/accounts/openai/keys/k1"],
  readRequests: ["GET", "/requests"],
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
  "readDataPolicy",
  "readTopupFee",
  "topupFeeChanges",
  "listCustomers",
  "readPricing",
  "pricingChanges",
  "readKeySettings",
  "keySettingsChanges",
  "readCatalogSettings",
  "catalogSettingsChanges",
  "readAccountSettings",
  "accountSettingsChanges",
  "readUserOrganizationLimit",
  "readPayments",
  "paymentChanges",
  "listPromoCodes",
  "readPromoCode",
  "promoCodeReport",
  "promoCodeChanges",
  "readProviders",
  "readProviderAccounts",
  "readRequests",
];

const ALLOWED: Record<string, Route[]> = {
  admin: Object.keys(ROUTES) as Route[],
  finance: [
    ...READ_ONLY,
    "adjustCredits",
    "updatePricing",
    "updateKeySettings",
    "updateCatalogSettings",
    "setTopupFee",
    "clearTopupFee",
    "updatePaymentSettings",
    "updatePaymentProvider",
    "createPromoCode",
    "updatePromoCode",
    "pausePromoCode",
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
    ["DELETE", "/payments/providers/stripe"],
    ["PUT", "/payments"],
    ["GET", "/users/u1/accounts/extra/../.."],
    ["DELETE", "/customers"],
    ["HEAD", "/summary"],
    ["DELETE", "/promo-codes/p1"],
    ["GET", "/promo-codes/p1/redemptions/extra"],
  ])("refuses %s %s, which no rule covers, to every role", (method, path) => {
    if (findAccessRule(method, path)) {
      return; // a rule exists; the matrix above checks it
    }
    for (const role of Object.keys(ALLOWED)) {
      expect(canAccessAdminRoute({ role }, method, path)).toBe(false);
    }
  });

  // there is no markup on requests (ADR 0010): the per-provider markup routes
  // are gone, and a request to one is refused to every role, admin included
  it.each([
    ["PUT", "/pricing/providers/openai"],
    ["DELETE", "/pricing/providers/openai"],
    ["PUT", "/pricing/markup"],
  ])("has no rule for %s %s and refuses it to every role", (method, path) => {
    expect(findAccessRule(method, path)).toBeUndefined();
    for (const role of [...Object.keys(ALLOWED), "admin,finance"]) {
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
    ["/payments/settings", "settings"],
    ["/key-settings", "key-settings"],
    ["/account-settings", "account-settings"],
    ["/catalog-settings", "catalog-settings"],
    ["/account-settings/users/u1", "u1"],
    ["/payments/providers/stripe", "stripe"],
    ["/providers/k1/terms", "k1"],
    ["/providers/accounts/openai", "openai"],
    ["/providers/accounts/openai/keys/k1", "openai"],
    ["/promo-codes/p1", "p1"],
    ["/promo-codes/p1/paused", "p1"],
    ["/model-prices", "model-prices"],
    ["/model-prices/import", "import"],
    ["/model-prices/suggestions/7/approve", "7"],
  ])("targets %s at %s", (path, target) => {
    expect(auditTarget(path)).toBe(target);
  });

  // a provider key's value is the one secret an admin route carries
  it.each([
    ["POST", "/providers/accounts/openai/keys"],
    ["PUT", "/providers/accounts/openai/keys/k1"],
  ])("keeps the key value of %s %s out of the log", (method, path) => {
    const audit = findAccessRule(method, path)?.audit;
    expect(
      withoutSecrets(
        { name: "main", value: "sk-live-secret", models: ["*"] },
        audit?.secrets ?? [],
      ),
    ).toEqual({ name: "main", value: "[redacted]", models: ["*"] });
  });

  it("leaves a body with no secret as it is", () => {
    expect(withoutSecrets({ region: "sa" }, [])).toEqual({ region: "sa" });
    expect(withoutSecrets(undefined, ["value"])).toBeUndefined();
  });
});
