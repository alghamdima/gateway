import { afterAll, beforeAll, describe, expect, it } from "vitest";

import type { TestDatabase } from "@workspace/db/testing";
import type postgres from "postgres";

// Needs real Postgres: these tests are about constraints, roles, triggers and
// concurrency. They run against a throwaway database created next to the one
// in DATABASE_URL and skip when it is not set.
const adminUrl = process.env.DATABASE_URL;

const USD = 1_000_000;

describe.skipIf(!adminUrl)("credits ledger", () => {
  let database: TestDatabase;
  let sql: postgres.Sql;
  let ledger: typeof import("../ledger");
  let adjust: typeof import("../adjust");
  let seq = 0;

  const createOrganization = async () => {
    const id = `org_${++seq}`;
    await sql`insert into organization (id, name, slug, created_at) values (${id}, ${id}, ${id}, now())`;
    return id;
  };

  const sumOfEntries = async (organizationId: string) => {
    const [row] = await sql<
      { total: string }[]
    >`select coalesce(sum(amount), 0) as total from credit_ledger_entry where organization_id = ${organizationId}`;
    return Number(row?.total);
  };

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;

    sql = (await import("postgres")).default(database.url, { max: 4 });
    ledger = await import("../ledger");
    adjust = await import("../adjust");
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  describe("idempotency", () => {
    it("changes the balance once when the same key is applied twice", async () => {
      const organizationId = await createOrganization();
      const entry = {
        organizationId,
        amount: 10 * USD,
        kind: "top_up",
        idempotencyKey: "pay_1",
      } as const;

      const first = await ledger.applyEntry(entry);
      const second = await ledger.applyEntry(entry);

      expect(first).toMatchObject({ isApplied: true, balance: 10 * USD });
      expect(second).toMatchObject({
        isApplied: false,
        balance: 10 * USD,
        entryId: first.entryId,
      });
      expect(await sumOfEntries(organizationId)).toBe(10 * USD);
      expect(await ledger.getBalance(organizationId)).toEqual({
        balance: 10 * USD,
        held: 0,
        available: 10 * USD,
      });
    });

    it("applies one of twenty concurrent deliveries of the same event", async () => {
      const organizationId = await createOrganization();
      const results = await Promise.all(
        Array.from({ length: 20 }, () =>
          ledger.applyEntry({
            organizationId,
            amount: 5 * USD,
            kind: "top_up",
            idempotencyKey: "pay_concurrent",
          }),
        ),
      );

      expect(results.filter((r) => r.isApplied)).toHaveLength(1);
      expect((await ledger.getBalance(organizationId)).balance).toBe(5 * USD);
    });

    it("refuses a key that was used for a different entry", async () => {
      const organizationId = await createOrganization();
      const other = await createOrganization();
      await ledger.applyEntry({
        organizationId,
        amount: USD,
        kind: "top_up",
        idempotencyKey: "pay_reused",
      });

      await expect(
        ledger.applyEntry({
          organizationId,
          amount: 2 * USD,
          kind: "top_up",
          idempotencyKey: "pay_reused",
        }),
      ).rejects.toThrow(/already used/);
      await expect(
        ledger.applyEntry({
          organizationId: other,
          amount: USD,
          kind: "top_up",
          idempotencyKey: "pay_reused",
        }),
      ).rejects.toThrow(/already used/);
      expect((await ledger.getBalance(organizationId)).balance).toBe(USD);
      expect((await ledger.getBalance(other)).balance).toBe(0);
    });
  });

  describe("append-only", () => {
    it("lets the gateway role insert entries but not change or delete them", async () => {
      const organizationId = await createOrganization();
      await ledger.applyEntry({
        organizationId,
        amount: USD,
        kind: "top_up",
        idempotencyKey: "pay_role",
      });

      const asGateway = async (
        query: (tx: postgres.TransactionSql) => unknown,
      ) =>
        sql.begin(async (tx) => {
          await tx`set local role gateway_ledger`;
          await query(tx);
        });

      await expect(
        asGateway(
          (tx) =>
            tx`update credit_ledger_entry set amount = 999 where organization_id = ${organizationId}`,
        ),
      ).rejects.toMatchObject({ code: "42501" });
      await expect(
        asGateway(
          (tx) =>
            tx`delete from credit_ledger_entry where organization_id = ${organizationId}`,
        ),
      ).rejects.toMatchObject({ code: "42501" });
      await expect(
        asGateway(
          (tx) =>
            tx`delete from credit_account where organization_id = ${organizationId}`,
        ),
      ).rejects.toMatchObject({ code: "42501" });
      await expect(
        asGateway((tx) => tx`select * from "user"`),
      ).rejects.toMatchObject({ code: "42501" });

      await asGateway(async (tx) => {
        await tx`insert into credit_ledger_entry (id, organization_id, amount, kind, idempotency_key) values ('gw_1', ${organizationId}, -250000, 'usage', 'req_1')`;
        await tx`update credit_account set balance = balance - 250000 where organization_id = ${organizationId}`;
        await tx`insert into credit_reservation (id, organization_id, amount, expires_at) values ('res_1', ${organizationId}, 1000, now() + interval '5 minutes')`;
        await tx`update credit_reservation set settled_at = now() where id = 'res_1'`;
      });

      expect((await ledger.getBalance(organizationId)).balance).toBe(750_000);
      expect(await sumOfEntries(organizationId)).toBe(750_000);
    });

    it("rejects a change even from the owning role", async () => {
      const organizationId = await createOrganization();
      await ledger.applyEntry({
        organizationId,
        amount: USD,
        kind: "top_up",
        idempotencyKey: "pay_owner",
      });

      await expect(
        sql`update credit_ledger_entry set amount = 5 where organization_id = ${organizationId}`,
      ).rejects.toThrow(/append-only/);
      await expect(
        sql`delete from credit_ledger_entry where organization_id = ${organizationId}`,
      ).rejects.toThrow(/append-only/);
      expect(await sumOfEntries(organizationId)).toBe(USD);
    });

    it("keeps the entries when the organization is deleted", async () => {
      const organizationId = await createOrganization();
      await ledger.applyEntry({
        organizationId,
        amount: USD,
        kind: "top_up",
        idempotencyKey: "pay_deleted_org",
      });

      await sql`delete from organization where id = ${organizationId}`;

      expect(await sumOfEntries(organizationId)).toBe(USD);
      expect(await ledger.getBalance(organizationId)).toEqual({
        balance: 0,
        held: 0,
        available: 0,
      });
    });
  });

  describe("rebuild", () => {
    it("equals the cached balance after a random sequence of entries", async () => {
      const organizationId = await createOrganization();
      // fixed seed, so a failure reproduces
      let state = 20261122;
      const random = () => {
        state = (state * 16807) % 2147483647;
        return state / 2147483647;
      };
      const kinds = ["top_up", "usage", "refund", "promo"] as const;
      const entries = Array.from({ length: 300 }, (_, i) => {
        const kind = kinds[Math.floor(random() * kinds.length)] ?? "usage";
        const magnitude = 1 + Math.floor(random() * 3 * USD);
        return {
          organizationId,
          kind,
          amount:
            kind === "usage" || kind === "refund" ? -magnitude : magnitude,
          idempotencyKey: `random_${i}`,
        };
      });

      // batches of 15 at a time, so entries race for the account row
      for (let i = 0; i < entries.length; i += 15) {
        await Promise.all(entries.slice(i, i + 15).map(ledger.applyEntry));
      }

      const expected = entries.reduce((sum, e) => sum + e.amount, 0);
      const cached = await ledger.getBalance(organizationId);
      expect(cached.balance).toBe(expected);
      expect(await ledger.rebuildBalance(organizationId)).toEqual(cached);
    });

    it("repairs a wrong cache, including the held amount", async () => {
      const organizationId = await createOrganization();
      await ledger.applyEntry({
        organizationId,
        amount: 7 * USD,
        kind: "top_up",
        idempotencyKey: "pay_repair",
      });
      await sql`insert into credit_reservation (id, organization_id, amount, expires_at) values ('res_open', ${organizationId}, 1500000, now() + interval '5 minutes')`;
      await sql`insert into credit_reservation (id, organization_id, amount, expires_at, settled_at) values ('res_done', ${organizationId}, 900000, now(), now())`;
      await sql`update credit_account set balance = 123, held = 456 where organization_id = ${organizationId}`;

      expect(await ledger.rebuildBalance(organizationId)).toEqual({
        balance: 7 * USD,
        held: 1_500_000,
        available: 5_500_000,
      });
      expect(await ledger.getBalance(organizationId)).toEqual({
        balance: 7 * USD,
        held: 1_500_000,
        available: 5_500_000,
      });
    });

    it("creates the account for an organization that has none", async () => {
      const organizationId = await createOrganization();

      expect(await ledger.rebuildBalance(organizationId)).toEqual({
        balance: 0,
        held: 0,
        available: 0,
      });
    });
  });

  describe("staff adjustment", () => {
    it("shows in both the ledger and the balance, with who and why", async () => {
      const organizationId = await createOrganization();
      await ledger.applyEntry({
        organizationId,
        amount: 4 * USD,
        kind: "top_up",
        idempotencyKey: "pay_before_adjustment",
      });

      const result = await adjust.adjustCredits({
        organizationId,
        amount: -1_500_000,
        reason: "refund for the outage on the 20th",
        staffId: "staff_1",
        idempotencyKey: "ticket_42",
      });
      const retried = await adjust.adjustCredits({
        organizationId,
        amount: -1_500_000,
        reason: "refund for the outage on the 20th",
        staffId: "staff_1",
        idempotencyKey: "ticket_42",
      });

      expect(result).toMatchObject({ isApplied: true, balance: 2_500_000 });
      expect(retried).toMatchObject({ isApplied: false, balance: 2_500_000 });

      const [entry] =
        await sql`select kind, amount, reference, metadata from credit_ledger_entry where id = ${result.entryId}`;
      expect(entry).toMatchObject({
        kind: "adjustment",
        amount: "-1500000",
        reference: "staff_1",
        metadata: {
          reason: "refund for the outage on the 20th",
          staffId: "staff_1",
        },
      });
      expect(await sumOfEntries(organizationId)).toBe(2_500_000);
    });
  });

  describe("history", () => {
    it("pages newest first without repeating or skipping an entry", async () => {
      const organizationId = await createOrganization();
      for (let i = 0; i < 7; i++) {
        await ledger.applyEntry({
          organizationId,
          amount: (i + 1) * 1000,
          kind: "top_up",
          idempotencyKey: `page_${i}`,
        });
      }

      const first = await ledger.getEntries({ organizationId, limit: 3 });
      const second = await ledger.getEntries({
        organizationId,
        limit: 3,
        before: first.at(-1)?.id,
      });
      const third = await ledger.getEntries({
        organizationId,
        limit: 3,
        before: second.at(-1)?.id,
      });

      expect([...first, ...second, ...third].map((e) => e.amount)).toEqual([
        7000, 6000, 5000, 4000, 3000, 2000, 1000,
      ]);
    });
  });
});
