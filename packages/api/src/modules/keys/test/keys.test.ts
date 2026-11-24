import { afterAll, beforeAll, beforeEach, describe, expect, it } from "vitest";

import { roles } from "@workspace/auth";

import type { TestDatabase } from "@workspace/db/testing";
import type {
  CreateVirtualKeyInput,
  CustomerRateLimit,
  GatewayClient,
  UpdateVirtualKeyInput,
} from "@workspace/gateway/server";
import type postgres from "postgres";

// Needs real Postgres: the unique keys, the foreign keys and the race on the
// gateway customer are the database's. Skips without DATABASE_URL.
const adminUrl = process.env.DATABASE_URL;

// ids stay unique across tests, like a real gateway's
let next = 0;

/** An in-memory gateway that records every call and can be told to fail. */
const fakeGateway = () => {
  const state = {
    customers: new Set<string>(),
    keys: new Map<string, CreateVirtualKeyInput & { isActive: boolean }>(),
    updates: [] as [string, UpdateVirtualKeyInput][],
    deletedKeys: [] as string[],
    deletedCustomers: [] as string[],
    rateLimits: [] as [string, CustomerRateLimit | null][],
    failRateLimit: false,
    failCreateKey: false,
    /** When set, every new key gets this value (to force a duplicate). */
    fixedValue: undefined as string | undefined,
    /** When set, createCustomer waits until this many calls are waiting. */
    customerBarrier: 0,
  };
  let waiting: (() => void)[] = [];
  const client: GatewayClient = {
    createCustomer: async () => {
      if (state.customerBarrier > 0) {
        await new Promise<void>((resolve) => {
          waiting.push(resolve);
          if (waiting.length >= state.customerBarrier) {
            waiting.forEach((release) => release());
            waiting = [];
          }
        });
      }
      const id = `cust_${++next}`;
      state.customers.add(id);
      return { id };
    },
    deleteCustomer: async (id) => {
      state.customers.delete(id);
      state.deletedCustomers.push(id);
    },
    createVirtualKey: async (input) => {
      if (state.failCreateKey) {
        throw new Error("gateway down");
      }
      // like the real gateway: virtual key names are unique across all customers
      if ([...state.keys.values()].some((key) => key.name === input.name)) {
        const { GatewayError } = await import("@workspace/gateway/server");
        throw new GatewayError(
          "A virtual key with this name already exists",
          409,
        );
      }
      const id = `vk_${++next}`;
      state.keys.set(id, { ...input, isActive: true });
      return { id, value: state.fixedValue ?? `sk-bf-${id}-${"x".repeat(30)}` };
    },
    updateVirtualKey: async (id, input) => {
      state.updates.push([id, input]);
      const key = state.keys.get(id);
      if (key && input.isActive !== undefined) {
        key.isActive = input.isActive;
      }
    },
    listModels: async () => [],
    setCustomerRateLimit: async (id, limit) => {
      if (state.failRateLimit) {
        // loaded here: a static import would open the database before the test points it at its own
        const { GatewayError } = await import("@workspace/gateway/server");
        throw new GatewayError("gateway down");
      }
      state.rateLimits.push([id, limit]);
    },
    deleteVirtualKey: async (id) => {
      state.keys.delete(id);
      state.deletedKeys.push(id);
    },
  };
  return { state, client };
};

describe.skipIf(!adminUrl)("organization API keys", () => {
  let database: TestDatabase;
  let sql: postgres.Sql;
  let keys: typeof import("../keys");
  let gateway: ReturnType<typeof fakeGateway>;
  let seq = 0;

  const createOrganization = async () => {
    const id = `org_keys_${++seq}`;
    await sql`insert into organization (id, name, slug, created_at) values (${id}, ${id}, ${id}, now())`;
    return id;
  };

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;
    sql = (await import("postgres")).default(database.url, { max: 4 });
    keys = await import("../keys");
  });

  beforeEach(() => {
    gateway = fakeGateway();
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  it("creates the gateway customer once, then a key under it, returned once and stored as a hash", async () => {
    const org = await createOrganization();
    const first = await keys.createKey(
      { organizationId: org, userId: "user_1", name: "prod" },
      gateway.client,
    );
    const second = await keys.createKey(
      { organizationId: org, userId: "user_1", name: "staging" },
      gateway.client,
    );

    expect(gateway.state.customers.size).toBe(1);
    const [mapping] =
      await sql`select customer_id from gateway_customer where organization_id = ${org}`;
    expect([...gateway.state.keys.values()].map((k) => k.customerId)).toEqual([
      mapping?.customer_id,
      mapping?.customer_id,
    ]);

    expect(first.key).toMatch(/^sk-bf-/);
    expect(first.apiKey).toMatchObject({
      name: "prod",
      prefix: first.key.slice(0, 12),
    });
    expect(first.apiKey).not.toHaveProperty("valueHash");
    expect(first.apiKey).not.toHaveProperty("virtualKeyId");

    const rows =
      await sql`select * from api_key where organization_id = ${org}`;
    expect(rows).toHaveLength(2);
    // the key itself is nowhere in the table
    for (const row of rows) {
      expect(JSON.stringify(row)).not.toContain(first.key);
      expect(JSON.stringify(row)).not.toContain(second.key);
    }
    expect(rows.find((r) => r.name === "prod")?.value_hash).toBe(
      keys.hashKey(first.key),
    );
  });

  it("records the spending limit, and gives the gateway only the expiry", async () => {
    const org = await createOrganization();
    const expiresAt = new Date(Date.now() + 86_400_000);
    const { apiKey } = await keys.createKey(
      {
        organizationId: org,
        userId: "user_1",
        name: "capped",
        limit: { limitMicroUsd: 5_000_000, reset: "weekly" },
        expiresAt,
      },
      gateway.client,
    );
    const [sent] = [...gateway.state.keys.values()];
    // the limit is enforced by the credits plugin from this record (ADR 0013)
    expect(sent).toMatchObject({ expiresAt });
    expect(sent).not.toHaveProperty("limit");
    expect(apiKey).toMatchObject({
      limitMicroUsd: 5_000_000,
      limitReset: "weekly",
    });
  });

  it("lets two organizations, and one organization twice, use the same key name", async () => {
    const first = await createOrganization();
    const second = await createOrganization();
    const made = await Promise.all([
      keys.createKey(
        { organizationId: first, userId: "u", name: "Production" },
        gateway.client,
      ),
      keys.createKey(
        { organizationId: second, userId: "u", name: "Production" },
        gateway.client,
      ),
    ]);
    const again = await keys.createKey(
      { organizationId: first, userId: "u", name: "Production" },
      gateway.client,
    );
    expect([...made, again].map((k) => k.apiKey.name)).toEqual([
      "Production",
      "Production",
      "Production",
    ]);
    // the gateway was given our ids, never the customer's name
    const sent = [...gateway.state.keys.values()].map((k) => k.name);
    expect(sent).not.toContain("Production");
    expect(sent).toEqual(
      expect.arrayContaining([
        made[0].apiKey.id,
        made[1].apiKey.id,
        again.apiKey.id,
      ]),
    );
  });

  it("renames only in the record", async () => {
    const org = await createOrganization();
    const { apiKey } = await keys.createKey(
      { organizationId: org, userId: "u", name: "old" },
      gateway.client,
    );
    const updated = await keys.updateKey(
      org,
      apiKey.id,
      { name: "new" },
      gateway.client,
    );
    expect(updated?.name).toBe("new");
    expect(gateway.state.updates).toEqual([]);
  });

  it("changes only the record when only the limit changes", async () => {
    const org = await createOrganization();
    const { apiKey } = await keys.createKey(
      { organizationId: org, userId: "u", name: "a" },
      gateway.client,
    );
    const updated = await keys.updateKey(
      org,
      apiKey.id,
      { limit: { limitMicroUsd: 2_000_000, reset: "daily" } },
      gateway.client,
    );
    expect(gateway.state.updates).toEqual([]);
    expect(updated).toMatchObject({
      limitMicroUsd: 2_000_000,
      limitReset: "daily",
    });
  });

  it("deletes the virtual key when recording it fails, and stores nothing", async () => {
    const org = await createOrganization();
    gateway.state.fixedValue = "sk-bf-duplicate-value-0000000000000000";
    await keys.createKey(
      { organizationId: org, userId: "u", name: "a" },
      gateway.client,
    );
    const before = await sql`select count(*)::int as n from api_key`;

    // same value -> same hash -> the insert fails on the unique hash
    await expect(
      keys.createKey(
        { organizationId: org, userId: "u", name: "b" },
        gateway.client,
      ),
    ).rejects.toMatchObject({
      cause: { constraint_name: "api_key_valueHash_unique" },
    });

    expect(gateway.state.keys.size).toBe(1);
    expect(gateway.state.deletedKeys).toHaveLength(1);
    expect(await sql`select count(*)::int as n from api_key`).toEqual(before);
  });

  it("stores nothing when the gateway refuses the key", async () => {
    const org = await createOrganization();
    gateway.state.failCreateKey = true;
    await expect(
      keys.createKey(
        { organizationId: org, userId: "u", name: "a" },
        gateway.client,
      ),
    ).rejects.toThrow(/gateway down/);
    expect(
      await sql`select * from api_key where organization_id = ${org}`,
    ).toHaveLength(0);
  });

  it("keeps one gateway customer when two first keys race, deleting the loser's", async () => {
    const org = await createOrganization();
    // all three have asked the gateway for a customer before any of them records one
    gateway.state.customerBarrier = 3;
    await Promise.all([
      keys.createKey(
        { organizationId: org, userId: "u", name: "a" },
        gateway.client,
      ),
      keys.createKey(
        { organizationId: org, userId: "u", name: "b" },
        gateway.client,
      ),
      keys.createKey(
        { organizationId: org, userId: "u", name: "c" },
        gateway.client,
      ),
    ]);
    const mappings =
      await sql`select customer_id from gateway_customer where organization_id = ${org}`;
    expect(mappings).toHaveLength(1);
    expect(gateway.state.deletedCustomers).toHaveLength(2);
    expect(gateway.state.customers).toEqual(
      new Set([mappings[0]?.customer_id]),
    );
    // every key sits under the customer that won
    for (const key of gateway.state.keys.values()) {
      expect(key.customerId).toBe(mappings[0]?.customer_id);
    }
  });

  it("disables and renames in the gateway, and removes the limit in the record", async () => {
    const org = await createOrganization();
    const { apiKey } = await keys.createKey(
      {
        organizationId: org,
        userId: "u",
        name: "a",
        limit: { limitMicroUsd: 1_000_000, reset: null },
      },
      gateway.client,
    );
    const updated = await keys.updateKey(
      org,
      apiKey.id,
      { disabled: true, name: "renamed", limit: null },
      gateway.client,
    );
    expect(gateway.state.updates).toEqual([
      [expect.any(String), { isActive: false }],
    ]);
    expect(updated).toMatchObject({
      name: "renamed",
      limitMicroUsd: null,
      limitReset: null,
    });
    expect(updated?.disabledAt).toBeInstanceOf(Date);

    const enabled = await keys.updateKey(
      org,
      apiKey.id,
      { disabled: false },
      gateway.client,
    );
    expect(enabled?.disabledAt).toBeNull();
  });

  it("will not touch another organization's key", async () => {
    const owner = await createOrganization();
    const other = await createOrganization();
    const { apiKey } = await keys.createKey(
      { organizationId: owner, userId: "u", name: "a" },
      gateway.client,
    );
    expect(
      await keys.updateKey(
        other,
        apiKey.id,
        { disabled: true },
        gateway.client,
      ),
    ).toBeUndefined();
    expect(await keys.deleteKey(other, apiKey.id, gateway.client)).toBe(false);
    expect(gateway.state.updates).toEqual([]);
    expect(gateway.state.deletedKeys).toEqual([]);
    expect(await keys.listKeys(other)).toEqual([]);
  });

  it("deletes the virtual key before the record", async () => {
    const org = await createOrganization();
    const { apiKey } = await keys.createKey(
      { organizationId: org, userId: "u", name: "a" },
      gateway.client,
    );
    expect(await keys.deleteKey(org, apiKey.id, gateway.client)).toBe(true);
    expect(gateway.state.keys.size).toBe(0);
    expect(await keys.listKeys(org)).toEqual([]);
  });

  it("keeps the record when the gateway cannot delete the key", async () => {
    const org = await createOrganization();
    const { apiKey } = await keys.createKey(
      { organizationId: org, userId: "u", name: "a" },
      gateway.client,
    );
    gateway.client.deleteVirtualKey = async () => {
      throw new Error("gateway down");
    };
    await expect(
      keys.deleteKey(org, apiKey.id, gateway.client),
    ).rejects.toThrow(/gateway down/);
    expect(await keys.listKeys(org)).toHaveLength(1);
  });
});

describe("who may manage keys in an organization", () => {
  it.each([
    ["owner", ["create", "read", "update", "delete"], []],
    ["admin", ["create", "read", "update", "delete"], []],
    ["member", ["read"], ["create", "update", "delete"]],
  ] as const)("%s", (role, allowed, refused) => {
    for (const action of allowed) {
      expect(roles[role].authorize({ apiKey: [action] }).success).toBe(true);
    }
    for (const action of refused) {
      expect(roles[role].authorize({ apiKey: [action] }).success).toBe(false);
    }
  });
});
