import { afterAll, beforeAll, describe, expect, it } from "vitest";

import type { TestDatabase } from "@workspace/db/testing";
import type postgres from "postgres";

// Needs real Postgres: the bounds, the single row, the append-only history and
// the gateway role's grants are enforced by the database. Skips without
// DATABASE_URL.
const adminUrl = process.env.DATABASE_URL;

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

  it("starts at no markup and a 5.5% top-up fee", async () => {
    const { settings, providers } = await pricing.getPricing();
    expect(settings).toMatchObject({ defaultMarkupBps: 0, topupFeeBps: 550 });
    expect(providers).toEqual([]);
  });

  it("records who changed the settings, and from what to what", async () => {
    const before = await pricing.getPricingSettings();
    const after = await pricing.updatePricingSettings({
      defaultMarkupBps: 300,
      topupFeeBps: 500,
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
      },
      after: { defaultMarkupBps: 300, topupFeeBps: 500 },
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
        staffId: "staff_1",
      }),
    ).rejects.toMatchObject({
      cause: { constraint_name: "pricing_settings_default_markup_range" },
    });
    await expect(
      pricing.updatePricingSettings({
        defaultMarkupBps: 0,
        topupFeeBps: 10_001,
        staffId: "staff_1",
      }),
    ).rejects.toMatchObject({
      cause: { constraint_name: "pricing_settings_topup_fee_range" },
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
    });
  });

  it("lists the latest changes first, with who made them", async () => {
    await sql`insert into "user" (id, name, email, email_verified, created_at, updated_at)
      values ('staff_named', 'Sara Admin', 'sara@example.com', true, now(), now())`;
    await pricing.updatePricingSettings({
      defaultMarkupBps: 42,
      topupFeeBps: 550,
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
      [{ defaultMarkupBps: 0, topupFeeBps: 10_000 }, true],
      [{ defaultMarkupBps: 0, topupFeeBps: 10_001 }, false],
      [{ defaultMarkupBps: 0 }, false],
    ])("settings %j", async (input, isValid) => {
      const { updatePricingSettingsSchema } = await import("../../../schema");
      expect(updatePricingSettingsSchema.safeParse(input).success).toBe(
        isValid,
      );
    });
  });
});
