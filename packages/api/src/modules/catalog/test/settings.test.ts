import { afterAll, beforeAll, describe, expect, it } from "vitest";

import type { TestDatabase } from "@workspace/db/testing";
import type postgres from "postgres";

// The catalog settings row and its append-only history, against real
// Postgres. Skips without DATABASE_URL.
const adminUrl = process.env.DATABASE_URL;

describe.skipIf(!adminUrl)("catalog settings (CAT-11)", () => {
  let database: TestDatabase;
  let sql: postgres.Sql;
  let settings: typeof import("../settings");

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;

    sql = (await import("postgres")).default(database.url, { max: 2 });
    settings = await import("../settings");
    await sql`insert into "user" (id, name, email, email_verified, created_at, updated_at)
      values ('staff', 'Staff', 'staff@example.com', true, now(), now())`;
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  it("starts at 20 answered calls, with traffic volume not public", async () => {
    expect(await settings.getCatalogSettings()).toMatchObject({
      minCallsForStats: 20,
      isTrafficPublic: false,
    });
  });

  it("records a change with before, after and who made it", async () => {
    const change = await settings.updateCatalogSettings({
      minCallsForStats: 50,
      isTrafficPublic: true,
      staffId: "staff",
    });

    const before = { minCallsForStats: 20, isTrafficPublic: false };
    const after = { minCallsForStats: 50, isTrafficPublic: true };
    expect(change.before).toEqual(before);
    expect(change.after).toEqual(after);
    expect(await settings.getCatalogSettingsChanges()).toMatchObject([
      {
        before,
        after,
        changedBy: "staff",
        changedByEmail: "staff@example.com",
      },
    ]);
  });

  it("keeps the change history append-only", async () => {
    await expect(sql`delete from catalog_settings_change`).rejects.toThrow(
      /append-only/,
    );
  });

  it("refuses zero calls and a number beyond the ceiling in the database", async () => {
    for (const minCallsForStats of [0, 1_000_001]) {
      await expect(
        settings.updateCatalogSettings({
          minCallsForStats,
          isTrafficPublic: false,
          staffId: "staff",
        }),
      ).rejects.toThrow(/catalog_settings/);
    }
  });
});
