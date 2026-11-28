import { Hono } from "hono";
import { createMiddleware } from "hono/factory";

import { Permissions, User } from "@workspace/auth";
import {
  checkoutSchema,
  getBillingPortalSchema,
  getSummarySchema,
  getUsageSchema,
} from "@workspace/billing-web/schema";
import * as web from "@workspace/billing-web/server";
import {
  getCustomersByReferenceId,
  getCustomersWithPurchasesByReferenceId,
} from "@workspace/billing/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { HttpException } from "@workspace/shared/utils";

import {
  enforceAuth,
  enforceOrganizationPermission,
  validate,
} from "../../middleware";
import { topUpWebhookCallbacks } from "../credits/top-up";

const enforceAccessToReference = (permissions: Permissions["billing"]) =>
  createMiddleware<
    {
      Variables: {
        user: User;
      };
    },
    string,
    {
      out: {
        json: {
          referenceId?: string;
        };
      };
    }
  >(async (c, next) => {
    const referenceId =
      c.req.valid("json")?.referenceId ??
      c.req.query("referenceId") ??
      c.var.user.id;
    const user = c.var.user;

    if (referenceId === user.id) {
      return next();
    }

    return enforceOrganizationPermission({
      organizationId: referenceId,
      permissions: {
        billing: permissions,
      },
    })(c, next);
  });

export const billingRouter = new Hono()
  .get(
    "/summary",
    validate("query", getSummarySchema),
    enforceAuth,
    enforceAccessToReference(["read"]),
    async (c) =>
      c.json(
        await getCustomersWithPurchasesByReferenceId(
          c.req.valid("query").referenceId ?? c.var.user.id,
        ),
      ),
  )
  .get(
    "/usage/:meterId",
    validate("query", getUsageSchema),
    enforceAuth,
    enforceAccessToReference(["read"]),
    async (c) => {
      const customers = await getCustomersByReferenceId(
        c.req.valid("query").referenceId,
        {
          provider: web.provider,
        },
      );

      const usage = await Promise.all(
        customers.map(async (customer) =>
          web.getUsage({
            ...c.req.valid("query"),
            meterId: c.req.param("meterId"),
            externalId: customer.externalId,
          }),
        ),
      );

      return c.json({
        usage: usage.reduce((acc, curr) => acc + curr.usage, 0),
      });
    },
  )
  .post(
    "/checkout",
    validate("json", checkoutSchema),
    enforceAuth,
    enforceAccessToReference(["create"]),
    async (c) =>
      c.json(
        await web.checkout({
          user: c.var.user,
          ...c.req.valid("json"),
        }),
      ),
  )
  .get(
    "/portal",
    validate("query", getBillingPortalSchema),
    enforceAuth,
    enforceAccessToReference(["update", "delete"]),
    async (c) =>
      c.json(
        await web.getBillingPortal({
          user: c.var.user,
          ...c.req.valid("query"),
        }),
      ),
  )
  // one URL per payment provider; each verifies its own signatures (ADMIN-03)
  .post("/webhook/:provider", (c) => {
    const strategy = web.getBillingProvider(c.req.param("provider"));
    if (!strategy) {
      throw new HttpException(HttpStatusCode.NOT_FOUND, {
        code: "error.notFound",
      });
    }
    return strategy.webhookHandler(c.req.raw, topUpWebhookCallbacks);
  });
