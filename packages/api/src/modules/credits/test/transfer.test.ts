import { afterAll, beforeAll, beforeEach, describe, expect, it } from "vitest";

import type { TestDatabase } from "@workspace/db/testing";
import type postgres from "postgres";

// Needs real Postgres: a transfer is about locks, constraints, lots and the
// ledger's invariants. Runs against a throwaway database created next to the
// one in DATABASE_URL and skips when it is not set.
const adminUrl = process.env.DATABASE_URL;

const SAR = 1_000_000;

/** The gateway's own reserve (apps/gateway/plugins/credits/store.go): what an API key can spend. */
const RESERVE_SQL = `
WITH hold AS (
  UPDATE credit_account SET held = held + $3, updated_at = now()
   WHERE organization_id = $2 AND balance - held >= $3
  RETURNING organization_id
)
INSERT INTO credit_reservation (id, organization_id, amount, expires_at, virtual_key_id,
                                usd_to_sar_rate_e6, fx_spread_bps, effective_usd_to_sar_rate_e6)
SELECT $1, organization_id, $3, now() + interval '5 minutes', NULL, 3750000, 0, 3750000 FROM hold
RETURNING id`;

const refusal = (reason: string) => ({
  code: `billing:error.transfer.${reason}`,
});

describe.skipIf(!adminUrl)("credit transfers (BILL-12, ADR 0020)", () => {
  let database: TestDatabase;
  let sql: postgres.Sql;
  let ledger: typeof import("../ledger");
  let transfers: typeof import("../transfer");
  let invariants: typeof import("../../reconciliation/invariants");
  let seq = 0;

  /** A user with a personal account, and an organization they joined a week ago. */
  const setup = async ({ joinedHoursAgo = 168 } = {}) => {
    const n = ++seq;
    const [userId, personalId, organizationId] = [
      `user_${n}`,
      `personal_${n}`,
      `org_${n}`,
    ];
    await sql`insert into "user" (id, name, email, email_verified, created_at, updated_at)
      values (${userId}, ${userId}, ${`${userId}@example.com`}, true, now(), now())`;
    await sql`insert into organization (id, name, slug, created_at)
      values (${personalId}, 'Personal', ${personalId}, now()), (${organizationId}, ${organizationId}, ${organizationId}, now())`;
    await sql`insert into personal_account (user_id, organization_id) values (${userId}, ${personalId})`;
    await sql`insert into member (id, organization_id, user_id, role, created_at) values
      (${`m_p_${n}`}, ${personalId}, ${userId}, 'owner', now()),
      (${`m_o_${n}`}, ${organizationId}, ${userId}, 'member', now() - make_interval(hours => ${joinedHoursAgo}))`;
    return { userId, personalId, organizationId };
  };

  const topUp = (organizationId: string, amount: number) =>
    ledger.applyEntry({
      organizationId,
      amount,
      kind: "top_up",
      idempotencyKey: `pay_${++seq}`,
    });

  const move = (
    who: { userId: string; organizationId: string },
    amountMicroSar: number,
    idempotencyKey = `move_${++seq}`,
  ) =>
    transfers.transferCredits({
      userId: who.userId,
      toOrganizationId: who.organizationId,
      amountMicroSar,
      idempotencyKey,
    });

  const balanceOf = async (organizationId: string) =>
    (await ledger.getBalance(organizationId)).balance;

  /** The reconciliation's own check finds nothing wrong with these accounts or with any transfer. */
  const expectNoFindings = async (...organizationIds: string[]) => {
    const findings = await invariants.checkLedgerInvariants();
    for (const list of [
      findings.balances,
      findings.held,
      findings.grantsWithoutLot,
      findings.lots,
    ]) {
      expect(
        list.filter((row) => organizationIds.includes(row.organizationId)),
      ).toEqual([]);
    }
    expect(findings.transfers).toEqual([]);
  };

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;

    sql = (await import("postgres")).default(database.url, { max: 4 });
    ledger = await import("../ledger");
    transfers = await import("../transfer");
    invariants = await import("../../reconciliation/invariants");
  });

  beforeEach(async () => {
    // no waits unless a test is about them
    await sql`update account_settings set transfer_member_tenure_hours = 0, transfer_cooldown_hours = 0`;
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  it("starts with the waits OpenRouter has in kind: a day for a new member, an hour between transfers", async () => {
    const [row] = await sql`
      select column_default as tenure,
             (select column_default from information_schema.columns
               where table_name = 'account_settings' and column_name = 'transfer_cooldown_hours') as cooldown
        from information_schema.columns
       where table_name = 'account_settings' and column_name = 'transfer_member_tenure_hours'`;
    expect(row).toEqual({ tenure: "24", cooldown: "1" });
  });

  it("moves bought credit: out of the personal account, into the organization, nothing made or lost", async () => {
    const who = await setup();
    await topUp(who.personalId, 100 * SAR);

    const result = await move(who, 40 * SAR);

    expect(result).toMatchObject({ isApplied: true, amountMicroSar: 40 * SAR });
    expect(await balanceOf(who.personalId)).toBe(60 * SAR);
    expect(await balanceOf(who.organizationId)).toBe(40 * SAR);
    const entries = await sql`
      select organization_id, kind::text as kind, amount::int8 as amount, reference
        from credit_ledger_entry where reference = ${result.transferId} order by amount`;
    expect(entries).toEqual([
      {
        organization_id: who.personalId,
        kind: "transfer_out",
        amount: String(-40 * SAR),
        reference: result.transferId,
      },
      {
        organization_id: who.organizationId,
        kind: "transfer_in",
        amount: String(40 * SAR),
        reference: result.transferId,
      },
    ]);
    await expectNoFindings(who.personalId, who.organizationId);
  });

  it("gives the organization a lot with the same expiry and no refund window", async () => {
    const who = await setup();
    await topUp(who.personalId, 100 * SAR);

    await move(who, 25 * SAR);

    const [lots] = await sql`
      select moved.source, moved.granted_micro_sar::int8 as granted, moved.refundable_until,
             moved.expires_at = bought.expires_at as same_expiry, bought.expires_at is not null as expires
        from credit_lot moved, credit_lot bought
       where moved.organization_id = ${who.organizationId} and bought.organization_id = ${who.personalId}`;
    expect(lots).toEqual({
      source: "transfer",
      granted: String(25 * SAR),
      refundable_until: null,
      same_expiry: true,
      expires: true,
    });
  });

  it("moves only bought credit: an adjustment's credit stays", async () => {
    const who = await setup();
    await topUp(who.personalId, 10 * SAR);
    await ledger.applyEntry({
      organizationId: who.personalId,
      amount: 50 * SAR,
      kind: "adjustment",
      idempotencyKey: `gift_${++seq}`,
    });

    expect(
      (await transfers.getTransferOptions(who.userId)).transferableMicroSar,
    ).toBe(10 * SAR);
    await expect(move(who, 20 * SAR)).rejects.toMatchObject(
      refusal("insufficientCredit"),
    );
    await move(who, 10 * SAR);

    expect(await balanceOf(who.personalId)).toBe(50 * SAR);
    expect(await balanceOf(who.organizationId)).toBe(10 * SAR);
    await expectNoFindings(who.personalId, who.organizationId);
  });

  it("counts what is left after spending, and leaves the rest spendable", async () => {
    const who = await setup();
    await topUp(who.personalId, 100 * SAR);
    await sql`insert into credit_ledger_entry (id, organization_id, amount, kind, idempotency_key,
        cost_micro_usd, usd_to_sar_rate_e6, fx_spread_bps, effective_usd_to_sar_rate_e6)
      values (${`use_${++seq}`}, ${who.personalId}, ${-30 * SAR}, 'usage', ${`use_key_${seq}`}, 1, 3750000, 0, 3750000)`;
    await ledger.rebuildBalance(who.personalId);

    expect(
      (await transfers.getTransferOptions(who.userId)).transferableMicroSar,
    ).toBe(70 * SAR);
    await move(who, 70 * SAR);

    expect(await balanceOf(who.personalId)).toBe(0);
    await expectNoFindings(who.personalId, who.organizationId);
  });

  it("takes from the lot that expires first, and each part keeps its own expiry", async () => {
    const who = await setup();
    await sql`update pricing_settings set credit_expiry_days = 365`;
    await topUp(who.personalId, 10 * SAR);
    await sql`update pricing_settings set credit_expiry_days = 30`;
    await topUp(who.personalId, 10 * SAR);
    await sql`update pricing_settings set credit_expiry_days = 365`;

    await move(who, 15 * SAR);

    const moved = await sql`
      select granted_micro_sar::int8 as granted,
             (select round(extract(epoch from (l.expires_at - now())) / 86400)) as days
        from credit_lot l where organization_id = ${who.organizationId} order by expires_at`;
    expect(moved).toEqual([
      { granted: String(10 * SAR), days: "30" },
      { granted: String(5 * SAR), days: "365" },
    ]);
    await expectNoFindings(who.personalId, who.organizationId);
  });

  it("moves nothing twice for the same key, and refuses the key for a different move", async () => {
    const who = await setup();
    await topUp(who.personalId, 100 * SAR);

    const first = await move(who, 30 * SAR, "same-key");
    const second = await move(who, 30 * SAR, "same-key");

    expect(second).toEqual({ ...first, isApplied: false });
    expect(await balanceOf(who.personalId)).toBe(70 * SAR);
    expect(await balanceOf(who.organizationId)).toBe(30 * SAR);
    await expect(move(who, 31 * SAR, "same-key")).rejects.toMatchObject(
      refusal("keyReused"),
    );
  });

  it("lets only one of two transfers at once have the same credit", async () => {
    const who = await setup();
    await topUp(who.personalId, 100 * SAR);

    const results = await Promise.allSettled([
      move(who, 60 * SAR),
      move(who, 60 * SAR),
    ]);

    expect(results.map((result) => result.status).sort()).toEqual([
      "fulfilled",
      "rejected",
    ]);
    expect(await balanceOf(who.personalId)).toBe(40 * SAR);
    expect(await balanceOf(who.organizationId)).toBe(60 * SAR);
    await expectNoFindings(who.personalId, who.organizationId);
  });

  it("does not move credit that requests in flight are holding", async () => {
    const who = await setup();
    await topUp(who.personalId, 100 * SAR);
    await sql.unsafe(RESERVE_SQL, [`hold_${++seq}`, who.personalId, 80 * SAR]);

    expect(
      (await transfers.getTransferOptions(who.userId)).transferableMicroSar,
    ).toBe(20 * SAR);
    await expect(move(who, 21 * SAR)).rejects.toMatchObject(
      refusal("insufficientCredit"),
    );
  });

  it("does not move a top-up that is waiting on a refund", async () => {
    const who = await setup();
    const { lotId } = await topUp(who.personalId, 100 * SAR);
    await sql`insert into credit_refund (id, organization_id, credit_lot_id, amount_micro_sar, provider, payment_id, requested_by)
      values (${`refund_${++seq}`}, ${who.personalId}, ${lotId}, ${100 * SAR}, 'stripe', 'pi_1', ${who.userId})`;

    expect(
      (await transfers.getTransferOptions(who.userId)).transferableMicroSar,
    ).toBe(0);
    await expect(move(who, SAR)).rejects.toMatchObject(
      refusal("insufficientCredit"),
    );
  });

  it("makes the moved credit what an organization key can spend, and no longer a personal key", async () => {
    const who = await setup();
    await topUp(who.personalId, 100 * SAR);
    await move(who, 100 * SAR);

    const personalHold = await sql.unsafe(RESERVE_SQL, [
      `hold_${++seq}`,
      who.personalId,
      SAR,
    ]);
    const organizationHold = await sql.unsafe(RESERVE_SQL, [
      `hold_${++seq}`,
      who.organizationId,
      100 * SAR,
    ]);

    expect(personalHold).toHaveLength(0);
    expect(organizationHold).toHaveLength(1);
  });

  describe("who may move credit where", () => {
    it("refuses an organization the user is not a member of", async () => {
      const [who, other] = [await setup(), await setup()];
      await topUp(who.personalId, 10 * SAR);

      await expect(
        move({ userId: who.userId, organizationId: other.organizationId }, SAR),
      ).rejects.toMatchObject(refusal("notMember"));
    });

    it("refuses a personal account as the destination, the user's own or another's", async () => {
      const [who, other] = [await setup(), await setup()];
      await topUp(who.personalId, 10 * SAR);

      for (const organizationId of [who.personalId, other.personalId]) {
        await expect(
          move({ userId: who.userId, organizationId }, SAR),
        ).rejects.toMatchObject(refusal("notAnOrganization"));
      }
    });

    it("never moves credit out of an organization: the source is always the caller's personal account", async () => {
      const who = await setup();
      await topUp(who.organizationId, 10 * SAR);

      await expect(move(who, SAR)).rejects.toMatchObject(
        refusal("insufficientCredit"),
      );
      expect(await balanceOf(who.organizationId)).toBe(10 * SAR);
    });

    it("lists the user's organizations as destinations, never a personal account", async () => {
      const who = await setup();

      const options = await transfers.getTransferOptions(who.userId);

      expect(options.personalOrganizationId).toBe(who.personalId);
      expect(options.destinations).toEqual([
        {
          id: who.organizationId,
          name: who.organizationId,
          slug: who.organizationId,
          availableAt: null,
          waitingFor: null,
        },
      ]);
    });
  });

  describe("the two waits", () => {
    it("makes a new member wait before the organization can receive their credit", async () => {
      await sql`update account_settings set transfer_member_tenure_hours = 24`;
      const [fresh, settled] = [
        await setup({ joinedHoursAgo: 1 }),
        await setup({ joinedHoursAgo: 25 }),
      ];
      await topUp(fresh.personalId, 10 * SAR);
      await topUp(settled.personalId, 10 * SAR);

      await expect(move(fresh, SAR)).rejects.toMatchObject(
        refusal("memberTooNew"),
      );
      await expect(move(settled, SAR)).resolves.toMatchObject({
        isApplied: true,
      });
      const [destination] = (await transfers.getTransferOptions(fresh.userId))
        .destinations;
      expect(destination).toMatchObject({ waitingFor: "memberTenure" });
      expect(
        new Date(destination?.availableAt ?? 0).getTime() - Date.now(),
      ).toBeGreaterThan(22 * 3_600_000);
    });

    it("makes an organization that just received a transfer wait for the next", async () => {
      await sql`update account_settings set transfer_cooldown_hours = 1`;
      const who = await setup();
      await topUp(who.personalId, 10 * SAR);

      await move(who, SAR);

      await expect(move(who, SAR)).rejects.toMatchObject(
        refusal("organizationCoolingDown"),
      );
      expect(
        (await transfers.getTransferOptions(who.userId)).destinations[0],
      ).toMatchObject({ waitingFor: "cooldown" });
      expect(await balanceOf(who.organizationId)).toBe(SAR);
    });
  });

  it("refuses the sixth attempt of a user within a minute, and counts each user apart", async () => {
    const now = Date.now();
    for (let attempt = 0; attempt < 5; attempt++) {
      await transfers.countTransferAttempt("user_guessing", now);
    }

    await expect(
      transfers.countTransferAttempt("user_guessing", now + 1000),
    ).rejects.toMatchObject(refusal("tooManyAttempts"));
    await expect(
      transfers.countTransferAttempt("user_other", now),
    ).resolves.toBeUndefined();
    // a minute after the window began, the count starts again
    await expect(
      transfers.countTransferAttempt("user_guessing", now + 61_000),
    ).resolves.toBeUndefined();
  });

  describe("reconciliation", () => {
    it("finds a transfer whose entries do not add up to what it records", async () => {
      await sql`insert into credit_transfer (id, from_organization_id, to_organization_id, amount_micro_sar, requested_by, idempotency_key)
        values ('transfer_broken', 'a', 'b', ${SAR}, 'u', 'broken')`;

      const findings = await invariants.checkLedgerInvariants();

      expect(findings.transfers).toEqual([
        { transferId: "transfer_broken", amountMicroSar: SAR, out: 0, in: 0 },
      ]);
      expect(invariants.hasLedgerFindings(findings)).toBe(true);
    });

    it("keeps the record of a transfer: it cannot be changed or deleted", async () => {
      await expect(
        sql`delete from credit_transfer where id = 'transfer_broken'`,
      ).rejects.toThrow(/append-only/);
    });
  });
});
