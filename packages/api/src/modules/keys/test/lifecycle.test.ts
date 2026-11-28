import {
  afterAll,
  beforeAll,
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from "vitest";

import { fakeGateway } from "./fake-gateway";

import type { TestDatabase } from "@workspace/db/testing";
import type { GatewayClient } from "@workspace/gateway-client/server";
import type postgres from "postgres";

// The key lifecycle (KEYS-03, -04, -05, -06, -11) and the key defaults
// (ADMIN-12), through the keys router as the web app calls it, against real
// Postgres, with an in-memory gateway. Skips without DATABASE_URL.
const adminUrl = process.env.DATABASE_URL;

const SAR = 1_000_000;
const HOUR = 3_600_000;
const DAY = 24 * HOUR;

const gatewayRef = vi.hoisted(() => ({
  current: undefined as GatewayClient | undefined,
}));

vi.mock("@workspace/gateway-client/server", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("@workspace/gateway-client/server")>();
  return {
    ...actual,
    getGateway: () => {
      if (!gatewayRef.current) {
        throw new Error("no gateway for this test");
      }
      return gatewayRef.current;
    },
  };
});

/** The organization roles' real permissions, behind a session the test picks by header. */
vi.mock("@workspace/auth/server", async () => {
  const { roles } = await import("@workspace/auth");
  const roleOf = (headers: Headers) =>
    (headers.get("x-test-role") ?? "member") as keyof typeof roles;
  return {
    auth: {
      api: {
        getSession: async ({ headers }: { headers: Headers }) => ({
          user: { id: `user_${roleOf(headers)}` },
        }),
        hasPermission: async ({
          body,
          headers,
        }: {
          body: { permissions: Record<string, string[]> };
          headers: Headers;
        }) => ({
          success: roles[roleOf(headers)].authorize(body.permissions).success,
        }),
      },
    },
    clientIp: () => null,
    recordStaffAction: async () => undefined,
  };
});
vi.mock("@workspace/i18n", async () => {
  const z = await import("zod");
  return { getValidationLocale: () => z.locales.en };
});
vi.mock("@workspace/i18n/server", () => ({ getLocaleFromRequest: () => "en" }));

/** The keys router, as the web app calls it, for a member of the given role. */
const call = async (
  method: string,
  path: string,
  { role = "owner", json }: { role?: string; json?: unknown } = {},
) => {
  const { Hono } = await import("hono");
  const { HttpException } = await import("@workspace/shared/utils");
  const { keysRouter } = await import("../router");
  const app = new Hono().route("/api/keys", keysRouter).onError((error) =>
    Response.json(
      {
        code: error instanceof HttpException ? error.code : error.message,
        message: error.message,
      },
      { status: error instanceof HttpException ? error.status : 500 },
    ),
  );
  return app.request(`/api/keys${path}`, {
    method,
    headers: {
      "x-test-role": role,
      ...(json === undefined ? {} : { "Content-Type": "application/json" }),
    },
    body: json === undefined ? undefined : JSON.stringify(json),
  });
};

interface KeyBody {
  id: string;
  name: string;
  prefix: string;
  limitMicroSar: number | null;
  limitReset: string | null;
  expiresAt: string | null;
  disabledAt: string | null;
  status: string;
}

describe.skipIf(!adminUrl)("the key lifecycle", () => {
  let database: TestDatabase;
  let sql: postgres.Sql;
  let gateway: ReturnType<typeof fakeGateway>;
  let keys: typeof import("../keys");
  let settings: typeof import("../settings");
  let lifetime: typeof import("../lifetime");
  let schema: typeof import("../../../schema");
  let seq = 0;

  const createOrganization = async () => {
    const id = `org_lifecycle_${++seq}`;
    await sql`insert into organization (id, name, slug, created_at) values (${id}, ${id}, ${id}, now())`;
    return id;
  };

  const setSettings = async (values: {
    defaultLimitMicroSar?: number | null;
    defaultLimitReset?: string | null;
    defaultExpirySeconds?: number | null;
    maxLifetimeSeconds?: number | null;
  }) => {
    await sql`update key_settings set
      default_limit_micro_sar = ${values.defaultLimitMicroSar ?? null},
      default_limit_reset = ${values.defaultLimitReset ?? null},
      default_expiry_seconds = ${values.defaultExpirySeconds === undefined ? 7_776_000 : values.defaultExpirySeconds},
      max_lifetime_seconds = ${values.maxLifetimeSeconds ?? null}
      where id = 1`;
  };

  const create = async (
    organizationId: string,
    json: Record<string, unknown>,
  ) => {
    const response = await call("POST", `/${organizationId}`, { json });
    return {
      response,
      body: (await response.json()) as {
        key: string;
        apiKey: KeyBody;
        code?: string;
      },
    };
  };

  const storedExpiry = async (keyId: string) => {
    const [row] = await sql<
      { expires_at: Date | null; virtual_key_id: string }[]
    >`select expires_at, virtual_key_id from api_key where id = ${keyId}`;
    return row;
  };

  let entrySeq = 0;
  /** A charge on the key, as the credits plugin writes it; `at` is SQL. */
  const usage = async (
    organizationId: string,
    virtualKeyId: string,
    amount: number,
    at = "now() AT TIME ZONE 'UTC'",
  ) => {
    const id = `entry_lifecycle_${++entrySeq}`;
    await sql.unsafe(
      `insert into credit_ledger_entry (id, organization_id, amount, kind, idempotency_key, virtual_key_id, created_at,
         cost_micro_usd, usd_to_sar_rate_e6, fx_spread_bps, effective_usd_to_sar_rate_e6)
       values ($1, $2, $3, 'usage', $1, $4, ${at}, $5, 3750000, 0, 3750000)`,
      [id, organizationId, -amount, virtualKeyId, Math.round(amount / 3.75)],
    );
  };

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;
    sql = (await import("postgres")).default(database.url, { max: 4 });
    keys = await import("../keys");
    settings = await import("../settings");
    lifetime = await import("../lifetime");
    schema = await import("../../../schema");
  });

  beforeEach(async () => {
    gateway = fakeGateway();
    gatewayRef.current = gateway.client;
    await setSettings({});
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  describe("defaults and expiry (ADMIN-12, KEYS-03)", () => {
    it("starts a new key from the platform's default limit, reset and expiry, and tells the form", async () => {
      const org = await createOrganization();
      await setSettings({
        defaultLimitMicroSar: 5 * SAR,
        defaultLimitReset: "weekly",
        defaultExpirySeconds: 604_800,
      });
      const before = Date.now();
      const { apiKey } = await keys.createKey(
        { organizationId: org, userId: "u", name: "defaults" },
        gateway.client,
      );
      expect(apiKey).toMatchObject({
        limitMicroSar: 5 * SAR,
        limitReset: "weekly",
      });
      const expiresAt = apiKey.expiresAt?.getTime() ?? 0;
      expect(expiresAt).toBeGreaterThanOrEqual(before + 7 * DAY);
      expect(expiresAt).toBeLessThanOrEqual(Date.now() + 7 * DAY);

      // an explicit "no limit" is not replaced by the default
      const { apiKey: open } = await keys.createKey(
        { organizationId: org, userId: "u", name: "open", limit: null },
        gateway.client,
      );
      expect(open.limitMicroSar).toBeNull();

      // the create form reads the same defaults; a member may read them
      const response = await call("GET", `/${org}/settings`, {
        role: "member",
      });
      expect(response.status).toBe(200);
      expect(await response.json()).toEqual({
        defaultLimitMicroSar: 5 * SAR,
        defaultLimitReset: "weekly",
        defaultExpiry: "7d",
        maxLifetime: null,
        limitPresetsMicroSar: [50 * SAR, 200 * SAR, 1000 * SAR],
      });
    });

    it("refuses an expiry beyond the maximum lifetime, and no expiration while one is set, before the gateway is asked", async () => {
      const org = await createOrganization();
      await setSettings({
        defaultExpirySeconds: 604_800,
        maxLifetimeSeconds: 604_800,
      });
      for (const expiry of ["30d", "1y", "never"]) {
        const { response, body } = await create(org, { name: "x", expiry });
        expect({ expiry, status: response.status }).toEqual({
          expiry,
          status: 422,
        });
        expect(body.code).toBe(
          "organization:keys.error.expiryBeyondMaxLifetime",
        );
      }
      expect(gateway.state.keys.size).toBe(0);

      const before = Date.now();
      const { response, body } = await create(org, {
        name: "week",
        expiry: "7d",
      });
      expect(response.status).toBe(201);
      const expiresAt = new Date(body.apiKey.expiresAt ?? 0).getTime();
      expect(expiresAt).toBeGreaterThanOrEqual(before + 7 * DAY);
      expect(expiresAt).toBeLessThanOrEqual(Date.now() + 7 * DAY);
    });

    it("requires an expiry, and gives the gateway exactly the expiry it records", async () => {
      const org = await createOrganization();
      const missing = await create(org, { name: "no expiry" });
      expect(missing.response.status).toBe(422);

      const { response, body } = await create(org, {
        name: "hour",
        expiry: "1h",
      });
      expect(response.status).toBe(201);
      const stored = await storedExpiry(body.apiKey.id);
      const sent = gateway.state.keys.get(stored?.virtual_key_id ?? "");
      expect(stored?.expires_at).toBeInstanceOf(Date);
      expect(sent?.expiresAt?.getTime()).toBe(stored?.expires_at?.getTime());

      // "never" without a maximum: no expiry in either place
      const never = await create(org, { name: "never", expiry: "never" });
      expect(never.response.status).toBe(201);
      const neverStored = await storedExpiry(never.body.apiKey.id);
      expect(neverStored?.expires_at).toBeNull();
      expect(
        gateway.state.keys.get(neverStored?.virtual_key_id ?? "")?.expiresAt,
      ).toBeNull();
    });

    it("refuses any attempt to change a key's expiry", async () => {
      const org = await createOrganization();
      const { body } = await create(org, { name: "fixed", expiry: "1d" });
      const before = await storedExpiry(body.apiKey.id);

      for (const json of [
        { expiresAt: new Date(Date.now() + 365 * DAY).toISOString() },
        { expiry: "1y" },
        { name: "renamed", expiresAt: null },
      ]) {
        const response = await call("PATCH", `/${org}/${body.apiKey.id}`, {
          json,
        });
        expect(response.status).toBe(422);
        expect(((await response.json()) as { message: string }).message).toBe(
          schema.EXPIRY_IMMUTABLE_MESSAGE,
        );
      }
      expect(await storedExpiry(body.apiKey.id)).toEqual(before);
      expect(gateway.state.updates).toEqual([]);
      // the name still changes alone
      const renamed = await call("PATCH", `/${org}/${body.apiKey.id}`, {
        json: { name: "renamed" },
      });
      expect(renamed.status).toBe(200);
      expect(await storedExpiry(body.apiKey.id)).toEqual(before);
    });
  });

  describe("the maximum lifetime on existing keys (ADMIN-12)", () => {
    it("cuts every key that would outlive a new maximum to created_at + maximum, in the gateway and here, once", async () => {
      const org = await createOrganization();
      const make = async (name: string, expiresAt: Date | null) =>
        (
          await keys.createKey(
            { organizationId: org, userId: "u", name, expiresAt },
            gateway.client,
          )
        ).apiKey;
      const forever = await make("forever", null);
      const year = await make("year", new Date(Date.now() + 365 * DAY));
      const hour = await make("hour", new Date(Date.now() + HOUR));
      const old = await make("old", null);
      // made three days ago: a one-day maximum has already passed for it
      await sql`update api_key set created_at = created_at - interval '3 days' where id = ${old.id}`;

      await setSettings({
        defaultExpirySeconds: 86_400,
        maxLifetimeSeconds: 86_400,
      });
      const started = Date.now();
      const first = await lifetime.applyMaxLifetime({
        maxLifetimeSeconds: 86_400,
        gateway: gateway.client,
      });
      // every organization's keys: the three here, and any other test's
      expect(first.failed).toBe(0);
      expect(first.shortened).toBeGreaterThanOrEqual(3);

      for (const key of [forever, year]) {
        const stored = await storedExpiry(key.id);
        expect(stored?.expires_at?.getTime()).toBe(
          key.createdAt.getTime() + DAY,
        );
        expect(
          gateway.state.keys.get(stored?.virtual_key_id ?? "")?.expiresAt,
        ).toEqual(stored?.expires_at);
      }
      // past its cut-off: it stops within seconds, the same moment in both places
      const oldStored = await storedExpiry(old.id);
      const oldExpiry = oldStored?.expires_at?.getTime() ?? 0;
      expect(oldExpiry).toBeGreaterThan(started);
      expect(oldExpiry).toBeLessThanOrEqual(Date.now() + 5_000);
      expect(
        gateway.state.keys.get(oldStored?.virtual_key_id ?? "")?.expiresAt,
      ).toEqual(oldStored?.expires_at);
      // already within the maximum: untouched
      expect((await storedExpiry(hour.id))?.expires_at?.getTime()).toBe(
        hour.expiresAt?.getTime(),
      );

      // a second run changes nothing, in the gateway or here
      const updates = gateway.state.updates.length;
      expect(
        await lifetime.applyMaxLifetime({
          maxLifetimeSeconds: 86_400,
          gateway: gateway.client,
        }),
      ).toEqual({ shortened: 0, failed: 0 });
      expect(gateway.state.updates).toHaveLength(updates);
    });

    it("leaves a key alone when the gateway cannot take its new expiry, and the next run finishes it", async () => {
      const org = await createOrganization();
      const { apiKey } = await keys.createKey(
        { organizationId: org, userId: "u", name: "later", expiresAt: null },
        gateway.client,
      );
      gateway.state.failUpdateKey = true;
      const failed = await lifetime.applyMaxLifetime({
        maxLifetimeSeconds: 2_592_000,
        gateway: gateway.client,
      });
      expect(failed.failed).toBeGreaterThanOrEqual(1);
      expect((await storedExpiry(apiKey.id))?.expires_at).toBeNull();

      gateway.state.failUpdateKey = false;
      await lifetime.applyMaxLifetime({
        maxLifetimeSeconds: 2_592_000,
        gateway: gateway.client,
      });
      expect((await storedExpiry(apiKey.id))?.expires_at?.getTime()).toBe(
        apiKey.createdAt.getTime() + 30 * DAY,
      );
    });

    it("records each change of the defaults with before and after, and refuses a default beyond the maximum", async () => {
      const change = await settings.updateKeySettings({
        staffId: "staff_1",
        defaultLimitMicroSar: 20 * SAR,
        defaultLimitReset: "monthly",
        defaultExpiry: "30d",
        maxLifetime: "90d",
        limitPresetsMicroSar: [100 * SAR, 10 * SAR, 100 * SAR],
      });
      expect(change.after).toEqual({
        defaultLimitMicroSar: 20 * SAR,
        defaultLimitReset: "monthly",
        defaultExpiry: "30d",
        maxLifetime: "90d",
        limitPresetsMicroSar: [10 * SAR, 100 * SAR],
      });
      const [row] = await sql<
        { before: unknown; after: unknown; changed_by: string }[]
      >`select before, after, changed_by from key_settings_change order by id desc limit 1`;
      expect(row).toEqual({
        before: change.before,
        after: change.after,
        changed_by: "staff_1",
      });
      // append-only
      await expect(
        sql`delete from key_settings_change where changed_by = 'staff_1'`,
      ).rejects.toThrow(/append-only/);

      // the API and the database both refuse a default that outlives the maximum
      const invalid = {
        defaultLimitMicroSar: null,
        defaultLimitReset: null,
        maxLifetime: "7d",
        limitPresetsMicroSar: [SAR],
      };
      for (const defaultExpiry of ["30d", "never"]) {
        expect(
          schema.updateKeySettingsSchema.safeParse({
            ...invalid,
            defaultExpiry,
          }).success,
        ).toBe(false);
      }
      expect(
        schema.updateKeySettingsSchema.safeParse({
          ...invalid,
          defaultExpiry: "7d",
        }).success,
      ).toBe(true);
      await expect(
        sql`update key_settings set default_expiry_seconds = 2592000, max_lifetime_seconds = 604800`,
      ).rejects.toThrow(/key_settings_default_within_max/);
      await expect(
        sql`update key_settings set default_expiry_seconds = null, max_lifetime_seconds = 604800`,
      ).rejects.toThrow(/key_settings_default_within_max/);
      await expect(
        sql`update key_settings set max_lifetime_seconds = 40000000`,
      ).rejects.toThrow(/key_settings_max_lifetime_value/);
    });
  });

  describe("search, status and usage (KEYS-04, -05, -06, -11)", () => {
    it("finds a key by a pasted key, by name or by prefix, and never sends the key back", async () => {
      const org = await createOrganization();
      const other = await createOrganization();
      const prod = await create(org, { name: "Production", expiry: "30d" });
      await create(org, { name: "Staging 100%", expiry: "30d" });
      const search = async (organizationId: string, query: string) => {
        const response = await call("POST", `/${organizationId}/search`, {
          role: "member",
          json: { query },
        });
        expect(response.status).toBe(200);
        const text = await response.text();
        return {
          text,
          names: (JSON.parse(text) as KeyBody[]).map((k) => k.name),
        };
      };

      const pasted = await search(org, `  ${prod.body.key} `);
      expect(pasted.names).toEqual(["Production"]);
      expect(pasted.text).not.toContain(prod.body.key);
      expect(pasted.text).not.toContain(keys.hashKey(prod.body.key));
      expect(pasted.text).not.toContain("virtualKeyId");

      expect((await search(org, "staging")).names).toEqual(["Staging 100%"]);
      // LIKE's wildcards are taken literally
      expect((await search(org, "%")).names).toEqual(["Staging 100%"]);
      expect((await search(org, "_")).names).toEqual([]);
      expect(
        (await search(org, prod.body.apiKey.prefix.slice(0, 9))).names,
      ).toEqual(expect.arrayContaining(["Production"]));
      // another organization's key matches nothing
      expect((await search(other, prod.body.key)).names).toEqual([]);
      // the key is never in a URL: there is no search by query string
      const list = await call(
        "GET",
        `/${org}?search=${encodeURIComponent(prod.body.key)}`,
        { role: "member" },
      );
      expect(((await list.json()) as KeyBody[]).length).toBe(2);
    });

    it("shows each key as enabled, disabled or expired", async () => {
      const org = await createOrganization();
      const enabled = await create(org, { name: "on", expiry: "1d" });
      const disabled = await create(org, { name: "off", expiry: "1d" });
      const expired = await create(org, { name: "gone", expiry: "1h" });
      await call("PATCH", `/${org}/${disabled.body.apiKey.id}`, {
        json: { disabled: true },
      });
      // a disabled key that has also expired shows expired: it cannot work again
      await sql`update api_key set created_at = now() - interval '2 hours', expires_at = now() - interval '1 hour',
                disabled_at = now() where id = ${expired.body.apiKey.id}`;
      const list = (await (
        await call("GET", `/${org}`, { role: "member" })
      ).json()) as KeyBody[];
      const statusOf = (id: string) => list.find((k) => k.id === id)?.status;
      expect(statusOf(enabled.body.apiKey.id)).toBe("enabled");
      expect(statusOf(disabled.body.apiKey.id)).toBe("disabled");
      expect(statusOf(expired.body.apiKey.id)).toBe("expired");

      // enabled again
      await call("PATCH", `/${org}/${disabled.body.apiKey.id}`, {
        json: { disabled: false },
      });
      const page = (await (
        await call("GET", `/${org}/${disabled.body.apiKey.id}`, {
          role: "member",
        })
      ).json()) as KeyBody;
      expect(page.status).toBe("enabled");
      expect(gateway.state.updates.map(([, input]) => input)).toEqual([
        { isActive: false },
        { isActive: true },
      ]);
    });

    it("gives usage figures equal to the ledger's sums, the same on the list, the page and GET /api/v1/key", async () => {
      const org = await createOrganization();
      const { body } = await create(org, {
        name: "spender",
        expiry: "30d",
        limit: { limitMicroSar: 50 * SAR, reset: "monthly" },
      });
      const { virtual_key_id: vk = "" } =
        (await storedExpiry(body.apiKey.id)) ?? {};
      // charges on both sides of each window's start, and two months back
      const utcNow = "now() AT TIME ZONE 'UTC'";
      await usage(org, vk, 1 * SAR);
      await usage(
        org,
        vk,
        2 * SAR,
        `date_trunc('day', ${utcNow}) - interval '1 second'`,
      );
      await usage(
        org,
        vk,
        3 * SAR,
        `date_trunc('week', ${utcNow}) - interval '1 second'`,
      );
      await usage(
        org,
        vk,
        4 * SAR,
        `date_trunc('month', ${utcNow}) - interval '1 second'`,
      );
      await usage(org, vk, 5 * SAR, `${utcNow} - interval '60 days'`);
      // an open hold counts against the limit, not as usage
      await sql`insert into credit_account (organization_id, balance, held) values (${org}, ${100 * SAR}, ${SAR / 2})`;
      await sql`insert into credit_reservation (id, organization_id, amount, expires_at, virtual_key_id, usd_to_sar_rate_e6, fx_spread_bps, effective_usd_to_sar_rate_e6)
                values ('hold_lifecycle', ${org}, ${SAR / 2}, now() + interval '5 minutes', ${vk}, 3750000, 0, 3750000)`;
      // another key's charge is not this key's
      const { body: otherKey } = await create(org, {
        name: "other",
        expiry: "30d",
      });
      const { virtual_key_id: otherVk = "" } =
        (await storedExpiry(otherKey.apiKey.id)) ?? {};
      await usage(org, otherVk, 7 * SAR);

      // the expected figures, summed here with date_trunc, independently of the code under test
      const [expected] = await sql<
        {
          total: string;
          daily: string;
          weekly: string;
          monthly: string;
          last: Date;
        }[]
      >`select -sum(amount) as total,
               coalesce(-sum(amount) filter (where created_at >= date_trunc('day', now() at time zone 'UTC')), 0) as daily,
               coalesce(-sum(amount) filter (where created_at >= date_trunc('week', now() at time zone 'UTC')), 0) as weekly,
               coalesce(-sum(amount) filter (where created_at >= date_trunc('month', now() at time zone 'UTC')), 0) as monthly,
               max(created_at) as last
          from credit_ledger_entry where virtual_key_id = ${vk} and kind = 'usage'`;
      const want = {
        total: Number(expected?.total),
        daily: Number(expected?.daily),
        weekly: Number(expected?.weekly),
        monthly: Number(expected?.monthly),
      };
      expect(want.total).toBe(15 * SAR);
      expect(want.daily).toBe(SAR);

      const page = (await (
        await call("GET", `/${org}/${body.apiKey.id}`, { role: "member" })
      ).json()) as KeyBody & {
        usage: {
          total: number;
          daily: number;
          weekly: number;
          monthly: number;
          held: number;
          spentAgainstLimit: number;
          lastUsedAt: string | null;
          resetsAt: string | null;
        };
        spendByDay: { day: string; spend: number }[];
      };
      expect(page.usage).toMatchObject({
        ...want,
        held: SAR / 2,
        spentAgainstLimit: want.monthly + SAR / 2,
      });
      // last used: the latest charge, to the millisecond
      const [last] = await sql<
        { iso: string }[]
      >`select to_char(max(created_at), 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') as iso
          from credit_ledger_entry where virtual_key_id = ${vk} and kind = 'usage'`;
      expect(page.usage.lastUsedAt).toBe(last?.iso);
      // the 30-day chart: every day, each day's charges, nothing from 60 days back
      expect(page.spendByDay).toHaveLength(30);
      expect(page.spendByDay.at(-1)?.spend).toBe(SAR);
      expect(page.spendByDay.reduce((sum, day) => sum + day.spend, 0)).toBe(
        10 * SAR,
      );

      const list = (await (
        await call("GET", `/${org}`, { role: "member" })
      ).json()) as (KeyBody & {
        usage: {
          total: number;
          spentAgainstLimit: number;
          lastUsedAt: string | null;
        };
      })[];
      const row = list.find((k) => k.id === body.apiKey.id);
      expect(row?.usage).toEqual({
        total: want.total,
        spentAgainstLimit: want.monthly + SAR / 2,
        lastUsedAt: last?.iso,
      });
      expect(list.find((k) => k.id === otherKey.apiKey.id)?.usage.total).toBe(
        7 * SAR,
      );

      // GET /api/v1/key for the same key gives the same figures, in SAR
      const { describeKey } = await import("../../v1/queries");
      const [calling] = await sql<
        {
          created_at: Date;
          expires_at: Date | null;
        }[]
      >`select created_at, expires_at from api_key where id = ${body.apiKey.id}`;
      const { data } = await describeKey({
        id: body.apiKey.id,
        organizationId: org,
        virtualKeyId: vk,
        name: "spender",
        prefix: body.apiKey.prefix,
        limitMicroSar: 50 * SAR,
        limitReset: "monthly",
        expiresAt: calling?.expires_at ?? null,
        createdAt: calling?.created_at ?? new Date(),
      });
      expect(data).toMatchObject({
        usage: page.usage.total / SAR,
        usage_daily: page.usage.daily / SAR,
        usage_weekly: page.usage.weekly / SAR,
        usage_monthly: page.usage.monthly / SAR,
        limit_remaining: (50 * SAR - page.usage.spentAgainstLimit) / SAR,
        limit_resets_at: page.usage.resetsAt,
      });
    });

    it("says never for a key that has not been used", async () => {
      const org = await createOrganization();
      const { body } = await create(org, { name: "fresh", expiry: "1d" });
      const page = (await (
        await call("GET", `/${org}/${body.apiKey.id}`, { role: "member" })
      ).json()) as { usage: { lastUsedAt: string | null; total: number } };
      expect(page.usage).toMatchObject({ lastUsedAt: null, total: 0 });
    });
  });

  describe("who may do what (unchanged: members read, owners and admins manage)", () => {
    it("lets a member read, search and open keys, and refuses them every change", async () => {
      const org = await createOrganization();
      const { body } = await create(org, { name: "k", expiry: "1d" });
      const id = body.apiKey.id;
      for (const [method, path, json] of [
        ["GET", `/${org}`],
        ["GET", `/${org}/settings`],
        ["GET", `/${org}/${id}`],
        ["POST", `/${org}/search`, { query: "k" }],
      ] as const) {
        expect(
          (await call(method, path, { role: "member", json })).status,
        ).toBe(200);
      }
      for (const [method, path, json] of [
        ["POST", `/${org}`, { name: "m", expiry: "1d" }],
        ["PATCH", `/${org}/${id}`, { disabled: true }],
        ["DELETE", `/${org}/${id}`],
      ] as const) {
        expect(
          (await call(method, path, { role: "member", json })).status,
        ).toBe(403);
      }
      for (const role of ["owner", "admin"]) {
        const made = await call("POST", `/${org}`, {
          role,
          json: { name: role, expiry: "1d" },
        });
        expect(made.status).toBe(201);
      }
    });

    it("finds no key of another organization by id", async () => {
      const org = await createOrganization();
      const other = await createOrganization();
      const { body } = await create(org, { name: "mine", expiry: "1d" });
      expect(
        (await call("GET", `/${other}/${body.apiKey.id}`, { role: "member" }))
          .status,
      ).toBe(404);
    });

    it("deletes the key from the gateway, then from the list", async () => {
      const org = await createOrganization();
      const { body } = await create(org, { name: "bye", expiry: "1d" });
      const { virtual_key_id: vk = "" } =
        (await storedExpiry(body.apiKey.id)) ?? {};
      expect((await call("DELETE", `/${org}/${body.apiKey.id}`)).status).toBe(
        200,
      );
      expect(gateway.state.deletedKeys).toContain(vk);
      expect(
        (await (await call("GET", `/${org}`)).json()) as KeyBody[],
      ).toEqual([]);
    });
  });
});
