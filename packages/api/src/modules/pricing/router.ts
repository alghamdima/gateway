import { Hono } from "hono";

import { validate } from "../../middleware";
import {
  updateCreditTermsSchema,
  updatePricingSettingsSchema,
} from "../../schema";
import {
  getPricing,
  getPricingChanges,
  updateCreditTerms,
  updatePricingSettings,
} from "./pricing";

import type { AuditChange } from "../admin/access-middleware";
import type { User } from "@workspace/auth";

/**
 * Mounted under /api/admin, behind enforceAdmin. Staff set the top-up fee,
 * the top-up limits and the USD to SAR rate; there is no markup on requests
 * (docs/platform/adr/0010-admin-controlled-pricing.md).
 */
export const pricingRouter = new Hono<{
  Variables: { user: User; auditChange?: AuditChange };
}>()
  .get("/", async (c) => c.json(await getPricing()))
  .get("/changes", async (c) => c.json(await getPricingChanges()))
  .put(
    "/settings",
    validate("json", updatePricingSettingsSchema),
    async (c) => {
      const { settings, before, after } = await updatePricingSettings({
        ...c.req.valid("json"),
        staffId: c.var.user.id,
      });
      // the audit log keeps the values before and after, as pricing_change does
      c.set("auditChange", { before, after });
      return c.json(settings);
    },
  )
  // the refund window and the credit expiry (ADMIN-23)
  .put(
    "/credit-terms",
    validate("json", updateCreditTermsSchema),
    async (c) => {
      const { creditTerms, before, after } = await updateCreditTerms({
        ...c.req.valid("json"),
        staffId: c.var.user.id,
      });
      c.set("auditChange", { before, after });
      return c.json(creditTerms);
    },
  );
