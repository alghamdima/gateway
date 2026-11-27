import { Hono } from "hono";
import * as z from "zod";

import { validate } from "../../middleware";
import {
  paymentProviderSchema,
  updatePaymentProviderSchema,
  updatePaymentSettingsSchema,
} from "../../schema";
import {
  getPaymentChanges,
  getPaymentsOverview,
  updatePaymentProvider,
  updatePaymentSettings,
} from "./settings";

import type { User } from "@workspace/auth";

const providerParamSchema = z.object({ provider: paymentProviderSchema });

/** Mounted under /api/admin, behind enforceAdmin; access rules in admin/access.ts. */
export const paymentsRouter = new Hono<{ Variables: { user: User } }>()
  .get("/", async (c) => {
    const overview = await getPaymentsOverview();
    // the URL the provider's dashboard must call, on the site staff are using
    const origin = new URL(c.req.url).origin;
    return c.json({
      ...overview,
      providers: overview.providers.map(({ webhookPath, ...provider }) => ({
        ...provider,
        webhookUrl: new URL(webhookPath, origin).toString(),
      })),
    });
  })
  .get("/changes", async (c) => c.json(await getPaymentChanges()))
  .put("/settings", validate("json", updatePaymentSettingsSchema), async (c) =>
    c.json(
      await updatePaymentSettings({
        ...c.req.valid("json"),
        staffId: c.var.user.id,
      }),
    ),
  )
  .put(
    "/providers/:provider",
    validate("param", providerParamSchema),
    validate("json", updatePaymentProviderSchema),
    async (c) =>
      c.json(
        await updatePaymentProvider({
          ...c.req.valid("param"),
          ...c.req.valid("json"),
          staffId: c.var.user.id,
        }),
      ),
  );
