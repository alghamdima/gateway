import { afterAll, beforeAll, describe, expect, it } from "vitest";

import { canAccessAdminRoute, findAccessRule } from "../../access";

import type { TestDatabase } from "@workspace/db/testing";
import type {
  CustomerRateLimit,
  GatewayClient,
} from "@workspace/gateway/server";
import type postgres from "postgres";

// Needs real Postgres: the record, its checks and the gateway customer mapping.
// Skips without DATABASE_URL.
const adminUrl = process.env.DATABASE_URL;

const down = async () => {
  // loaded here: a static import would open the database before the test points it at its own
  const { GatewayError } = await import("@workspace/gateway/server");
  return new GatewayError("gateway down");
};

/** A gateway that records customers and the rate limits pushed to them. */
const fakeGateway = () => {
  const state = {
    customers: 0,
    rateLimits: [] as [string, CustomerRateLimit | null][],
    isDown: false,
  };
  const client: GatewayClient = {
    createCustomer: async () => {
      if (state.isDown) {
        throw await down();
      }
      return { id: `cust_${++state.customers}` };
    },
    deleteCustomer: async () => {},
    createVirtualKey: async () => ({ id: "vk", value: "v" }),
    updateVirtualKey: async () => {},
    deleteVirtualKey: async () => {},
    listModels: async () => [],
    getCustomerCosts: async () => [],
    getLog: async () => undefined,
    setCustomerRateLimit: async (id, limit) => {
      if (state.isDown) {
        throw await down();
      }
      state.rateLimits.push([id, limit]);
    },
  };
  return { state, client };
};

describe.skipIf(!adminUrl)("organization rate limits", () => {
  let database: TestDatabase;
  let sql: postgres.Sql;
  let rateLimit: typeof import("../rate-limit");
  let seq = 0;

  const createOrganization = async () => {
    const id = `org_rl_${++seq}`;
    await sql`insert into organization (id, name, slug, created_at) values (${id}, ${id}, ${id}, now())`;
    return id;
  };

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;
    sql = (await import("postgres")).default(database.url, { max: 4 });
    rateLimit = await import("../rate-limit");
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  it("has no limit until staff set one", async () => {
    const org = await createOrganization();
    expect(await rateLimit.getOrganizationRateLimit(org)).toEqual({
      requestsPerMinute: null,
      tokensPerMinute: null,
      updatedBy: null,
      updatedAt: null,
    });
  });

  it("pushes the limit to the organization's gateway customer, then records who set it", async () => {
    const org = await createOrganization();
    const gateway = fakeGateway();

    const saved = await rateLimit.setOrganizationRateLimit(
      {
        organizationId: org,
        staffId: "staff_1",
        requestsPerMinute: 600,
        tokensPerMinute: 1_000_000,
      },
      gateway.client,
    );

    // the organization had no gateway customer yet: one is created and mapped
    const [mapping] = await sql<
      { customer_id: string }[]
    >`select customer_id from gateway_customer where organization_id = ${org}`;
    expect(gateway.state.rateLimits).toEqual([
      [
        mapping?.customer_id,
        { requestsPerMinute: 600, tokensPerMinute: 1_000_000 },
      ],
    ]);
    expect(saved).toMatchObject({
      requestsPerMinute: 600,
      tokensPerMinute: 1_000_000,
      updatedBy: "staff_1",
    });
    expect(await rateLimit.getOrganizationRateLimit(org)).toMatchObject({
      requestsPerMinute: 600,
    });

    // a change replaces it; removing both kinds removes the limit in both places
    await rateLimit.setOrganizationRateLimit(
      {
        organizationId: org,
        staffId: "staff_2",
        requestsPerMinute: 60,
        tokensPerMinute: null,
      },
      gateway.client,
    );
    expect(await rateLimit.getOrganizationRateLimit(org)).toMatchObject({
      requestsPerMinute: 60,
      tokensPerMinute: null,
      updatedBy: "staff_2",
    });
    await rateLimit.setOrganizationRateLimit(
      {
        organizationId: org,
        staffId: "staff_2",
        requestsPerMinute: null,
        tokensPerMinute: null,
      },
      gateway.client,
    );
    expect(gateway.state.rateLimits.at(-1)).toEqual([
      mapping?.customer_id,
      null,
    ]);
    expect(
      (await rateLimit.getOrganizationRateLimit(org)).requestsPerMinute,
    ).toBeNull();
    const [count] = await sql<
      { n: string }[]
    >`select count(*) as n from organization_rate_limit where organization_id = ${org}`;
    expect(Number(count?.n)).toBe(0);
  });

  it("records nothing when the gateway is down, and answers 503", async () => {
    const org = await createOrganization();
    const gateway = fakeGateway();
    gateway.state.isDown = true;

    await expect(
      rateLimit.setOrganizationRateLimit(
        {
          organizationId: org,
          staffId: "staff_1",
          requestsPerMinute: 10,
          tokensPerMinute: null,
        },
        gateway.client,
      ),
    ).rejects.toMatchObject({ status: 503 });
    expect(
      (await rateLimit.getOrganizationRateLimit(org)).requestsPerMinute,
    ).toBeNull();
  });

  it("refuses an organization that does not exist, without touching the gateway", async () => {
    const gateway = fakeGateway();
    await expect(
      rateLimit.setOrganizationRateLimit(
        {
          organizationId: "org_missing",
          staffId: "s",
          requestsPerMinute: 10,
          tokensPerMinute: null,
        },
        gateway.client,
      ),
    ).rejects.toMatchObject({ status: 404 });
    expect(gateway.state.rateLimits).toEqual([]);
  });

  it("is a check in the database too: zero or negative is refused", async () => {
    const org = await createOrganization();
    await expect(
      sql`insert into organization_rate_limit (organization_id, requests_per_minute, updated_by) values (${org}, 0, 's')`,
    ).rejects.toMatchObject({
      constraint_name: "organization_rate_limit_requests_positive",
    });
  });
});

describe("who may set an organization's rate limit", () => {
  const path = "/organizations/o1/rate-limit";

  it("only staff who may update organizations, and the change is audited", () => {
    const rule = findAccessRule("PUT", path);
    expect(rule?.audit).toEqual({
      action: "organization.rate-limit",
      target: "organization",
    });
    expect(canAccessAdminRoute({ role: "admin" }, "PUT", path)).toBe(true);
    expect(canAccessAdminRoute({ role: "finance" }, "PUT", path)).toBe(false);
    expect(canAccessAdminRoute({ role: "support" }, "PUT", path)).toBe(false);
    // reading it is reading the organization
    expect(canAccessAdminRoute({ role: "support" }, "GET", path)).toBe(true);
  });
});
