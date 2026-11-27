import { afterAll, beforeAll, describe, expect, it, vi } from "vitest";

import type { User } from "@workspace/auth";
import type { TestDatabase } from "@workspace/db/testing";
import type postgres from "postgres";

// Needs real Postgres: the bounds, the single row, the append-only history and
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

/**
 * What the migration seeds besides the fee: top-ups of SAR 20 to SAR 3,750,
 * quick picks 40/100/200/400, and the peg (3.75) with no bank spread.
 */
const LIMITS = {
  topupMinMicroSar: 20 * SAR,
  topupMaxMicroSar: 3750 * SAR,
  topupPresetsMicroSar: [40 * SAR, 100 * SAR, 200 * SAR, 400 * SAR],
  usdToSarRateE6: 3_750_000,
  fxSpreadBps: 0,
};

describe.skipIf(!adminUrl)("admin pricing", () => {
  let database: TestDatabase;
  let sql: postgres.Sql;
  let pricing: typeof import("../pricing");

  const changes = async (target?: string) =>
    sql<
      {
        id: number;
        target: string;
        before: Record<string, number> | null;
        after: Record<string, number> | null;
        changed_by: string;
      }[]
    >`select id, target, before, after, changed_by from pricing_change
      where ${target ? sql`target = ${target}` : sql`true`} order by id`;

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;

    sql = (await import("postgres")).default(database.url, { max: 4 });
    pricing = await import("../pricing");
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  it("starts at no markup, a 5.5% top-up fee (SAR 3 minimum), top-ups of SAR 20 to SAR 3,750 and the peg", async () => {
    const { settings, providers } = await pricing.getPricing();
    expect(settings).toMatchObject({
      defaultMarkupBps: 0,
      topupFeeBps: 550,
      topupFeeMinMicroSar: 3_000_000,
      ...LIMITS,
      effectiveUsdToSarRateE6: 3_750_000,
    });
    expect(providers).toEqual([]);
  });

  it("records who changed the settings, and from what to what", async () => {
    const before = await pricing.getPricingSettings();
    const { settings: after } = await pricing.updatePricingSettings({
      defaultMarkupBps: 300,
      topupFeeBps: 500,
      topupFeeMinMicroSar: 1_000_000,
      ...LIMITS,
      staffId: "staff_1",
    });
    expect(after).toMatchObject({
      defaultMarkupBps: 300,
      topupFeeBps: 500,
      updatedBy: "staff_1",
    });

    const [last] = (await changes("settings")).slice(-1);
    expect(last).toMatchObject({
      before: {
        defaultMarkupBps: before.defaultMarkupBps,
        topupFeeBps: before.topupFeeBps,
        topupFeeMinMicroSar: before.topupFeeMinMicroSar,
        topupMinMicroSar: before.topupMinMicroSar,
        topupMaxMicroSar: before.topupMaxMicroSar,
        topupPresetsMicroSar: before.topupPresetsMicroSar,
      },
      after: {
        defaultMarkupBps: 300,
        topupFeeBps: 500,
        topupFeeMinMicroSar: 1_000_000,
        ...LIMITS,
      },
      changed_by: "staff_1",
    });
  });

  it("refuses values outside the bounds and records nothing", async () => {
    const count = (await changes()).length;
    const current = await pricing.getPricingSettings();

    await expect(
      pricing.updatePricingSettings({
        defaultMarkupBps: -1,
        topupFeeBps: 0,
        topupFeeMinMicroSar: 3_000_000,
        ...LIMITS,
        staffId: "staff_1",
      }),
    ).rejects.toMatchObject({
      cause: { constraint_name: "pricing_settings_default_markup_range" },
    });
    await expect(
      pricing.updatePricingSettings({
        defaultMarkupBps: 0,
        topupFeeBps: 10_001,
        topupFeeMinMicroSar: 3_000_000,
        ...LIMITS,
        staffId: "staff_1",
      }),
    ).rejects.toMatchObject({
      cause: { constraint_name: "pricing_settings_topup_fee_range" },
    });
    await expect(
      pricing.updatePricingSettings({
        defaultMarkupBps: 0,
        topupFeeBps: 0,
        topupFeeMinMicroSar: 400_000_001,
        ...LIMITS,
        staffId: "staff_1",
      }),
    ).rejects.toMatchObject({
      cause: { constraint_name: "pricing_settings_topup_fee_min_range" },
    });
    await expect(
      pricing.setProviderMarkup({
        provider: "openai",
        markupBps: 100_001,
        staffId: "staff_1",
      }),
    ).rejects.toMatchObject({
      cause: { constraint_name: "provider_markup_range" },
    });

    expect(await pricing.getPricingSettings()).toEqual(current);
    expect(await changes()).toHaveLength(count);
  });

  it("sets, changes and removes a provider's markup, recording each step", async () => {
    await pricing.setProviderMarkup({
      provider: "anthropic",
      markupBps: 1000,
      staffId: "staff_1",
    });
    await pricing.setProviderMarkup({
      provider: "anthropic",
      markupBps: 750,
      staffId: "staff_2",
    });

    const { providers } = await pricing.getPricing();
    expect(providers).toHaveLength(1);
    expect(providers[0]).toMatchObject({
      provider: "anthropic",
      markupBps: 750,
      updatedBy: "staff_2",
    });

    expect(
      await pricing.removeProviderMarkup({
        provider: "anthropic",
        staffId: "staff_3",
      }),
    ).toBe(true);
    expect(
      await pricing.removeProviderMarkup({
        provider: "anthropic",
        staffId: "staff_3",
      }),
    ).toBe(false);
    expect((await pricing.getPricing()).providers).toEqual([]);

    expect(
      (await changes("provider:anthropic")).map(
        ({ before, after, changed_by }) => ({ before, after, changed_by }),
      ),
    ).toEqual([
      { before: null, after: { markupBps: 1000 }, changed_by: "staff_1" },
      {
        before: { markupBps: 1000 },
        after: { markupBps: 750 },
        changed_by: "staff_2",
      },
      { before: { markupBps: 750 }, after: null, changed_by: "staff_3" },
    ]);
  });

  it("keeps the history in order when staff save at the same time", async () => {
    await Promise.all(
      Array.from({ length: 12 }, (_, i) =>
        pricing.updatePricingSettings({
          defaultMarkupBps: 1000 + i,
          topupFeeBps: 100 + i,
          topupFeeMinMicroSar: 3_000_000,
          ...LIMITS,
          staffId: `staff_${i}`,
        }),
      ),
    );

    // each change starts from the values the previous one left
    const history = await changes("settings");
    for (let i = 1; i < history.length; i++) {
      expect(history[i]?.before).toEqual(history[i - 1]?.after);
    }
    const { settings } = await pricing.getPricing();
    expect(history.at(-1)?.after).toEqual({
      defaultMarkupBps: settings.defaultMarkupBps,
      topupFeeBps: settings.topupFeeBps,
      topupFeeMinMicroSar: settings.topupFeeMinMicroSar,
      topupMinMicroSar: settings.topupMinMicroSar,
      topupMaxMicroSar: settings.topupMaxMicroSar,
      topupPresetsMicroSar: settings.topupPresetsMicroSar,
      usdToSarRateE6: settings.usdToSarRateE6,
      fxSpreadBps: settings.fxSpreadBps,
      effectiveUsdToSarRateE6: settings.effectiveUsdToSarRateE6,
    });
  });

  it("lists the latest changes first, with who made them", async () => {
    await sql`insert into "user" (id, name, email, email_verified, created_at, updated_at)
      values ('staff_named', 'Sara Admin', 'sara@example.com', true, now(), now())`;
    await pricing.updatePricingSettings({
      defaultMarkupBps: 42,
      topupFeeBps: 550,
      topupFeeMinMicroSar: 3_000_000,
      ...LIMITS,
      staffId: "staff_named",
    });
    const [latest, previous] = await pricing.getPricingChanges(2);
    expect(latest).toMatchObject({
      target: "settings",
      after: { defaultMarkupBps: 42, topupFeeBps: 550 },
      changedBy: "staff_named",
      changedByName: "Sara Admin",
    });
    expect(previous?.id).toBeLessThan(latest?.id ?? 0);
  });

  it("keeps the history append-only", async () => {
    await expect(
      sql`update pricing_change set changed_by = 'x'`,
    ).rejects.toThrow(/append-only/);
    await expect(sql`delete from pricing_change`).rejects.toThrow(
      /append-only/,
    );
  });

  it("lets the gateway read the prices and nothing more", async () => {
    await sql.begin(async (tx) => {
      await tx`set local role gateway_ledger`;
      const rows = await tx`select default_markup_bps from pricing_settings`;
      expect(rows).toHaveLength(1);
      await tx`select provider, markup_bps from provider_markup`;
    });
    for (const statement of [
      "update pricing_settings set default_markup_bps = 0",
      "insert into provider_markup (provider, markup_bps) values ('x', 0)",
      "select * from pricing_change",
    ]) {
      await expect(
        sql.begin(async (tx) => {
          await tx`set local role gateway_ledger`;
          await tx.unsafe(statement);
        }),
      ).rejects.toThrow(/permission denied/);
    }
  });

  describe("top-up limits (ADMIN-05)", () => {
    const fees = {
      defaultMarkupBps: 0,
      topupFeeBps: 550,
      topupFeeMinMicroSar: 3_000_000,
      usdToSarRateE6: 3_750_000,
      fxSpreadBps: 0,
    };

    it("records the limits and quick picks before and after, picks once and smallest first", async () => {
      const before = await pricing.getPricingSettings();
      const { settings: after } = await pricing.updatePricingSettings({
        ...fees,
        topupMinMicroSar: 20 * SAR,
        topupMaxMicroSar: 5000 * SAR,
        topupPresetsMicroSar: [500 * SAR, 20 * SAR, 100 * SAR, 20 * SAR],
        staffId: "staff_limits",
      });
      expect(after).toMatchObject({
        topupMinMicroSar: 20 * SAR,
        topupMaxMicroSar: 5000 * SAR,
        topupPresetsMicroSar: [20 * SAR, 100 * SAR, 500 * SAR],
      });

      const [last] = (await changes("settings")).slice(-1);
      expect(last).toMatchObject({
        before: {
          topupMinMicroSar: before.topupMinMicroSar,
          topupMaxMicroSar: before.topupMaxMicroSar,
          topupPresetsMicroSar: before.topupPresetsMicroSar,
        },
        after: {
          topupMinMicroSar: 20 * SAR,
          topupMaxMicroSar: 5000 * SAR,
          topupPresetsMicroSar: [20 * SAR, 100 * SAR, 500 * SAR],
        },
        changed_by: "staff_limits",
      });

      await pricing.updatePricingSettings({
        ...fees,
        ...LIMITS,
        staffId: "staff_limits",
      });
    });

    it.each([
      ["a minimum of zero", { topupMinMicroSar: 0 }, "limits"],
      [
        "a maximum under the minimum",
        { topupMinMicroSar: 100 * SAR, topupMaxMicroSar: 99 * SAR },
        "limits",
      ],
      [
        "a maximum over the SAR 4,000,000 ceiling",
        { topupMaxMicroSar: 4_000_000 * SAR + 1 },
        "limits",
      ],
      ["no quick picks", { topupPresetsMicroSar: [] }, "presets"],
      [
        "nine quick picks",
        {
          topupPresetsMicroSar: Array.from(
            { length: 9 },
            (_, i) => (20 + i) * SAR,
          ),
        },
        "presets",
      ],
      [
        "a quick pick under the minimum",
        { topupPresetsMicroSar: [19 * SAR, 40 * SAR] },
        "presets",
      ],
      [
        "a quick pick over the maximum",
        { topupPresetsMicroSar: [40 * SAR, 3751 * SAR] },
        "presets",
      ],
    ])(
      "the database refuses %s and records nothing",
      async (_, change, check) => {
        const count = (await changes()).length;
        const current = await pricing.getPricingSettings();

        await expect(
          pricing.updatePricingSettings({
            ...fees,
            ...LIMITS,
            ...change,
            staffId: "staff_1",
          }),
        ).rejects.toMatchObject({
          cause: { constraint_name: `pricing_settings_topup_${check}_range` },
        });

        expect(await pricing.getPricingSettings()).toEqual(current);
        expect(await changes()).toHaveLength(count);
      },
    );

    it("answers the admin API with the rule a cross-field check broke", async () => {
      const { Hono } = await import("hono");
      const { HttpException } = await import("@workspace/shared/utils");
      const { pricingRouter } = await import("../router");
      const staff: User = {
        id: "staff_http",
        name: "Staff",
        email: "staff@example.com",
        emailVerified: true,
        banned: false,
        isAnonymous: false,
        twoFactorEnabled: false,
        createdAt: new Date(),
        updatedAt: new Date(),
      };
      // enforceAdmin puts the signed-in staff user here in the real app
      const app = new Hono<{ Variables: { user: User } }>()
        .use(async (c, next) => {
          c.set("user", staff);
          await next();
        })
        .route("/", pricingRouter)
        .onError((error) =>
          Response.json(
            { message: error.message },
            { status: error instanceof HttpException ? error.status : 500 },
          ),
        );
      const put = (body: Record<string, unknown>) =>
        app.request("/settings", {
          method: "PUT",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ ...fees, ...LIMITS, ...body }),
        });

      const maxBelowMin = await put({
        topupMinMicroSar: 50 * SAR,
        topupMaxMicroSar: 40 * SAR,
        topupPresetsMicroSar: [50 * SAR],
      });
      expect(maxBelowMin.status).toBe(422);
      expect(await maxBelowMin.json()).toEqual({
        message: "The top-up maximum must be at least the minimum",
      });

      const presetOutside = await put({ topupPresetsMicroSar: [2 * SAR] });
      expect(presetOutside.status).toBe(422);
      expect(await presetOutside.json()).toEqual({
        message: "Each quick-pick amount must be within the top-up limits",
      });

      const valid = await put({ topupMaxMicroSar: 2000 * SAR });
      expect(valid.status).toBe(200);
      expect(await valid.json()).toMatchObject({
        topupMaxMicroSar: 2000 * SAR,
        updatedBy: "staff_http",
      });
      await put({});
    });
  });

  describe("USD to SAR rate and bank spread (ADMIN-06)", () => {
    const others = {
      defaultMarkupBps: 0,
      topupFeeBps: 550,
      topupFeeMinMicroSar: 3_000_000,
      topupMinMicroSar: 20 * SAR,
      topupMaxMicroSar: 3750 * SAR,
      topupPresetsMicroSar: [40 * SAR, 100 * SAR, 200 * SAR, 400 * SAR],
    };

    it("records the rate, the spread and the effective rate before and after", async () => {
      await pricing.updatePricingSettings({
        ...others,
        usdToSarRateE6: 3_750_000,
        fxSpreadBps: 0,
        staffId: "staff_fx",
      });
      const { settings } = await pricing.updatePricingSettings({
        ...others,
        usdToSarRateE6: 3_760_000,
        fxSpreadBps: 250,
        staffId: "staff_fx",
      });
      // 3.76 + 2.5% = 3.854, worked out from the two stored values
      expect(settings).toMatchObject({
        usdToSarRateE6: 3_760_000,
        fxSpreadBps: 250,
        effectiveUsdToSarRateE6: 3_854_000,
      });
      expect(await pricing.getPricingSettings()).toMatchObject({
        effectiveUsdToSarRateE6: 3_854_000,
      });

      const [last] = (await changes("settings")).slice(-1);
      expect(last).toMatchObject({
        before: {
          usdToSarRateE6: 3_750_000,
          fxSpreadBps: 0,
          effectiveUsdToSarRateE6: 3_750_000,
        },
        after: {
          usdToSarRateE6: 3_760_000,
          fxSpreadBps: 250,
          effectiveUsdToSarRateE6: 3_854_000,
        },
        changed_by: "staff_fx",
      });

      await pricing.updatePricingSettings({
        ...others,
        ...LIMITS,
        staffId: "staff_fx",
      });
    });

    it.each([
      ["a rate under 3", { usdToSarRateE6: 2_999_999 }, "usd_to_sar_rate"],
      ["a rate over 5", { usdToSarRateE6: 5_000_001 }, "usd_to_sar_rate"],
      [
        "a rate typed ten times too big",
        { usdToSarRateE6: 37_500_000 },
        "usd_to_sar_rate",
      ],
      ["a spread over 10%", { fxSpreadBps: 1001 }, "fx_spread"],
      ["a negative spread", { fxSpreadBps: -1 }, "fx_spread"],
    ])(
      "the database refuses %s and records nothing",
      async (_, change, check) => {
        const count = (await changes()).length;
        const current = await pricing.getPricingSettings();

        await expect(
          pricing.updatePricingSettings({
            ...others,
            ...LIMITS,
            ...change,
            staffId: "staff_1",
          }),
        ).rejects.toMatchObject({
          cause: { constraint_name: `pricing_settings_${check}_range` },
        });

        expect(await pricing.getPricingSettings()).toEqual(current);
        expect(await changes()).toHaveLength(count);
      },
    );

    it.each([
      [3_000_000, true],
      [5_000_000, true],
      [2_999_999, false],
      [5_000_001, false],
      [3.75, false],
    ])("the admin API takes a rate of %d: %s", async (rate, isValid) => {
      const { updatePricingSettingsSchema } = await import("../../../schema");
      expect(
        updatePricingSettingsSchema.safeParse({
          ...others,
          ...LIMITS,
          usdToSarRateE6: rate,
        }).success,
      ).toBe(isValid);
    });

    it.each([
      [0, true],
      [1000, true],
      [1001, false],
      [-1, false],
    ])(
      "the admin API takes a spread of %d bps: %s",
      async (spread, isValid) => {
        const { updatePricingSettingsSchema } = await import("../../../schema");
        expect(
          updatePricingSettingsSchema.safeParse({
            ...others,
            ...LIMITS,
            fxSpreadBps: spread,
          }).success,
        ).toBe(isValid);
      },
    );

    it("writes the values before and after to the admin audit log", async () => {
      const { Hono } = await import("hono");
      const { enforceStaffAccess } =
        await import("../../admin/access-middleware");
      const { pricingRouter } = await import("../router");
      const staff: User = {
        id: "staff_audit",
        name: "Staff",
        email: "audit@example.com",
        emailVerified: true,
        banned: false,
        isAnonymous: false,
        twoFactorEnabled: false,
        role: "admin",
        createdAt: new Date(),
        updatedAt: new Date(),
      };
      const app = new Hono<{ Variables: { user: User } }>()
        .use(async (c, next) => {
          c.set("user", staff);
          await next();
        })
        .use("/api/admin/*", enforceStaffAccess)
        .route("/api/admin/pricing", pricingRouter);

      audited.length = 0;
      const response = await app.request("/api/admin/pricing/settings", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          ...others,
          ...LIMITS,
          usdToSarRateE6: 3_800_000,
          fxSpreadBps: 100,
        }),
      });
      expect(response.status).toBe(200);
      expect(audited).toHaveLength(1);
      expect(audited[0]).toMatchObject({
        actorId: "staff_audit",
        action: "pricing.update",
        details: {
          before: { usdToSarRateE6: 3_750_000, fxSpreadBps: 0 },
          after: {
            usdToSarRateE6: 3_800_000,
            fxSpreadBps: 100,
            effectiveUsdToSarRateE6: 3_838_000,
          },
        },
      });

      await pricing.updatePricingSettings({
        ...others,
        ...LIMITS,
        staffId: "staff_audit",
      });
    });
  });

  describe("input the admin API accepts", () => {
    it.each([
      ["a provider name in the gateway's format", "openai", true],
      ["a provider with a dot or dash", "azure-openai.eu", true],
      ["upper case", "OpenAI", false],
      ["a path", "../openai", false],
      ["empty", "", false],
    ])("provider: %s", async (_, provider, isValid) => {
      const { pricingProviderSchema } = await import("../../../schema");
      expect(pricingProviderSchema.safeParse(provider).success).toBe(isValid);
    });

    it.each([
      [0, true],
      [100_000, true],
      [-5, false],
      [1.5, false],
      [100_001, false],
    ])("markup %s bps", async (markupBps, isValid) => {
      const { setProviderMarkupSchema } = await import("../../../schema");
      expect(setProviderMarkupSchema.safeParse({ markupBps }).success).toBe(
        isValid,
      );
    });

    it.each([
      [
        {
          defaultMarkupBps: 0,
          topupFeeBps: 10_000,
          topupFeeMinMicroSar: 0,
          ...LIMITS,
        },
        true,
      ],
      [
        {
          defaultMarkupBps: 0,
          topupFeeBps: 10_001,
          topupFeeMinMicroSar: 0,
          ...LIMITS,
        },
        false,
      ],
      [
        {
          defaultMarkupBps: 0,
          topupFeeBps: 550,
          topupFeeMinMicroSar: -1,
          ...LIMITS,
        },
        false,
      ],
      [
        {
          defaultMarkupBps: 0,
          topupFeeBps: 550,
          topupFeeMinMicroSar: 0.5,
          ...LIMITS,
        },
        false,
      ],
      [{ defaultMarkupBps: 0, topupFeeBps: 550 }, false],
      [
        { defaultMarkupBps: 0, topupFeeBps: 550, topupFeeMinMicroSar: 0 },
        false,
      ],
    ])("settings %j", async (input, isValid) => {
      const { updatePricingSettingsSchema } = await import("../../../schema");
      expect(updatePricingSettingsSchema.safeParse(input).success).toBe(
        isValid,
      );
    });

    it.each([
      ["the seeded limits", {}, true],
      [
        "a maximum equal to the minimum",
        { topupMaxMicroSar: 20 * SAR, topupPresetsMicroSar: [20 * SAR] },
        true,
      ],
      [
        "eight quick picks",
        {
          topupPresetsMicroSar: Array.from(
            { length: 8 },
            (_, i) => (20 + i) * SAR,
          ),
        },
        true,
      ],
      [
        "a maximum under the minimum",
        { topupMaxMicroSar: 19 * SAR, topupPresetsMicroSar: [19 * SAR] },
        false,
      ],
      ["a minimum of zero", { topupMinMicroSar: 0 }, false],
      [
        "a maximum over the ceiling",
        { topupMaxMicroSar: 4_000_000 * SAR + 10_000 },
        false,
      ],
      [
        "an amount that is not whole halalas",
        { topupMinMicroSar: 20 * SAR + 1 },
        false,
      ],
      ["no quick picks", { topupPresetsMicroSar: [] }, false],
      [
        "nine quick picks",
        {
          topupPresetsMicroSar: Array.from(
            { length: 9 },
            (_, i) => (20 + i) * SAR,
          ),
        },
        false,
      ],
      [
        "a quick pick under the minimum",
        { topupPresetsMicroSar: [1 * SAR] },
        false,
      ],
      [
        "a quick pick over the maximum",
        { topupPresetsMicroSar: [4000 * SAR] },
        false,
      ],
    ])("top-up limits: %s", async (_, change, isValid) => {
      const { updatePricingSettingsSchema } = await import("../../../schema");
      expect(
        updatePricingSettingsSchema.safeParse({
          defaultMarkupBps: 0,
          topupFeeBps: 550,
          topupFeeMinMicroSar: 3_000_000,
          ...LIMITS,
          ...change,
        }).success,
      ).toBe(isValid);
    });

    it("names the field and the rule when the limits do not fit together", async () => {
      const { updatePricingSettingsSchema } = await import("../../../schema");
      const issues = (change: Record<string, unknown>) =>
        updatePricingSettingsSchema
          .safeParse({
            defaultMarkupBps: 0,
            topupFeeBps: 550,
            topupFeeMinMicroSar: 3_000_000,
            ...LIMITS,
            ...change,
          })
          .error?.issues.map(({ path, message }) => ({ path, message }));

      expect(
        issues({
          topupMinMicroSar: 50 * SAR,
          topupMaxMicroSar: 40 * SAR,
          topupPresetsMicroSar: [45 * SAR],
        }),
      ).toContainEqual({
        path: ["topupMaxMicroSar"],
        message: "The top-up maximum must be at least the minimum",
      });
      expect(issues({ topupPresetsMicroSar: [40 * SAR, 3751 * SAR] })).toEqual([
        {
          path: ["topupPresetsMicroSar", 1],
          message: "Each quick-pick amount must be within the top-up limits",
        },
      ]);
    });
  });
});
