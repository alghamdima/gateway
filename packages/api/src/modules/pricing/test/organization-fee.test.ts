import {
  afterAll,
  beforeAll,
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from "vitest";

import type { User } from "@workspace/auth";
import type { TestDatabase } from "@workspace/db/testing";
import type postgres from "postgres";

// Needs real Postgres: the bounds, the cascade, the append-only history and
// the gateway role's grants are enforced by the database. Skips without
// DATABASE_URL.
const adminUrl = process.env.DATABASE_URL;

// The router's middleware imports the auth server and the translations (JSX the
// test runner does not compile); the request test needs only the English
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
const DAY = 24 * 60 * 60 * 1000;
/** What migration 0018 and 0033 seed: 5.5% with a SAR 3 minimum. */
const DEFAULT_FEE = { topupFeeBps: 550, topupFeeMinMicroSar: 3 * SAR };

/** The admin organizations router behind the real staff access check. */
const staffApp = async (role: string) => {
  const { Hono } = await import("hono");
  const { HttpException } = await import("@workspace/shared/utils");
  const { enforceStaffAccess } = await import("../../admin/access-middleware");
  const { organizationsRouter } =
    await import("../../admin/organizations/router");
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
    .route("/api/admin/organizations", organizationsRouter)
    .onError((error) =>
      Response.json(
        { message: error.message },
        { status: error instanceof HttpException ? error.status : 500 },
      ),
    );
};

describe.skipIf(!adminUrl)("top-up fee per organization (ADMIN-24)", () => {
  let database: TestDatabase;
  let sql: postgres.Sql;
  let fees: typeof import("../organization-fee");
  let seq = 0;

  const createOrganization = async () => {
    const id = `org_fee_${++seq}`;
    await sql`insert into organization (id, name, slug, created_at) values (${id}, ${id}, ${id}, now())`;
    return id;
  };

  const changes = async (organizationId: string) =>
    sql<
      {
        before: Record<string, unknown> | null;
        after: Record<string, unknown> | null;
        changed_by: string;
      }[]
    >`select before, after, changed_by from pricing_change
      where target = ${`organization:${organizationId}`} order by id`;

  const request = async (
    role: string,
    method: string,
    path: string,
    body?: unknown,
  ) =>
    (await staffApp(role)).request(`/api/admin/organizations${path}`, {
      method,
      headers: { "Content-Type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
    });

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;

    sql = (await import("postgres")).default(database.url, { max: 4 });
    fees = await import("../organization-fee");
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

  it("pays the default while the organization has no fee of its own", async () => {
    const organizationId = await createOrganization();
    expect(await fees.getOrganizationTopupFee(organizationId)).toEqual({
      default: DEFAULT_FEE,
      override: null,
      effective: { ...DEFAULT_FEE, source: "default" },
    });
  });

  it("uses the organization's fee, the default for a field it leaves out, and leaves other organizations alone", async () => {
    const organizationId = await createOrganization();
    const other = await createOrganization();

    await fees.setOrganizationTopupFee({
      organizationId,
      staffId: "staff_1",
      feeBps: 200,
      feeMinMicroSar: null,
      expiresAt: null,
      note: "Launch partner",
    });

    expect(await fees.getEffectiveTopupFee(organizationId)).toEqual({
      topupFeeBps: 200,
      topupFeeMinMicroSar: DEFAULT_FEE.topupFeeMinMicroSar,
      source: "organization",
    });
    expect(await fees.getEffectiveTopupFee(other)).toEqual({
      ...DEFAULT_FEE,
      source: "default",
    });
  });

  it("falls back to the default once the fee expires, with nobody acting", async () => {
    const organizationId = await createOrganization();
    const expiresAt = new Date(Date.now() + DAY);
    await fees.setOrganizationTopupFee({
      organizationId,
      staffId: "staff_1",
      feeBps: 900,
      feeMinMicroSar: 10 * SAR,
      expiresAt,
      note: "",
    });

    expect(
      (await fees.getEffectiveTopupFee(organizationId, new Date())).source,
    ).toBe("organization");
    expect(
      await fees.getEffectiveTopupFee(
        organizationId,
        new Date(expiresAt.getTime() + 1),
      ),
    ).toEqual({ ...DEFAULT_FEE, source: "default" });
    // the timestamp round-trips exactly, so the expiry is the moment staff chose
    expect(
      (await fees.getOrganizationTopupFee(organizationId)).override?.expiresAt,
    ).toEqual(expiresAt);
  });

  it("falls back to the default once the fee is cleared, and records it", async () => {
    const organizationId = await createOrganization();
    await fees.setOrganizationTopupFee({
      organizationId,
      staffId: "staff_1",
      feeBps: 100,
      feeMinMicroSar: 0,
      expiresAt: null,
      note: "Trial",
    });
    await fees.clearOrganizationTopupFee({
      organizationId,
      staffId: "staff_2",
    });

    expect(await fees.getEffectiveTopupFee(organizationId)).toEqual({
      ...DEFAULT_FEE,
      source: "default",
    });
    expect((await changes(organizationId)).at(-1)).toEqual({
      before: {
        feeBps: 100,
        feeMinMicroSar: 0,
        expiresAt: null,
        note: "Trial",
      },
      after: null,
      changed_by: "staff_2",
    });
    // clearing what is not set changes and records nothing
    await fees.clearOrganizationTopupFee({
      organizationId,
      staffId: "staff_2",
    });
    expect(await changes(organizationId)).toHaveLength(2);
  });

  it("records each change from what to what, in order", async () => {
    const organizationId = await createOrganization();
    const expiresAt = new Date("2099-01-01T00:00:00.000Z");
    await fees.setOrganizationTopupFee({
      organizationId,
      staffId: "staff_1",
      feeBps: 300,
      feeMinMicroSar: null,
      expiresAt: null,
      note: "",
    });
    await fees.setOrganizationTopupFee({
      organizationId,
      staffId: "staff_2",
      feeBps: 700,
      feeMinMicroSar: 5 * SAR,
      expiresAt,
      note: "Higher risk",
    });

    expect(await changes(organizationId)).toEqual([
      {
        before: null,
        after: { feeBps: 300, feeMinMicroSar: null, expiresAt: null, note: "" },
        changed_by: "staff_1",
      },
      {
        before: {
          feeBps: 300,
          feeMinMicroSar: null,
          expiresAt: null,
          note: "",
        },
        after: {
          feeBps: 700,
          feeMinMicroSar: 5 * SAR,
          expiresAt: expiresAt.toISOString(),
          note: "Higher risk",
        },
        changed_by: "staff_2",
      },
    ]);
    const [latest] = await fees.getOrganizationTopupFeeChanges(organizationId);
    expect(latest).toMatchObject({
      changedBy: "staff_2",
      after: { feeBps: 700 },
    });
  });

  it("refuses values outside the default's bounds in the database, and records nothing", async () => {
    const organizationId = await createOrganization();
    const set = (feeBps: number | null, feeMinMicroSar: number | null) =>
      fees.setOrganizationTopupFee({
        organizationId,
        staffId: "staff_1",
        feeBps,
        feeMinMicroSar,
        expiresAt: null,
        note: "",
      });

    await expect(set(10_001, null)).rejects.toMatchObject({
      cause: { constraint_name: "organization_topup_fee_bps_range" },
    });
    await expect(set(-1, null)).rejects.toMatchObject({
      cause: { constraint_name: "organization_topup_fee_bps_range" },
    });
    await expect(set(null, 400_000_001)).rejects.toMatchObject({
      cause: { constraint_name: "organization_topup_fee_min_range" },
    });
    await expect(
      sql`insert into organization_topup_fee (organization_id, updated_by) values (${organizationId}, 's')`,
    ).rejects.toMatchObject({
      constraint_name: "organization_topup_fee_sets_something",
    });
    await expect(set(null, null)).rejects.toMatchObject({ status: 400 });
    await expect(
      fees.setOrganizationTopupFee({
        organizationId,
        staffId: "staff_1",
        feeBps: 100,
        feeMinMicroSar: null,
        expiresAt: new Date(Date.now() - 1000),
        note: "",
      }),
    ).rejects.toMatchObject({ status: 400 });

    expect((await fees.getOrganizationTopupFee(organizationId)).override).toBe(
      null,
    );
    expect(await changes(organizationId)).toHaveLength(0);
  });

  it("refuses an organization that does not exist", async () => {
    await expect(
      fees.setOrganizationTopupFee({
        organizationId: "org_missing",
        staffId: "staff_1",
        feeBps: 100,
        feeMinMicroSar: null,
        expiresAt: null,
        note: "",
      }),
    ).rejects.toMatchObject({ status: 404 });
  });

  it("goes with the organization when it is deleted; its history stays", async () => {
    const organizationId = await createOrganization();
    await fees.setOrganizationTopupFee({
      organizationId,
      staffId: "staff_1",
      feeBps: 100,
      feeMinMicroSar: null,
      expiresAt: null,
      note: "",
    });
    await sql`delete from organization where id = ${organizationId}`;

    const rows =
      await sql`select 1 from organization_topup_fee where organization_id = ${organizationId}`;
    expect(rows).toHaveLength(0);
    expect(await changes(organizationId)).toHaveLength(1);
  });

  it("is not readable by the gateway: the fee is charged at top-up only", async () => {
    await expect(
      sql.begin(async (tx) => {
        await tx`set local role gateway_ledger`;
        await tx`select * from organization_topup_fee`;
      }),
    ).rejects.toThrow(/permission denied/);
  });

  describe("admin routes", () => {
    it("lets admin and finance set and clear it, with before and after in the audit log", async () => {
      const organizationId = await createOrganization();
      const expiresAt = new Date(Date.now() + 30 * DAY).toISOString();

      const set = await request(
        "finance",
        "PUT",
        `/${organizationId}/top-up-fee`,
        {
          feeBps: 250,
          feeMinMicroSar: 1 * SAR,
          expiresAt,
          note: "Volume deal",
        },
      );
      expect(set.status).toBe(200);
      expect(await set.json()).toMatchObject({
        feeBps: 250,
        feeMinMicroSar: 1 * SAR,
        expiresAt,
        note: "Volume deal",
        updatedBy: "staff_finance",
      });
      expect(audited).toEqual([
        expect.objectContaining({
          action: "pricing.organization-fee",
          targetType: "organization",
          targetId: organizationId,
          details: expect.objectContaining({
            before: null,
            after: {
              feeBps: 250,
              feeMinMicroSar: 1 * SAR,
              expiresAt,
              note: "Volume deal",
            },
          }),
        }),
      ]);

      const read = await request(
        "admin",
        "GET",
        `/${organizationId}/top-up-fee`,
      );
      expect(await read.json()).toMatchObject({
        default: DEFAULT_FEE,
        effective: {
          topupFeeBps: 250,
          topupFeeMinMicroSar: 1 * SAR,
          source: "organization",
        },
      });

      const cleared = await request(
        "admin",
        "DELETE",
        `/${organizationId}/top-up-fee`,
      );
      expect(await cleared.json()).toEqual({ isCleared: true });
      expect(audited.at(-1)).toMatchObject({
        action: "pricing.organization-fee-clear",
        details: { before: { feeBps: 250 }, after: null },
      });
    });

    it("lets support read it but not change it", async () => {
      const organizationId = await createOrganization();

      expect(
        (await request("support", "GET", `/${organizationId}/top-up-fee`))
          .status,
      ).toBe(200);
      expect(
        (
          await request(
            "support",
            "GET",
            `/${organizationId}/top-up-fee/changes`,
          )
        ).status,
      ).toBe(200);
      const put = await request(
        "support",
        "PUT",
        `/${organizationId}/top-up-fee`,
        {
          feeBps: 0,
          feeMinMicroSar: 0,
          expiresAt: null,
          note: "",
        },
      );
      expect(put.status).toBe(403);
      expect(
        (await request("support", "DELETE", `/${organizationId}/top-up-fee`))
          .status,
      ).toBe(403);

      expect(
        (await fees.getOrganizationTopupFee(organizationId)).override,
      ).toBe(null);
      expect(audited).toEqual([]);
    });

    it.each([
      ["a fee over 100%", { feeBps: 10_001, feeMinMicroSar: null }],
      ["a negative fee", { feeBps: -1, feeMinMicroSar: null }],
      ["a minimum over SAR 400", { feeBps: null, feeMinMicroSar: 400_000_001 }],
      ["a fraction of a basis point", { feeBps: 1.5, feeMinMicroSar: null }],
      ["nothing set", { feeBps: null, feeMinMicroSar: null }],
      [
        "an expiry in the past",
        {
          feeBps: 100,
          feeMinMicroSar: null,
          expiresAt: "2020-01-01T00:00:00Z",
        },
      ],
      [
        "a note too long",
        { feeBps: 100, feeMinMicroSar: null, note: "x".repeat(501) },
      ],
    ])("refuses %s with a 422 and records nothing", async (_, values) => {
      const organizationId = await createOrganization();
      const response = await request(
        "admin",
        "PUT",
        `/${organizationId}/top-up-fee`,
        {
          expiresAt: null,
          note: "",
          ...values,
        },
      );
      expect(response.status).toBe(422);
      expect(await changes(organizationId)).toHaveLength(0);
      expect(audited).toEqual([]);
    });
  });
});
