import { afterAll, beforeAll, describe, expect, it, vi } from "vitest";

import type { TestDatabase } from "@workspace/db/testing";
import type postgres from "postgres";

// Needs real Postgres: the settings row, its append-only change table and the
// per-user limit. Runs against a throwaway database created next to the one
// in DATABASE_URL and skips when it is not set.
const adminUrl = process.env.DATABASE_URL;

let sql: postgres.Sql;

// The auth server cannot load under vitest (it renders emails). What applies
// to a user and what they own are tested in @workspace/auth; here they are the
// same two reads, so the module's own answers can be checked.
vi.mock("@workspace/auth/server", () => ({
  countOwnedOrganizations: () => Promise.resolve(0),
  getOwnedOrganizationLimit: async (userId: string) => {
    const [row] = await sql<{ max: number }[]>`
      select coalesce(
        (select max_owned_organizations from user_organization_limit where user_id = ${userId}),
        (select max_owned_organizations from account_settings)
      ) as max`;
    return row?.max;
  },
}));

describe.skipIf(!adminUrl)("account settings (ADR 0020)", () => {
  let database: TestDatabase;
  let settings: typeof import("../settings");

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;

    sql = (await import("postgres")).default(database.url, { max: 2 });
    settings = await import("../settings");
    await sql`insert into "user" (id, name, email, email_verified, created_at, updated_at)
      values ('staff', 'Staff', 'staff@example.com', true, now(), now()),
             ('customer', 'Customer', 'customer@example.com', true, now(), now())`;
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  it("starts at 5 organizations per user", async () => {
    expect(await settings.getAccountSettings()).toMatchObject({
      maxOwnedOrganizations: 5,
    });
  });

  it("records a change with before, after and who made it", async () => {
    const change = await settings.updateAccountSettings({
      maxOwnedOrganizations: 3,
      staffId: "staff",
    });

    expect(change.before).toEqual({ maxOwnedOrganizations: 5 });
    expect(change.after).toEqual({ maxOwnedOrganizations: 3 });
    expect(await settings.getAccountSettingsChanges()).toMatchObject([
      {
        before: { maxOwnedOrganizations: 5 },
        after: { maxOwnedOrganizations: 3 },
        changedBy: "staff",
        changedByEmail: "staff@example.com",
      },
    ]);
  });

  it("keeps the change history append-only", async () => {
    await expect(sql`delete from account_settings_change`).rejects.toThrow(
      /append-only/,
    );
  });

  it("refuses a limit beyond the ceiling in the database", async () => {
    await expect(
      settings.updateAccountSettings({
        maxOwnedOrganizations: 1001,
        staffId: "staff",
      }),
    ).rejects.toThrow(/account_settings/);
  });

  it("gives one user their own limit, and takes it away again", async () => {
    const raised = await settings.setUserOrganizationLimit({
      userId: "customer",
      staffId: "staff",
      maxOwnedOrganizations: 20,
    });

    expect(raised.before).toEqual({ maxOwnedOrganizations: null });
    expect(raised.limit).toEqual({ own: 20, effective: 20, owned: 0 });

    const cleared = await settings.setUserOrganizationLimit({
      userId: "customer",
      staffId: "staff",
      maxOwnedOrganizations: null,
    });

    expect(cleared.before).toEqual({ maxOwnedOrganizations: 20 });
    expect(cleared.limit).toEqual({ own: null, effective: 3, owned: 0 });
  });

  it("answers 404 for a user that does not exist", async () => {
    await expect(
      settings.setUserOrganizationLimit({
        userId: "nobody",
        staffId: "staff",
        maxOwnedOrganizations: 2,
      }),
    ).rejects.toMatchObject({ status: 404 });
  });
});
