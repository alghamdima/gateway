import { afterAll, beforeAll, describe, expect, it } from "vitest";

import type { TestDatabase } from "@workspace/db/testing";
import type postgres from "postgres";

// Needs real Postgres for the append-only trigger. Skips without DATABASE_URL.
const adminUrl = process.env.DATABASE_URL;

describe.skipIf(!adminUrl)("admin audit log", () => {
  let database: TestDatabase;
  let sql: postgres.Sql;
  let queries: typeof import("../queries");

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;
    sql = (await import("postgres")).default(database.url, { max: 2 });
    queries = await import("../queries");

    await sql`insert into "user" (id, name, email, email_verified, created_at, updated_at)
      values ('staff_a', 'Amal', 'amal@example.com', true, now(), now())`;
    for (let i = 1; i <= 5; i++) {
      await sql`insert into admin_audit_log (actor_id, actor_role, action, target_type, target_id)
        values (${i % 2 ? "staff_a" : "staff_b"}, 'admin', ${`test.action-${i}`}, 'user', ${`u${i}`})`;
    }
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  it("lists newest first, with the actor's name when the user still exists", async () => {
    const rows = await queries.getAdminActions({ limit: 10 });
    expect(rows.map((row) => row.action)).toEqual([
      "test.action-5",
      "test.action-4",
      "test.action-3",
      "test.action-2",
      "test.action-1",
    ]);
    expect(rows[0]).toMatchObject({ actorId: "staff_a", actorName: "Amal" });
    expect(rows[1]).toMatchObject({ actorId: "staff_b", actorName: null });
  });

  it("pages with the last id seen, and filters by actor", async () => {
    const first = await queries.getAdminActions({ limit: 2 });
    const next = await queries.getAdminActions({
      limit: 2,
      before: first.at(-1)?.id,
    });
    expect(next.map((row) => row.action)).toEqual([
      "test.action-3",
      "test.action-2",
    ]);
    const byB = await queries.getAdminActions({
      limit: 10,
      actorId: "staff_b",
    });
    expect(byB.map((row) => row.targetId)).toEqual(["u4", "u2"]);
  });

  it("cannot be edited or deleted", async () => {
    await expect(
      sql`update admin_audit_log set action = 'rewritten'`,
    ).rejects.toThrow(/append-only/);
    await expect(sql`delete from admin_audit_log`).rejects.toThrow(
      /append-only/,
    );
  });
});
