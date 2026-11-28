import { Hono } from "hono";
import * as z from "zod";

import { auth } from "@workspace/auth/server";

import {
  enforceAuth,
  enforceOrganizationPermission,
  validate,
} from "../../middleware";
import { customerPromoCodeSchema } from "../promo-codes/schema";
import { getBalance, getEntries } from "./ledger";
import { getCreditLots } from "./lots";
import { getLowBalanceSetting, setLowBalanceSetting } from "./low-balance";
import { requestRefund } from "./refund";
import {
  getTopUpOptions,
  MICRO_SAR_PER_HALALA,
  quoteTopUp,
  startTopUp,
} from "./top-up";
import {
  countTransferAttempt,
  getTransferOptions,
  transferCredits,
  transferGateRefusal,
} from "./transfer";

const getEntriesQuerySchema = z.object({
  limit: z.coerce.number().int().min(1).max(100).default(25),
  before: z.string().optional(),
});

/**
 * Credit to buy in micro-SAR: whole halalas. The limits per payment are admin
 * settings, checked against the stored values when the request is handled
 * (quoteTopUp).
 */
const topUpAmountSchema = z.coerce
  .number()
  .int()
  .positive()
  .refine((value) => value % MICRO_SAR_PER_HALALA === 0, {
    message: "Must be a whole number of halalas",
  });

/** An optional promo code as typed (BILL-08); the promo codes module checks it. */
const promoCodeField = customerPromoCodeSchema.optional();

export const creditsRouter = new Hono()
  .use(enforceAuth)
  // Moving credit from the user's personal account to one of their
  // organizations (BILL-12, ADR 0020). No organization permission is asked:
  // the credit is the user's own, and any member may fund their organization.
  .get("/transfer/options", async (c) =>
    c.json({
      ...(await getTransferOptions(c.var.user.id)),
      isTwoFactorEnabled: c.var.user.twoFactorEnabled === true,
    }),
  )
  .post(
    "/transfer",
    validate(
      "json",
      z.object({
        toOrganizationId: z.string().min(1),
        amountMicroSar: topUpAmountSchema,
        /** A fresh code from the user's authenticator app, asked at every move. */
        code: z.string().regex(/^\d{6}$/),
        /** The form's key for this one move: sending it again moves nothing twice. */
        idempotencyKey: z.string().min(16).max(100),
      }),
    ),
    async (c) => {
      const { code, ...move } = c.req.valid("json");
      if (c.var.user.twoFactorEnabled !== true) {
        throw transferGateRefusal("twoFactorRequired");
      }
      await countTransferAttempt(c.var.user.id);
      try {
        await auth.api.verifyTOTP({
          body: { code },
          headers: c.req.raw.headers,
        });
      } catch {
        throw transferGateRefusal("invalidCode");
      }
      return c.json(await transferCredits({ ...move, userId: c.var.user.id }));
    },
  )
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
  // each credit lot's remainder, expiry and refund (BILL-10, BILL-17, ADR 0019)
  .get(
    "/:organizationId/lots",
    async (c, next) =>
      enforceOrganizationPermission({
        organizationId: c.req.param("organizationId"),
        permissions: { billing: ["read"] },
      })(c, next),
    async (c) => c.json(await getCreditLots(c.req.param("organizationId"))),
  )
  // the self-serve refund: who may pay may refund (BILL-10)
  .post(
    "/:organizationId/lots/:lotId/refund",
    async (c, next) =>
      enforceOrganizationPermission({
        organizationId: c.req.param("organizationId"),
        permissions: { billing: ["create"] },
      })(c, next),
    async (c) =>
      c.json(
        await requestRefund({
          organizationId: c.req.param("organizationId"),
          lotId: c.req.param("lotId"),
          userId: c.var.user.id,
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
        // micro-SAR, whole halalas; null turns the alert off
        thresholdMicroSar: z
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
          c.req.valid("json").thresholdMicroSar,
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
    async (c) => c.json(await getTopUpOptions(c.req.param("organizationId"))),
  )
  .get(
    "/:organizationId/top-up/quote",
    async (c, next) =>
      enforceOrganizationPermission({
        organizationId: c.req.param("organizationId"),
        permissions: { billing: ["create"] },
      })(c, next),
    validate(
      "query",
      z.object({ amount: topUpAmountSchema, code: promoCodeField }),
    ),
    async (c) =>
      c.json(
        await quoteTopUp(
          c.req.param("organizationId"),
          c.req.valid("query").amount,
          c.req.valid("query").code,
        ),
      ),
  )
  .post(
    "/:organizationId/top-up",
    async (c, next) =>
      enforceOrganizationPermission({
        organizationId: c.req.param("organizationId"),
        permissions: { billing: ["create"] },
      })(c, next),
    validate(
      "json",
      z.object({ amount: topUpAmountSchema, promoCode: promoCodeField }),
    ),
    async (c) =>
      c.json(
        await startTopUp({
          user: c.var.user,
          organizationId: c.req.param("organizationId"),
          creditMicroSar: c.req.valid("json").amount,
          promoCode: c.req.valid("json").promoCode,
          origin: new URL(c.req.url).origin,
        }),
      ),
  );
