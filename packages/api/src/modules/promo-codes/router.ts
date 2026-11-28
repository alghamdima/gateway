import { Hono } from "hono";
import * as z from "zod";

import {
  enforceAuth,
  enforceOrganizationPermission,
  validate,
} from "../../middleware";
import {
  createPromoCode,
  getPromoCode,
  getPromoCodeChanges,
  getPromoCodeReport,
  listPromoCodes,
  setPromoCodePaused,
  updatePromoCode,
} from "./codes";
import { redeemFreeCredit } from "./redeem";
import {
  customerPromoCodeSchema,
  listPromoCodesQuerySchema,
  savePromoCodeSchema,
  setPromoCodePausedSchema,
} from "./schema";

import type { AuditChange } from "../admin/access-middleware";
import type { User } from "@workspace/auth";

/**
 * Staff's promo codes (BILL-08), mounted under /api/admin/promo-codes behind
 * enforceAdmin; access rules in admin/access.ts (promoCodes: read, update).
 * Each change sets auditChange, so the audit log keeps before and after.
 */
export const promoCodesAdminRouter = new Hono<{
  Variables: { user: User; auditChange?: AuditChange };
}>()
  .get("/", validate("query", listPromoCodesQuerySchema), async (c) =>
    c.json(await listPromoCodes(c.req.valid("query"))),
  )
  .post("/", validate("json", savePromoCodeSchema), async (c) => {
    const { code, before, after } = await createPromoCode({
      ...c.req.valid("json"),
      staffId: c.var.user.id,
    });
    c.set("auditChange", { before, after: { id: code.id, ...after } });
    return c.json(code);
  })
  .get("/:id", async (c) => c.json(await getPromoCode(c.req.param("id"))))
  .get("/:id/report", async (c) =>
    c.json(await getPromoCodeReport(c.req.param("id"))),
  )
  .get("/:id/changes", async (c) =>
    c.json(await getPromoCodeChanges(c.req.param("id"))),
  )
  .put("/:id", validate("json", savePromoCodeSchema), async (c) => {
    const { code, before, after } = await updatePromoCode({
      ...c.req.valid("json"),
      id: c.req.param("id"),
      staffId: c.var.user.id,
    });
    c.set("auditChange", { before, after });
    return c.json(code);
  })
  .put("/:id/paused", validate("json", setPromoCodePausedSchema), async (c) => {
    const { code, before, after } = await setPromoCodePaused({
      id: c.req.param("id"),
      isPaused: c.req.valid("json").isPaused,
      staffId: c.var.user.id,
    });
    c.set("auditChange", { before, after });
    return c.json(code);
  });

/**
 * A customer redeems a free-credit code for their organization (BILL-08),
 * mounted under /api/promo-codes. The same permission as a top-up: owners
 * and admins of the organization.
 */
export const promoCodesRouter = new Hono().use(enforceAuth).post(
  "/:organizationId/redeem",
  async (c, next) =>
    enforceOrganizationPermission({
      organizationId: c.req.param("organizationId"),
      permissions: { billing: ["create"] },
    })(c, next),
  validate("json", z.object({ code: customerPromoCodeSchema })),
  async (c) =>
    c.json(
      await redeemFreeCredit({
        organizationId: c.req.param("organizationId"),
        code: c.req.valid("json").code,
        userId: c.var.user.id,
      }),
    ),
);
