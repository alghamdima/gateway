import { afterAll, beforeAll, describe, expect, it } from "vitest";

import type { TestDatabase } from "@workspace/db/testing";
import type { GatewayClient, GatewayProvider } from "@workspace/gateway/server";
import type postgres from "postgres";

// Needs real Postgres: deleting a key removes its terms. Skips without DATABASE_URL.
const adminUrl = process.env.DATABASE_URL;

const KEY = {
  name: "main",
  maskedValue: "sk-l****cret",
  models: ["*"],
  weight: 1,
  isEnabled: true,
};

/** A gateway holding the given providers, recording what it is asked to do. */
const fakeGateway = (
  providers: GatewayProvider[],
  fail?: { status?: number; body?: string },
) => {
  const calls: unknown[][] = [];
  const act = async (...call: unknown[]) => {
    calls.push(call);
    if (fail) {
      // loaded here: a static import would open the database before the test points it at its own
      const { GatewayError } = await import("@workspace/gateway/server");
      throw new GatewayError(
        `gateway answered ${fail.status}: ${fail.body ?? ""}`,
        fail.status,
      );
    }
  };
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
    listProviderKeys: async () => [],
    searchRequests: async () => ({ requests: [], total: 0 }),
    listProviders: async () => {
      await act("listProviders");
      return providers;
    },
    createProvider: async (input) => act("createProvider", input),
    deleteProvider: async (name) => act("deleteProvider", name),
    createProviderKey: async (provider, input) => {
      await act("createProviderKey", provider, input);
      return { id: "k_new", ...KEY, name: input.name };
    },
    updateProviderKey: async (provider, keyId, change) => {
      await act("updateProviderKey", provider, keyId, change);
      return { id: keyId, ...KEY };
    },
    deleteProviderKey: async (provider, keyId) =>
      act("deleteProviderKey", provider, keyId),
    refreshProviderModels: async (provider) =>
      act("refreshProviderModels", provider),
  };
  return { client, calls };
};

const HUMAIN: GatewayProvider = {
  name: "humain",
  baseProvider: "openai",
  baseUrl: "https://api.humain.example",
  status: "active",
  keys: [
    { id: "k_riyadh", ...KEY, name: "riyadh" },
    { id: "k_dammam", ...KEY, name: "dammam" },
  ],
};

const TERMS = {
  region: "sa",
  retentionDays: 0,
  trainsOnData: false,
  agreement: "direct",
  source: "contract 2026-11",
  verifiedOn: "2026-11-24",
} as const;

describe.skipIf(!adminUrl)("provider accounts", () => {
  let database: TestDatabase;
  let sql: postgres.Sql;
  let accounts: typeof import("../accounts");
  let providers: typeof import("../providers");

  const termsOf = async () =>
    (await sql`select key_id from provider_endpoint order by key_id`).map(
      (row) => String(row.key_id),
    );

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;
    sql = (await import("postgres")).default(database.url, { max: 4 });
    accounts = await import("../accounts");
    providers = await import("../providers");
    await sql`insert into "user" (id, name, email, email_verified, created_at, updated_at) values ('staff1', 'Staff', 'staff@example.com', true, now(), now())`;
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  it("passes a new provider and a new key to the gateway as given", async () => {
    const gateway = fakeGateway([]);
    await accounts.createProviderAccount(
      {
        name: "humain",
        baseProvider: "openai",
        baseUrl: "https://api.humain.example",
      },
      gateway.client,
    );
    const key = await accounts.createProviderKey(
      "humain",
      { name: "riyadh", value: "sk-live-secret" },
      gateway.client,
    );

    expect(gateway.calls).toEqual([
      [
        "createProvider",
        {
          name: "humain",
          baseProvider: "openai",
          baseUrl: "https://api.humain.example",
        },
      ],
      [
        "createProviderKey",
        "humain",
        { name: "riyadh", value: "sk-live-secret" },
      ],
    ]);
    // what comes back is the masked key, never the secret
    expect(JSON.stringify(key)).not.toContain("sk-live-secret");
  });

  it.each([
    [409, "", 409, "admin:providers.accounts.error.exists"],
    [404, "", 404, "error.notFound"],
    [503, "", 503, "error.general"],
    [undefined, "", 503, "error.general"],
  ])(
    "answers a gateway %s with %s %s",
    async (gatewayStatus, body, status, code) => {
      const gateway = fakeGateway([], { status: gatewayStatus, body });
      await expect(
        accounts.createProviderAccount({ name: "openai" }, gateway.client),
      ).rejects.toMatchObject({ status, code });
    },
  );

  it("passes on why the gateway refused, so staff can fix it", async () => {
    const gateway = fakeGateway([], {
      status: 400,
      body: '{"error":{"message":"Invalid base URL: private address"}}',
    });
    await expect(
      accounts.createProviderAccount(
        { name: "x1", baseProvider: "openai", baseUrl: "http://10.0.0.1" },
        gateway.client,
      ),
    ).rejects.toMatchObject({
      status: 400,
      code: "admin:providers.accounts.error.refused",
      message: "Invalid base URL: private address",
    });
  });

  it("removes a key's terms with the key, and only that key's", async () => {
    for (const keyId of ["k_riyadh", "k_dammam"]) {
      await providers.setProviderTerms(
        { keyId, ...TERMS, staffId: "staff1" },
        listing(),
      );
    }
    const gateway = fakeGateway([HUMAIN]);

    await accounts.deleteProviderKey(
      "humain",
      "k_riyadh",
      "staff1",
      gateway.client,
    );

    expect(gateway.calls).toEqual([
      ["deleteProviderKey", "humain", "k_riyadh"],
    ]);
    expect(await termsOf()).toEqual(["k_dammam"]);
  });

  it("keeps the terms when the gateway did not delete the key", async () => {
    const gateway = fakeGateway([HUMAIN], { status: 503 });
    await expect(
      accounts.deleteProviderKey(
        "humain",
        "k_dammam",
        "staff1",
        gateway.client,
      ),
    ).rejects.toMatchObject({ status: 503 });
    expect(await termsOf()).toEqual(["k_dammam"]);
  });

  it("removes the terms of every key of a deleted provider", async () => {
    await providers.setProviderTerms(
      { keyId: "k_riyadh", ...TERMS, staffId: "staff1" },
      listing(),
    );
    const gateway = fakeGateway([HUMAIN]);

    await accounts.deleteProviderAccount("humain", "staff1", gateway.client);

    expect(gateway.calls).toEqual([
      ["listProviders"],
      ["deleteProvider", "humain"],
    ]);
    expect(await termsOf()).toEqual([]);
  });
});

/** The gateway as setProviderTerms needs it: one that lists HUMAIN's keys. */
function listing() {
  const { client } = fakeGateway([HUMAIN]);
  return {
    ...client,
    listProviderKeys: async () =>
      HUMAIN.keys.map((key) => ({
        id: key.id,
        name: key.name,
        provider: HUMAIN.name,
        isEnabled: true,
      })),
  };
}
