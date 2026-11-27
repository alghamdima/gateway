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
vi.mock("@workspace/auth/server", () => ({ auth: {} }));
vi.mock("@workspace/i18n", async () => {
  const z = await import("zod");
  return { getValidationLocale: () => z.locales.en };
});
vi.mock("@workspace/i18n/server", () => ({ getLocaleFromRequest: () => "en" }));

const USD = 1_000_000;

/** The top-up limits the migration seeds: $5 to $1,000, quick picks 10/25/50/100. */
const LIMITS = {
  topupMinMicroUsd: 5 * USD,
  topupMaxMicroUsd: 1000 * USD,
  topupPresetsMicroUsd: [10 * USD, 25 * USD, 50 * USD, 100 * USD],
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

  it("starts at no markup, a 5.5% top-up fee and top-ups of $5 to $1,000", async () => {
    const { settings, providers } = await pricing.getPricing();
    expect(settings).toMatchObject({
      defaultMarkupBps: 0,
      topupFeeBps: 550,
      topupFeeMinMicroUsd: 800_000,
      ...LIMITS,
    });
    expect(providers).toEqual([]);
  });

  it("records who changed the settings, and from what to what", async () => {
    const before = await pricing.getPricingSettings();
    const after = await pricing.updatePricingSettings({
      defaultMarkupBps: 300,
      topupFeeBps: 500,
      topupFeeMinMicroUsd: 1_000_000,
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
        topupFeeMinMicroUsd: before.topupFeeMinMicroUsd,
        topupMinMicroUsd: before.topupMinMicroUsd,
        topupMaxMicroUsd: before.topupMaxMicroUsd,
        topupPresetsMicroUsd: before.topupPresetsMicroUsd,
      },
      after: {
        defaultMarkupBps: 300,
        topupFeeBps: 500,
        topupFeeMinMicroUsd: 1_000_000,
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
        topupFeeMinMicroUsd: 800_000,
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
        topupFeeMinMicroUsd: 800_000,
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
        topupFeeMinMicroUsd: 100_000_001,
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
          topupFeeMinMicroUsd: 800_000,
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
      topupFeeMinMicroUsd: settings.topupFeeMinMicroUsd,
      topupMinMicroUsd: settings.topupMinMicroUsd,
      topupMaxMicroUsd: settings.topupMaxMicroUsd,
      topupPresetsMicroUsd: settings.topupPresetsMicroUsd,
    });
  });

  it("lists the latest changes first, with who made them", async () => {
    await sql`insert into "user" (id, name, email, email_verified, created_at, updated_at)
      values ('staff_named', 'Sara Admin', 'sara@example.com', true, now(), now())`;
    await pricing.updatePricingSettings({
      defaultMarkupBps: 42,
      topupFeeBps: 550,
      topupFeeMinMicroUsd: 800_000,
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
      topupFeeMinMicroUsd: 800_000,
    };

    it("records the limits and quick picks before and after, picks once and smallest first", async () => {
      const before = await pricing.getPricingSettings();
      const after = await pricing.updatePricingSettings({
        ...fees,
        topupMinMicroUsd: 20 * USD,
        topupMaxMicroUsd: 5000 * USD,
        topupPresetsMicroUsd: [500 * USD, 20 * USD, 100 * USD, 20 * USD],
        staffId: "staff_limits",
      });
      expect(after).toMatchObject({
        topupMinMicroUsd: 20 * USD,
        topupMaxMicroUsd: 5000 * USD,
        topupPresetsMicroUsd: [20 * USD, 100 * USD, 500 * USD],
      });

      const [last] = (await changes("settings")).slice(-1);
      expect(last).toMatchObject({
        before: {
          topupMinMicroUsd: before.topupMinMicroUsd,
          topupMaxMicroUsd: before.topupMaxMicroUsd,
          topupPresetsMicroUsd: before.topupPresetsMicroUsd,
        },
        after: {
          topupMinMicroUsd: 20 * USD,
          topupMaxMicroUsd: 5000 * USD,
          topupPresetsMicroUsd: [20 * USD, 100 * USD, 500 * USD],
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
      ["a minimum of zero", { topupMinMicroUsd: 0 }, "limits"],
      [
        "a maximum under the minimum",
        { topupMinMicroUsd: 100 * USD, topupMaxMicroUsd: 99 * USD },
        "limits",
      ],
      [
        "a maximum over the $1,000,000 ceiling",
        { topupMaxMicroUsd: 1_000_000 * USD + 1 },
        "limits",
      ],
      ["no quick picks", { topupPresetsMicroUsd: [] }, "presets"],
      [
        "nine quick picks",
        {
          topupPresetsMicroUsd: Array.from(
            { length: 9 },
            (_, i) => (10 + i) * USD,
          ),
        },
        "presets",
      ],
      [
        "a quick pick under the minimum",
        { topupPresetsMicroUsd: [4 * USD, 10 * USD] },
        "presets",
      ],
      [
        "a quick pick over the maximum",
        { topupPresetsMicroUsd: [10 * USD, 1001 * USD] },
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
        topupMinMicroUsd: 50 * USD,
        topupMaxMicroUsd: 40 * USD,
        topupPresetsMicroUsd: [50 * USD],
      });
      expect(maxBelowMin.status).toBe(422);
      expect(await maxBelowMin.json()).toEqual({
        message: "The top-up maximum must be at least the minimum",
      });

      const presetOutside = await put({ topupPresetsMicroUsd: [2 * USD] });
      expect(presetOutside.status).toBe(422);
      expect(await presetOutside.json()).toEqual({
        message: "Each quick-pick amount must be within the top-up limits",
      });

      const valid = await put({ topupMaxMicroUsd: 2000 * USD });
      expect(valid.status).toBe(200);
      expect(await valid.json()).toMatchObject({
        topupMaxMicroUsd: 2000 * USD,
        updatedBy: "staff_http",
      });
      await put({});
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
          topupFeeMinMicroUsd: 0,
          ...LIMITS,
        },
        true,
      ],
      [
        {
          defaultMarkupBps: 0,
          topupFeeBps: 10_001,
          topupFeeMinMicroUsd: 0,
          ...LIMITS,
        },
        false,
      ],
      [
        {
          defaultMarkupBps: 0,
          topupFeeBps: 550,
          topupFeeMinMicroUsd: -1,
          ...LIMITS,
        },
        false,
      ],
      [
        {
          defaultMarkupBps: 0,
          topupFeeBps: 550,
          topupFeeMinMicroUsd: 0.5,
          ...LIMITS,
        },
        false,
      ],
      [{ defaultMarkupBps: 0, topupFeeBps: 550 }, false],
      [
        { defaultMarkupBps: 0, topupFeeBps: 550, topupFeeMinMicroUsd: 0 },
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
        { topupMaxMicroUsd: 5 * USD, topupPresetsMicroUsd: [5 * USD] },
        true,
      ],
      [
        "eight quick picks",
        {
          topupPresetsMicroUsd: Array.from(
            { length: 8 },
            (_, i) => (10 + i) * USD,
          ),
        },
        true,
      ],
      [
        "a maximum under the minimum",
        { topupMaxMicroUsd: 4 * USD, topupPresetsMicroUsd: [4 * USD] },
        false,
      ],
      ["a minimum of zero", { topupMinMicroUsd: 0 }, false],
      [
        "a maximum over the ceiling",
        { topupMaxMicroUsd: 1_000_000 * USD + 10_000 },
        false,
      ],
      [
        "an amount that is not whole cents",
        { topupMinMicroUsd: 5 * USD + 1 },
        false,
      ],
      ["no quick picks", { topupPresetsMicroUsd: [] }, false],
      [
        "nine quick picks",
        {
          topupPresetsMicroUsd: Array.from(
            { length: 9 },
            (_, i) => (10 + i) * USD,
          ),
        },
        false,
      ],
      [
        "a quick pick under the minimum",
        { topupPresetsMicroUsd: [1 * USD] },
        false,
      ],
      [
        "a quick pick over the maximum",
        { topupPresetsMicroUsd: [2000 * USD] },
        false,
      ],
    ])("top-up limits: %s", async (_, change, isValid) => {
      const { updatePricingSettingsSchema } = await import("../../../schema");
      expect(
        updatePricingSettingsSchema.safeParse({
          defaultMarkupBps: 0,
          topupFeeBps: 550,
          topupFeeMinMicroUsd: 800_000,
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
            topupFeeMinMicroUsd: 800_000,
            ...LIMITS,
            ...change,
          })
          .error?.issues.map(({ path, message }) => ({ path, message }));

      expect(
        issues({
          topupMinMicroUsd: 50 * USD,
          topupMaxMicroUsd: 40 * USD,
          topupPresetsMicroUsd: [45 * USD],
        }),
      ).toContainEqual({
        path: ["topupMaxMicroUsd"],
        message: "The top-up maximum must be at least the minimum",
      });
      expect(issues({ topupPresetsMicroUsd: [10 * USD, 1001 * USD] })).toEqual([
        {
          path: ["topupPresetsMicroUsd", 1],
          message: "Each quick-pick amount must be within the top-up limits",
        },
      ]);
    });
  });
});
