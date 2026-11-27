import { createHmac, randomUUID } from "node:crypto";
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

// Self-serve refunds and refunds made in Stripe's dashboard (BILL-10, ADR
// 0019) against real Postgres, with Stripe replaced by a fake that keeps its
// idempotency keys. Skips without DATABASE_URL.
const adminUrl = process.env.DATABASE_URL;

const SAR = 1_000_000;
const WEBHOOK_SECRET = "whsec_live_secret";

vi.mock(
  "../../../../../billing/web/src/providers/stripe/env",
  async (importOriginal) => ({
    ...(await importOriginal<object>()),
    env: {
      STRIPE_SECRET_KEY: "sk_live_not_used",
      STRIPE_WEBHOOK_SECRET: WEBHOOK_SECRET,
      STRIPE_TEST_SECRET_KEY: "sk_test_not_used",
      STRIPE_TEST_WEBHOOK_SECRET: "whsec_test_secret",
    },
  }),
);

interface FakeRefund {
  id: string;
  object: "refund";
  amount: number;
  currency: "sar";
  payment_intent: string;
  status: string;
  metadata: Record<string, string>;
}

/**
 * Stripe's refunds API as the code uses it. The same idempotency key returns
 * the same refund, as Stripe does; `next` says how the next new refund ends.
 */
const stripeFake = vi.hoisted(() => ({
  next: "succeeded" as "succeeded" | "pending" | "refused" | "unreachable",
  byKey: new Map<string, FakeRefund>(),
  creates: [] as { params: Record<string, unknown>; key: string }[],
  charges: new Map<string, FakeRefund[]>(),
  /** Runs inside create, before it returns: a webhook that overtakes our own write. */
  beforeReturn: null as null | ((refund: FakeRefund) => Promise<void>),
}));

vi.mock("../../../../../billing/web/src/providers/stripe/sdk", () => ({
  stripe: () => ({
    refunds: {
      create: async (
        params: {
          payment_intent: string;
          amount: number;
          metadata: Record<string, string>;
        },
        options: { idempotencyKey: string },
      ) => {
        stripeFake.creates.push({ params, key: options.idempotencyKey });
        const seen = stripeFake.byKey.get(options.idempotencyKey);
        if (seen) {
          return seen;
        }
        if (stripeFake.next === "unreachable") {
          throw Object.assign(new Error("connect ECONNREFUSED"), {
            type: "StripeConnectionError",
          });
        }
        if (stripeFake.next === "refused") {
          throw Object.assign(new Error("Charge has already been refunded."), {
            type: "StripeInvalidRequestError",
          });
        }
        const refund: FakeRefund = {
          id: `re_${randomUUID()}`,
          object: "refund",
          amount: params.amount,
          currency: "sar",
          payment_intent: params.payment_intent,
          status: stripeFake.next,
          metadata: params.metadata,
        };
        stripeFake.byKey.set(options.idempotencyKey, refund);
        await stripeFake.beforeReturn?.(refund);
        return refund;
      },
      list: async ({ charge }: { charge: string }) => ({
        data: stripeFake.charges.get(charge) ?? [],
      }),
    },
  }),
}));

/** The organization roles' real permissions, behind a session the test picks by header. */
vi.mock("@workspace/auth/server", async () => {
  const { roles } = await import("@workspace/auth");
  const roleOf = (headers: Headers) =>
    (headers.get("x-test-role") ?? "member") as keyof typeof roles;
  return {
    auth: {
      api: {
        getSession: async ({ headers }: { headers: Headers }) => ({
          user: { id: `user_${roleOf(headers)}` },
        }),
        hasPermission: async ({
          body,
          headers,
        }: {
          body: { permissions: Record<string, string[]> };
          headers: Headers;
        }) => ({
          success: roles[roleOf(headers)].authorize(body.permissions).success,
        }),
      },
    },
    clientIp: () => null,
    recordStaffAction: async () => undefined,
  };
});
vi.mock("@workspace/i18n", async () => {
  const z = await import("zod");
  return { getValidationLocale: () => z.locales.en };
});
vi.mock("@workspace/i18n/server", () => ({ getLocaleFromRequest: () => "en" }));
// the credits router reaches the low-balance emails, whose templates the test runner does not compile
vi.mock("@workspace/email/server", () => ({
  sendEmail: async () => undefined,
}));

/** Stripe-Signature for a payload. */
const sign = (payload: string) => {
  const timestamp = Math.floor(Date.now() / 1000);
  const signature = createHmac("sha256", WEBHOOK_SECRET)
    .update(`${timestamp}.${payload}`)
    .digest("hex");
  return `t=${timestamp},v1=${signature}`;
};
const event = (type: string, object: unknown) =>
  JSON.stringify({
    id: `evt_${randomUUID()}`,
    object: "event",
    type,
    api_version: "2026-11-18.endive",
    created: Math.floor(Date.now() / 1000),
    livemode: true,
    data: { object },
  });

/** The gateway's own reserve and settle (apps/gateway/plugins/credits/store.go), one statement each. */
const RESERVE_SQL = `
WITH hold AS (
  UPDATE credit_account SET held = held + $3, updated_at = now()
   WHERE organization_id = $2 AND balance - held >= $3
  RETURNING organization_id
)
INSERT INTO credit_reservation (id, organization_id, amount, expires_at, virtual_key_id,
                                usd_to_sar_rate_e6, fx_spread_bps, effective_usd_to_sar_rate_e6)
SELECT $1, organization_id, $3, now() + interval '5 minutes', NULL, 3750000, 0, 3750000 FROM hold
RETURNING id`;
const SETTLE_SQL = `
WITH released AS (
  UPDATE credit_reservation SET settled_at = now()
   WHERE id = $1 AND organization_id = $2 AND settled_at IS NULL
  RETURNING amount
), charged AS (
  INSERT INTO credit_ledger_entry (id, organization_id, amount, kind, idempotency_key, reference, metadata, virtual_key_id,
                                   cost_micro_usd, usd_to_sar_rate_e6, fx_spread_bps, effective_usd_to_sar_rate_e6)
  SELECT $3, $2, -$4::bigint, 'usage', $5, $1, 'null'::jsonb,
         (SELECT virtual_key_id FROM credit_reservation WHERE id = $1), 1, 3750000, 0, 3750000
   WHERE $4::bigint > 0
  ON CONFLICT (idempotency_key) DO NOTHING
  RETURNING amount
), account AS (
  UPDATE credit_account
     SET balance = balance + coalesce((SELECT amount FROM charged), 0),
         held = held - coalesce((SELECT amount FROM released), 0),
         updated_at = now()
   WHERE organization_id = $2
     AND (EXISTS (SELECT 1 FROM charged) OR EXISTS (SELECT 1 FROM released))
  RETURNING 1
)
SELECT (SELECT count(*) FROM charged) > 0 AS charged`;

/** The credits router, as the web app calls it, for a member of the given role. */
const app = async () => {
  const { Hono } = await import("hono");
  const { HttpException } = await import("@workspace/shared/utils");
  const { creditsRouter } = await import("../router");
  return new Hono()
    .route("/api/credits", creditsRouter)
    .onError((error) =>
      Response.json(
        { code: error instanceof HttpException ? error.code : error.message },
        { status: error instanceof HttpException ? error.status : 500 },
      ),
    );
};

/** A refund staff made in Stripe's dashboard: no metadata of ours. */
const dashboardRefund = (paymentId: string, halalas: number): FakeRefund => ({
  id: `re_${randomUUID()}`,
  object: "refund" as const,
  amount: halalas,
  currency: "sar" as const,
  payment_intent: paymentId,
  status: "succeeded",
  metadata: {},
});

describe.skipIf(!adminUrl)("credit lots, refunds and expiry", () => {
  let database: TestDatabase;
  let sql: postgres.Sql;
  let topUp: typeof import("../top-up");
  let refund: typeof import("../refund");
  let lots: typeof import("../lots");
  let expiry: typeof import("../expiry");
  let pricing: typeof import("../../pricing/pricing");
  let seq = 0;

  const createOrganization = async () => {
    const id = `org_lots_${++seq}_${randomUUID().slice(0, 6)}`;
    await sql`insert into organization (id, name, slug, created_at) values (${id}, ${id}, ${id}, now())`;
    return id;
  };

  /** A top-up as its verified webhook credits it; returns its lot. */
  const buy = async (
    organizationId: string,
    creditMicroSar: number,
    options: {
      feeMicroSar?: number;
      paymentId?: string;
      bonusCode?: { id: string; bonusMicroSar: number };
    } = {},
  ) => {
    const paymentId = options.paymentId ?? `pi_${randomUUID()}`;
    const feeMicroSar = options.feeMicroSar ?? 5_500_000;
    await topUp.applyTopUpEvent({
      type: "paid",
      provider: "stripe",
      mode: "live",
      paymentId,
      organizationId,
      creditMicroSar,
      feeMicroSar,
      feeRate: null,
      promo: options.bonusCode
        ? {
            codeId: options.bonusCode.id,
            feeDiscountMicroSar: 0,
            bonusMicroSar: options.bonusCode.bonusMicroSar,
          }
        : null,
      collectedMicroSar: creditMicroSar + feeMicroSar,
    });
    const [lot] = await sql<{ id: string }[]>`
      select l.id from credit_lot l join credit_ledger_entry e on e.id = l.entry_id
       where e.idempotency_key = ${`top_up:stripe:${paymentId}`}`;
    if (!lot) {
      throw new Error("no lot for the top-up");
    }
    return { lotId: lot.id, paymentId };
  };

  /** Spends as the gateway does: a hold, then its settlement. */
  const spend = async (organizationId: string, amount: number) => {
    const id = randomUUID();
    const held = await sql.unsafe(RESERVE_SQL, [id, organizationId, amount]);
    if (held.length === 0) {
      return false;
    }
    await sql.unsafe(SETTLE_SQL, [
      id,
      organizationId,
      randomUUID(),
      amount,
      `usage:${id}`,
    ]);
    return true;
  };

  /** Moves a lot's dates as if time had passed: the lot is append-only, so its trigger is bypassed. */
  const setLotDates = async (
    lotId: string,
    dates: { expiresAt?: string | null; refundableUntil?: string | null },
  ) =>
    sql.begin(async (tx) => {
      await tx`set local session_replication_role = replica`;
      if (dates.expiresAt !== undefined) {
        await tx`update credit_lot set expires_at = ${dates.expiresAt === null ? null : tx.unsafe(dates.expiresAt)},
                 created_at = now() - interval '400 days' where id = ${lotId}`;
      }
      if (dates.refundableUntil !== undefined) {
        await tx`update credit_lot set refundable_until = ${dates.refundableUntil === null ? null : tx.unsafe(dates.refundableUntil)},
                 created_at = now() - interval '400 days' where id = ${lotId}`;
      }
    });

  const account = async (organizationId: string) => {
    const [row] = await sql<{ balance: string; held: string }[]>`
      select balance, held from credit_account where organization_id = ${organizationId}`;
    return { balance: Number(row?.balance ?? 0), held: Number(row?.held ?? 0) };
  };
  const ledgerSum = async (organizationId: string) => {
    const [row] = await sql<{ sum: string }[]>`
      select coalesce(sum(amount), 0) as sum from credit_ledger_entry where organization_id = ${organizationId}`;
    return Number(row?.sum ?? 0);
  };
  const entriesOf = async (organizationId: string, kind: string) =>
    sql<
      {
        amount: string;
        credit_lot_id: string | null;
        idempotency_key: string;
      }[]
    >`
      select amount, credit_lot_id, idempotency_key from credit_ledger_entry
       where organization_id = ${organizationId} and kind = ${kind} order by created_at, id`;
  const remainingOf = async (organizationId: string) => {
    const { db } = await import("@workspace/db/server");
    return (await lots.getLotRemainders(db, organizationId)).remaining;
  };
  /** The invariants of ADR 0019: balance is the ledger, held is the open holds, lots add up to the balance. */
  const expectBooksBalance = async (organizationId: string) => {
    const { balance, held } = await account(organizationId);
    expect(balance).toBe(await ledgerSum(organizationId));
    const [open] = await sql<{ sum: string }[]>`
      select coalesce(sum(amount), 0) as sum from credit_reservation
       where organization_id = ${organizationId} and settled_at is null`;
    expect(held).toBe(Number(open?.sum ?? 0));
    const remaining = [...(await remainingOf(organizationId)).values()].reduce(
      (a, b) => a + b,
      0,
    );
    expect(remaining).toBe(Math.max(balance, 0));
  };

  const setTerms = (
    refundWindowHours: number,
    creditExpiryDays: number | null,
  ) =>
    pricing.updateCreditTerms({
      refundWindowHours,
      creditExpiryDays,
      staffId: "staff_terms",
    });

  const refundAs = async (
    role: string,
    organizationId: string,
    lotId: string,
  ) =>
    (await app()).request(
      `/api/credits/${organizationId}/lots/${lotId}/refund`,
      {
        method: "POST",
        headers: { "x-test-role": role },
      },
    );

  const deliver = async (payload: string) => {
    const { webhookHandler } = await import("@workspace/billing-web/server");
    return webhookHandler(
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
  };

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;
    sql = (await import("postgres")).default(database.url, {
      max: 12,
      onnotice: () => undefined,
    });
    topUp = await import("../top-up");
    refund = await import("../refund");
    lots = await import("../lots");
    expiry = await import("../expiry");
    pricing = await import("../../pricing/pricing");
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  beforeEach(async () => {
    stripeFake.next = "succeeded";
    stripeFake.byKey.clear();
    stripeFake.creates.length = 0;
    stripeFake.charges.clear();
    stripeFake.beforeReturn = null;
    await setTerms(24, 365);
  });

  describe("lots", () => {
    it("opens a lot per grant with the terms in force at the time, and none for usage or a debit", async () => {
      const organizationId = await createOrganization();
      const { lotId } = await buy(organizationId, 100 * SAR);
      await setTerms(0, null);
      const second = await buy(organizationId, 50 * SAR);
      const { adjustCredits } = await import("../adjust");
      await adjustCredits({
        organizationId,
        amount: 3 * SAR,
        reason: "goodwill",
        staffId: "staff",
      });
      await adjustCredits({
        organizationId,
        amount: -1 * SAR,
        reason: "correction",
        staffId: "staff",
      });
      await spend(organizationId, 2 * SAR);

      const rows = await sql<
        {
          id: string;
          source: string;
          granted: string;
          expiry_days: number | null;
          window_hours: number | null;
        }[]
      >`
        select id, source, granted_micro_sar as granted,
               round(extract(epoch from expires_at - created_at) / 86400)::int as expiry_days,
               round(extract(epoch from refundable_until - created_at) / 3600)::int as window_hours
          from credit_lot where organization_id = ${organizationId} order by created_at, source desc`;
      expect(rows).toEqual([
        {
          id: lotId,
          source: "top_up",
          granted: String(100 * SAR),
          expiry_days: 365,
          window_hours: 24,
        },
        {
          id: second.lotId,
          source: "top_up",
          granted: String(50 * SAR),
          expiry_days: null,
          window_hours: null,
        },
        {
          id: expect.any(String),
          source: "adjustment",
          granted: String(3 * SAR),
          expiry_days: null,
          window_hours: null,
        },
      ]);
      await expectBooksBalance(organizationId);
    });

    it("keeps a lot append-only: only the expiry job's mark may change it, once", async () => {
      const organizationId = await createOrganization();
      const { lotId } = await buy(organizationId, 10 * SAR);
      await expect(
        sql`update credit_lot set granted_micro_sar = 1 where id = ${lotId}`,
      ).rejects.toThrow(/append-only/);
      await expect(
        sql`delete from credit_lot where id = ${lotId}`,
      ).rejects.toThrow(/append-only/);
      await sql`update credit_lot set expired_at = now() where id = ${lotId}`;
      await expect(
        sql`update credit_lot set expired_at = now() where id = ${lotId}`,
      ).rejects.toThrow(/append-only/);
    });

    it("refuses an expiry entry with no lot, a refund that adds credit, and a lot on any other kind", async () => {
      const organizationId = await createOrganization();
      const { lotId } = await buy(organizationId, 10 * SAR);
      const insert = (kind: string, amount: number, lot: string | null) =>
        sql`insert into credit_ledger_entry (id, organization_id, amount, kind, idempotency_key, credit_lot_id)
            values (${randomUUID()}, ${organizationId}, ${amount}, ${kind}, ${randomUUID()}, ${lot})`;
      await expect(insert("expiry", -1, null)).rejects.toMatchObject({
        constraint_name: "credit_ledger_entry_lot_kind",
      });
      await expect(insert("expiry", 1, lotId)).rejects.toMatchObject({
        constraint_name: "credit_ledger_entry_lot_kind",
      });
      await expect(insert("adjustment", -1, lotId)).rejects.toMatchObject({
        constraint_name: "credit_ledger_entry_lot_kind",
      });
      await expect(insert("refund", 1, lotId)).rejects.toMatchObject({
        constraint_name: "credit_ledger_entry_refund_is_debit",
      });
    });
  });

  describe("self-serve refund", () => {
    it("returns the unused part of that purchase's credit in whole halalas, never the fee", async () => {
      const organizationId = await createOrganization();
      const { lotId, paymentId } = await buy(organizationId, 100 * SAR, {
        feeMicroSar: 5_500_000,
      });
      await spend(organizationId, 30_004_000); // SAR 30.004 used: SAR 69.996 left

      const result = await refund.requestRefund({
        organizationId,
        lotId,
        userId: "user_admin",
      });

      expect(result).toEqual({
        status: "succeeded",
        amountMicroSar: 69_990_000,
      });
      expect(stripeFake.creates).toHaveLength(1);
      expect(stripeFake.creates[0]?.params).toMatchObject({
        payment_intent: paymentId,
        amount: 6999, // halalas: the credit, never the fee
        metadata: { purpose: "credits_refund", creditMicroSar: "69990000" },
      });
      expect(await entriesOf(organizationId, "refund")).toEqual([
        {
          amount: String(-69_990_000),
          credit_lot_id: lotId,
          idempotency_key: expect.stringMatching(/^refund:stripe:re_/),
        },
      ]);
      expect(await account(organizationId)).toEqual({
        balance: 6_000,
        held: 0,
      });
      await expectBooksBalance(organizationId);
    });

    it("never refunds a promo bonus: it is a lot of its own, spent first", async () => {
      const organizationId = await createOrganization();
      const [code] = await sql<{ id: string }[]>`
        insert into promo_code (id, code, kind, bonus_bps, bonus_cap_micro_sar, created_by, updated_by)
        values (${randomUUID()}, ${`BONUS${seq}`}, 'topup_bonus', 1000, 10000000, 'staff', 'staff') returning id`;
      const { lotId } = await buy(organizationId, 100 * SAR, {
        bonusCode: { id: code?.id ?? "", bonusMicroSar: 10 * SAR },
      });
      await spend(organizationId, 4 * SAR); // from the bonus first

      const result = await refund.requestRefund({
        organizationId,
        lotId,
        userId: "user_admin",
      });
      expect(result.amountMicroSar).toBe(100 * SAR);
      expect((await account(organizationId)).balance).toBe(6 * SAR); // the rest of the bonus
      await expectBooksBalance(organizationId);
    });

    it("refuses once the window has passed, when refunds are off, and for a purchase made while they were off", async () => {
      const organizationId = await createOrganization();
      const old = await buy(organizationId, 10 * SAR);
      await setLotDates(old.lotId, {
        refundableUntil: "now() - interval '1 second'",
      });
      await expect(
        refund.requestRefund({ organizationId, lotId: old.lotId, userId: "u" }),
      ).rejects.toMatchObject({
        status: 409,
        code: "billing:error.refund.windowClosed",
      });

      const fresh = await buy(organizationId, 10 * SAR);
      await setTerms(0, 365);
      await expect(
        refund.requestRefund({
          organizationId,
          lotId: fresh.lotId,
          userId: "u",
        }),
      ).rejects.toMatchObject({
        code: "billing:error.refund.refundsOff",
      });
      const boughtWhileOff = await buy(organizationId, 10 * SAR);
      await setTerms(24, 365);
      await expect(
        refund.requestRefund({
          organizationId,
          lotId: boughtWhileOff.lotId,
          userId: "u",
        }),
      ).rejects.toMatchObject({
        code: "billing:error.refund.windowClosed",
      });
      expect(stripeFake.creates).toHaveLength(0);
      expect(await entriesOf(organizationId, "refund")).toEqual([]);
    });

    it("refuses when nothing whole is left, for a lot that is not a top-up, and for another organization's lot", async () => {
      const organizationId = await createOrganization();
      const { lotId } = await buy(organizationId, 10 * SAR);
      await spend(organizationId, 9_999_000);
      await expect(
        refund.requestRefund({ organizationId, lotId, userId: "u" }),
      ).rejects.toMatchObject({
        code: "billing:error.refund.nothingLeft",
      });
      const { adjustCredits } = await import("../adjust");
      const adjusted = await adjustCredits({
        organizationId,
        amount: 5 * SAR,
        reason: "x",
        staffId: "s",
      });
      await expect(
        refund.requestRefund({
          organizationId,
          lotId: adjusted.lotId ?? "",
          userId: "u",
        }),
      ).rejects.toMatchObject({ code: "billing:error.refund.notRefundable" });
      const other = await createOrganization();
      await expect(
        refund.requestRefund({ organizationId: other, lotId, userId: "u" }),
      ).rejects.toMatchObject({
        status: 404,
      });
    });

    it("refuses credit that requests in flight are holding", async () => {
      const organizationId = await createOrganization();
      const { lotId } = await buy(organizationId, 10 * SAR);
      await sql.unsafe(RESERVE_SQL, [randomUUID(), organizationId, 1 * SAR]);
      await expect(
        refund.requestRefund({ organizationId, lotId, userId: "u" }),
      ).rejects.toMatchObject({
        code: "billing:error.refund.creditInUse",
      });
      expect(stripeFake.creates).toHaveLength(0);
    });

    it("lets organization owners and admins refund, not members", async () => {
      const organizationId = await createOrganization();
      const { lotId } = await buy(organizationId, 10 * SAR);
      const member = await refundAs("member", organizationId, lotId);
      expect(member.status).toBe(403);
      expect(stripeFake.creates).toHaveLength(0);
      const admin = await refundAs("admin", organizationId, lotId);
      expect(admin.status).toBe(200);
      expect(await admin.json()).toEqual({
        status: "succeeded",
        amountMicroSar: 10 * SAR,
      });
      // a second click on a refund already made
      const again = await refundAs("owner", organizationId, lotId);
      expect(again.status).toBe(409);
      expect(await again.json()).toEqual({
        code: "billing:error.refund.alreadyRefunded",
      });
    });

    it("counts a double click once: one request, one key, one entry", async () => {
      const organizationId = await createOrganization();
      const { lotId } = await buy(organizationId, 10 * SAR);
      const results = await Promise.allSettled([
        refund.requestRefund({ organizationId, lotId, userId: "u" }),
        refund.requestRefund({ organizationId, lotId, userId: "u" }),
      ]);
      expect(
        results.filter((r) => r.status === "fulfilled").length,
      ).toBeGreaterThanOrEqual(1);
      for (const r of results) {
        if (r.status === "rejected") {
          expect(r.reason).toMatchObject({
            code: "billing:error.refund.alreadyRefunded",
          });
        }
      }
      expect(new Set(stripeFake.creates.map((call) => call.key)).size).toBe(1);
      expect(stripeFake.byKey.size).toBe(1);
      expect(await entriesOf(organizationId, "refund")).toHaveLength(1);
      expect(await account(organizationId)).toEqual({ balance: 0, held: 0 });
      await expectBooksBalance(organizationId);
    });

    it("counts its webhooks once, even when one overtakes our own write", async () => {
      const organizationId = await createOrganization();
      const { lotId, paymentId } = await buy(organizationId, 10 * SAR);
      // refund.created arrives while the refund call is still returning
      stripeFake.beforeReturn = async (made) => {
        expect((await deliver(event("refund.created", made))).status).toBe(200);
      };
      await refund.requestRefund({ organizationId, lotId, userId: "u" });
      const made = [...stripeFake.byKey.values()][0];
      stripeFake.charges.set("ch_1", made ? [made] : []);
      await deliver(event("refund.updated", made));
      await deliver(
        event("charge.refunded", {
          id: "ch_1",
          object: "charge",
          payment_intent: paymentId,
        }),
      );
      await deliver(event("refund.created", made));

      expect(await entriesOf(organizationId, "refund")).toHaveLength(1);
      expect(await account(organizationId)).toEqual({ balance: 0, held: 0 });
      await expectBooksBalance(organizationId);
    });

    it("keeps a request pending when Stripe cannot be reached, and a retry sends the same key", async () => {
      const organizationId = await createOrganization();
      const { lotId } = await buy(organizationId, 10 * SAR);
      stripeFake.next = "unreachable";
      await expect(
        refund.requestRefund({ organizationId, lotId, userId: "u" }),
      ).rejects.toMatchObject({
        status: 502,
        code: "billing:error.refund.providerUnavailable",
      });
      // held meanwhile, so it cannot be spent
      expect(await account(organizationId)).toEqual({
        balance: 10 * SAR,
        held: 10 * SAR,
      });
      expect(await spend(organizationId, 1 * SAR)).toBe(false);

      stripeFake.next = "succeeded";
      expect(
        await refund.requestRefund({ organizationId, lotId, userId: "u" }),
      ).toEqual({
        status: "succeeded",
        amountMicroSar: 10 * SAR,
      });
      expect(new Set(stripeFake.creates.map((call) => call.key)).size).toBe(1);
      expect(await account(organizationId)).toEqual({ balance: 0, held: 0 });
      await expectBooksBalance(organizationId);
    });

    it("frees the credit when Stripe refuses, and does not try again", async () => {
      const organizationId = await createOrganization();
      const { lotId } = await buy(organizationId, 10 * SAR);
      stripeFake.next = "refused";
      await expect(
        refund.requestRefund({ organizationId, lotId, userId: "u" }),
      ).rejects.toMatchObject({
        code: "billing:error.refund.failed",
      });
      expect(await account(organizationId)).toEqual({
        balance: 10 * SAR,
        held: 0,
      });
      await expect(
        refund.requestRefund({ organizationId, lotId, userId: "u" }),
      ).rejects.toMatchObject({
        code: "billing:error.refund.failed",
      });
      expect(stripeFake.creates).toHaveLength(1);
      await expectBooksBalance(organizationId);
    });

    it("holds a pending refund until its webhook says it succeeded or failed", async () => {
      const organizationId = await createOrganization();
      const first = await buy(organizationId, 10 * SAR);
      const second = await buy(organizationId, 20 * SAR);
      stripeFake.next = "pending";
      expect(
        await refund.requestRefund({
          organizationId,
          lotId: first.lotId,
          userId: "u",
        }),
      ).toEqual({
        status: "pending",
        amountMicroSar: 10 * SAR,
      });
      expect(await account(organizationId)).toEqual({
        balance: 30 * SAR,
        held: 10 * SAR,
      });
      const made = [...stripeFake.byKey.values()][0];
      await deliver(event("refund.updated", { ...made, status: "succeeded" }));
      expect(await account(organizationId)).toEqual({
        balance: 20 * SAR,
        held: 0,
      });

      await refund.requestRefund({
        organizationId,
        lotId: second.lotId,
        userId: "u",
      });
      const failed = [...stripeFake.byKey.values()][1];
      await deliver(event("refund.failed", { ...failed, status: "failed" }));
      expect(await account(organizationId)).toEqual({
        balance: 20 * SAR,
        held: 0,
      });
      const [row] = await sql<
        { status: string }[]
      >`select status from credit_refund where credit_lot_id = ${second.lotId}`;
      expect(row?.status).toBe("failed");
      await expectBooksBalance(organizationId);
    });
  });

  describe("refund made in Stripe's dashboard", () => {
    it("removes the money refunded once, whichever events Stripe sends", async () => {
      const organizationId = await createOrganization();
      const { lotId, paymentId } = await buy(organizationId, 100 * SAR, {
        feeMicroSar: 5_500_000,
      });
      const made = dashboardRefund(paymentId, 3000);
      stripeFake.charges.set("ch_dash", [made]);
      await deliver(
        event("charge.refunded", {
          id: "ch_dash",
          object: "charge",
          payment_intent: paymentId,
          refunds: null,
        }),
      );
      await deliver(event("refund.created", made));
      await deliver(event("refund.updated", made));
      await deliver(
        event("charge.refunded", {
          id: "ch_dash",
          object: "charge",
          payment_intent: paymentId,
          refunds: null,
        }),
      );

      expect(await entriesOf(organizationId, "refund")).toEqual([
        {
          amount: String(-30 * SAR),
          credit_lot_id: lotId,
          idempotency_key: `refund:stripe:${made.id}`,
        },
      ]);
      await expectBooksBalance(organizationId);
    });

    it("matches partial refunds by payment and stops at the purchase's credit: the fee removes nothing", async () => {
      const organizationId = await createOrganization();
      const { paymentId } = await buy(organizationId, 100 * SAR, {
        feeMicroSar: 5_500_000,
      });
      const first = dashboardRefund(paymentId, 6000);
      await deliver(event("refund.created", first));
      // redelivered: recognised by its id, not worked out again (which would now give SAR 40)
      expect((await deliver(event("refund.updated", first))).status).toBe(200);
      await deliver(event("refund.created", dashboardRefund(paymentId, 8000))); // SAR 40 of it is credit
      await deliver(event("refund.created", dashboardRefund(paymentId, 550))); // the fee
      expect(
        (await entriesOf(organizationId, "refund")).map((row) =>
          Number(row.amount),
        ),
      ).toEqual([-60 * SAR, -40 * SAR]);
      expect((await account(organizationId)).balance).toBe(0);
      await expectBooksBalance(organizationId);
    });

    it("hides the refund button once staff refunded the purchase", async () => {
      const organizationId = await createOrganization();
      const { lotId, paymentId } = await buy(organizationId, 100 * SAR);
      await deliver(event("refund.created", dashboardRefund(paymentId, 1000)));
      const view = await lots.getCreditLots(organizationId);
      expect(view.lots.find((lot) => lot.id === lotId)?.refund.state).toBe(
        "refunded",
      );
      await expect(
        refund.requestRefund({ organizationId, lotId, userId: "u" }),
      ).rejects.toMatchObject({
        code: "billing:error.refund.alreadyRefunded",
      });
    });
  });

  describe("expiry", () => {
    it("expires what is left of a lot past its date, once, with the balance in the same transaction", async () => {
      const organizationId = await createOrganization();
      const { lotId } = await buy(organizationId, 10 * SAR);
      await spend(organizationId, 3 * SAR);
      await setLotDates(lotId, { expiresAt: "now() - interval '1 minute'" });

      const first = await expiry.runCreditExpiryJob();
      expect(first).toMatchObject({
        entries: 1,
        expiredMicroSar: 7 * SAR,
        failed: 0,
      });
      expect(await entriesOf(organizationId, "expiry")).toEqual([
        {
          amount: String(-7 * SAR),
          credit_lot_id: lotId,
          idempotency_key: `expiry:${lotId}`,
        },
      ]);
      expect((await account(organizationId)).balance).toBe(0);

      const second = await expiry.runCreditExpiryJob();
      expect(second).toMatchObject({ organizations: 0, entries: 0 });
      expect(await entriesOf(organizationId, "expiry")).toHaveLength(1);
      await expectBooksBalance(organizationId);
    });

    it("attributes spending to the lot that expires first, across several lots with spending in between", async () => {
      const organizationId = await createOrganization();
      const a = await buy(organizationId, 10 * SAR); // expires in 365 days
      await spend(organizationId, 4 * SAR); // a: 6
      await setTerms(24, 30);
      const b = await buy(organizationId, 10 * SAR); // expires in 30 days: spent before a
      await spend(organizationId, 12 * SAR); // b: 0, a: 4
      await setTerms(24, 400);
      const c = await buy(organizationId, 5 * SAR); // after a
      await spend(organizationId, 6 * SAR); // a: 0, c: 3
      await buy(organizationId, 2 * SAR); // d: 2

      const remaining = await remainingOf(organizationId);
      expect(remaining.get(a.lotId)).toBe(0);
      expect(remaining.get(b.lotId)).toBe(0);
      expect(remaining.get(c.lotId)).toBe(3 * SAR);

      // 401 days pass: every lot is due, and their order is the same as when they were spent
      const d = (
        await sql<{ id: string }[]>`
        select id from credit_lot where organization_id = ${organizationId} and id not in ${sql([a.lotId, b.lotId, c.lotId])}`
      )[0];
      for (const lotId of [a.lotId, b.lotId, c.lotId, d?.id ?? ""]) {
        await setLotDates(lotId, {
          expiresAt: "expires_at - interval '401 days'",
        });
      }
      const result = await expiry.runCreditExpiryJob();
      expect(result).toMatchObject({
        lotsClosed: 4,
        entries: 2,
        expiredMicroSar: 5 * SAR,
      });
      expect(
        (await entriesOf(organizationId, "expiry")).map((row) => [
          row.credit_lot_id,
          Number(row.amount),
        ]),
      ).toEqual(
        expect.arrayContaining([
          [c.lotId, -3 * SAR],
          [d?.id, -2 * SAR],
        ]),
      );
      expect((await account(organizationId)).balance).toBe(0);
      await expectBooksBalance(organizationId);
    });

    it("expires promo credit on its code's own date, and on the purchase rule without one", async () => {
      const organizationId = await createOrganization();
      const [dated] = await sql<{ id: string }[]>`
        insert into promo_code (id, code, kind, credit_micro_sar, credit_expires_at, created_by, updated_by)
        values (${randomUUID()}, ${`DATED${seq}`}, 'free_credit', 5000000, now() + interval '10 days', 's', 's') returning id, code`;
      const [plain] = await sql<{ id: string }[]>`
        insert into promo_code (id, code, kind, credit_micro_sar, created_by, updated_by)
        values (${randomUUID()}, ${`PLAIN${seq}`}, 'free_credit', 5000000, 's', 's') returning id`;
      const { redeemFreeCredit } = await import("../../promo-codes/redeem");
      await redeemFreeCredit({
        organizationId,
        code: `DATED${seq}`,
        userId: "u",
      });
      await redeemFreeCredit({
        organizationId,
        code: `PLAIN${seq}`,
        userId: "u",
      });
      const rows = await sql<{ promo_code_id: string; days: number }[]>`
        select e.promo_code_id, round(extract(epoch from l.expires_at - l.created_at) / 86400)::int as days
          from credit_lot l join credit_ledger_entry e on e.id = l.entry_id
         where l.organization_id = ${organizationId} order by days`;
      expect(rows).toEqual([
        { promo_code_id: dated?.id, days: 10 },
        { promo_code_id: plain?.id, days: 365 },
      ]);
      // a code whose credit date has passed grants nothing more
      await sql.begin(async (tx) => {
        await tx`update promo_code set credit_expires_at = now() - interval '1 second' where id = ${dated?.id ?? ""}`;
      });
      const other = await createOrganization();
      await expect(
        redeemFreeCredit({
          organizationId: other,
          code: `DATED${seq}`,
          userId: "u",
        }),
      ).rejects.toMatchObject({
        status: 400,
      });
    });

    it("waits for credit held by requests in flight, then expires it", async () => {
      const organizationId = await createOrganization();
      const { lotId } = await buy(organizationId, 10 * SAR);
      const holdId = randomUUID();
      await sql.unsafe(RESERVE_SQL, [holdId, organizationId, 2 * SAR]);
      await setLotDates(lotId, { expiresAt: "now() - interval '1 minute'" });
      expect(await expiry.runCreditExpiryJob()).toMatchObject({
        entries: 0,
        deferred: 1,
      });
      await sql.unsafe(SETTLE_SQL, [
        holdId,
        organizationId,
        randomUUID(),
        1 * SAR,
        `usage:${holdId}`,
      ]);
      expect(await expiry.runCreditExpiryJob()).toMatchObject({
        entries: 1,
        expiredMicroSar: 9 * SAR,
      });
      expect(await account(organizationId)).toEqual({ balance: 0, held: 0 });
      await expectBooksBalance(organizationId);
    });

    it("loses no update when it runs, twice at once, while the gateway settles", async () => {
      const organizationId = await createOrganization();
      const lotIds: string[] = [];
      for (let i = 0; i < 4; i++) {
        lotIds.push((await buy(organizationId, 50 * SAR)).lotId);
      }
      for (const lotId of lotIds.slice(0, 3)) {
        await setLotDates(lotId, { expiresAt: "now() - interval '1 minute'" });
      }
      const spends = Array.from({ length: 60 }, () =>
        spend(organizationId, 1_234_567),
      );
      const jobs = [expiry.runCreditExpiryJob(), expiry.runCreditExpiryJob()];
      await Promise.all([...spends, ...jobs]);
      await expiry.runCreditExpiryJob();

      const expiries = await entriesOf(organizationId, "expiry");
      expect(new Set(expiries.map((row) => row.credit_lot_id)).size).toBe(
        expiries.length,
      );
      const [closed] = await sql<{ count: string }[]>`
        select count(*) from credit_lot where organization_id = ${organizationId} and expired_at is not null`;
      expect(Number(closed?.count)).toBe(3);
      await expectBooksBalance(organizationId);
      const remaining = await remainingOf(organizationId);
      expect(lotIds.slice(0, 3).map((id) => remaining.get(id))).toEqual([
        0, 0, 0,
      ]);
    });
  });

  describe("credits page", () => {
    it("lists each open lot's remainder, expiry and refund, and warns before credit expires", async () => {
      const organizationId = await createOrganization();
      const bought = await buy(organizationId, 10 * SAR);
      await setTerms(0, 400);
      const later = await buy(organizationId, 20 * SAR);
      await setTerms(24, 365);
      await spend(organizationId, 1_005_000);
      await setLotDates(later.lotId, {
        expiresAt: "now() + interval '20 days'",
      });

      let view = await lots.getCreditLots(organizationId);
      expect(view.areRefundsEnabled).toBe(true);
      expect(
        view.lots.map((lot) => [
          lot.id,
          lot.remainingMicroSar,
          lot.refund.state,
          lot.refund.amountMicroSar,
        ]),
      ).toEqual([
        [later.lotId, 18_995_000, "none", 18_990_000],
        [bought.lotId, 10 * SAR, "available", 10 * SAR],
      ]);
      expect(view.expiring).toMatchObject({
        withinDays: 30,
        amountMicroSar: 18_995_000,
      });

      await setLotDates(later.lotId, {
        expiresAt: "now() + interval '6 days'",
      });
      view = await lots.getCreditLots(organizationId);
      expect(view.expiring).toMatchObject({
        withinDays: 7,
        amountMicroSar: 18_995_000,
      });

      await refund.requestRefund({
        organizationId,
        lotId: bought.lotId,
        userId: "u",
      });
      view = await lots.getCreditLots(organizationId);
      expect(view.lots.map((lot) => lot.id)).toEqual([later.lotId]);
    });
  });

  describe("reconciliation", () => {
    it("finds nothing on books that balance, and names an organization whose cache or lots disagree", async () => {
      const { checkLedgerInvariants } =
        await import("../../reconciliation/invariants");
      const organizationId = await createOrganization();
      await buy(organizationId, 10 * SAR);
      await spend(organizationId, 1 * SAR);
      const clean = await checkLedgerInvariants();
      expect(
        clean.balances.filter((row) => row.organizationId === organizationId),
      ).toEqual([]);
      expect(
        clean.lots.filter((row) => row.organizationId === organizationId),
      ).toEqual([]);

      // a credit written past the API: no lot, and the cached balance does not move
      await sql`insert into credit_ledger_entry (id, organization_id, amount, kind, idempotency_key)
                values (${randomUUID()}, ${organizationId}, 5000000, 'top_up', ${randomUUID()})`;
      const broken = await checkLedgerInvariants();
      expect(broken.balances).toContainEqual({
        organizationId,
        balance: 9 * SAR,
        ledgerSum: 14 * SAR,
      });
      expect(
        broken.grantsWithoutLot.map((row) => row.organizationId),
      ).toContain(organizationId);
    });
  });
});
