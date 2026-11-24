import { afterAll, beforeAll, describe, expect, it } from "vitest";

import type { TestDatabase } from "@workspace/db/testing";
import type {
  GatewayClient,
  GatewayProviderKey,
} from "@workspace/gateway/server";
import type postgres from "postgres";

// Needs real Postgres: the table and its checks. Skips without DATABASE_URL.
const adminUrl = process.env.DATABASE_URL;

/** A gateway that holds the given provider keys. */
const fakeGateway = (keys: GatewayProviderKey[], isDown = false) => {
  const client: GatewayClient = {
    createCustomer: async () => ({ id: "cust" }),
    deleteCustomer: async () => {},
    createVirtualKey: async () => ({ id: "vk", value: "v" }),
    updateVirtualKey: async () => {},
    deleteVirtualKey: async () => {},
    listModels: async () => [],
    getCustomerCosts: async () => [],
    getLog: async () => undefined,
    setCustomerRateLimit: async () => {},
    setVirtualKeyAccess: async () => {},
    listProviderKeys: async () => {
      if (isDown) {
        // loaded here: a static import would open the database before the test points it at its own
        const { GatewayError } = await import("@workspace/gateway/server");
        throw new GatewayError("gateway down");
      }
      return keys;
    },
  };
  return client;
};

const KEYS: GatewayProviderKey[] = [
  { id: "k_openai", name: "main", provider: "openai", isEnabled: true },
  { id: "k_local", name: "riyadh", provider: "humain", isEnabled: true },
  { id: "k_azure", name: "dammam", provider: "azure", isEnabled: false },
];

const TERMS = {
  region: "sa",
  retentionDays: 0,
  trainsOnData: false,
  agreement: "direct",
  source: "contract 2026-11",
  verifiedOn: "2026-11-24",
} as const;

describe.skipIf(!adminUrl)("provider catalog", () => {
  let database: TestDatabase;
  let sql: postgres.Sql;
  let providers: typeof import("../providers");

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;
    sql = (await import("postgres")).default(database.url, { max: 4 });
    providers = await import("../providers");
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  it("lists every gateway key, and a key with no terms guarantees nothing", async () => {
    const catalog = await providers.getProviderCatalog(fakeGateway(KEYS));
    expect(catalog.map((entry) => entry.keyId)).toEqual([
      "k_azure",
      "k_local",
      "k_openai",
    ]);
    for (const entry of catalog) {
      expect(entry).toMatchObject({
        isInGateway: true,
        terms: null,
        isZeroRetention: false,
        isNeverTrainedOn: false,
      });
    }
    expect(catalog[0]).toMatchObject({ name: "dammam", isEnabled: false });
  });

  it("records terms for a gateway key with who set them, and replaces them on the next save", async () => {
    const gateway = fakeGateway(KEYS);
    const saved = await providers.setProviderTerms(
      { keyId: "k_local", staffId: "staff_1", ...TERMS },
      gateway,
    );
    expect(saved).toMatchObject({ ...TERMS, updatedBy: "staff_1" });

    const entry = (await providers.getProviderCatalog(gateway)).find(
      (e) => e.keyId === "k_local",
    );
    expect(entry).toMatchObject({
      provider: "humain",
      terms: { ...TERMS, updatedBy: "staff_1" },
      isZeroRetention: true,
      isNeverTrainedOn: true,
    });

    await providers.setProviderTerms(
      {
        keyId: "k_local",
        staffId: "staff_2",
        ...TERMS,
        retentionDays: 30,
      },
      gateway,
    );
    const rows = await sql<
      { retention_days: number; updated_by: string }[]
    >`select retention_days, updated_by from provider_endpoint where key_id = 'k_local'`;
    expect(rows).toEqual([{ retention_days: 30, updated_by: "staff_2" }]);
  });

  it.each([
    ["retention unknown", { retentionDays: null }, false, true],
    ["kept for a day", { retentionDays: 1 }, false, true],
    ["training unknown", { trainsOnData: null }, true, false],
    ["trains on data", { trainsOnData: true }, true, false],
  ])(
    "treats %s as the worst case",
    async (_name, change, isZeroRetention, isNeverTrainedOn) => {
      const gateway = fakeGateway(KEYS);
      await providers.setProviderTerms(
        { keyId: "k_openai", staffId: "staff_1", ...TERMS, ...change },
        gateway,
      );
      const entry = (await providers.getProviderCatalog(gateway)).find(
        (e) => e.keyId === "k_openai",
      );
      expect(entry).toMatchObject({ isZeroRetention, isNeverTrainedOn });
    },
  );

  it("refuses terms for a key the gateway does not have", async () => {
    await expect(
      providers.setProviderTerms(
        { keyId: "k_typo", staffId: "staff_1", ...TERMS },
        fakeGateway(KEYS),
      ),
    ).rejects.toMatchObject({ status: 404 });
    const rows =
      await sql`select 1 from provider_endpoint where key_id = 'k_typo'`;
    expect(rows).toHaveLength(0);
  });

  it("still shows terms whose key has left the gateway, marked as gone", async () => {
    const gateway = fakeGateway(KEYS);
    await providers.setProviderTerms(
      { keyId: "k_azure", staffId: "staff_1", ...TERMS, region: "eu" },
      gateway,
    );
    const without = fakeGateway(KEYS.filter((key) => key.id !== "k_azure"));
    const entry = (await providers.getProviderCatalog(without)).find(
      (e) => e.keyId === "k_azure",
    );
    expect(entry).toMatchObject({
      provider: "azure",
      isInGateway: false,
      isEnabled: false,
      terms: { region: "eu" },
    });
  });

  it("removes terms, back to unknown", async () => {
    const gateway = fakeGateway(KEYS);
    await providers.setProviderTerms(
      { keyId: "k_openai", staffId: "staff_1", ...TERMS },
      gateway,
    );
    expect(await providers.removeProviderTerms("k_openai", "staff_2")).toBe(
      true,
    );
    expect(await providers.removeProviderTerms("k_openai", "staff_2")).toBe(
      false,
    );
    const entry = (await providers.getProviderCatalog(gateway)).find(
      (e) => e.keyId === "k_openai",
    );
    expect(entry).toMatchObject({ terms: null, isZeroRetention: false });
  });

  it("keeps every change of a key's terms, and their removal, with who made it", async () => {
    const gateway = fakeGateway([
      { id: "k_hist", name: "history", provider: "humain", isEnabled: true },
    ]);
    await providers.setProviderTerms(
      { keyId: "k_hist", staffId: "staff_1", ...TERMS },
      gateway,
    );
    await providers.setProviderTerms(
      { keyId: "k_hist", staffId: "staff_2", ...TERMS, retentionDays: 30 },
      gateway,
    );
    await providers.removeProviderTerms("k_hist", "staff_3");
    // removing what is already gone writes nothing
    await providers.removeProviderTerms("k_hist", "staff_3");

    const history = await sql<
      {
        retention_days: number | null;
        is_removed: boolean;
        changed_by: string;
        region: string | null;
      }[]
    >`select retention_days, is_removed, changed_by, region from provider_endpoint_change where key_id = 'k_hist' order by id`;
    expect(history).toEqual([
      {
        retention_days: 0,
        is_removed: false,
        changed_by: "staff_1",
        region: "sa",
      },
      {
        retention_days: 30,
        is_removed: false,
        changed_by: "staff_2",
        region: "sa",
      },
      {
        retention_days: null,
        is_removed: true,
        changed_by: "staff_3",
        region: null,
      },
    ]);
    await expect(
      sql`update provider_endpoint_change set retention_days = 0 where key_id = 'k_hist'`,
    ).rejects.toThrow("append-only");
    await expect(
      sql`delete from provider_endpoint_change where key_id = 'k_hist'`,
    ).rejects.toThrow("append-only");
  });

  it("the database refuses a removal that carries terms, and a change that carries none", async () => {
    await expect(
      sql`insert into provider_endpoint_change (key_id, provider, is_removed, region, agreement, source, verified_on, changed_by)
          values ('k_bad', 'p', true, 'sa', 'direct', 's', '2026-11-24', 'u')`,
    ).rejects.toThrow("provider_endpoint_change_removal_has_no_terms");
    await expect(
      sql`insert into provider_endpoint_change (key_id, provider, is_removed, changed_by)
          values ('k_bad', 'p', false, 'u')`,
    ).rejects.toThrow("provider_endpoint_change_removal_has_no_terms");
  });

  it("answers 503 when the gateway cannot be reached, and saves nothing", async () => {
    const down = fakeGateway(KEYS, true);
    await expect(providers.getProviderCatalog(down)).rejects.toMatchObject({
      status: 503,
    });
    await expect(
      providers.setProviderTerms(
        { keyId: "k_new", staffId: "staff_1", ...TERMS },
        down,
      ),
    ).rejects.toMatchObject({ status: 503 });
  });

  it("the database refuses a region or an agreement outside the lists, and negative retention", async () => {
    const insert = (region: string, agreement: string, retention: number) =>
      sql`insert into provider_endpoint (key_id, provider, region, retention_days, agreement, source, verified_on, updated_by)
          values (${`bad_${region}_${agreement}_${retention}`}, 'p', ${region}, ${retention}, ${agreement}, 's', '2026-11-24', 'u')`;
    await expect(insert("mars", "direct", 0)).rejects.toThrow(
      "provider_endpoint_region",
    );
    await expect(insert("sa", "handshake", 0)).rejects.toThrow(
      "provider_endpoint_agreement",
    );
    await expect(insert("sa", "direct", -1)).rejects.toThrow(
      "provider_endpoint_retention_not_negative",
    );
  });
});
