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
const USD = 1_000_000;

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
  creditMicroUsd: number;
  feeMicroUsd: number;
  amountTotalCents?: number;
  paymentStatus?: string;
}) => ({
  id: `cs_${input.paymentIntent}`,
  object: "checkout.session",
  mode: "payment",
  customer: "cus_1",
  currency: "usd",
  payment_intent: input.paymentIntent,
  payment_status: input.paymentStatus ?? "paid",
  amount_total:
    input.amountTotalCents ??
    (input.creditMicroUsd + input.feeMicroUsd) / 10_000,
  metadata: {
    purpose: "credits_top_up",
    organizationId: input.organizationId,
    creditMicroUsd: String(input.creditMicroUsd),
    feeMicroUsd: String(input.feeMicroUsd),
  },
});

const refund = (input: {
  id: string;
  paymentIntent: string;
  amountCents: number;
  status?: string;
}) => ({
  id: input.id,
  object: "refund",
  amount: input.amountCents,
  currency: "usd",
  payment_intent: input.paymentIntent,
  status: input.status ?? "succeeded",
});

/** Sets the top-up limits as staff would in the admin pricing settings. */
const setLimits = async (
  topupMinMicroUsd: number,
  topupMaxMicroUsd: number,
  topupPresetsMicroUsd: number[],
) => {
  const { updatePricingSettings } = await import("../../pricing/pricing");
  await updatePricingSettings({
    defaultMarkupBps: 0,
    topupFeeBps: 550,
    topupFeeMinMicroUsd: 800_000,
    topupMinMicroUsd,
    topupMaxMicroUsd,
    topupPresetsMicroUsd,
    staffId: "staff_limits",
  });
};
/** Back to the limits the migration seeds. */
const seeded = () =>
  setLimits(5 * USD, 1000 * USD, [10 * USD, 25 * USD, 50 * USD, 100 * USD]);

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
      // migration defaults: 5.5% with a $0.80 minimum
      expect(await topUp.quoteTopUp(10 * USD)).toEqual({
        creditMicroUsd: 10 * USD,
        feeMicroUsd: 800_000,
        totalMicroUsd: 10_800_000,
      });
      expect((await topUp.quoteTopUp(100 * USD)).feeMicroUsd).toBe(5_500_000);
    });

    it("asks Stripe for a payment with a credits line, a fee line and the metadata the webhook reads", async () => {
      const organizationId = await createOrganization();

      const result = await topUp.startTopUp({
        user: { id: "user_1", email: "owner@example.com" },
        organizationId,
        creditMicroUsd: 25 * USD,
        origin: "https://app.example.com",
      });

      expect(result).toMatchObject({
        url: "https://checkout.stripe.test/1",
        creditMicroUsd: 25 * USD,
        feeMicroUsd: 1_380_000, // 5.5% of $25 = $1.375, half up to $1.38
        totalMicroUsd: 26_380_000,
      });
      expect(stripeCalls.sessions).toHaveLength(1);
      const params = stripeCalls.sessions[0];
      const metadata = {
        purpose: "credits_top_up",
        provider: "stripe",
        mode: "live",
        organizationId,
        creditMicroUsd: "25000000",
        feeMicroUsd: "1380000",
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
            currency: "usd",
            unit_amount: 2500,
            product_data: { name: "Credits" },
          },
        },
        {
          quantity: 1,
          price_data: {
            currency: "usd",
            unit_amount: 138,
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
        expect(await topUp.getTopUpOptions()).toEqual({
          isAvailable: true,
          minMicroUsd: 5 * USD,
          maxMicroUsd: 1000 * USD,
          presetsMicroUsd: [10 * USD, 25 * USD, 50 * USD, 100 * USD],
          feeBps: 550,
          feeMinMicroUsd: 800_000,
        });
      });

      it("refuses an amount under the stored minimum or over the stored maximum", async () => {
        const organizationId = await createOrganization();
        await expect(topUp.quoteTopUp(5 * USD - 10_000)).rejects.toMatchObject({
          status: 400,
          code: "billing:error.topUpAmount",
        });
        await expect(
          topUp.quoteTopUp(1000 * USD + 10_000),
        ).rejects.toMatchObject({
          status: 400,
          code: "billing:error.topUpAmount",
        });
        await expect(
          topUp.startTopUp({
            user: { id: "user_1", email: "owner@example.com" },
            organizationId,
            creditMicroUsd: 1500 * USD,
            origin: "https://app.example.com",
          }),
        ).rejects.toMatchObject({ status: 400 });
        expect(stripeCalls.sessions).toHaveLength(0);
        // the limits themselves are allowed
        expect((await topUp.quoteTopUp(5 * USD)).creditMicroUsd).toBe(5 * USD);
        expect((await topUp.quoteTopUp(1000 * USD)).creditMicroUsd).toBe(
          1000 * USD,
        );
      });

      it("follows the limits as soon as staff change them", async () => {
        const organizationId = await createOrganization();
        try {
          await setLimits(20 * USD, 2000 * USD, [20 * USD, 1500 * USD]);

          expect(await topUp.getTopUpOptions()).toMatchObject({
            minMicroUsd: 20 * USD,
            maxMicroUsd: 2000 * USD,
            presetsMicroUsd: [20 * USD, 1500 * USD],
          });
          const result = await topUp.startTopUp({
            user: { id: "user_1", email: "owner@example.com" },
            organizationId,
            creditMicroUsd: 1500 * USD,
            origin: "https://app.example.com",
          });
          expect(result).toMatchObject({ creditMicroUsd: 1500 * USD });
          expect(stripeCalls.sessions).toHaveLength(1);
          // $10 was allowed before the minimum went up to $20
          await expect(topUp.quoteTopUp(10 * USD)).rejects.toMatchObject({
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
          creditMicroUsd: 10 * USD,
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
        creditMicroUsd: 10 * USD,
        feeMicroUsd: 800_000,
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
          amount: String(10 * USD),
          idempotency_key: "top_up:stripe:pi_once",
        },
      ]);
      expect(await balance(organizationId)).toBe(10 * USD);
    });

    it("credits a delayed payment only when the money arrives", async () => {
      const organizationId = await createOrganization();
      const input = {
        organizationId,
        paymentIntent: "pi_delayed",
        creditMicroUsd: 50 * USD,
        feeMicroUsd: 2_750_000,
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
      expect(await balance(organizationId)).toBe(50 * USD);
    });

    it("rejects an event whose signature does not match and writes nothing", async () => {
      const organizationId = await createOrganization();
      const payload = event(
        "checkout.session.completed",
        session({
          organizationId,
          paymentIntent: "pi_forged",
          creditMicroUsd: 1_000 * USD,
          feeMicroUsd: 0,
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
            creditMicroUsd: 10 * USD,
            feeMicroUsd: 800_000,
            amountTotalCents: 500,
          }),
        ),
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
            creditMicroUsd: 10 * USD,
            feeMicroUsd: 800_000,
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
            creditMicroUsd: 10 * USD,
            feeMicroUsd: 800_000,
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
        amountCents: 1080,
      };

      await deliver(
        event("refund.created", refund({ ...full, status: "pending" })),
      );
      expect(await balance(organizationId)).toBe(10 * USD);

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
          amount: String(-10 * USD),
          idempotency_key: "refund:stripe:re_full",
        },
      ]);
    });

    it("removes the refunded share of the payment on a partial refund", async () => {
      const organizationId = await paidTopUp("pi_partial");

      // $5.40 of $10.80 paid is half: half of the $10 credit
      await deliver(
        event(
          "refund.created",
          refund({
            id: "re_half",
            paymentIntent: "pi_partial",
            amountCents: 540,
          }),
        ),
      );
      expect(await balance(organizationId)).toBe(5 * USD);
    });

    it("ignores a refund of a payment that was not a top-up", async () => {
      const response = await deliver(
        event(
          "refund.created",
          refund({
            id: "re_plan",
            paymentIntent: "pi_plan",
            amountCents: 1900,
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

  describe("payment gateway chosen by staff (ADMIN-03)", () => {
    const start = (organizationId: string) =>
      topUp.startTopUp({
        user: { id: "user_1", email: "owner@example.com" },
        organizationId,
        creditMicroUsd: 10 * USD,
        origin: "https://app.example.com",
      });
    const unavailable = {
      status: 503,
      code: "billing:error.topUpsUnavailable",
    };

    it("refuses new top-ups while they are switched off, and takes them again when on", async () => {
      const organizationId = await createOrganization();
      await setPayments({ topupsEnabled: false });

      expect((await topUp.getTopUpOptions()).isAvailable).toBe(false);
      await expect(topUp.quoteTopUp(10 * USD)).rejects.toMatchObject(
        unavailable,
      );
      await expect(start(organizationId)).rejects.toMatchObject(unavailable);
      expect(stripeCalls.sessions).toHaveLength(0);

      await setPayments({ topupsEnabled: true });
      expect((await topUp.getTopUpOptions()).isAvailable).toBe(true);
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
      expect((await topUp.getTopUpOptions()).isAvailable).toBe(false);
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
          creditMicroUsd: 10 * USD,
          feeMicroUsd: 800_000,
        });

      it("credits a live payment even while top-ups are off: the money was taken", async () => {
        const organizationId = await createOrganization();
        await setPayments({ topupsEnabled: false });

        const response = await deliver(
          event("checkout.session.completed", paid(organizationId, "pi_off")),
        );
        expect(response.status).toBe(200);
        expect(await balance(organizationId)).toBe(10 * USD);
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
        expect(await balance(organizationId)).toBe(10 * USD);
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

  describe("refundedCredit", () => {
    it.each([
      { refunded: 10_800_000, expected: 10 * USD, label: "full refund" },
      { refunded: 5_400_000, expected: 5 * USD, label: "half" },
      { refunded: 1, expected: 0, label: "less than a credit micro-USD" },
      { refunded: 20 * USD, expected: 10 * USD, label: "more than collected" },
    ])("$label", ({ refunded, expected }) => {
      expect(
        topUp.refundedCredit({
          creditMicroUsd: 10 * USD,
          collectedMicroUsd: 10_800_000,
          refundedMicroUsd: refunded,
        }),
      ).toBe(expected);
    });
  });
});
