import { Hono } from "hono";
import * as z from "zod";

import { HttpStatusCode } from "@workspace/shared/constants";
import { HttpException } from "@workspace/shared/utils";

import { validate } from "../../middleware";
import {
  pricingProviderSchema,
  setProviderMarkupSchema,
  updatePricingSettingsSchema,
} from "../../schema";
import {
  getPricing,
  getPricingChanges,
  removeProviderMarkup,
  setProviderMarkup,
  updatePricingSettings,
} from "./pricing";

import type { AuditChange } from "../admin/access-middleware";
import type { User } from "@workspace/auth";

const providerParamSchema = z.object({ provider: pricingProviderSchema });

/** Mounted under /api/admin, behind enforceAdmin. */
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
  .put(
    "/providers/:provider",
    validate("param", providerParamSchema),
    validate("json", setProviderMarkupSchema),
    async (c) =>
      c.json(
        await setProviderMarkup({
          ...c.req.valid("param"),
          ...c.req.valid("json"),
          staffId: c.var.user.id,
        }),
      ),
  )
  .delete(
    "/providers/:provider",
    validate("param", providerParamSchema),
    async (c) => {
      const isRemoved = await removeProviderMarkup({
        ...c.req.valid("param"),
        staffId: c.var.user.id,
      });
      if (!isRemoved) {
        throw new HttpException(HttpStatusCode.NOT_FOUND, {
          code: "error.notFound",
        });
      }
      return c.json({ isRemoved });
    },
  );
