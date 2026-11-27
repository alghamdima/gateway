import { Hono } from "hono";
import * as z from "zod";

import {
  enforceAuth,
  enforceOrganizationPermission,
  validate,
} from "../../middleware";
import { getBalance, getEntries } from "./ledger";
import { getLowBalanceSetting, setLowBalanceSetting } from "./low-balance";
import {
  getTopUpOptions,
  MICRO_USD_PER_CENT,
  quoteTopUp,
  startTopUp,
} from "./top-up";

const getEntriesQuerySchema = z.object({
  limit: z.coerce.number().int().min(1).max(100).default(25),
  before: z.string().optional(),
});

/**
 * Credit to buy in micro-USD: whole cents. The limits per payment are admin
 * settings, checked against the stored values when the request is handled
 * (quoteTopUp).
 */
const topUpAmountSchema = z.coerce
  .number()
  .int()
  .positive()
  .refine((value) => value % MICRO_USD_PER_CENT === 0, {
    message: "Must be a whole number of cents",
  });

export const creditsRouter = new Hono()
  .use(enforceAuth)
  .get(
    "/:organizationId",
    async (c, next) =>
      enforceOrganizationPermission({
        organizationId: c.req.param("organizationId"),
        permissions: { billing: ["read"] },
      })(c, next),
    async (c) => c.json(await getBalance(c.req.param("organizationId"))),
  )
  .get(
    "/:organizationId/entries",
    async (c, next) =>
      enforceOrganizationPermission({
        organizationId: c.req.param("organizationId"),
        permissions: { billing: ["read"] },
      })(c, next),
    validate("query", getEntriesQuerySchema),
    async (c) =>
      c.json(
        await getEntries({
          organizationId: c.req.param("organizationId"),
          ...c.req.valid("query"),
        }),
      ),
  )
  .get(
    "/:organizationId/low-balance-alert",
    async (c, next) =>
      enforceOrganizationPermission({
        organizationId: c.req.param("organizationId"),
        permissions: { billing: ["read"] },
      })(c, next),
    async (c) =>
      c.json(await getLowBalanceSetting(c.req.param("organizationId"))),
  )
  .put(
    "/:organizationId/low-balance-alert",
    async (c, next) =>
      enforceOrganizationPermission({
        organizationId: c.req.param("organizationId"),
        permissions: { billing: ["update"] },
      })(c, next),
    validate(
      "json",
      z.object({
        // micro-USD, whole cents; null turns the alert off
        thresholdMicroUsd: z
          .number()
          .int()
          .positive()
          .max(1_000_000 * 1_000_000)
          .refine((value) => value % 10_000 === 0)
          .nullable(),
      }),
    ),
    async (c) =>
      c.json(
        await setLowBalanceSetting(
          c.req.param("organizationId"),
          c.req.valid("json").thresholdMicroUsd,
        ),
      ),
  )
  .get(
    "/:organizationId/top-up/options",
    async (c, next) =>
      enforceOrganizationPermission({
        organizationId: c.req.param("organizationId"),
        permissions: { billing: ["create"] },
      })(c, next),
    async (c) => c.json(await getTopUpOptions()),
  )
  .get(
    "/:organizationId/top-up/quote",
    async (c, next) =>
      enforceOrganizationPermission({
        organizationId: c.req.param("organizationId"),
        permissions: { billing: ["create"] },
      })(c, next),
    validate("query", z.object({ amount: topUpAmountSchema })),
    async (c) => c.json(await quoteTopUp(c.req.valid("query").amount)),
  )
  .post(
    "/:organizationId/top-up",
    async (c, next) =>
      enforceOrganizationPermission({
        organizationId: c.req.param("organizationId"),
        permissions: { billing: ["create"] },
      })(c, next),
    validate("json", z.object({ amount: topUpAmountSchema })),
    async (c) =>
      c.json(
        await startTopUp({
          user: c.var.user,
          organizationId: c.req.param("organizationId"),
          creditMicroUsd: c.req.valid("json").amount,
          origin: new URL(c.req.url).origin,
        }),
      ),
  );
