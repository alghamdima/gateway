import { Hono } from "hono";

import * as mobile from "@workspace/billing-mobile/server";
import {
  checkoutSchema,
  getBillingPortalSchema,
} from "@workspace/billing-web/schema";
import * as web from "@workspace/billing-web/server";
import { getCustomersWithPurchasesByReferenceId } from "@workspace/billing/server";

import { enforceAuth, validate } from "../../middleware";

export const billingRouter = new Hono()
  .get("/me", enforceAuth, async (c) =>
    c.json(await getCustomersWithPurchasesByReferenceId(c.var.user.id)),
  )
  .post("/checkout", validate("json", checkoutSchema), enforceAuth, async (c) =>
    c.json(
      await web.checkout({
        user: c.var.user,
        ...c.req.valid("json"),
      }),
    ),
  )
  .get(
    "/portal",
    enforceAuth,
    validate("query", getBillingPortalSchema),
    async (c) =>
      c.json(
        await web.getBillingPortal({
          user: c.var.user,
          ...c.req.valid("query"),
        }),
      ),
  )
  .post(`/webhook/${web.provider}`, (c) => web.webhookHandler(c.req.raw))
  .post(`/webhook/${mobile.provider}`, (c) => mobile.webhookHandler(c.req.raw));
