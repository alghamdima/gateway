import { Hono } from "hono";
import { timingSafeEqual } from "node:crypto";

import { env } from "../../env";
import { runDailyReconciliation } from "./daily";

import type { Context } from "hono";

const isAuthorized = (header: string | undefined) => {
  const secret = env.CRON_SECRET;
  if (!secret || !header) {
    return false;
  }
  const expected = Buffer.from(`Bearer ${secret}`);
  const given = Buffer.from(header);
  return expected.length === given.length && timingSafeEqual(expected, given);
};

const reconcileYesterday = async (c: Context) => {
  if (!isAuthorized(c.req.header("authorization"))) {
    return c.json({ error: "unauthorized" }, 401);
  }
  const result = await runDailyReconciliation({
    origin: new URL(c.req.url).origin,
  });
  return c.json({
    id: result.id,
    status: result.status,
    alerted: result.alerted,
  });
};

/**
 * /api/internal: jobs a scheduler calls with `Authorization: Bearer
 * <CRON_SECRET>`. Without CRON_SECRET set, every call is refused. GET is
 * accepted because some schedulers (Vercel Cron) only send GET; a POST must
 * carry a JSON content type to pass the CSRF check.
 */
export const internalRouter = new Hono()
  .get("/reconciliation", reconcileYesterday)
  .post("/reconciliation", reconcileYesterday);
