import { Hono } from "hono";

import { enforceAdmin, enforceAuth } from "../../middleware";
import { accountSettingsAdminRouter } from "../accounts/admin-router";
import { catalogSettingsAdminRouter } from "../catalog/admin-router";
import { keySettingsAdminRouter } from "../keys/admin-router";
import { modelPricesAdminRouter } from "../model-prices/router";
import { paymentsRouter } from "../payments/router";
import { pricingRouter } from "../pricing/router";
import { promoCodesAdminRouter } from "../promo-codes/router";
import { listReconciliationRuns } from "../reconciliation/reconcile";
import {
  contactRequestsAdminRouter,
  siteSettingsAdminRouter,
} from "../site/admin-router";
import { enforceStaffAccess } from "./access-middleware";
import { auditRouter } from "./audit/router";
import { getCustomersCount } from "./customers/queries";
import { customersRouter } from "./customers/router";
import { getOrganizationsCount } from "./organizations/queries";
import { organizationsRouter } from "./organizations/router";
import { providersRouter } from "./providers/router";
import { requestsRouter } from "./requests/router";
import { getUsersCount } from "./users/queries";
import { usersRouter } from "./users/router";

export const adminRouter = new Hono()
  .use(enforceAuth)
  .use(enforceAdmin)
  .use(enforceStaffAccess)
  .route("/users", usersRouter)
  .route("/organizations", organizationsRouter)
  .route("/customers", customersRouter)
  .route("/pricing", pricingRouter)
  .route("/key-settings", keySettingsAdminRouter)
  .route("/account-settings", accountSettingsAdminRouter)
  .route("/catalog-settings", catalogSettingsAdminRouter)
  .route("/site-settings", siteSettingsAdminRouter)
  .route("/contact-requests", contactRequestsAdminRouter)
  .route("/model-prices", modelPricesAdminRouter)
  .route("/payments", paymentsRouter)
  .route("/promo-codes", promoCodesAdminRouter)
  .route("/providers", providersRouter)
  .route("/requests", requestsRouter)
  .route("/audit", auditRouter)
  // the daily gateway/ledger comparisons, newest first (credits:read, access.ts)
  .get("/reconciliation", async (c) => c.json(await listReconciliationRuns()))
  .get("/summary", async (c) => {
    const [users, organizations, customers] = await Promise.all([
      getUsersCount(),
      getOrganizationsCount(),
      getCustomersCount(),
    ]);

    return c.json({ users, organizations, customers });
  });
