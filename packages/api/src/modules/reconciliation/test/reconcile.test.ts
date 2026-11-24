import { afterAll, beforeAll, describe, expect, it, vi } from "vitest";

import type { TestDatabase } from "@workspace/db/testing";
import type { CustomerCost, GatewayClient } from "@workspace/gateway/server";
import type postgres from "postgres";

// Needs real Postgres: the comparison reads ledger sums and reservations.
// Skips without DATABASE_URL.
const adminUrl = process.env.DATABASE_URL;

const CRON_SECRET = "a-test-cron-secret-of-at-least-32-chars";

/** Emails are recorded, not sent. */
const sent = vi.hoisted(
  () =>
    [] as {
      to: string;
      template: string;
      variables: Record<string, unknown>;
    }[],
);
vi.mock("@workspace/email/server", () => ({
  sendEmail: async (email: {
    to: string;
    template: string;
    variables: Record<string, unknown>;
  }) => {
    sent.push(email);
  },
}));

/** A gateway that reports the given costs per customer, or is down. */
const gatewayReporting = (
  costs: CustomerCost[],
  options: { isDown?: boolean } = {},
): GatewayClient => ({
  createCustomer: async () => ({ id: "c" }),
  deleteCustomer: async () => {},
  createVirtualKey: async () => ({ id: "vk", value: "v" }),
  updateVirtualKey: async () => {},
  deleteVirtualKey: async () => {},
  listModels: async () => [],
  listProviderKeys: async () => [],
  getCustomerCosts: async () => {
    if (options.isDown) {
      // loaded here: a static import would open the database before the test points it at its own
      const { GatewayError } = await import("@workspace/gateway/server");
      throw new GatewayError("gateway down");
    }
    return costs;
  },
  setCustomerRateLimit: async () => {},
  getLog: async () => undefined,
});

describe.skipIf(!adminUrl)("daily reconciliation", () => {
  let database: TestDatabase;
  let sql: postgres.Sql;
  let reconciliation: typeof import("../reconcile");
  let daily: typeof import("../daily");
  let internalRouter: typeof import("../router").internalRouter;
  let seq = 0;

  // the day checked is 2026-11-23 (UTC)
  const DAY = {
    from: new Date("2026-11-23T00:00:00Z"),
    to: new Date("2026-11-24T00:00:00Z"),
  };

  const createOrganization = async () => {
    const id = `org_rec_${++seq}`;
    await sql`insert into organization (id, name, slug, created_at) values (${id}, ${id}, ${id}, now())`;
    await sql`insert into gateway_customer (organization_id, customer_id) values (${id}, ${`cust_${id}`})`;
    return id;
  };

  const charged = async (
    organizationId: string,
    providerCostUsd: number,
    at: string,
  ) => {
    const id = `rec_${++seq}`;
    await sql`insert into credit_ledger_entry (id, organization_id, amount, kind, idempotency_key, metadata, created_at)
      values (${id}, ${organizationId}, ${-Math.round(providerCostUsd * 1_000_000)}, 'usage', ${id},
              ${sql.json({ provider_cost_usd: providerCostUsd })}, ${at})`;
  };

  /** A hold the reaper released after it expired, as when a settlement never arrives. */
  const reapedHold = async (
    organizationId: string,
    amount: number,
    settledAt: string,
  ) => {
    const id = `res_${++seq}`;
    await sql`insert into credit_reservation (id, organization_id, amount, expires_at, settled_at, created_at)
      values (${id}, ${organizationId}, ${amount}, ${settledAt}::timestamp - interval '1 second', ${settledAt}, ${settledAt}::timestamp - interval '5 minutes')`;
    return id;
  };

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;
    process.env.CRON_SECRET = CRON_SECRET;
    sql = (await import("postgres")).default(database.url, { max: 4 });
    reconciliation = await import("../reconcile");
    daily = await import("../daily");
    ({ internalRouter } = await import("../router"));
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  it("is ok when every organization's gateway cost equals its ledger cost", async () => {
    const org = await createOrganization();
    await charged(org, 0.00045, "2026-11-23T10:00:00Z");
    await charged(org, 0.0003, "2026-11-23T23:59:59Z");
    // outside the day: ignored on both sides
    await charged(org, 5, "2026-11-22T23:59:59Z");
    await charged(org, 5, "2026-11-24T00:00:00Z");

    const result = await reconciliation.reconcile(
      DAY,
      gatewayReporting([
        { customerId: `cust_${org}`, requests: 2, costUsd: 0.00075 },
      ]),
    );
    expect(result.status).toBe("ok");
    expect(result.findings.organizations).toEqual([]);
    const [run] = await sql<
      { status: string }[]
    >`select status from reconciliation_run where id = ${result.id}`;
    expect(run?.status).toBe("ok");
  });

  it("reports a dropped settlement: the gateway's cost, the missing charge, and the reaped hold", async () => {
    const org = await createOrganization();
    await charged(org, 0.00045, "2026-11-20T10:00:00Z");
    // the second request reached the provider, but its settlement never arrived
    await reapedHold(org, 2000, "2026-11-20T11:05:00Z");
    // an attempt that cost nothing closes its hold before expiry, with no entry: not lost
    await sql`insert into credit_reservation (id, organization_id, amount, expires_at, settled_at, created_at)
      values ('res_free', ${org}, 3000, '2026-11-20T13:10:00Z', '2026-11-20T13:00:05Z', '2026-11-20T13:00:00Z')`;
    // a hold reaped and then settled late is not lost: its charge is in the ledger
    const late = await reapedHold(org, 2000, "2026-11-20T12:05:00Z");
    await sql`insert into credit_ledger_entry (id, organization_id, amount, kind, idempotency_key, metadata, created_at)
      values ('late', ${org}, -450, 'usage', ${`usage:${late}`}, ${sql.json({ provider_cost_usd: 0.00045 })}, '2026-11-20T12:06:00Z')`;

    const result = await reconciliation.reconcile(
      {
        from: new Date("2026-11-20T00:00:00Z"),
        to: new Date("2026-11-21T00:00:00Z"),
      },
      gatewayReporting([
        { customerId: `cust_${org}`, requests: 3, costUsd: 0.00135 },
      ]),
    );
    expect(result.status).toBe("mismatch");
    expect(result.findings.organizations).toEqual([
      {
        organizationId: org,
        gatewayCostUsd: 0.00135,
        ledgerCostUsd: 0.0009,
        differenceUsd: expect.closeTo(0.00045, 9),
      },
    ]);
    expect(result.findings.reapedHolds).toEqual([
      { organizationId: org, holds: 1, heldMicroUsd: 2000 },
    ]);
  });

  it("lists cost no organization owns, and ignores rounding", async () => {
    const org = await createOrganization();
    await charged(org, 0.1, "2026-11-21T10:00:00Z");
    const result = await reconciliation.reconcile(
      {
        from: new Date("2026-11-21T00:00:00Z"),
        to: new Date("2026-11-22T00:00:00Z"),
      },
      gatewayReporting([
        // floating point sums of the same costs can differ in the last digits
        { customerId: `cust_${org}`, requests: 1, costUsd: 0.1000000000001 },
        { customerId: "unassigned", requests: 4, costUsd: 0.002 },
        { customerId: "cust_never_mapped", requests: 1, costUsd: 0 },
      ]),
    );
    expect(result.findings.organizations).toEqual([]);
    expect(result.findings.unattributed).toEqual([
      { customerId: "unassigned", costUsd: 0.002 },
    ]);
    expect(result.status).toBe("mismatch");
  });

  it("records an error, and alerts, when the gateway cannot be read", async () => {
    sent.length = 0;
    const result = await daily.runDailyReconciliation({
      now: new Date("2026-11-24T00:30:00Z"),
      origin: "https://app.example.com",
      gateway: gatewayReporting([], { isDown: true }),
      alertTo: "ops@example.com",
    });
    expect(result.status).toBe("error");
    expect(result.findings.error).toContain("gateway down");
    expect(result.alerted).toBe(true);
    expect(sent).toEqual([
      {
        to: "ops@example.com",
        template: "reconciliation-alert",
        variables: {
          url: "https://app.example.com/admin/reconciliation",
          period: "2026-11-23",
          status: "error",
          organizations: 0,
          reapedHolds: 0,
          unattributed: 0,
        },
      },
    ]);
  });

  it("emails no one when the day is clean", async () => {
    sent.length = 0;
    const result = await daily.runDailyReconciliation({
      now: new Date("2026-12-01T00:30:00Z"),
      origin: "https://app.example.com",
      gateway: gatewayReporting([]),
      alertTo: "ops@example.com",
    });
    expect(result).toMatchObject({ status: "ok", alerted: false });
    expect(sent).toEqual([]);
  });

  it("runs from /api/internal/reconciliation only with the cron secret", async () => {
    const post = (authorization?: string) =>
      internalRouter.request("/reconciliation", {
        method: "POST",
        headers: authorization ? { Authorization: authorization } : {},
      });
    expect((await post()).status).toBe(401);
    expect((await post("Bearer wrong")).status).toBe(401);
    expect((await post(`Bearer ${CRON_SECRET}x`)).status).toBe(401);
    // GET too, for schedulers that only send GET; refused the same way
    expect((await internalRouter.request("/reconciliation")).status).toBe(401);
  });
});
