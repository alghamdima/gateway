import {
  afterAll,
  beforeAll,
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from "vitest";

import type { TestDatabase } from "@workspace/db/testing";
import type postgres from "postgres";

// Needs real Postgres: the check reads accounts, ledger entries and members.
// Skips without DATABASE_URL.
const adminUrl = process.env.DATABASE_URL;

const USD = 1_000_000;

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

describe.skipIf(!adminUrl)("low-balance emails", () => {
  let database: TestDatabase;
  let sql: postgres.Sql;
  let lowBalance: typeof import("../low-balance");
  let seq = 0;

  /** An organization with an owner, an admin and a plain member. */
  const createOrganization = async () => {
    const id = `org_low_${++seq}`;
    await sql`insert into organization (id, name, slug, created_at) values (${id}, ${`Org ${seq}`}, ${id}, now())`;
    for (const role of ["owner", "admin", "member"]) {
      const userId = `${id}_${role}`;
      await sql`insert into "user" (id, name, email, email_verified, created_at, updated_at)
        values (${userId}, ${role}, ${`${role}@${id}.example`}, true, now(), now())`;
      await sql`insert into member (id, organization_id, user_id, role, created_at)
        values (${`m_${userId}`}, ${id}, ${userId}, ${role}, now())`;
    }
    return id;
  };

  const entry = async (
    organizationId: string,
    amount: number,
    kind = amount > 0 ? "top_up" : "usage",
  ) => {
    const id = `low_${++seq}`;
    await sql`insert into credit_ledger_entry (id, organization_id, amount, kind, idempotency_key) values (${id}, ${organizationId}, ${amount}, ${kind}, ${id})`;
    await sql`insert into credit_account (organization_id, balance) values (${organizationId}, ${amount})
      on conflict (organization_id) do update set balance = credit_account.balance + ${amount}`;
  };

  const check = () =>
    lowBalance.checkLowBalances({ origin: "https://app.example.com" });
  const sentTo = (organizationId: string) =>
    sent
      .filter((email) => email.to.endsWith(`@${organizationId}.example`))
      .map((email) => email.to)
      .sort();

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;
    sql = (await import("postgres")).default(database.url, { max: 4 });
    lowBalance = await import("../low-balance");
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  beforeEach(() => {
    sent.length = 0;
  });

  it("emails the owners and admins once per dip, and again after a top-up and a new dip", async () => {
    const org = await createOrganization();
    await entry(org, 10 * USD);
    await check();
    expect(sentTo(org)).toEqual([]);

    await entry(org, -6 * USD); // $4 left, under the $5 default
    await check();
    expect(sentTo(org)).toEqual([
      `admin@${org}.example`,
      `owner@${org}.example`,
    ]);
    const email = sent.find((e) => e.to === `owner@${org}.example`);
    expect(email).toMatchObject({
      template: "low-balance",
      variables: {
        url: `https://app.example.com/dashboard/${org}/credits`,
        available: "$4.00",
        threshold: "$5.00",
      },
    });

    sent.length = 0;
    await check();
    expect(sentTo(org)).toEqual([]); // still low: not again

    await entry(org, 20 * USD); // back above
    await check();
    expect(sentTo(org)).toEqual([]);
    await entry(org, -22 * USD); // $2 left: a new dip
    await check();
    expect(sentTo(org)).toHaveLength(2);
  });

  it("counts requests in flight against what is available", async () => {
    const org = await createOrganization();
    await entry(org, 6 * USD);
    await sql`update credit_account set held = ${2 * USD} where organization_id = ${org}`;
    await check();
    expect(sentTo(org)).toHaveLength(2);
  });

  it("leaves alone an organization that never had credit, and one that turned the alert off", async () => {
    const fresh = await createOrganization();
    await sql`insert into credit_account (organization_id, balance) values (${fresh}, 0)`;
    const off = await createOrganization();
    await entry(off, 10 * USD);
    await entry(off, -9 * USD);
    await lowBalance.setLowBalanceSetting(off, null);

    await check();
    expect(sentTo(fresh)).toEqual([]);
    expect(sentTo(off)).toEqual([]);
  });

  it("uses the organization's own threshold, and a new threshold starts a new watch", async () => {
    const org = await createOrganization();
    await entry(org, 30 * USD);
    await lowBalance.setLowBalanceSetting(org, 50 * USD);
    expect(await lowBalance.getLowBalanceSetting(org)).toEqual({
      thresholdMicroUsd: 50 * USD,
    });

    await check();
    expect(sentTo(org)).toHaveLength(2);
    sent.length = 0;
    await lowBalance.setLowBalanceSetting(org, 40 * USD);
    await check();
    expect(sentTo(org)).toHaveLength(2);
  });

  it("has the $5 default for an organization that never chose", async () => {
    const org = await createOrganization();
    expect(await lowBalance.getLowBalanceSetting(org)).toEqual({
      thresholdMicroUsd: 5 * USD,
    });
  });
});
