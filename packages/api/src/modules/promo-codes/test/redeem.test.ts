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

import type { SavePromoCodeInput } from "../schema";
import type { TestDatabase } from "@workspace/db/testing";
import type postgres from "postgres";

// Needs real Postgres: the row lock on the code, the unique index on the
// redemptions and the ledger's idempotency keys are what hold the limits under
// concurrent requests and webhook retries. Skips without DATABASE_URL.
const adminUrl = process.env.DATABASE_URL;

const WEBHOOK_SECRET = "whsec_live_secret";
const SAR = 1_000_000;

vi.mock(
  "../../../../../billing/web/src/providers/stripe/env",
  async (importOriginal) => ({
    ...(await importOriginal<object>()),
    env: {
      STRIPE_SECRET_KEY: "sk_live_not_used",
      STRIPE_WEBHOOK_SECRET: WEBHOOK_SECRET,
    },
  }),
);

/** What would be sent to Stripe, instead of sending it. */
const stripeCalls = vi.hoisted(() => ({
  sessions: [] as Record<string, unknown>[],
}));
vi.mock("../../../../../billing/web/src/providers/stripe/sdk", () => ({
  stripe: () => ({
    customers: {
      create: async () => ({ id: "cus_1" }),
      retrieve: async (id: string) => ({ id }),
    },
    checkout: {
      sessions: {
        create: async (params: Record<string, unknown>) => {
          stripeCalls.sessions.push(params);
          return { id: "cs_1", url: "https://checkout.stripe.test/1" };
        },
      },
    },
  }),
}));

const sign = (payload: string) => {
  const timestamp = Math.floor(Date.now() / 1000);
  const signature = createHmac("sha256", WEBHOOK_SECRET)
    .update(`${timestamp}.${payload}`)
    .digest("hex");
  return `t=${timestamp},v1=${signature}`;
};

let eventSeq = 0;
const event = (type: string, object: Record<string, unknown>) =>
  JSON.stringify({
    id: `evt_promo_${++eventSeq}`,
    object: "event",
    type,
    api_version: "2026-11-18.endive",
    created: Math.floor(Date.now() / 1000),
    livemode: true,
    data: { object },
  });

/** A paid session with the metadata the checkout wrote. */
const paidSession = (
  paymentIntent: string,
  metadata: Record<string, string>,
) => ({
  id: `cs_${paymentIntent}`,
  object: "checkout.session",
  mode: "payment",
  customer: "cus_1",
  currency: "sar",
  payment_intent: paymentIntent,
  payment_status: "paid",
  amount_total:
    (Number(metadata.creditMicroSar) + Number(metadata.feeMicroSar)) / 10_000,
  metadata,
});

const base = (overrides: Partial<SavePromoCodeInput>): SavePromoCodeInput => ({
  code: "UNSET",
  kind: "fee_discount",
  feeDiscountBps: null,
  feeDiscountMicroSar: null,
  bonusBps: null,
  bonusCapMicroSar: null,
  creditMicroSar: null,
  maxUses: null,
  onePerOrganization: true,
  startsAt: null,
  endsAt: null,
  creditExpiresAt: null,
  minTopupMicroSar: null,
  allOrganizations: true,
  organizationIds: [],
  note: "",
  ...overrides,
});

describe.skipIf(!adminUrl)("using promo codes (BILL-08)", () => {
  let database: TestDatabase;
  let sql: postgres.Sql;
  let codes: typeof import("../codes");
  let redeem: typeof import("../redeem");
  let topUp: typeof import("../../credits/top-up");
  let webhookHandler: typeof import("@workspace/billing-web/server").webhookHandler;
  let seq = 0;

  const createOrganization = async () => {
    const id = `org_promo_${++seq}`;
    await sql`insert into organization (id, name, slug, created_at) values (${id}, ${id}, ${id}, now())`;
    return id;
  };
  const createCode = async (overrides: Partial<SavePromoCodeInput>) =>
    (
      await codes.createPromoCode({
        ...base({ code: `PROMO-${++seq}`, ...overrides }),
        staffId: "staff_1",
      })
    ).code;
  const feeDiscount = (overrides: Partial<SavePromoCodeInput> = {}) =>
    createCode({ kind: "fee_discount", feeDiscountBps: 5000, ...overrides });
  const bonus = (overrides: Partial<SavePromoCodeInput> = {}) =>
    createCode({
      kind: "topup_bonus",
      bonusBps: 1000,
      bonusCapMicroSar: 50 * SAR,
      ...overrides,
    });
  const freeCredit = (overrides: Partial<SavePromoCodeInput> = {}) =>
    createCode({ kind: "free_credit", creditMicroSar: 25 * SAR, ...overrides });

  const deliver = async (payload: string) =>
    webhookHandler(
      new Request("http://localhost/api/billing/webhook/stripe", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "Stripe-Signature": sign(payload),
        },
        body: payload,
      }),
      topUp.topUpWebhookCallbacks,
    );

  /** Starts a checkout with the code, then returns the metadata Stripe would send back. */
  const checkout = async (
    organizationId: string,
    creditMicroSar: number,
    code?: string,
  ) => {
    await topUp.startTopUp({
      user: { id: "user_1", email: "owner@example.com" },
      organizationId,
      creditMicroSar,
      promoCode: code,
      origin: "https://app.example.com",
    });
    const params = stripeCalls.sessions.at(-1);
    return params?.metadata as Record<string, string>;
  };

  const entries = async (organizationId: string) =>
    sql<
      {
        kind: string;
        amount: string;
        promo_code_id: string | null;
        topup_fee_source: string | null;
        idempotency_key: string;
        metadata: Record<string, unknown> | null;
      }[]
    >`
      select kind, amount, promo_code_id, topup_fee_source, idempotency_key, metadata
      from credit_ledger_entry where organization_id = ${organizationId} order by created_at, kind desc`;
  const balance = async (organizationId: string) => {
    const [row] = await sql<
      { balance: string }[]
    >`select balance from credit_account where organization_id = ${organizationId}`;
    return Number(row?.balance ?? 0);
  };
  const redemptions = async (codeId: string) =>
    sql<
      {
        organization_id: string;
        fee_discount_micro_sar: string;
        credit_micro_sar: string;
        payment_id: string | null;
      }[]
    >`
      select organization_id, fee_discount_micro_sar, credit_micro_sar, payment_id
      from promo_code_redemption where promo_code_id = ${codeId} order by created_at`;
  const uses = async (codeId: string) => {
    const [row] = await sql<
      { uses: number }[]
    >`select uses from promo_code where id = ${codeId}`;
    return row?.uses;
  };

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;
    sql = (await import("postgres")).default(database.url, { max: 4 });
    ({ webhookHandler } = await import("@workspace/billing-web/server"));
    codes = await import("../codes");
    redeem = await import("../redeem");
    topUp = await import("../../credits/top-up");
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  beforeEach(() => {
    stripeCalls.sessions.length = 0;
  });

  describe("the quote", () => {
    it("takes a percentage off the fee and shows base fee, discount and total", async () => {
      const organizationId = await createOrganization();
      const code = await feeDiscount();
      // the default fee: 5.5% of SAR 100 = SAR 5.50; half of it off
      expect(
        await topUp.quoteTopUp(
          organizationId,
          100 * SAR,
          code.code.toLowerCase(),
        ),
      ).toEqual({
        creditMicroSar: 100 * SAR,
        baseFeeMicroSar: 5_500_000,
        feeDiscountMicroSar: 2_750_000,
        feeMicroSar: 2_750_000,
        totalMicroSar: 102_750_000,
        bonusMicroSar: 0,
        creditReceivedMicroSar: 100 * SAR,
        promo: {
          status: "applied",
          codeId: code.id,
          code: code.code,
          kind: "fee_discount",
          feeDiscountMicroSar: 2_750_000,
          bonusMicroSar: 0,
        },
      });
    });

    it("applies on top of the organization's own fee, and a fixed discount stops at zero", async () => {
      const organizationId = await createOrganization();
      const { setOrganizationTopupFee } =
        await import("../../pricing/organization-fee");
      await setOrganizationTopupFee({
        organizationId,
        staffId: "staff_1",
        feeBps: 200,
        feeMinMicroSar: null,
        expiresAt: null,
        note: "",
      });
      const code = await feeDiscount({
        feeDiscountBps: null,
        feeDiscountMicroSar: 10 * SAR,
      });
      // the organization pays 2% of SAR 400 = SAR 8; SAR 10 off takes all of it
      expect(
        await topUp.quoteTopUp(organizationId, 400 * SAR, code.code),
      ).toMatchObject({
        baseFeeMicroSar: 8 * SAR,
        feeDiscountMicroSar: 8 * SAR,
        feeMicroSar: 0,
        totalMicroSar: 400 * SAR,
      });
    });

    it("adds a bonus of a percentage of the credit, up to its cap", async () => {
      const organizationId = await createOrganization();
      const code = await bonus();
      expect(
        await topUp.quoteTopUp(organizationId, 200 * SAR, code.code),
      ).toMatchObject({
        feeMicroSar: 11 * SAR,
        bonusMicroSar: 20 * SAR,
        creditReceivedMicroSar: 220 * SAR,
        totalMicroSar: 211 * SAR,
      });
      // 10% of SAR 1,000 is SAR 100: the cap is SAR 50
      expect(
        await topUp.quoteTopUp(organizationId, 1000 * SAR, code.code),
      ).toMatchObject({
        bonusMicroSar: 50 * SAR,
        creditReceivedMicroSar: 1050 * SAR,
      });
    });

    describe("refuses with a clear reason, and checkout refuses the same", () => {
      const refused = async (
        organizationId: string,
        code: string,
        reason: string,
        creditMicroSar = 100 * SAR,
      ) => {
        const quote = await topUp.quoteTopUp(
          organizationId,
          creditMicroSar,
          code,
        );
        expect(quote.promo).toMatchObject({ status: "refused", reason });
        // the price without the code
        expect(quote).toMatchObject({
          feeDiscountMicroSar: 0,
          bonusMicroSar: 0,
          feeMicroSar: quote.baseFeeMicroSar,
        });
        await expect(
          topUp.startTopUp({
            user: { id: "user_1", email: "owner@example.com" },
            organizationId,
            creditMicroSar,
            promoCode: code,
            origin: "https://app.example.com",
          }),
        ).rejects.toMatchObject({
          status: 400,
          code: `billing:promo.refused.${reason}`,
        });
        expect(stripeCalls.sessions).toHaveLength(0);
      };

      it("unknown", async () => {
        expect.hasAssertions();
        const organizationId = await createOrganization();
        await refused(organizationId, "NO-SUCH-CODE", "unknown");
        await refused(organizationId, "not a code!", "unknown");
      });

      it("paused", async () => {
        expect.hasAssertions();
        const code = await feeDiscount();
        await codes.setPromoCodePaused({
          id: code.id,
          isPaused: true,
          staffId: "staff_1",
        });
        await refused(await createOrganization(), code.code, "paused");
      });

      it("not started", async () => {
        expect.hasAssertions();
        const code = await feeDiscount({
          startsAt: new Date(Date.now() + 60_000),
        });
        await refused(await createOrganization(), code.code, "not_started");
      });

      it("expired", async () => {
        expect.hasAssertions();
        const code = await feeDiscount({
          startsAt: new Date(Date.now() - 120_000),
          endsAt: new Date(Date.now() - 60_000),
        });
        await refused(await createOrganization(), code.code, "expired");
      });

      it("used up", async () => {
        expect.hasAssertions();
        const code = await freeCredit({ maxUses: 1 });
        await redeem.redeemFreeCredit({
          organizationId: await createOrganization(),
          code: code.code,
          userId: "u",
        });
        await expect(
          redeem.redeemFreeCredit({
            organizationId: await createOrganization(),
            code: code.code,
            userId: "u",
          }),
        ).rejects.toMatchObject({
          status: 400,
          code: "billing:promo.refused.used_up",
        });
        const topUpCode = await feeDiscount({ maxUses: 1 });
        await sql`update promo_code set uses = 1 where id = ${topUpCode.id}`;
        await refused(await createOrganization(), topUpCode.code, "used_up");
      });

      it("already used by this organization", async () => {
        expect.hasAssertions();
        const organizationId = await createOrganization();
        const code = await bonus();
        const metadata = await checkout(organizationId, 100 * SAR, code.code);
        await deliver(
          event("checkout.session.completed", paidSession("pi_once", metadata)),
        );
        stripeCalls.sessions.length = 0;
        await refused(organizationId, code.code, "already_used");
      });

      it("not for this organization", async () => {
        expect.hasAssertions();
        const listed = await createOrganization();
        const code = await feeDiscount({
          allOrganizations: false,
          organizationIds: [listed],
        });
        await refused(
          await createOrganization(),
          code.code,
          "not_for_organization",
        );
        expect(
          (await topUp.quoteTopUp(listed, 100 * SAR, code.code)).promo?.status,
        ).toBe("applied");
      });

      it("below the minimum top-up", async () => {
        expect.hasAssertions();
        const code = await bonus({ minTopupMicroSar: 200 * SAR });
        await refused(
          await createOrganization(),
          code.code,
          "below_minimum",
          199_990_000,
        );
      });

      it("wrong kind for a top-up", async () => {
        expect.hasAssertions();
        const code = await freeCredit();
        await refused(await createOrganization(), code.code, "wrong_kind");
        const topUpCode = await feeDiscount();
        await expect(
          redeem.redeemFreeCredit({
            organizationId: await createOrganization(),
            code: topUpCode.code,
            userId: "u",
          }),
        ).rejects.toMatchObject({ code: "billing:promo.refused.wrong_kind" });
      });

      it("no effect: nothing to take off a fee of zero", async () => {
        expect.hasAssertions();
        const organizationId = await createOrganization();
        const { setOrganizationTopupFee } =
          await import("../../pricing/organization-fee");
        await setOrganizationTopupFee({
          organizationId,
          staffId: "s",
          feeBps: 0,
          feeMinMicroSar: 0,
          expiresAt: null,
          note: "",
        });
        await refused(organizationId, (await feeDiscount()).code, "no_effect");
      });
    });
  });

  describe("checkout", () => {
    it("charges the discounted fee and carries the code, discount, bonus and fee source in the metadata", async () => {
      const organizationId = await createOrganization();
      const code = await feeDiscount();
      const metadata = await checkout(organizationId, 100 * SAR, code.code);
      expect(metadata).toMatchObject({
        creditMicroSar: "100000000",
        feeMicroSar: "2750000",
        feeBps: "550",
        feeSource: "promo",
        promoCodeId: code.id,
        promoFeeDiscountMicroSar: "2750000",
        promoBonusMicroSar: "0",
      });
      const lines = stripeCalls.sessions[0]?.line_items as {
        price_data: { unit_amount: number };
      }[];
      expect(lines.map((line) => line.price_data.unit_amount)).toEqual([
        10_000, 275,
      ]);
    });

    it("keeps the fee source of a bonus code: its fee is not discounted", async () => {
      const metadata = await checkout(
        await createOrganization(),
        100 * SAR,
        (await bonus()).code,
      );
      expect(metadata).toMatchObject({
        feeSource: "default",
        promoBonusMicroSar: "10000000",
        promoFeeDiscountMicroSar: "0",
      });
    });

    it("carries no code when none was given", async () => {
      const metadata = await checkout(await createOrganization(), 100 * SAR);
      expect(metadata).not.toHaveProperty("promoCodeId");
    });
  });

  describe("the payment webhook", () => {
    it("records the fee source promo, the code and the discount on the top-up, and the use", async () => {
      const organizationId = await createOrganization();
      const code = await feeDiscount();
      const metadata = await checkout(organizationId, 100 * SAR, code.code);
      await deliver(
        event("checkout.session.completed", paidSession("pi_fee", metadata)),
      );

      expect(await entries(organizationId)).toEqual([
        expect.objectContaining({
          kind: "top_up",
          amount: String(100 * SAR),
          promo_code_id: code.id,
          topup_fee_source: "promo",
          metadata: expect.objectContaining({
            feeMicroSar: 2_750_000,
            promoFeeDiscountMicroSar: 2_750_000,
            promoBonusMicroSar: 0,
          }),
        }),
      ]);
      expect(await redemptions(code.id)).toEqual([
        {
          organization_id: organizationId,
          fee_discount_micro_sar: "2750000",
          credit_micro_sar: "0",
          payment_id: "stripe:pi_fee",
        },
      ]);
      expect(await uses(code.id)).toBe(1);
    });

    it("adds the bonus as its own promo entry, tied to the code and the payment", async () => {
      const organizationId = await createOrganization();
      const code = await bonus();
      const metadata = await checkout(organizationId, 200 * SAR, code.code);
      await deliver(
        event("checkout.session.completed", paidSession("pi_bonus", metadata)),
      );

      const rows = await entries(organizationId);
      expect(rows).toHaveLength(2);
      expect(rows.find((row) => row.kind === "top_up")).toMatchObject({
        amount: String(200 * SAR),
        promo_code_id: code.id,
        topup_fee_source: "default",
      });
      expect(rows.find((row) => row.kind === "promo")).toMatchObject({
        amount: String(20 * SAR),
        promo_code_id: code.id,
        idempotency_key: "promo:stripe:pi_bonus",
        metadata: expect.objectContaining({ paymentId: "stripe:pi_bonus" }),
      });
      expect(await balance(organizationId)).toBe(220 * SAR);
    });

    it("grants once, however many times and ways the payment is delivered, even at once", async () => {
      const organizationId = await createOrganization();
      // a repeatable code, so only the payment's own idempotency stops a second grant
      const code = await bonus({ maxUses: 5, onePerOrganization: false });
      const metadata = await checkout(organizationId, 100 * SAR, code.code);
      const paid = paidSession("pi_retry", metadata);
      const responses = [
        await deliver(event("checkout.session.completed", paid)),
        await deliver(event("checkout.session.completed", paid)),
        ...(await Promise.all([
          deliver(event("checkout.session.async_payment_succeeded", paid)),
          deliver(event("checkout.session.completed", paid)),
          deliver(event("checkout.session.completed", paid)),
        ])),
      ];

      // every delivery is acknowledged, so Stripe stops retrying
      expect(responses.map((response) => response.status)).toEqual([
        200, 200, 200, 200, 200,
      ]);
      expect(await balance(organizationId)).toBe(110 * SAR);
      expect(
        (await entries(organizationId)).map((row) => row.kind).sort(),
      ).toEqual(["promo", "top_up"]);
      expect(await redemptions(code.id)).toHaveLength(1);
      expect(await uses(code.id)).toBe(1);
    });

    it("honours a code that ran out or was paused after the checkout, and counts the use", async () => {
      const first = await createOrganization();
      const second = await createOrganization();
      const code = await bonus({ maxUses: 1 });
      // both checkouts open while one use is left
      const one = await checkout(first, 100 * SAR, code.code);
      const two = await checkout(second, 100 * SAR, code.code);
      await deliver(
        event("checkout.session.completed", paidSession("pi_first", one)),
      );
      await codes.setPromoCodePaused({
        id: code.id,
        isPaused: true,
        staffId: "staff_1",
      });
      await deliver(
        event("checkout.session.completed", paidSession("pi_second", two)),
      );

      expect(await balance(first)).toBe(110 * SAR);
      expect(await balance(second)).toBe(110 * SAR);
      expect(await uses(code.id)).toBe(2);
      // and no new checkout gets it
      expect(
        (
          await topUp.quoteTopUp(
            await createOrganization(),
            100 * SAR,
            code.code,
          )
        ).promo,
      ).toMatchObject({
        status: "refused",
      });
    });

    it("gives one organization a one-time bonus once, even across two checkouts paid one after the other", async () => {
      const organizationId = await createOrganization();
      const code = await bonus();
      const one = await checkout(organizationId, 100 * SAR, code.code);
      const two = await checkout(organizationId, 100 * SAR, code.code);
      await Promise.all([
        deliver(
          event("checkout.session.completed", paidSession("pi_twice_1", one)),
        ),
        deliver(
          event("checkout.session.completed", paidSession("pi_twice_2", two)),
        ),
      ]);

      // both payments are credited; the bonus once
      expect(await balance(organizationId)).toBe(210 * SAR);
      expect(await redemptions(code.id)).toHaveLength(1);
      expect(await uses(code.id)).toBe(1);
    });

    it("credits a payment naming a code that does not exist, without it", async () => {
      const organizationId = await createOrganization();
      const metadata = {
        purpose: "credits_top_up",
        organizationId,
        creditMicroSar: String(100 * SAR),
        feeMicroSar: String(5_500_000),
        promoCodeId: "no_such_code",
        promoFeeDiscountMicroSar: "0",
        promoBonusMicroSar: String(10 * SAR),
      };
      await deliver(
        event("checkout.session.completed", paidSession("pi_ghost", metadata)),
      );
      expect(await entries(organizationId)).toEqual([
        expect.objectContaining({ kind: "top_up", promo_code_id: null }),
      ]);
      expect(await balance(organizationId)).toBe(100 * SAR);
    });
  });

  describe("free credit", () => {
    it("grants a promo entry tied to the code at once", async () => {
      const organizationId = await createOrganization();
      const code = await freeCredit();
      const result = await redeem.redeemFreeCredit({
        organizationId,
        code: ` ${code.code.toLowerCase()} `,
        userId: "user_9",
      });
      expect(result).toEqual({
        code: code.code,
        creditMicroSar: 25 * SAR,
        balance: 25 * SAR,
      });
      expect(await entries(organizationId)).toEqual([
        expect.objectContaining({
          kind: "promo",
          amount: String(25 * SAR),
          promo_code_id: code.id,
        }),
      ]);
      expect(await redemptions(code.id)).toEqual([
        {
          organization_id: organizationId,
          fee_discount_micro_sar: "0",
          credit_micro_sar: String(25 * SAR),
          payment_id: null,
        },
      ]);
      expect(await uses(code.id)).toBe(1);
    });

    it("lets one organization redeem a one-time code once, of many tries at the same moment", async () => {
      const organizationId = await createOrganization();
      const code = await freeCredit();
      const results = await Promise.allSettled(
        Array.from({ length: 8 }, () =>
          redeem.redeemFreeCredit({
            organizationId,
            code: code.code,
            userId: "u",
          }),
        ),
      );
      expect(
        results.filter((result) => result.status === "fulfilled"),
      ).toHaveLength(1);
      for (const result of results.filter((r) => r.status === "rejected")) {
        expect(result.reason).toMatchObject({
          code: "billing:promo.refused.already_used",
        });
      }
      expect(await balance(organizationId)).toBe(25 * SAR);
      expect(await uses(code.id)).toBe(1);
    });

    it("holds the total uses when organizations redeem at the same moment", async () => {
      const code = await freeCredit({ maxUses: 3 });
      const organizations = await Promise.all(
        Array.from({ length: 10 }, () => createOrganization()),
      );
      const results = await Promise.allSettled(
        organizations.map((organizationId) =>
          redeem.redeemFreeCredit({
            organizationId,
            code: code.code,
            userId: "u",
          }),
        ),
      );
      expect(
        results.filter((result) => result.status === "fulfilled"),
      ).toHaveLength(3);
      for (const result of results.filter((r) => r.status === "rejected")) {
        expect(result.reason).toMatchObject({
          code: "billing:promo.refused.used_up",
        });
      }
      expect(await uses(code.id)).toBe(3);
      expect(await redemptions(code.id)).toHaveLength(3);
    });

    it("lets one organization redeem a repeatable code more than once, within the total", async () => {
      const organizationId = await createOrganization();
      const code = await freeCredit({ onePerOrganization: false, maxUses: 2 });
      await redeem.redeemFreeCredit({
        organizationId,
        code: code.code,
        userId: "u",
      });
      await redeem.redeemFreeCredit({
        organizationId,
        code: code.code,
        userId: "u",
      });
      await expect(
        redeem.redeemFreeCredit({
          organizationId,
          code: code.code,
          userId: "u",
        }),
      ).rejects.toMatchObject({
        code: "billing:promo.refused.used_up",
      });
      expect(await balance(organizationId)).toBe(50 * SAR);
    });

    it("is refused by the database without a code, or a second time for a one-time code", async () => {
      const organizationId = await createOrganization();
      await expect(
        sql`insert into credit_ledger_entry (id, organization_id, amount, kind, idempotency_key) values ('e_nocode', ${organizationId}, 1, 'promo', 'k_nocode')`,
      ).rejects.toMatchObject({
        constraint_name: "credit_ledger_entry_promo_code",
      });
      const code = await freeCredit();
      await sql`insert into promo_code_redemption (id, promo_code_id, organization_id, kind, one_per_organization, idempotency_key)
        values ('r_1', ${code.id}, ${organizationId}, 'free_credit', true, 'k_r1')`;
      await expect(
        sql`insert into promo_code_redemption (id, promo_code_id, organization_id, kind, one_per_organization, idempotency_key)
          values ('r_2', ${code.id}, ${organizationId}, 'free_credit', true, 'k_r2')`,
      ).rejects.toMatchObject({
        constraint_name: "promo_code_redemption_once_per_organization",
      });
    });
  });

  describe("the report", () => {
    it("adds up to what the ledger holds for the code", async () => {
      const code = await bonus({ onePerOrganization: false });
      const discount = await feeDiscount({ onePerOrganization: false });
      const free = await freeCredit({ onePerOrganization: false });
      const first = await createOrganization();
      const second = await createOrganization();
      let payment = 0;
      for (const [organizationId, credit] of [
        [first, 100 * SAR],
        [second, 250 * SAR],
        [first, 1000 * SAR],
      ] as const) {
        for (const promo of [code, discount]) {
          const metadata = await checkout(organizationId, credit, promo.code);
          await deliver(
            event(
              "checkout.session.completed",
              paidSession(`pi_report_${++payment}`, metadata),
            ),
          );
        }
      }
      await redeem.redeemFreeCredit({
        organizationId: second,
        code: free.code,
        userId: "u",
      });

      // straight from the ledger, without the report's code
      const ledger = async (id: string) => {
        const [row] = await sql<{ granted: string; waived: string }[]>`
          select
            coalesce(sum(amount) filter (where kind = 'promo'), 0) as granted,
            coalesce(sum((metadata ->> 'promoFeeDiscountMicroSar')::bigint) filter (where kind = 'top_up'), 0) as waived
          from credit_ledger_entry where promo_code_id = ${id}`;
        return {
          creditGrantedMicroSar: Number(row?.granted),
          feeWaivedMicroSar: Number(row?.waived),
        };
      };

      const bonusReport = await codes.getPromoCodeReport(code.id);
      expect(bonusReport).toMatchObject({
        uses: 3,
        organizations: 2,
        ...(await ledger(code.id)),
      });
      // 10% of 100 and 250, and the SAR 50 cap on 1,000
      expect(bonusReport.creditGrantedMicroSar).toBe(
        10 * SAR + 25 * SAR + 50 * SAR,
      );
      expect(
        bonusReport.redemptions.reduce(
          (sum, row) => sum + row.creditMicroSar,
          0,
        ),
      ).toBe(bonusReport.creditGrantedMicroSar);

      const discountReport = await codes.getPromoCodeReport(discount.id);
      expect(discountReport).toMatchObject({
        uses: 3,
        organizations: 2,
        ...(await ledger(discount.id)),
      });
      // half of SAR 5.50, SAR 13.75 and SAR 55
      expect(discountReport.feeWaivedMicroSar).toBe(
        2_750_000 + 6_880_000 + 27_500_000,
      );
      expect(
        discountReport.redemptions.reduce(
          (sum, row) => sum + row.feeDiscountMicroSar,
          0,
        ),
      ).toBe(discountReport.feeWaivedMicroSar);

      const freeReport = await codes.getPromoCodeReport(free.id);
      expect(freeReport).toMatchObject({
        uses: 1,
        organizations: 1,
        ...(await ledger(free.id)),
      });
      expect(freeReport).toMatchObject({
        creditGrantedMicroSar: 25 * SAR,
        feeWaivedMicroSar: 0,
      });
    });
  });

  describe("the gateway's view", () => {
    it("counts promo credit in the balance the gateway reserves against", async () => {
      const organizationId = await createOrganization();
      await redeem.redeemFreeCredit({
        organizationId,
        code: (await freeCredit()).code,
        userId: "u",
      });
      // the credits plugin's reserve statement (apps/gateway/plugins/credits/store.go, reserveSQL),
      // run as the role the gateway connects as: it reads the balance, never the promo tables
      const reserved = await sql.begin(async (tx) => {
        await tx`set local role gateway_ledger`;
        return tx`
          update credit_account set held = held + ${20 * SAR}
          where organization_id = ${organizationId} and balance - held >= ${20 * SAR} returning balance, held`;
      });
      expect(reserved).toEqual([
        { balance: String(25 * SAR), held: String(20 * SAR) },
      ]);
      await expect(
        sql.begin(async (tx) => {
          await tx`set local role gateway_ledger`;
          return tx`select count(*) from promo_code`;
        }),
      ).rejects.toThrow(/permission denied/);
      const { rebuildBalance } = await import("../../credits/ledger");
      expect((await rebuildBalance(organizationId)).balance).toBe(25 * SAR);
    });
  });
});
