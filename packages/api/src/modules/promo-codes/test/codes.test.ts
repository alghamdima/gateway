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
import type { User } from "@workspace/auth";
import type { TestDatabase } from "@workspace/db/testing";
import type postgres from "postgres";

// Needs real Postgres: uniqueness, the checks on each kind, the append-only
// history and the row locks are the database's. Skips without DATABASE_URL.
const adminUrl = process.env.DATABASE_URL;

// The router's middleware imports the auth server and the translations (JSX the
// test runner does not compile); the request tests need only the English
// validation messages.
const audited = vi.hoisted(() => [] as Record<string, unknown>[]);
vi.mock("@workspace/auth/server", () => ({
  auth: {},
  clientIp: () => null,
  recordStaffAction: async (entry: Record<string, unknown>) => {
    audited.push(entry);
  },
}));
vi.mock("@workspace/i18n", async () => {
  const z = await import("zod");
  return { getValidationLocale: () => z.locales.en };
});
vi.mock("@workspace/i18n/server", () => ({ getLocaleFromRequest: () => "en" }));

const SAR = 1_000_000;

const input = (
  overrides: Partial<SavePromoCodeInput> = {},
): SavePromoCodeInput => ({
  code: "WELCOME",
  kind: "fee_discount",
  feeDiscountBps: 5000,
  feeDiscountMicroSar: null,
  bonusBps: null,
  bonusCapMicroSar: null,
  creditMicroSar: null,
  maxUses: null,
  onePerOrganization: true,
  startsAt: null,
  endsAt: null,
  minTopupMicroSar: null,
  allOrganizations: true,
  organizationIds: [],
  note: "",
  ...overrides,
});

/** The admin promo codes router behind the real staff access check. */
const staffApp = async (role: string) => {
  const { Hono } = await import("hono");
  const { HttpException } = await import("@workspace/shared/utils");
  const { enforceStaffAccess } = await import("../../admin/access-middleware");
  const { promoCodesAdminRouter } = await import("../router");
  const staff: User = {
    id: `staff_${role}`,
    name: "Staff",
    email: `${role}@example.com`,
    emailVerified: true,
    banned: false,
    isAnonymous: false,
    twoFactorEnabled: true,
    role,
    createdAt: new Date(),
    updatedAt: new Date(),
  };
  return new Hono<{ Variables: { user: User } }>()
    .use(async (c, next) => {
      c.set("user", staff);
      await next();
    })
    .use("/api/admin/*", enforceStaffAccess)
    .route("/api/admin/promo-codes", promoCodesAdminRouter)
    .onError((error) =>
      Response.json(
        { message: error.message },
        { status: error instanceof HttpException ? error.status : 500 },
      ),
    );
};

describe.skipIf(!adminUrl)("promo codes managed by staff (BILL-08)", () => {
  let database: TestDatabase;
  let sql: postgres.Sql;
  let codes: typeof import("../codes");
  let redeem: typeof import("../redeem");
  let seq = 0;

  const createOrganization = async () => {
    const id = `org_codes_${++seq}`;
    await sql`insert into organization (id, name, slug, created_at) values (${id}, ${id}, ${id}, now())`;
    return id;
  };
  const uniqueCode = (prefix: string) => `${prefix}-${++seq}`;
  const create = (overrides: Partial<SavePromoCodeInput> = {}) =>
    codes.createPromoCode({
      ...input({ code: uniqueCode("CODE"), ...overrides }),
      staffId: "staff_1",
    });
  const changes = async (id: string) =>
    sql<
      {
        before: Record<string, unknown> | null;
        after: Record<string, unknown> | null;
        changed_by: string;
      }[]
    >`
      select before, after, changed_by from promo_code_change where promo_code_id = ${id} order by id`;

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;
    sql = (await import("postgres")).default(database.url, { max: 2 });
    codes = await import("../codes");
    redeem = await import("../redeem");
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  beforeEach(() => {
    audited.length = 0;
  });

  it("stores the code normalized and records its creation", async () => {
    const { code, before, after } = await codes.createPromoCode({
      ...input({ code: "SPRING-26" }),
      staffId: "staff_1",
    });
    expect(code.code).toBe("SPRING-26");
    expect(before).toBeNull();
    expect(after).toMatchObject({
      code: "SPRING-26",
      kind: "fee_discount",
      feeDiscountBps: 5000,
    });
    expect(await changes(code.id)).toEqual([
      {
        before: null,
        after: expect.objectContaining({ code: "SPRING-26" }),
        changed_by: "staff_1",
      },
    ]);
  });

  it("refuses a second code with the same letters, whatever their case", async () => {
    await create({ code: "DOUBLE" });
    await expect(create({ code: "DOUBLE" })).rejects.toMatchObject({
      status: 409,
    });
    // the same through the API, typed in lower case
    const app = await staffApp("admin");
    const response = await app.request("/api/admin/promo-codes", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ...input(), code: " double " }),
    });
    expect(response.status).toBe(409);
    // and the database itself: one row per normalized code, in the allowed letters only
    await expect(
      sql`insert into promo_code (id, code, kind, fee_discount_bps, created_by, updated_by) values ('p_dup', 'DOUBLE', 'fee_discount', 1, 's', 's')`,
    ).rejects.toMatchObject({ constraint_name: "promo_code_code_unique" });
    await expect(
      sql`insert into promo_code (id, code, kind, fee_discount_bps, created_by, updated_by) values ('p_low', 'double', 'fee_discount', 1, 's', 's')`,
    ).rejects.toMatchObject({ constraint_name: "promo_code_code_format" });
  });

  it("refuses in the database a code whose amounts do not match its kind", async () => {
    await expect(
      sql`insert into promo_code (id, code, kind, fee_discount_bps, bonus_bps, created_by, updated_by) values ('p_mix', 'MIXED', 'fee_discount', 1, 1, 's', 's')`,
    ).rejects.toMatchObject({
      constraint_name: "promo_code_amounts_match_kind",
    });
    await expect(
      sql`insert into promo_code (id, code, kind, credit_micro_sar, created_by, updated_by) values ('p_part', 'PART', 'free_credit', 1, 's', 's')`,
    ).rejects.toMatchObject({ constraint_name: "promo_code_amounts_halalas" });
  });

  it("records an edit with the values before and after, and the organizations chosen", async () => {
    const organizationId = await createOrganization();
    const { code } = await create();
    const { before, after } = await codes.updatePromoCode({
      ...input({
        code: code.code,
        feeDiscountBps: 2500,
        allOrganizations: false,
        organizationIds: [organizationId],
      }),
      id: code.id,
      staffId: "staff_2",
    });
    expect(before).toMatchObject({
      feeDiscountBps: 5000,
      allOrganizations: true,
      organizationIds: [],
    });
    expect(after).toMatchObject({
      feeDiscountBps: 2500,
      allOrganizations: false,
      organizationIds: [organizationId],
    });
    expect(
      (await codes.getPromoCode(code.id)).organizations.map((org) => org.id),
    ).toEqual([organizationId]);
    expect(await changes(code.id)).toHaveLength(2);
  });

  it("changes the kind of an unused code, but not once it has been used", async () => {
    const organizationId = await createOrganization();
    const { code } = await create({
      kind: "free_credit",
      feeDiscountBps: null,
      creditMicroSar: 5 * SAR,
    });
    await codes.updatePromoCode({
      ...input({
        code: code.code,
        kind: "free_credit",
        feeDiscountBps: null,
        creditMicroSar: 10 * SAR,
      }),
      id: code.id,
      staffId: "staff_1",
    });
    await redeem.redeemFreeCredit({
      organizationId,
      code: code.code,
      userId: "user_1",
    });
    await expect(
      codes.updatePromoCode({
        ...input({ code: code.code }),
        id: code.id,
        staffId: "staff_1",
      }),
    ).rejects.toMatchObject({ status: 409 });
    // other terms still change
    const { after } = await codes.updatePromoCode({
      ...input({
        code: code.code,
        kind: "free_credit",
        feeDiscountBps: null,
        creditMicroSar: 7 * SAR,
        maxUses: 10,
      }),
      id: code.id,
      staffId: "staff_1",
    });
    expect(after).toMatchObject({
      kind: "free_credit",
      creditMicroSar: 7 * SAR,
      maxUses: 10,
    });
  });

  it("pauses and resumes, recording each change once", async () => {
    const { code } = await create();
    await codes.setPromoCodePaused({
      id: code.id,
      isPaused: true,
      staffId: "staff_1",
    });
    // pausing what is paused records nothing
    await codes.setPromoCodePaused({
      id: code.id,
      isPaused: true,
      staffId: "staff_1",
    });
    expect((await codes.getPromoCode(code.id)).status).toBe("paused");
    await codes.setPromoCodePaused({
      id: code.id,
      isPaused: false,
      staffId: "staff_1",
    });
    expect((await codes.getPromoCode(code.id)).status).toBe("active");
    const rows = await changes(code.id);
    expect(
      rows.map((row) => [row.before?.isPaused ?? null, row.after?.isPaused]),
    ).toEqual([
      [null, false],
      [false, true],
      [true, false],
    ]);
  });

  it("lists codes by search and status", async () => {
    const paused = await create({ code: uniqueCode("FINDME") });
    await codes.setPromoCodePaused({
      id: paused.code.id,
      isPaused: true,
      staffId: "staff_1",
    });
    await create({ code: uniqueCode("FINDME") });
    await create({ code: uniqueCode("OTHER") });
    const found = await codes.listPromoCodes({ q: "findme" });
    expect(found).toHaveLength(2);
    expect(
      (await codes.listPromoCodes({ q: "findme", status: "paused" })).map(
        (row) => row.id,
      ),
    ).toEqual([paused.code.id]);
    // the wildcard of LIKE is a letter like any other
    expect(await codes.listPromoCodes({ q: "%" })).toHaveLength(0);
  });

  it("keeps the history append-only", async () => {
    const { code } = await create();
    await expect(
      sql`update promo_code_change set changed_by = 'x' where promo_code_id = ${code.id}`,
    ).rejects.toThrow(/append-only/);
    await expect(
      sql`delete from promo_code_change where promo_code_id = ${code.id}`,
    ).rejects.toThrow(/append-only/);
  });

  describe("through the admin API", () => {
    it("lets finance create a code and audits it with what it became", async () => {
      const app = await staffApp("finance");
      const response = await app.request("/api/admin/promo-codes", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          ...input(),
          code: uniqueCode("fin").toLowerCase(),
        }),
      });
      expect(response.status).toBe(200);
      const created = (await response.json()) as { id: string; code: string };
      expect(created.code).toMatch(/^FIN-/);
      expect(audited).toEqual([
        expect.objectContaining({
          action: "promo.create",
          details: expect.objectContaining({
            before: null,
            after: expect.objectContaining({
              id: created.id,
              code: created.code,
            }),
          }),
        }),
      ]);
    });

    it("lets support read codes and reports but change nothing", async () => {
      const { code } = await create();
      const app = await staffApp("support");
      expect((await app.request("/api/admin/promo-codes")).status).toBe(200);
      expect(
        (await app.request(`/api/admin/promo-codes/${code.id}/report`)).status,
      ).toBe(200);
      const pause = await app.request(
        `/api/admin/promo-codes/${code.id}/paused`,
        {
          method: "PUT",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ isPaused: true }),
        },
      );
      expect(pause.status).toBe(403);
      expect((await codes.getPromoCode(code.id)).isPaused).toBe(false);
      expect(audited).toEqual([]);
    });

    it("audits a pause with the values before and after", async () => {
      const { code } = await create();
      const app = await staffApp("admin");
      const response = await app.request(
        `/api/admin/promo-codes/${code.id}/paused`,
        {
          method: "PUT",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ isPaused: true }),
        },
      );
      expect(response.status).toBe(200);
      expect(audited).toEqual([
        expect.objectContaining({
          action: "promo.pause",
          targetId: code.id,
          details: expect.objectContaining({
            before: expect.objectContaining({ isPaused: false }),
            after: expect.objectContaining({ isPaused: true }),
          }),
        }),
      ]);
    });

    it("refuses a code that breaks the rules with a readable message", async () => {
      const app = await staffApp("admin");
      const response = await app.request("/api/admin/promo-codes", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ ...input(), code: "NO SPACES" }),
      });
      expect(response.status).toBe(422);
      expect(await codes.listPromoCodes({ q: "NO SPACES" })).toEqual([]);
      // a rule across fields keeps the message it wrote
      const mixed = await app.request("/api/admin/promo-codes", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          ...input(),
          code: uniqueCode("MIX"),
          bonusBps: 100,
        }),
      });
      expect(mixed.status).toBe(422);
      expect(JSON.stringify(await mixed.json())).toContain(
        "Only a top-up bonus has a bonus percentage and cap",
      );
    });
  });
});
