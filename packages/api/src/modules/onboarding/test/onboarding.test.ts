import { afterAll, beforeAll, describe, expect, it, vi } from "vitest";

import { fakeGateway } from "../../keys/test/fake-gateway";

import type { TestDatabase } from "@workspace/db/testing";
import type postgres from "postgres";

// The welcome wizard's state and its starter key (ONB-03, ONB-08), against
// real Postgres with an in-memory gateway. Skips without DATABASE_URL.
const adminUrl = process.env.DATABASE_URL;

let sql: postgres.Sql;

// The auth server cannot load under vitest (it renders emails). These are the
// two functions the module takes from it, as packages/auth/src/lib/personal.ts
// has them, where they are tested against the same tables.
vi.mock("@workspace/auth/server", () => ({
  getPersonalAccount: async (userId: string) => {
    const [row] = await sql`
      select o.id, o.slug, p.onboarded_at as "onboardedAt"
        from personal_account p join organization o on o.id = p.organization_id
       where p.user_id = ${userId}`;
    return row;
  },
  completeOnboarding: async (userId: string, accountType: string) => {
    await sql`update personal_account set onboarded_at = now(), account_type = ${accountType}
               where user_id = ${userId} and onboarded_at is null`;
  },
}));

const refusal = (reason: string) => ({
  code: `common:onboarding.error.${reason}`,
});

describe.skipIf(!adminUrl)("onboarding (ONB-03, ONB-08)", () => {
  let database: TestDatabase;
  let onboarding: typeof import("../onboarding");
  let seq = 0;

  /** A new user with their personal account, and an organization they own. */
  const newUser = async () => {
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
      (${`m_p_${n}`}, ${personalId}, ${userId}, 'owner', now()), (${`m_o_${n}`}, ${organizationId}, ${userId}, 'owner', now())`;
    return { userId, personalId, organizationId };
  };

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;

    sql = (await import("postgres")).default(database.url, { max: 2 });
    onboarding = await import("../onboarding");
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  it("is still to be done for a new user", async () => {
    const who = await newUser();

    expect(await onboarding.getOnboarding(who.userId)).toEqual({
      isCompleted: false,
      personal: { id: who.personalId, slug: who.personalId },
      hasStarterKey: false,
    });
  });

  it("mints one starter key in the personal account, from the platform's key defaults", async () => {
    const who = await newUser();
    const gateway = fakeGateway().client;
    await sql`update key_settings set default_limit_micro_sar = 50000000`;

    const starter = await onboarding.createStarterKey(
      { userId: who.userId, organizationId: who.personalId },
      gateway,
    );

    expect(starter.key).toEqual(expect.any(String));
    expect(starter.limitMicroSar).toBe(50_000_000);
    expect(starter.expiresAt).toBeInstanceOf(Date);
    const keys =
      await sql`select name, organization_id from api_key where created_by = ${who.userId}`;
    expect(keys).toEqual([
      { name: "Starter key", organization_id: who.personalId },
    ]);
    await expect(
      onboarding.createStarterKey(
        { userId: who.userId, organizationId: who.personalId },
        gateway,
      ),
    ).rejects.toMatchObject(refusal("starterKeyExists"));
    await sql`update key_settings set default_limit_micro_sar = null`;
  });

  it("mints it in an organization the user owns, and in nobody else's", async () => {
    const [who, other] = [await newUser(), await newUser()];
    const gateway = fakeGateway().client;

    await expect(
      onboarding.createStarterKey(
        { userId: who.userId, organizationId: other.organizationId },
        gateway,
      ),
    ).rejects.toMatchObject(refusal("notOwner"));
    await expect(
      onboarding.createStarterKey(
        { userId: who.userId, organizationId: other.personalId },
        gateway,
      ),
    ).rejects.toMatchObject(refusal("notOwner"));

    await onboarding.createStarterKey(
      { userId: who.userId, organizationId: who.organizationId },
      gateway,
    );

    const keys =
      await sql`select organization_id from api_key where created_by = ${who.userId}`;
    expect(keys).toEqual([{ organization_id: who.organizationId }]);
  });

  it("ends once, keeps the first answer, and mints no starter key afterwards", async () => {
    const who = await newUser();

    const done = await onboarding.finishOnboarding(who.userId, "individual");
    await onboarding.finishOnboarding(who.userId, "organization");

    expect(done.isCompleted).toBe(true);
    const [row] =
      await sql`select account_type from personal_account where user_id = ${who.userId}`;
    expect(row?.account_type).toBe("individual");
    await expect(
      onboarding.createStarterKey(
        { userId: who.userId, organizationId: who.personalId },
        fakeGateway().client,
      ),
    ).rejects.toMatchObject(refusal("completed"));
  });
});
