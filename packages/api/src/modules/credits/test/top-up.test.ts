import { createHmac } from "node:crypto";
import {
  afterAll,
  beforeAll,
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from "vitest";

import type { TestDatabase } from "@workspace/db/testing";
import type postgres from "postgres";

// Needs real Postgres: the ledger's idempotency keys are what make a
// redelivered webhook count once. Skips without DATABASE_URL.
const adminUrl = process.env.DATABASE_URL;

const WEBHOOK_SECRET = "whsec_live_secret";
const TEST_WEBHOOK_SECRET = "whsec_test_secret";
const SAR = 1_000_000;
/** An organization with no fee of its own: it pays the default (ADMIN-24). */
const NO_OVERRIDE = "org_default_fee";

/** Stripe's secrets as env would hold them; tests change them (ADMIN-03). */
const stripeEnv = vi.hoisted((): Record<string, string | undefined> => ({}));
const BOTH_MODES = {
  STRIPE_SECRET_KEY: "sk_live_not_used",
  STRIPE_WEBHOOK_SECRET: "whsec_live_secret",
  STRIPE_TEST_SECRET_KEY: "sk_test_not_used",
  STRIPE_TEST_WEBHOOK_SECRET: "whsec_test_secret",
};
const setEnv = (values: Partial<typeof BOTH_MODES>) => {
  for (const name of Object.keys(BOTH_MODES)) {
    stripeEnv[name] = undefined;
  }
  Object.assign(stripeEnv, values);
};
vi.mock(
  "../../../../../billing/web/src/providers/stripe/env",
  async (importOriginal) => ({
    ...(await importOriginal<object>()),
    env: stripeEnv,
  }),
);

/** Records what would be sent to Stripe, and with which mode's client, instead of sending it. */
const stripeCalls = vi.hoisted(() => ({
  modes: [] as (string | undefined)[],
  sessions: [] as Record<string, unknown>[],
  customers: [] as Record<string, unknown>[],
}));

vi.mock("../../../../../billing/web/src/providers/stripe/sdk", () => ({
  stripe: (mode?: string) => {
    stripeCalls.modes.push(mode);
    return {
      customers: {
        create: async (params: Record<string, unknown>) => {
          stripeCalls.customers.push(params);
          return { id: `cus_${stripeCalls.customers.length}` };
        },
        retrieve: async (id: string) => ({ id }),
      },
      checkout: {
        sessions: {
          create: async (params: Record<string, unknown>) => {
            stripeCalls.sessions.push(params);
            return { id: "cs_test_1", url: "https://checkout.stripe.test/1" };
          },
        },
      },
    };
  },
}));

/** The Stripe-Signature header Stripe sends: HMAC-SHA256 of "timestamp.payload". */
const sign = (payload: string, secret = WEBHOOK_SECRET) => {
  const timestamp = Math.floor(Date.now() / 1000);
  const signature = createHmac("sha256", secret)
    .update(`${timestamp}.${payload}`)
    .digest("hex");
  return `t=${timestamp},v1=${signature}`;
};

let eventSeq = 0;
/** An event as Stripe sends it; live mode unless the test says otherwise. */
const event = (
  type: string,
  object: Record<string, unknown>,
  livemode = true,
) =>
  JSON.stringify({
    id: `evt_${++eventSeq}`,
    object: "event",
    type,
    api_version: "2026-11-18.endive",
    created: Math.floor(Date.now() / 1000),
    livemode,
    data: { object },
  });

const session = (input: {
  organizationId: string;
  paymentIntent: string;
  creditMicroSar: number;
  feeMicroSar: number;
  amountTotalHalalas?: number;
  paymentStatus?: string;
  /** What the checkout carries about the fee rate (ADMIN-24). */
  feeRate?: Record<string, string>;
}) => ({
  id: `cs_${input.paymentIntent}`,
  object: "checkout.session",
  mode: "payment",
  customer: "cus_1",
  currency: "sar",
  payment_intent: input.paymentIntent,
  payment_status: input.paymentStatus ?? "paid",
  amount_total:
    input.amountTotalHalalas ??
    (input.creditMicroSar + input.feeMicroSar) / 10_000,
  metadata: {
    purpose: "credits_top_up",
    organizationId: input.organizationId,
    creditMicroSar: String(input.creditMicroSar),
    feeMicroSar: String(input.feeMicroSar),
    ...input.feeRate,
  },
});

const refund = (input: {
  id: string;
  paymentIntent: string;
  amountHalalas: number;
  status?: string;
}) => ({
  id: input.id,
  object: "refund",
  amount: input.amountHalalas,
  currency: "sar",
  payment_intent: input.paymentIntent,
  status: input.status ?? "succeeded",
});

/** Sets the top-up limits as staff would in the admin pricing settings. */
const setLimits = async (
  topupMinMicroSar: number,
  topupMaxMicroSar: number,
  topupPresetsMicroSar: number[],
) => {
  const { updatePricingSettings } = await import("../../pricing/pricing");
  await updatePricingSettings({
    topupFeeBps: 550,
    topupFeeMinMicroSar: 3_000_000,
    topupMinMicroSar,
    topupMaxMicroSar,
    topupPresetsMicroSar,
    usdToSarRateE6: 3_750_000,
    fxSpreadBps: 0,
    staffId: "staff_limits",
  });
};
/** Sets an organization's own top-up fee as staff would in the admin (ADMIN-24). */
const setOwnFee = async (
  organizationId: string,
  values: { feeBps: number | null; feeMinMicroSar: number | null },
) => {
  const { setOrganizationTopupFee } =
    await import("../../pricing/organization-fee");
  await setOrganizationTopupFee({
    organizationId,
    staffId: "staff_fee",
    expiresAt: null,
    note: "",
    ...values,
  });
};

/** A quote without a promo code (BILL-08): no discount, no bonus. */
const plain = (amounts: {
  creditMicroSar: number;
  feeMicroSar: number;
  totalMicroSar: number;
}) => ({
  ...amounts,
  baseFeeMicroSar: amounts.feeMicroSar,
  feeDiscountMicroSar: 0,
  bonusMicroSar: 0,
  creditReceivedMicroSar: amounts.creditMicroSar,
  promo: null,
});

/** Back to the limits the migration seeds. */
const seeded = () =>
  setLimits(20 * SAR, 3750 * SAR, [40 * SAR, 100 * SAR, 200 * SAR, 400 * SAR]);

/** Changes the payment settings as staff would in the admin (ADMIN-03). */
const setPayments = async (input: {
  topupsEnabled?: boolean;
  mode?: "test" | "live";
}) => {
  const payments = await import("../../payments/settings");
  if (input.mode) {
    await payments.updatePaymentProvider({
      provider: "stripe",
      enabled: true,
      mode: input.mode,
      staffId: "staff_payments",
    });
  }
  if (input.topupsEnabled !== undefined) {
    await payments.updatePaymentSettings({
      topupsEnabled: input.topupsEnabled,
      activeProvider: "stripe",
      staffId: "staff_payments",
    });
  }
};

describe.skipIf(!adminUrl)("credit top-ups", () => {
  let database: TestDatabase;
  let sql: postgres.Sql;
  let webhookHandler: typeof import("@workspace/billing-web/server").webhookHandler;
  let topUp: typeof import("../top-up");
  let seq = 0;

  const createOrganization = async () => {
    const id = `org_topup_${++seq}`;
    await sql`insert into organization (id, name, slug, created_at) values (${id}, ${id}, ${id}, now())`;
    return id;
  };

  // the handler and callbacks the webhook route uses (billing/router.ts)
  const deliver = async (payload: string, signature = sign(payload)) =>
    webhookHandler(
      new Request("http://localhost/api/billing/webhook/stripe", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "Stripe-Signature": signature,
        },
        body: payload,
      }),
      topUp.topUpWebhookCallbacks,
    );

  const entries = async (organizationId: string) =>
    sql<{ kind: string; amount: string; idempotency_key: string }[]>`
      select kind, amount, idempotency_key from credit_ledger_entry
      where organization_id = ${organizationId} order by created_at`;

  const balance = async (organizationId: string) => {
    const [row] = await sql<
      { balance: string }[]
    >`select balance from credit_account where organization_id = ${organizationId}`;
    return Number(row?.balance ?? 0);
  };

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;

    sql = (await import("postgres")).default(database.url, { max: 4 });
    ({ webhookHandler } = await import("@workspace/billing-web/server"));
    topUp = await import("../top-up");
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  /** Back to what the migration seeds: top-ups on, Stripe active and live. */
  const resetPayments = async () => {
    await sql`update payment_provider set enabled = true, mode = 'live'`;
    await sql`update payment_settings set topups_enabled = true, active_provider = 'stripe'`;
    (await import("../../payments/settings")).clearPaymentSettingsCache();
  };
  beforeEach(async () => {
    stripeCalls.modes.length = 0;
    stripeCalls.sessions.length = 0;
    stripeCalls.customers.length = 0;
    setEnv(BOTH_MODES);
    await resetPayments();
  });

  describe("checkout", () => {
    it("quotes the fee from the admin settings: the minimum, then the percentage", async () => {
      // migration defaults: 5.5% with a SAR 3 minimum
      expect(await topUp.quoteTopUp(NO_OVERRIDE, 40 * SAR)).toEqual(
        plain({
          creditMicroSar: 40 * SAR,
          feeMicroSar: 3_000_000,
          totalMicroSar: 43_000_000,
        }),
      );
      expect((await topUp.quoteTopUp(NO_OVERRIDE, 100 * SAR)).feeMicroSar).toBe(
        5_500_000,
      );
    });

    it("asks Stripe for a payment in SAR halalas with a credits line, a fee line and the metadata the webhook reads", async () => {
      const organizationId = await createOrganization();

      const result = await topUp.startTopUp({
        user: { id: "user_1", email: "owner@example.com" },
        organizationId,
        creditMicroSar: 125 * SAR,
        origin: "https://app.example.com",
      });

      expect(result).toMatchObject({
        url: "https://checkout.stripe.test/1",
        creditMicroSar: 125 * SAR,
        feeMicroSar: 6_880_000, // 5.5% of SAR 125 = SAR 6.875, half up to SAR 6.88
        totalMicroSar: 131_880_000,
      });
      expect(stripeCalls.sessions).toHaveLength(1);
      const params = stripeCalls.sessions[0];
      const metadata = {
        purpose: "credits_top_up",
        provider: "stripe",
        mode: "live",
        organizationId,
        creditMicroSar: "125000000",
        feeMicroSar: "6880000",
      };
      expect(params).toMatchObject({
        mode: "payment",
        customer: "cus_1",
        client_reference_id: organizationId,
        metadata,
        payment_intent_data: { metadata },
        success_url: `https://app.example.com/dashboard/${organizationId}/credits?top_up=success`,
        cancel_url: `https://app.example.com/dashboard/${organizationId}/credits?top_up=cancelled`,
      });
      expect(params?.line_items).toEqual([
        {
          quantity: 1,
          price_data: {
            currency: "sar",
            unit_amount: 12500, // halalas: Stripe's minor unit for SAR
            product_data: { name: "Credits" },
          },
        },
        {
          quantity: 1,
          price_data: {
            currency: "sar",
            unit_amount: 688,
            product_data: { name: "Payment processing fee" },
          },
        },
      ]);
      expect(params).not.toHaveProperty("payment_method_types");
      // the live client: the mode migration 0032 seeds
      expect(stripeCalls.modes).toEqual(["live", "live"]);
      // starting a checkout credits nothing
      expect(await entries(organizationId)).toHaveLength(0);
    });

    describe("limits per payment, set by staff (ADMIN-05)", () => {
      it("gives the form the stored limits, quick picks and fee", async () => {
        expect(await topUp.getTopUpOptions(NO_OVERRIDE)).toEqual({
          isAvailable: true,
          minMicroSar: 20 * SAR,
          maxMicroSar: 3750 * SAR,
          presetsMicroSar: [40 * SAR, 100 * SAR, 200 * SAR, 400 * SAR],
          feeBps: 550,
          feeMinMicroSar: 3_000_000,
        });
      });

      it("refuses an amount under the stored minimum or over the stored maximum", async () => {
        const organizationId = await createOrganization();
        await expect(
          topUp.quoteTopUp(NO_OVERRIDE, 20 * SAR - 10_000),
        ).rejects.toMatchObject({
          status: 400,
          code: "billing:error.topUpAmount",
        });
        await expect(
          topUp.quoteTopUp(NO_OVERRIDE, 3750 * SAR + 10_000),
        ).rejects.toMatchObject({
          status: 400,
          code: "billing:error.topUpAmount",
        });
        await expect(
          topUp.startTopUp({
            user: { id: "user_1", email: "owner@example.com" },
            organizationId,
            creditMicroSar: 4000 * SAR,
            origin: "https://app.example.com",
          }),
        ).rejects.toMatchObject({ status: 400 });
        expect(stripeCalls.sessions).toHaveLength(0);
        // the limits themselves are allowed
        expect(
          (await topUp.quoteTopUp(NO_OVERRIDE, 20 * SAR)).creditMicroSar,
        ).toBe(20 * SAR);
        expect(
          (await topUp.quoteTopUp(NO_OVERRIDE, 3750 * SAR)).creditMicroSar,
        ).toBe(3750 * SAR);
      });

      it("follows the limits as soon as staff change them", async () => {
        const organizationId = await createOrganization();
        try {
          await setLimits(50 * SAR, 5000 * SAR, [50 * SAR, 4500 * SAR]);

          expect(await topUp.getTopUpOptions(NO_OVERRIDE)).toMatchObject({
            minMicroSar: 50 * SAR,
            maxMicroSar: 5000 * SAR,
            presetsMicroSar: [50 * SAR, 4500 * SAR],
          });
          const result = await topUp.startTopUp({
            user: { id: "user_1", email: "owner@example.com" },
            organizationId,
            creditMicroSar: 4500 * SAR,
            origin: "https://app.example.com",
          });
          expect(result).toMatchObject({ creditMicroSar: 4500 * SAR });
          expect(stripeCalls.sessions).toHaveLength(1);
          // SAR 40 was allowed before the minimum went up to SAR 50
          await expect(
            topUp.quoteTopUp(NO_OVERRIDE, 40 * SAR),
          ).rejects.toMatchObject({
            status: 400,
          });
        } finally {
          await seeded();
        }
      });
    });

    it("refuses an organization that does not exist", async () => {
      await expect(
        topUp.startTopUp({
          user: { id: "user_1", email: "owner@example.com" },
          organizationId: "org_missing",
          creditMicroSar: 40 * SAR,
          origin: "https://app.example.com",
        }),
      ).rejects.toMatchObject({ status: 404 });
      expect(stripeCalls.sessions).toHaveLength(0);
    });
  });

  describe("payment webhook", () => {
    it("credits a paid session once, however many times and ways it is delivered", async () => {
      const organizationId = await createOrganization();
      const paid = session({
        organizationId,
        paymentIntent: "pi_once",
        creditMicroSar: 10 * SAR,
        feeMicroSar: 800_000,
      });

      const first = await deliver(event("checkout.session.completed", paid));
      expect(first.status).toBe(200);
      expect(
        (await deliver(event("checkout.session.completed", paid))).status,
      ).toBe(200);
      expect(
        (await deliver(event("checkout.session.async_payment_succeeded", paid)))
          .status,
      ).toBe(200);

      expect(await entries(organizationId)).toEqual([
        {
          kind: "top_up",
          amount: String(10 * SAR),
          idempotency_key: "top_up:stripe:pi_once",
        },
      ]);
      expect(await balance(organizationId)).toBe(10 * SAR);
    });

    it("credits a delayed payment only when the money arrives", async () => {
      const organizationId = await createOrganization();
      const input = {
        organizationId,
        paymentIntent: "pi_delayed",
        creditMicroSar: 50 * SAR,
        feeMicroSar: 2_750_000,
      };

      await deliver(
        event(
          "checkout.session.completed",
          session({ ...input, paymentStatus: "unpaid" }),
        ),
      );
      expect(await entries(organizationId)).toHaveLength(0);

      await deliver(
        event("checkout.session.async_payment_succeeded", session(input)),
      );
      expect(await balance(organizationId)).toBe(50 * SAR);
    });

    it("rejects an event whose signature does not match and writes nothing", async () => {
      const organizationId = await createOrganization();
      const payload = event(
        "checkout.session.completed",
        session({
          organizationId,
          paymentIntent: "pi_forged",
          creditMicroSar: 1_000 * SAR,
          feeMicroSar: 0,
        }),
      );

      await expect(
        deliver(payload, sign(payload, "whsec_someone_else")),
      ).rejects.toMatchObject({ status: 400 });
      expect(await entries(organizationId)).toHaveLength(0);
    });

    it("credits nothing when Stripe collected a different amount than credit plus fee", async () => {
      const organizationId = await createOrganization();
      const response = await deliver(
        event(
          "checkout.session.completed",
          session({
            organizationId,
            paymentIntent: "pi_mismatch",
            creditMicroSar: 10 * SAR,
            feeMicroSar: 800_000,
            amountTotalHalalas: 500,
          }),
        ),
      );

      expect(response.status).toBe(200);
      expect(await entries(organizationId)).toHaveLength(0);
    });

    it("credits nothing for a payment that was not in SAR", async () => {
      const organizationId = await createOrganization();
      const response = await deliver(
        event("checkout.session.completed", {
          ...session({
            organizationId,
            paymentIntent: "pi_usd",
            creditMicroSar: 40 * SAR,
            feeMicroSar: 3_000_000,
          }),
          currency: "usd",
        }),
      );

      expect(response.status).toBe(200);
      expect(await entries(organizationId)).toHaveLength(0);
    });

    it("acknowledges a payment for a deleted organization without crediting it", async () => {
      const response = await deliver(
        event(
          "checkout.session.completed",
          session({
            organizationId: "org_deleted",
            paymentIntent: "pi_deleted",
            creditMicroSar: 10 * SAR,
            feeMicroSar: 800_000,
          }),
        ),
      );

      expect(response.status).toBe(200);
      const [row] = await sql<
        { count: string }[]
      >`select count(*) from credit_ledger_entry where idempotency_key = 'top_up:stripe:pi_deleted'`;
      expect(Number(row?.count)).toBe(0);
    });
  });

  describe("refund webhook", () => {
    const paidTopUp = async (paymentIntent: string) => {
      const organizationId = await createOrganization();
      await deliver(
        event(
          "checkout.session.completed",
          session({
            organizationId,
            paymentIntent,
            creditMicroSar: 10 * SAR,
            feeMicroSar: 800_000,
          }),
        ),
      );
      return organizationId;
    };

    it("removes exactly the credit on a full refund, once, and only when it succeeded", async () => {
      const organizationId = await paidTopUp("pi_full");
      const full = {
        id: "re_full",
        paymentIntent: "pi_full",
        amountHalalas: 1080,
      };

      await deliver(
        event("refund.created", refund({ ...full, status: "pending" })),
      );
      expect(await balance(organizationId)).toBe(10 * SAR);

      await deliver(event("refund.updated", refund(full)));
      await deliver(event("refund.updated", refund(full)));
      await deliver(event("refund.created", refund(full)));

      expect(await balance(organizationId)).toBe(0);
      const refunds = (await entries(organizationId)).filter(
        (entry) => entry.kind === "refund",
      );
      expect(refunds).toEqual([
        {
          kind: "refund",
          amount: String(-10 * SAR),
          idempotency_key: "refund:stripe:re_full",
        },
      ]);
    });

    it("removes the money refunded as credit on a partial refund (ADR 0019)", async () => {
      const organizationId = await paidTopUp("pi_partial");

      // SAR 5.40 of SAR 10.80 paid back removes SAR 5.40 of the SAR 10 credit
      await deliver(
        event(
          "refund.created",
          refund({
            id: "re_half",
            paymentIntent: "pi_partial",
            amountHalalas: 540,
          }),
        ),
      );
      expect(await balance(organizationId)).toBe(4_600_000);
    });

    it("ignores a refund of a payment that was not a top-up", async () => {
      const response = await deliver(
        event(
          "refund.created",
          refund({
            id: "re_plan",
            paymentIntent: "pi_plan",
            amountHalalas: 1900,
          }),
        ),
      );

      expect(response.status).toBe(200);
      const [row] = await sql<
        { count: string }[]
      >`select count(*) from credit_ledger_entry where idempotency_key = 'refund:stripe:re_plan'`;
      expect(Number(row?.count)).toBe(0);
    });
  });

  describe("fee per organization (ADMIN-24)", () => {
    const recorded = async (organizationId: string) =>
      sql<
        {
          topup_fee_bps: number | null;
          topup_fee_min_micro_sar: string | null;
          topup_fee_source: string | null;
        }[]
      >`select topup_fee_bps, topup_fee_min_micro_sar, topup_fee_source
        from credit_ledger_entry where organization_id = ${organizationId}`;

    it("quotes, shows and charges the organization's fee, and leaves other organizations on the default", async () => {
      const organizationId = await createOrganization();
      const other = await createOrganization();
      // 2% with the default SAR 3 minimum
      await setOwnFee(organizationId, { feeBps: 200, feeMinMicroSar: null });

      expect(await topUp.getTopUpOptions(organizationId)).toMatchObject({
        feeBps: 200,
        feeMinMicroSar: 3_000_000,
      });
      expect(await topUp.getTopUpOptions(other)).toMatchObject({
        feeBps: 550,
        feeMinMicroSar: 3_000_000,
      });
      // 2% of SAR 400 is SAR 8; the default would be SAR 22
      expect(await topUp.quoteTopUp(organizationId, 400 * SAR)).toEqual(
        plain({
          creditMicroSar: 400 * SAR,
          feeMicroSar: 8 * SAR,
          totalMicroSar: 408 * SAR,
        }),
      );
      expect((await topUp.quoteTopUp(other, 400 * SAR)).feeMicroSar).toBe(
        22 * SAR,
      );
      // under the percentage, the default minimum still applies
      expect(
        (await topUp.quoteTopUp(organizationId, 40 * SAR)).feeMicroSar,
      ).toBe(3 * SAR);

      const result = await topUp.startTopUp({
        user: { id: "user_1", email: "owner@example.com" },
        organizationId,
        creditMicroSar: 400 * SAR,
        origin: "https://app.example.com",
      });
      expect(result).toEqual({
        url: "https://checkout.stripe.test/1",
        ...plain({
          creditMicroSar: 400 * SAR,
          feeMicroSar: 8 * SAR,
          totalMicroSar: 408 * SAR,
        }),
      });
      expect(stripeCalls.sessions[0]?.metadata).toMatchObject({
        feeMicroSar: String(8 * SAR),
        feeBps: "200",
        feeMinMicroSar: "3000000",
        feeSource: "organization",
      });
    });

    it("carries the default's rate and source when the organization has none", async () => {
      const organizationId = await createOrganization();
      await topUp.startTopUp({
        user: { id: "user_1", email: "owner@example.com" },
        organizationId,
        creditMicroSar: 100 * SAR,
        origin: "https://app.example.com",
      });
      expect(stripeCalls.sessions[0]?.metadata).toMatchObject({
        feeBps: "550",
        feeMinMicroSar: "3000000",
        feeSource: "default",
      });
    });

    it("records on the top-up the fee rate, minimum and source the payment was quoted at", async () => {
      const organizationId = await createOrganization();
      const paid = session({
        organizationId,
        paymentIntent: "pi_own_fee",
        creditMicroSar: 400 * SAR,
        feeMicroSar: 8 * SAR,
        feeRate: {
          feeBps: "200",
          feeMinMicroSar: "3000000",
          feeSource: "organization",
        },
      });
      // the organization's fee changing after the checkout does not rewrite the payment
      await setOwnFee(organizationId, { feeBps: 900, feeMinMicroSar: 0 });

      expect(
        (await deliver(event("checkout.session.completed", paid))).status,
      ).toBe(200);
      expect(await recorded(organizationId)).toEqual([
        {
          topup_fee_bps: 200,
          topup_fee_min_micro_sar: "3000000",
          topup_fee_source: "organization",
        },
      ]);
    });

    it("still credits a payment that carries no rate or an unknown source, without recording one", async () => {
      const before = await createOrganization();
      const unknown = await createOrganization();
      // a checkout started before the rate was carried
      const old = session({
        organizationId: before,
        paymentIntent: "pi_no_rate",
        creditMicroSar: 40 * SAR,
        feeMicroSar: 3 * SAR,
      });
      const odd = session({
        organizationId: unknown,
        paymentIntent: "pi_odd_source",
        creditMicroSar: 40 * SAR,
        feeMicroSar: 3 * SAR,
        feeRate: {
          feeBps: "550",
          feeMinMicroSar: "3000000",
          feeSource: "coupon",
        },
      });

      await deliver(event("checkout.session.completed", old));
      await deliver(event("checkout.session.completed", odd));

      for (const organizationId of [before, unknown]) {
        expect(await balance(organizationId)).toBe(40 * SAR);
        expect(await recorded(organizationId)).toEqual([
          {
            topup_fee_bps: null,
            topup_fee_min_micro_sar: null,
            topup_fee_source: null,
          },
        ]);
      }
    });

    it("refuses a fee rate on anything but a top-up", async () => {
      const organizationId = await createOrganization();
      await expect(
        sql`insert into credit_ledger_entry (id, organization_id, amount, kind, idempotency_key, topup_fee_bps, topup_fee_min_micro_sar, topup_fee_source)
          values ('e_fee', ${organizationId}, 1, 'adjustment', 'k_fee', 1, 1, 'default')`,
      ).rejects.toMatchObject({
        constraint_name: "credit_ledger_entry_topup_fee",
      });
    });
  });

  describe("payment gateway chosen by staff (ADMIN-03)", () => {
    const start = (organizationId: string) =>
      topUp.startTopUp({
        user: { id: "user_1", email: "owner@example.com" },
        organizationId,
        creditMicroSar: 40 * SAR,
        origin: "https://app.example.com",
      });
    const unavailable = {
      status: 503,
      code: "billing:error.topUpsUnavailable",
    };

    it("refuses new top-ups while they are switched off, and takes them again when on", async () => {
      const organizationId = await createOrganization();
      await setPayments({ topupsEnabled: false });

      expect((await topUp.getTopUpOptions(NO_OVERRIDE)).isAvailable).toBe(
        false,
      );
      await expect(
        topUp.quoteTopUp(NO_OVERRIDE, 40 * SAR),
      ).rejects.toMatchObject(unavailable);
      await expect(start(organizationId)).rejects.toMatchObject(unavailable);
      expect(stripeCalls.sessions).toHaveLength(0);

      await setPayments({ topupsEnabled: true });
      expect((await topUp.getTopUpOptions(NO_OVERRIDE)).isAvailable).toBe(true);
      expect(await start(organizationId)).toMatchObject({
        url: "https://checkout.stripe.test/1",
      });
      expect(stripeCalls.sessions).toHaveLength(1);
    });

    it("refuses new top-ups while the active provider is disabled", async () => {
      // the admin API cannot do this (it refuses to disable the active
      // provider); the check stands on its own anyway
      await sql`update payment_provider set enabled = false`;
      (await import("../../payments/settings")).clearPaymentSettingsCache();
      await expect(start(await createOrganization())).rejects.toMatchObject(
        unavailable,
      );
      expect(stripeCalls.sessions).toHaveLength(0);
    });

    it.each([
      ["the live secrets are missing", {}],
      [
        "the live webhook secret is missing",
        { ...BOTH_MODES, STRIPE_WEBHOOK_SECRET: undefined },
      ],
      [
        "the live key is a test key",
        { ...BOTH_MODES, STRIPE_SECRET_KEY: "sk_test_named_as_live" },
      ],
    ])("refuses new top-ups when %s", async (_, values) => {
      setEnv(values);
      expect((await topUp.getTopUpOptions(NO_OVERRIDE)).isAvailable).toBe(
        false,
      );
      await expect(start(await createOrganization())).rejects.toMatchObject(
        unavailable,
      );
      expect(stripeCalls.sessions).toHaveLength(0);
    });

    it("pays with the client of the mode staff chose, and marks the payment with it", async () => {
      const organizationId = await createOrganization();
      await setPayments({ mode: "test" });

      await start(organizationId);

      expect(stripeCalls.modes).toEqual(["test", "test"]);
      expect(stripeCalls.sessions[0]?.metadata).toMatchObject({
        provider: "stripe",
        mode: "test",
      });
      // test and live are separate Stripe customers
      const [customer] = await sql<
        { provider: string }[]
      >`select provider from customer where reference_id = ${organizationId}`;
      expect(customer?.provider).toBe("stripe_test");
    });

    it("refuses new top-ups in test mode once its secrets are gone", async () => {
      await setPayments({ mode: "test" });
      setEnv({ ...BOTH_MODES, STRIPE_TEST_SECRET_KEY: undefined });
      await expect(start(await createOrganization())).rejects.toMatchObject(
        unavailable,
      );
    });

    describe("webhook", () => {
      const paid = (organizationId: string, paymentIntent: string) =>
        session({
          organizationId,
          paymentIntent,
          creditMicroSar: 10 * SAR,
          feeMicroSar: 800_000,
        });

      it("credits a live payment even while top-ups are off: the money was taken", async () => {
        const organizationId = await createOrganization();
        await setPayments({ topupsEnabled: false });

        const response = await deliver(
          event("checkout.session.completed", paid(organizationId, "pi_off")),
        );
        expect(response.status).toBe(200);
        expect(await balance(organizationId)).toBe(10 * SAR);
        const [entry] = await sql<
          { metadata: Record<string, unknown> }[]
        >`select metadata from credit_ledger_entry where idempotency_key = 'top_up:stripe:pi_off'`;
        expect(entry?.metadata).toMatchObject({
          provider: "stripe",
          mode: "live",
        });
      });

      it("verifies a test event with the test secret and credits it while in test mode", async () => {
        const organizationId = await createOrganization();
        await setPayments({ mode: "test" });
        const payload = event(
          "checkout.session.completed",
          paid(organizationId, "pi_test_mode"),
          false,
        );

        // a test event signed with the live secret is refused
        await expect(
          deliver(payload, sign(payload, WEBHOOK_SECRET)),
        ).rejects.toMatchObject({ status: 400 });
        expect(await balance(organizationId)).toBe(0);

        const response = await deliver(
          payload,
          sign(payload, TEST_WEBHOOK_SECRET),
        );
        expect(response.status).toBe(200);
        expect(await balance(organizationId)).toBe(10 * SAR);
      });

      it("refuses a live event signed with the test secret", async () => {
        const organizationId = await createOrganization();
        const payload = event(
          "checkout.session.completed",
          paid(organizationId, "pi_live_wrong_secret"),
        );
        await expect(
          deliver(payload, sign(payload, TEST_WEBHOOK_SECRET)),
        ).rejects.toMatchObject({ status: 400 });
        expect(await balance(organizationId)).toBe(0);
      });

      it("refuses a test event when no test webhook secret is set", async () => {
        setEnv({ ...BOTH_MODES, STRIPE_TEST_WEBHOOK_SECRET: undefined });
        const organizationId = await createOrganization();
        const payload = event(
          "checkout.session.completed",
          paid(organizationId, "pi_test_unset"),
          false,
        );
        await expect(
          deliver(payload, sign(payload, TEST_WEBHOOK_SECRET)),
        ).rejects.toMatchObject({ status: 400 });
        expect(await balance(organizationId)).toBe(0);
      });

      it("refuses an event that does not say its mode", async () => {
        const payload = JSON.stringify({
          id: "evt_no_mode",
          object: "event",
          type: "checkout.session.completed",
          data: { object: {} },
        });
        await expect(deliver(payload)).rejects.toMatchObject({ status: 400 });
      });

      it("acknowledges a test payment without crediting it once the provider is live", async () => {
        const organizationId = await createOrganization();
        const payload = event(
          "checkout.session.completed",
          paid(organizationId, "pi_test_while_live"),
          false,
        );

        const response = await deliver(
          payload,
          sign(payload, TEST_WEBHOOK_SECRET),
        );
        expect(response.status).toBe(200);
        expect(await balance(organizationId)).toBe(0);
      });
    });
  });

  describe("dashboardRefundCredit", () => {
    it.each([
      {
        refunded: 10_800_000,
        already: 0,
        expected: 10 * SAR,
        label: "full refund: the credit, not the fee",
      },
      {
        refunded: 5_400_000,
        already: 0,
        expected: 5_400_000,
        label: "part: the money refunded",
      },
      {
        refunded: 800_000,
        already: 10 * SAR,
        expected: 0,
        label: "the fee after the credit: nothing",
      },
      {
        refunded: 3 * SAR,
        already: 8 * SAR,
        expected: 2 * SAR,
        label: "up to what earlier refunds left",
      },
      {
        refunded: 20 * SAR,
        already: 0,
        expected: 10 * SAR,
        label: "more than collected",
      },
    ])("$label", async ({ refunded, already, expected }) => {
      const { dashboardRefundCredit } = await import("../refund");
      expect(
        dashboardRefundCredit({
          grantedMicroSar: 10 * SAR,
          alreadyRefundedMicroSar: already,
          refundedMicroSar: refunded,
        }),
      ).toBe(expected);
    });
  });
});
