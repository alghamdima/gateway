import { afterAll, beforeAll, describe, expect, it } from "vitest";

import type { TestDatabase } from "@workspace/db/testing";
import type {
  GatewayClient,
  GatewayProviderKey,
  VirtualKeyAccess,
} from "@workspace/gateway-client/server";
import type postgres from "postgres";

// Needs real Postgres: the policy row, its versions and the keys it is pushed
// to. Skips without DATABASE_URL.
const adminUrl = process.env.DATABASE_URL;

const KEYS: GatewayProviderKey[] = [
  { id: "k_riyadh", name: "riyadh", provider: "humain", isEnabled: true },
  { id: "k_virginia", name: "virginia", provider: "openai", isEnabled: true },
];

const down = async () => {
  // loaded here: a static import would open the database before the test points it at its own
  const { GatewayError } = await import("@workspace/gateway-client/server");
  return new GatewayError("gateway down");
};

/** A gateway that records what each virtual key was allowed, and can fail. */
const fakeGateway = () => {
  const state = {
    pushes: [] as [string, VirtualKeyAccess][],
    catalogReads: 0,
    isDown: false,
    failingKeys: new Set<string>(),
  };
  const client: GatewayClient = {
    createCustomer: async () => ({ id: "cust" }),
    deleteCustomer: async () => {},
    createVirtualKey: async () => ({ id: "vk", value: "v" }),
    updateVirtualKey: async () => {},
    deleteVirtualKey: async () => {},
    listModels: async () => [],
    getCustomerCosts: async () => [],
    getEndpointStats: async () => {
      throw new Error("not used");
    },
    getGenerationSummary: async () => {
      throw new Error("not used");
    },
    getAttempts: async () => [],
    getLog: async () => undefined,
    setCustomerRateLimit: async () => {},
    searchRequests: async () => ({ requests: [], total: 0 }),
    syncPrices: async () => {},
    listPricingOverrides: async () => [],
    createPricingOverride: async () => {
      throw new Error("not used");
    },
    updatePricingOverride: async () => {
      throw new Error("not used");
    },
    deletePricingOverride: async () => {},
    listDatasheetEntries: async () => [],
    listProviders: async () => [],
    createProvider: async () => {},
    deleteProvider: async () => {},
    createProviderKey: async () => ({
      id: "pk",
      name: "",
      maskedValue: "",
      models: [],
      weight: 1,
      isEnabled: true,
    }),
    updateProviderKey: async () => ({
      id: "pk",
      name: "",
      maskedValue: "",
      models: [],
      weight: 1,
      isEnabled: true,
    }),
    deleteProviderKey: async () => {},
    refreshProviderModels: async () => {},
    listProviderKeys: async () => {
      state.catalogReads++;
      if (state.isDown) {
        throw await down();
      }
      return KEYS;
    },
    setVirtualKeyAccess: async (id, access) => {
      if (state.isDown || state.failingKeys.has(id)) {
        throw await down();
      }
      state.pushes.push([id, access]);
    },
  };
  return { state, client };
};

const STRICT = {
  requireZeroRetention: true,
  forbidTraining: true,
  allowedRegions: ["sa" as const],
  allowedProviders: null,
};
const ONLY_RIYADH: VirtualKeyAccess = {
  allowAll: false,
  providers: [{ provider: "humain", keyIds: ["k_riyadh"] }],
};
const NOTHING: VirtualKeyAccess = { allowAll: false, providers: [] };

describe.skipIf(!adminUrl)("organization data policy", () => {
  let database: TestDatabase;
  let sql: postgres.Sql;
  let policy: typeof import("../policy");
  let providers: typeof import("../../admin/providers/providers");
  let seq = 0;

  const createOrganization = async (keys: number) => {
    const id = `org_dp_${++seq}`;
    await sql`insert into organization (id, name, slug, created_at) values (${id}, ${id}, ${id}, now())`;
    const virtualKeys: string[] = [];
    for (let i = 0; i < keys; i++) {
      const vk = `vk_${id}_${i}`;
      await sql`insert into api_key (id, organization_id, created_by, name, virtual_key_id, prefix, value_hash)
                values (${vk}, ${id}, 'user_1', ${vk}, ${vk}, 'sk-bf-', ${vk})`;
      virtualKeys.push(vk);
    }
    return { id, virtualKeys };
  };

  const stored = async (organizationId: string) => {
    const [row] = await sql<
      { version: number; applied_version: number }[]
    >`select version, applied_version from organization_data_policy where organization_id = ${organizationId}`;
    return row;
  };

  const riyadhTerms = (retentionDays: number) =>
    providers.setProviderTerms(
      {
        keyId: "k_riyadh",
        staffId: "staff_1",
        region: "sa",
        retentionDays,
        trainsOnData: false,
        agreement: "direct",
        source: "contract",
        verifiedOn: "2026-11-24",
      },
      fakeGateway().client,
    );

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;
    sql = (await import("postgres")).default(database.url, { max: 4 });
    policy = await import("../policy");
    providers = await import("../../admin/providers/providers");
    // riyadh: in the Kingdom, nothing kept, never trained on. virginia: nothing recorded.
    await riyadhTerms(0);
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  it("is open until someone sets it, and shows each endpoint without key names", async () => {
    const org = await createOrganization(1);
    const current = await policy.getDataPolicy(org.id, fakeGateway().client);
    expect(current).toMatchObject({
      requireZeroRetention: false,
      forbidTraining: false,
      allowedRegions: null,
      allowedProviders: null,
      version: 0,
      isApplied: true,
    });
    expect(current.endpoints).toEqual([
      {
        provider: "humain",
        region: "sa",
        isZeroRetention: true,
        isNeverTrainedOn: true,
        isAllowed: true,
      },
      {
        provider: "openai",
        region: null,
        isZeroRetention: false,
        isNeverTrainedOn: false,
        isAllowed: true,
      },
    ]);
    expect(JSON.stringify(current)).not.toMatch(/k_riyadh|virginia/);
  });

  it("pushes the allowlist to every key of the organization, and only to them", async () => {
    const org = await createOrganization(2);
    const other = await createOrganization(1);
    const gateway = fakeGateway();

    const saved = await policy.setDataPolicy(
      { organizationId: org.id, userId: "user_1", ...STRICT },
      gateway.client,
    );

    expect(gateway.state.pushes).toEqual([
      [org.virtualKeys[0], ONLY_RIYADH],
      [org.virtualKeys[1], ONLY_RIYADH],
    ]);
    expect(saved).toMatchObject({ ...STRICT, version: 1, isApplied: true });
    expect(saved.endpoints.map((e) => e.isAllowed)).toEqual([true, false]);
    expect(await stored(org.id)).toEqual({ version: 1, applied_version: 1 });
    expect(await stored(other.id)).toBeUndefined();
  });

  it("counts each change as a new version, and opening the policy reaches everything again", async () => {
    const org = await createOrganization(1);
    const gateway = fakeGateway();
    await policy.setDataPolicy(
      { organizationId: org.id, userId: "user_1", ...STRICT },
      gateway.client,
    );
    const reopened = await policy.setDataPolicy(
      {
        organizationId: org.id,
        userId: "user_2",
        requireZeroRetention: false,
        forbidTraining: false,
        allowedRegions: null,
        allowedProviders: null,
      },
      gateway.client,
    );
    expect(reopened).toMatchObject({ version: 2, isApplied: true });
    // each version is in the history, as it was set and by whom
    const history = await sql<
      {
        version: number;
        allowed_regions: string[] | null;
        changed_by: string;
      }[]
    >`select version, allowed_regions, changed_by from organization_data_policy_change where organization_id = ${org.id} order by id`;
    expect(history).toEqual([
      { version: 1, allowed_regions: ["sa"], changed_by: "user_1" },
      { version: 2, allowed_regions: null, changed_by: "user_2" },
    ]);
    expect(gateway.state.pushes.at(-1)).toEqual([
      org.virtualKeys[0],
      { allowAll: true },
    ]);
  });

  it("reaches nothing when no endpoint satisfies the policy", async () => {
    const org = await createOrganization(1);
    const gateway = fakeGateway();
    await policy.setDataPolicy(
      {
        organizationId: org.id,
        userId: "user_1",
        ...STRICT,
        allowedRegions: ["eu"],
      },
      gateway.client,
    );
    expect(gateway.state.pushes).toEqual([[org.virtualKeys[0], NOTHING]]);
  });

  it("gives a new key the organization's policy, reading the catalog only when the policy restricts", async () => {
    const open = await createOrganization(0);
    const strict = await createOrganization(0);
    const gateway = fakeGateway();
    await policy.setDataPolicy(
      { organizationId: strict.id, userId: "user_1", ...STRICT },
      gateway.client,
    );
    gateway.state.catalogReads = 0;

    expect(await policy.accessForOrganization(open.id, gateway.client)).toEqual(
      { allowAll: true },
    );
    expect(gateway.state.catalogReads).toBe(0);
    expect(
      await policy.accessForOrganization(strict.id, gateway.client),
    ).toEqual(ONLY_RIYADH);
  });

  it("records nothing when the gateway cannot be read", async () => {
    const org = await createOrganization(1);
    const gateway = fakeGateway();
    gateway.state.isDown = true;
    await expect(
      policy.setDataPolicy(
        { organizationId: org.id, userId: "user_1", ...STRICT },
        gateway.client,
      ),
    ).rejects.toMatchObject({ status: 503 });
    expect(await stored(org.id)).toBeUndefined();
  });

  it("keeps a policy marked as not applied when a key could not be updated, and finishes on the next run", async () => {
    const org = await createOrganization(2);
    const gateway = fakeGateway();
    gateway.state.failingKeys.add(org.virtualKeys[1] ?? "");

    await expect(
      policy.setDataPolicy(
        { organizationId: org.id, userId: "user_1", ...STRICT },
        gateway.client,
      ),
    ).rejects.toMatchObject({ status: 503 });
    expect(await stored(org.id)).toEqual({ version: 1, applied_version: 0 });
    expect(await policy.getDataPolicy(org.id, gateway.client)).toMatchObject({
      ...STRICT,
      isApplied: false,
    });

    gateway.state.failingKeys.clear();
    gateway.state.pushes = [];
    expect(await policy.applyPendingPolicies(gateway.client)).toEqual({
      applied: 1,
      failed: [],
    });
    expect(gateway.state.pushes).toEqual([
      [org.virtualKeys[0], ONLY_RIYADH],
      [org.virtualKeys[1], ONLY_RIYADH],
    ]);
    expect(await stored(org.id)).toEqual({ version: 1, applied_version: 1 });
    // nothing left to do
    expect(await policy.applyPendingPolicies(gateway.client)).toEqual({
      applied: 0,
      failed: [],
    });
  });

  it("drops an endpoint from strict organizations when its terms get worse, and one failing organization does not stop the others", async () => {
    const first = await createOrganization(1);
    const second = await createOrganization(1);
    const gateway = fakeGateway();
    for (const org of [first, second]) {
      await policy.setDataPolicy(
        { organizationId: org.id, userId: "user_1", ...STRICT },
        gateway.client,
      );
    }

    // staff learn the endpoint keeps data for 30 days
    await riyadhTerms(30);
    expect(await stored(first.id)).toEqual({ version: 1, applied_version: 0 });

    gateway.state.pushes = [];
    gateway.state.failingKeys.add(first.virtualKeys[0] ?? "");
    const result = await policy.applyPendingPolicies(gateway.client);
    expect(result.failed).toContain(first.id);
    expect(result.failed).not.toContain(second.id);
    expect(gateway.state.pushes).toContainEqual([
      second.virtualKeys[0],
      NOTHING,
    ]);
    expect(await stored(second.id)).toEqual({ version: 1, applied_version: 1 });
    expect(await stored(first.id)).toEqual({ version: 1, applied_version: 0 });

    gateway.state.failingKeys.clear();
    await policy.applyPendingPolicies(gateway.client);
    expect(gateway.state.pushes).toContainEqual([
      first.virtualKeys[0],
      NOTHING,
    ]);
    await riyadhTerms(0);
    await policy.applyPendingPolicies(gateway.client);
  });

  it("refuses a policy for an organization that does not exist", async () => {
    await expect(
      policy.setDataPolicy(
        { organizationId: "org_missing", userId: "user_1", ...STRICT },
        fakeGateway().client,
      ),
    ).rejects.toMatchObject({ status: 404 });
  });

  it("the database refuses an empty list, which would allow nothing by accident", async () => {
    const org = await createOrganization(0);
    await expect(
      sql`insert into organization_data_policy (organization_id, allowed_regions, updated_by) values (${org.id}, '{}', 'u')`,
    ).rejects.toThrow("organization_data_policy_regions");
    await expect(
      sql`insert into organization_data_policy (organization_id, allowed_providers, updated_by) values (${org.id}, '{}', 'u')`,
    ).rejects.toThrow("organization_data_policy_providers");
    await expect(
      sql`insert into organization_data_policy (organization_id, allowed_regions, updated_by) values (${org.id}, '{mars}', 'u')`,
    ).rejects.toThrow("organization_data_policy_regions");
  });
});
