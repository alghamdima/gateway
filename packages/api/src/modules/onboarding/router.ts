import { Hono } from "hono";
import * as z from "zod";

import { enforceAuth, validate } from "../../middleware";
import {
  ACCOUNT_TYPES,
  createStarterKey,
  finishOnboarding,
  getOnboarding,
} from "./onboarding";

/** The welcome wizard's own routes (ONB-03, ONB-08): the user acts on their own onboarding only. */
export const onboardingRouter = new Hono()
  .use(enforceAuth)
  .get("/", async (c) => c.json(await getOnboarding(c.var.user.id)))
  .post(
    "/starter-key",
    validate("json", z.object({ organizationId: z.string().min(1) })),
    async (c) =>
      c.json(
        await createStarterKey({
          userId: c.var.user.id,
          organizationId: c.req.valid("json").organizationId,
        }),
      ),
  )
  .post(
    "/complete",
    validate("json", z.object({ accountType: z.enum(ACCOUNT_TYPES) })),
    async (c) =>
      c.json(
        await finishOnboarding(c.var.user.id, c.req.valid("json").accountType),
      ),
  );
