import { readFile } from "node:fs/promises";
import { afterAll, beforeAll, describe, expect, it, vi } from "vitest";

import type { TestDatabase } from "@workspace/db/testing";
import type postgres from "postgres";

// Needs real Postgres: the personal account is rows in three tables and a
// migration backfill. Runs against a throwaway database created next to the
// one in DATABASE_URL and skips when it is not set.
const adminUrl = process.env.DATABASE_URL;

const removeOrganizationFromGateway = vi.fn<(id: string) => Promise<void>>();
vi.mock("@workspace/gateway-client/server", () => ({
  removeOrganizationFromGateway,
}));
vi.mock("@workspace/billing/server", () => ({
  getCustomersWithPurchasesByReferenceId: () => Promise.resolve([]),
  getOrganizationBillableSeatsCount: () => Promise.resolve(0),
}));
vi.mock("@workspace/billing-web/server", () => ({}));

// the hooks read only the ids; the rest of Better Auth's objects is not needed
const hookArgs = (organizationId: string) =>
  ({
    organization: { id: organizationId },
    member: { organizationId },
  }) as never;

describe.skipIf(!adminUrl)("personal accounts (ADR 0020)", () => {
  let database: TestDatabase;
  let sql: postgres.Sql;
  let personal: typeof import("../personal");
  let server: typeof import("../server");
  let hooks: typeof import("../../hooks").hooks;
  let seq = 0;

  const createUser = async () => {
    const id = `user_${++seq}`;
    await sql`insert into "user" (id, name, email, email_verified, created_at, updated_at)
      values (${id}, ${id}, ${`${id}@example.com`}, true, now(), now())`;
    return id;
  };

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;

    sql = (await import("postgres")).default(database.url, { max: 2 });
    personal = await import("../personal");
    server = await import("../server");
    ({ hooks } = await import("../../hooks"));
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  it("gives a new user one personal account, with them as its only owner", async () => {
    const userId = await createUser();

    await hooks.user.create.after({ id: userId } as never);

    const organizationId = await personal.getPersonalOrganizationId(userId);
    expect(organizationId).toEqual(expect.any(String));
    const members =
      await sql`select user_id, role from member where organization_id = ${organizationId}`;
    expect(members).toEqual([{ user_id: userId, role: "owner" }]);
    const [organization] =
      await sql`select name from organization where id = ${organizationId}`;
    expect(organization?.name).toBe("Personal");
  });

  it("does not create a second one for the same user", async () => {
    const userId = await createUser();

    const first = await personal.createPersonalAccount(userId);
    const second = await personal.createPersonalAccount(userId);

    expect(second).toBe(first);
    const rows = await sql`select 1 from member where user_id = ${userId}`;
    expect(rows).toHaveLength(1);
  });

  it.each([
    ["inviting into it", "beforeCreateInvitation"],
    ["accepting an invitation to it", "beforeAcceptInvitation"],
    ["adding a member", "beforeAddMember"],
    ["removing its member", "beforeRemoveMember"],
    ["changing a role in it", "beforeUpdateMemberRole"],
    ["renaming it", "beforeUpdateOrganization"],
    ["deleting it", "beforeDeleteOrganization"],
  ] as const)("refuses %s", async (_, hook) => {
    const organizationId = await personal.createPersonalAccount(
      await createUser(),
    );

    await expect(
      hooks.organization[hook](hookArgs(organizationId)),
    ).rejects.toThrow(personal.PERSONAL_ACCOUNT_CLOSED_MESSAGE);
    expect(removeOrganizationFromGateway).not.toHaveBeenCalled();
  });

  it("leaves an ordinary organization open to the same changes", async () => {
    await sql`insert into organization (id, name, slug, created_at) values ('org_open', 'Open', 'open', now())`;

    await expect(
      hooks.organization.beforeCreateInvitation(hookArgs("org_open")),
    ).resolves.toBeUndefined();
    await expect(
      hooks.organization.beforeUpdateOrganization(hookArgs("org_open")),
    ).resolves.toBeUndefined();
  });

  it("does not stop its user deleting their account, and goes with them", async () => {
    const userId = await createUser();
    const organizationId = await personal.createPersonalAccount(userId);
    const user = { id: userId } as never;

    expect(
      await server.getOrganizationsBlockingAccountDeletion({ userId }),
    ).toEqual([]);

    await hooks.user.delete.before(user);
    expect(removeOrganizationFromGateway).toHaveBeenCalledWith(organizationId);
    await sql`delete from "user" where id = ${userId}`;
    await hooks.user.delete.after(user);

    expect(
      await sql`select 1 from organization where id = ${organizationId}`,
    ).toHaveLength(0);
  });

  it("still blocks deletion for the sole owner of an organization", async () => {
    const userId = await createUser();
    await personal.createPersonalAccount(userId);
    await sql`insert into organization (id, name, slug, created_at) values ('org_sole', 'Sole', 'sole', now())`;
    await sql`insert into member (id, organization_id, user_id, role, created_at) values ('m_sole', 'org_sole', ${userId}, 'owner', now())`;

    const blocking = await server.getOrganizationsBlockingAccountDeletion({
      userId,
    });

    expect(blocking.map((organization) => organization.id)).toEqual([
      "org_sole",
    ]);
  });

  describe("the limit on organizations owned", () => {
    const own = async (userId: string, organizations: number) => {
      for (let i = 0; i < organizations; i++) {
        const id = `org_owned_${++seq}`;
        await sql`insert into organization (id, name, slug, created_at) values (${id}, ${id}, ${id}, now())`;
        await sql`insert into member (id, organization_id, user_id, role, created_at) values (${`m_${id}`}, ${id}, ${userId}, 'owner', now())`;
      }
    };
    const create = (userId: string) =>
      hooks.organization.beforeCreateOrganization({
        organization: {},
        user: { id: userId },
      } as never);

    it("starts at 5 for everyone and does not count the personal account", async () => {
      const userId = await createUser();
      await personal.createPersonalAccount(userId);
      await own(userId, 4);

      await expect(create(userId)).resolves.toBeUndefined();

      await own(userId, 1);
      await expect(create(userId)).rejects.toThrow(
        personal.organizationLimitMessage(5),
      );
    });

    it("does not count organizations the user is only a member of", async () => {
      const userId = await createUser();
      for (let i = 0; i < 6; i++) {
        const id = `org_joined_${i}`;
        await sql`insert into organization (id, name, slug, created_at) values (${id}, ${id}, ${id}, now())`;
        await sql`insert into member (id, organization_id, user_id, role, created_at) values (${`m_${id}`}, ${id}, ${userId}, 'member', now())`;
      }

      await expect(create(userId)).resolves.toBeUndefined();
    });

    it("follows the general setting, and one user's own limit over it", async () => {
      const [limited, raised] = [await createUser(), await createUser()];
      await own(limited, 1);
      await own(raised, 1);
      await sql`update account_settings set max_owned_organizations = 1`;
      await sql`insert into user_organization_limit (user_id, max_owned_organizations) values (${raised}, 2)`;

      await expect(create(limited)).rejects.toThrow(
        personal.organizationLimitMessage(1),
      );
      await expect(create(raised)).resolves.toBeUndefined();

      await sql`update account_settings set max_owned_organizations = 5`;
    });
  });

  it("backfills an existing user in the migration", async () => {
    const userId = await createUser();
    const migration = await readFile(
      new URL(
        "../../../../db/migrations/0042_personal_accounts.sql",
        import.meta.url,
      ),
      "utf8",
    );
    const backfill = migration.split("--> statement-breakpoint").at(-1) ?? "";
    // the migration runs on a database where nobody has one yet
    await sql`delete from organization where id in (select organization_id from personal_account)`;

    await sql.unsafe(backfill);

    const [row] = await sql`
      select o.name, m.role from personal_account p
      join organization o on o.id = p.organization_id
      join member m on m.organization_id = o.id and m.user_id = p.user_id
      where p.user_id = ${userId}`;
    expect(row).toEqual({ name: "Personal", role: "owner" });
    const [counts] = await sql`
      select (select count(*) from "user")::int as users,
             (select count(*) from personal_account)::int as accounts`;
    expect(counts?.accounts).toBe(counts?.users);
  });
});
