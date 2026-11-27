import { Hono } from "hono";
import { timingSafeEqual } from "node:crypto";

import { runCreditExpiryJob } from "../credits/expiry";
import { checkLowBalances } from "../credits/low-balance";
import { applyPendingPolicies } from "../data-policy/policy";
import {
  runModelPricesJob,
  runModelPriceSuggestionsJob,
} from "../model-prices/jobs";
import { runDailyReconciliation } from "./daily";
import { env } from "./env";

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

const lowBalances = async (c: Context) => {
  if (!isAuthorized(c.req.header("authorization"))) {
    return c.json({ error: "unauthorized" }, 401);
  }
  return c.json(await checkLowBalances({ origin: new URL(c.req.url).origin }));
};

const dataPolicies = async (c: Context) => {
  if (!isAuthorized(c.req.header("authorization"))) {
    return c.json({ error: "unauthorized" }, 401);
  }
  return c.json(await applyPendingPolicies());
};

const modelPrices = async (c: Context) => {
  if (!isAuthorized(c.req.header("authorization"))) {
    return c.json({ error: "unauthorized" }, 401);
  }
  const { pushed, check, repaired, orphansDeleted } = await runModelPricesJob();
  return c.json({
    pushed,
    repaired: repaired ?? null,
    orphansDeleted,
    drift: check.drift.length,
  });
};

const creditExpiry = async (c: Context) => {
  if (!isAuthorized(c.req.header("authorization"))) {
    return c.json({ error: "unauthorized" }, 401);
  }
  return c.json(await runCreditExpiryJob());
};

const modelPriceSuggestions = async (c: Context) => {
  if (!isAuthorized(c.req.header("authorization"))) {
    return c.json({ error: "unauthorized" }, 401);
  }
  return c.json(await runModelPriceSuggestionsJob());
};

/**
 * /api/internal: jobs a scheduler calls with `Authorization: Bearer
 * <CRON_SECRET>`. Without CRON_SECRET set, every call is refused. GET is
 * accepted because some schedulers (Vercel Cron) only send GET; a POST must
 * carry a JSON content type to pass the CSRF check.
 *
 * - /reconciliation: once a day, after 00:00 UTC (step 10.1)
 * - /low-balance: hourly (step 10.2)
 * - /data-policy: every few minutes; pushes data policies whose keys do not
 *   carry them yet, after a gateway failure (step D2)
 * - /model-prices: every minute; pushes model prices whose time has come or
 *   that the gateway lacks, and mends drift (ADMIN-09, ADR 0018)
 * - /model-price-suggestions: daily, after the gateway's own 24 h price sync;
 *   proposes the datasheet's prices that differ from ours (ADR 0018)
 * - /credit-expiry: hourly; expires what is left of credit lots past their
 *   expiry date, idempotently (BILL-17, ADR 0019)
 */
export const internalRouter = new Hono()
  .get("/reconciliation", reconcileYesterday)
  .post("/reconciliation", reconcileYesterday)
  .get("/low-balance", lowBalances)
  .post("/low-balance", lowBalances)
  .get("/data-policy", dataPolicies)
  .post("/data-policy", dataPolicies)
  .get("/model-prices", modelPrices)
  .post("/model-prices", modelPrices)
  .get("/model-price-suggestions", modelPriceSuggestions)
  .post("/model-price-suggestions", modelPriceSuggestions)
  .get("/credit-expiry", creditExpiry)
  .post("/credit-expiry", creditExpiry);
