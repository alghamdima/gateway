import {
  afterAll,
  beforeAll,
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from "vitest";

import type { CreateVersionInput } from "../prices";
import type { Rates } from "../rates";
import type { FakeGateway } from "./fake-gateway";
import type { User } from "@workspace/auth";
import type { TestDatabase } from "@workspace/db/testing";
import type { GatewayClient } from "@workspace/gateway-client/server";
import type postgres from "postgres";

// Needs real Postgres: the temporal uniqueness, the append-only trigger, the
// checks and the row locks are the database's. Skips without DATABASE_URL.
const adminUrl = process.env.DATABASE_URL;

const gateway = vi.hoisted(() => {
  const ref: { client?: GatewayClient } = {};
  return ref;
});
const audited = vi.hoisted(() => [] as Record<string, unknown>[]);
vi.mock("@workspace/gateway-client/server", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("@workspace/gateway-client/server")>();
  return {
    ...actual,
    getGateway: () => {
      if (!gateway.client) {
        throw new Error("no fake gateway");
      }
      return gateway.client;
    },
  };
});
// The router's middleware imports the auth server and the translations (JSX
// the test runner does not compile); the request tests need neither.
vi.mock("@workspace/auth/server", () => ({
  auth: {},
  clientIp: () => null,
  recordStaffAction: async (entry: Record<string, unknown>) => {
    audited.push(entry);
  },
}));
vi.mock("@workspace/i18n", async () => {
  const z = await import("zod");
  return { getValidationLocale: () => z.locales.en };
});
vi.mock("@workspace/i18n/server", () => ({ getLocaleFromRequest: () => "en" }));

const MINUTE = 60_000;
const DAY = 24 * 60 * MINUTE;

const rates = (overrides: Partial<Rates> = {}): Rates => ({
  inputUsd: "0.00000015",
  outputUsd: "0.0000006",
  cacheReadUsd: null,
  cacheWriteUsd: null,
  cacheWrite1hUsd: null,
  otherRates: {},
  ...overrides,
});

const datasheetRow = (name: string, input: number, output = 6e-7) => ({
  provider: "anthropic",
  model: name,
  mode: "chat",
  isCustom: false,
  upstream: { input_cost_per_token: input, output_cost_per_token: output },
  effective: { input_cost_per_token: input, output_cost_per_token: output },
});

const body = (name: string) =>
  JSON.stringify({
    provider: "openai",
    model: name,
    rates: {
      inputUsd: "0.000001",
      outputUsd: "0.000002",
      cacheReadUsd: null,
      cacheWriteUsd: null,
      cacheWrite1hUsd: null,
    },
  });

const byText = (a: string, b: string) => a.localeCompare(b);

/** The admin model-prices router behind the real staff access check. */
const staffApp = async (role: string) => {
  const { Hono } = await import("hono");
  const { HttpException } = await import("@workspace/shared/utils");
  const { enforceStaffAccess } = await import("../../admin/access-middleware");
  const { modelPricesAdminRouter } = await import("../router");
  const staff: User = {
    id: `staff_${role}`,
    name: "Staff",
    email: `${role}@example.com`,
    emailVerified: true,
    banned: false,
    isAnonymous: false,
    twoFactorEnabled: true,
    role,
    createdAt: new Date(),
    updatedAt: new Date(),
  };
  return new Hono<{ Variables: { user: User } }>()
    .use(async (c, next) => {
      c.set("user", staff);
      await next();
    })
    .use("/api/admin/*", enforceStaffAccess)
    .route("/api/admin/model-prices", modelPricesAdminRouter)
    .onError((error) =>
      Response.json(
        { message: error.message },
        { status: error instanceof HttpException ? error.status : 500 },
      ),
    );
};

describe.skipIf(!adminUrl)("our model price table (ADMIN-09)", () => {
  let database: TestDatabase;
  let sql: postgres.Sql;
  let prices: typeof import("../prices");
  let push: typeof import("../push");
  let suggestions: typeof import("../suggestions");
  let importer: typeof import("../import");
  let fakeGateway: () => FakeGateway;
  let fake: FakeGateway;
  let seq = 0;

  const model = (prefix = "m") => `${prefix}-${++seq}`;
  const create = (input: Partial<CreateVersionInput> & { model: string }) =>
    prices.createPriceVersion({
      provider: "openai",
      providerKeyId: null,
      isRemoved: false,
      rates: rates(),
      note: "",
      staffId: "staff_1",
      ...input,
    });
  const count = async (table: string) =>
    Number(
      (await sql`select count(*)::int as n from ${sql(table)}`)[0]?.n ?? 0,
    );

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;
    sql = (await import("postgres")).default(database.url, {
      max: 2,
      onnotice: () => undefined,
    });
    prices = await import("../prices");
    push = await import("../push");
    suggestions = await import("../suggestions");
    importer = await import("../import");
    // imported only now: the gateway client's module reads DATABASE_URL when first loaded
    ({ fakeGateway } = await import("./fake-gateway"));
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  beforeEach(() => {
    fake = fakeGateway();
    gateway.client = fake.client;
    audited.length = 0;
  });

  describe("time", () => {
    it("answers the price at any time: a later version supersedes, the earlier one is untouched", async () => {
      const name = model();
      const t0 = new Date(Date.now() - 10 * DAY);
      const t1 = new Date(t0.getTime() + 2 * DAY);
      const first = await create({
        model: name,
        now: t0,
        rates: rates({ inputUsd: "0.000001" }),
      });
      // saved at t0, in force from t1
      const second = await create({
        model: name,
        now: t0,
        effectiveFrom: t1,
        rates: rates({ inputUsd: "0.000002" }),
      });
      const target = { provider: "openai", model: name, providerKeyId: null };

      expect(
        await prices.getVersionAt(target, new Date(t0.getTime() - 1)),
      ).toBeUndefined();
      expect((await prices.getVersionAt(target, t0))?.id).toBe(
        first.version.id,
      );
      expect(
        (await prices.getVersionAt(target, new Date(t1.getTime() - 1)))?.id,
      ).toBe(first.version.id);
      expect((await prices.getVersionAt(target, t1))?.id).toBe(
        second.version.id,
      );
      expect((await prices.getVersionAt(target, new Date()))?.inputUsd).toBe(
        "0.000002",
      );
      // the superseded version still says what it said
      const [old] =
        await sql`select input_usd from model_price where id = ${first.version.id}`;
      expect(old?.input_usd).toBe("0.000001000000000000000000000000");
      // a future version is saved, not in force
      const future = await create({
        model: name,
        effectiveFrom: new Date(Date.now() + DAY),
        rates: rates({ inputUsd: "0.000009" }),
      });
      expect((await prices.getVersionAt(target, new Date()))?.id).toBe(
        second.version.id,
      );
      const listed = await prices.listCurrentPrices({ q: name });
      expect(listed.prices).toHaveLength(1);
      expect(listed.prices[0]?.id).toBe(second.version.id);
      expect(listed.prices[0]?.scheduledFrom?.getTime()).toBe(
        future.version.effectiveFrom.getTime(),
      );
    });

    it("never has two prices in force at once: one version per target and instant", async () => {
      const name = model();
      const at = new Date(Date.now() + DAY);
      await create({ model: name, effectiveFrom: at });
      await expect(
        create({
          model: name,
          effectiveFrom: at,
          rates: rates({ inputUsd: "0.1" }),
        }),
      ).rejects.toMatchObject({ status: 409 });
      // the database refuses it whoever writes it, also for a key's price (NULLS NOT DISTINCT)
      await expect(
        sql`insert into model_price (provider, model, effective_from, input_usd, source, created_by)
          values ('openai', ${name}, ${at}, '0.1', 'manual', 's')`,
      ).rejects.toMatchObject({
        constraint_name: "model_price_target_effective_from_unique",
      });
      // the same instant for another target is fine
      await create({
        model: name,
        providerKeyId: "k_other",
        effectiveFrom: at,
      });
    });

    it("never edits or deletes a version", async () => {
      const { version } = await create({ model: model() });
      await expect(
        sql`update model_price set input_usd = '0.9' where id = ${version.id}`,
      ).rejects.toThrow(/append-only/);
      await expect(
        sql`delete from model_price where id = ${version.id}`,
      ).rejects.toThrow(/append-only/);
    });

    it("refuses to rewrite history: a price cannot start in the past", async () => {
      await expect(
        create({
          model: model(),
          effectiveFrom: new Date(Date.now() - 10 * MINUTE),
        }),
      ).rejects.toMatchObject({ status: 400 });
      await expect(
        sql`insert into model_price (provider, model, effective_from, input_usd, source, created_by)
          values ('openai', ${model()}, now() - interval '1 hour', '0.1', 'manual', 's')`,
      ).rejects.toMatchObject({ constraint_name: "model_price_not_backdated" });
    });

    it("checks the rates in the database: units, decimals, ceilings, removals", async () => {
      const insert = (
        otherRates: postgres.JSONValue,
        input: string | null = "0.1",
      ) =>
        sql`insert into model_price (provider, model, effective_from, input_usd, other_rates, source, created_by)
          values ('openai', ${model()}, now(), ${input}, ${sql.json(otherRates)}, 'manual', 's')`;
      await insert({ output_cost_per_image: "0.04" });
      for (const bad of [
        { output_cost_per_image: 0.04 }, // a float, not an exact decimal
        { output_cost_per_image: "2000" }, // above the unit ceiling
        { output_cost_per_image: "1e-3" }, // not a plain decimal
        { Input: "0.1" }, // not a field name
        { input_cost_per_token: "0.1" }, // a typed rate kept twice
        [],
      ]) {
        await expect(insert(bad)).rejects.toMatchObject({
          constraint_name: "model_price_rates_range",
        });
      }
      await expect(insert({}, "2")).rejects.toMatchObject({
        constraint_name: "model_price_rates_range",
      });
      // a price with no rate at all, and a removal of a provider-wide price
      await expect(insert({}, null)).rejects.toMatchObject({
        constraint_name: "model_price_removal",
      });
      await expect(
        sql`insert into model_price (provider, model, effective_from, is_removed, source, created_by)
          values ('openai', ${model()}, now(), true, 'manual', 's')`,
      ).rejects.toMatchObject({ constraint_name: "model_price_removal" });
    });
  });

  describe("a provider key's own price", () => {
    it("wins over the provider's price for requests that key serves, until it is removed", async () => {
      const name = model();
      const t0 = new Date(Date.now() - 5 * DAY);
      await create({
        model: name,
        now: t0,
        rates: rates({ inputUsd: "0.000002" }),
      });
      await create({
        model: name,
        now: t0,
        providerKeyId: "k_negotiated",
        rates: rates({ inputUsd: "0.000001" }),
      });
      const at = new Date();
      const via = (providerKeyId: string | null) =>
        prices.resolvePriceAt({
          provider: "openai",
          model: name,
          providerKeyId,
          at,
        });

      expect((await via("k_negotiated"))?.inputUsd).toBe("0.000001");
      expect((await via("k_other"))?.inputUsd).toBe("0.000002");
      expect((await via(null))?.inputUsd).toBe("0.000002");

      await create({
        model: name,
        providerKeyId: "k_negotiated",
        isRemoved: true,
        rates: undefined,
      });
      expect(
        (
          await prices.resolvePriceAt({
            provider: "openai",
            model: name,
            providerKeyId: "k_negotiated",
            at: new Date(),
          })
        )?.inputUsd,
      ).toBe("0.000002");
      // the key's price before its removal is still the answer for that time
      expect((await via("k_negotiated"))?.inputUsd).toBe("0.000001");
    });

    it("must set every rate the provider's price sets: the gateway does not merge them", async () => {
      const name = model();
      await create({
        model: name,
        rates: rates({ cacheReadUsd: "0.00000007" }),
      });
      await expect(
        create({ model: name, providerKeyId: "k_partial", rates: rates() }),
      ).rejects.toMatchObject({ status: 400 });
      await create({
        model: name,
        providerKeyId: "k_partial",
        rates: rates({ cacheReadUsd: "0.00000005" }),
      });
    });
  });

  describe("the push to the gateway", () => {
    it("is idempotent, follows new versions in the same override, and removes a key's ended price", async () => {
      const name = model("push");
      const { version } = await create({ model: name });
      const target = { provider: "openai", model: name, providerKeyId: null };

      expect(await push.pushNow(fake.client, target)).toMatchObject({
        pushed: 1,
      });
      expect(await push.pushNow(fake.client, target)).toMatchObject({
        pushed: 0,
      });
      // pushing that one target again by hand changes nothing either
      expect(await push.pushTarget(fake.client, target, version)).toEqual({
        status: "unchanged",
      });
      expect(fake.calls).toEqual([`create lemru-price:${version.id}`]);
      const [override] = [...fake.overrides.values()];
      expect(override).toMatchObject({
        name: `lemru-price:${version.id}`,
        scopeKind: "provider",
        providerId: "openai",
        pattern: name,
        requestTypes: [
          "chat_completion",
          "text_completion",
          "responses",
          "embedding",
        ],
        patch: { input_cost_per_token: 1.5e-7, output_cost_per_token: 6e-7 },
      });

      const next = await create({
        model: name,
        rates: rates({ inputUsd: "0.0000002" }),
      });
      await push.pushNow(fake.client, target);
      expect(fake.calls.at(-1)).toBe(
        `update ${override?.id} lemru-price:${next.version.id}`,
      );
      expect(fake.overrides.size).toBe(1);

      // a key's price is its own override, at the key's scope; removing it deletes the override
      const key = { ...target, providerKeyId: "k_push" };
      await create({
        model: name,
        providerKeyId: "k_push",
        rates: rates({ inputUsd: "0.0000001" }),
      });
      await push.pushNow(fake.client, key);
      expect(
        [...fake.overrides.values()].map((item) => item.scopeKind).sort(),
      ).toEqual(["provider", "provider_key"]);
      await create({
        model: name,
        providerKeyId: "k_push",
        isRemoved: true,
        rates: undefined,
      });
      expect(await push.pushNow(fake.client, key)).toMatchObject({
        removed: 1,
      });
      expect(fake.overrides.size).toBe(1);
    });

    it("never makes a duplicate: a lost state row adopts our override, a lost override is created again", async () => {
      const name = model("adopt");
      const { version } = await create({ model: name });
      const target = { provider: "openai", model: name, providerKeyId: null };
      await push.pushNow(fake.client, target);
      await sql`delete from model_price_push where model = ${name}`;
      await push.pushNow(fake.client, target);
      expect(fake.overrides.size).toBe(1);
      expect(
        fake.calls.filter((call) => call.startsWith("create")),
      ).toHaveLength(1);

      // the gateway lost it: the next forced push creates it again
      fake.overrides.clear();
      await push.pushDuePrices(fake.client, new Date(), { force: true });
      expect([...fake.overrides.values()].map((item) => item.name)).toContain(
        `lemru-price:${version.id}`,
      );
    });

    it("keeps a failed push's error and retries it on the next run", async () => {
      const name = model("fail");
      await create({ model: name });
      const target = { provider: "openai", model: name, providerKeyId: null };
      fake.failNext(500);
      expect(await push.pushNow(fake.client, target)).toMatchObject({
        failed: 1,
      });
      const [state] =
        await sql`select last_error, pushed_price_id from model_price_push where model = ${name}`;
      expect(state?.last_error).toMatch(/500/);
      expect(state?.pushed_price_id).toBeNull();
      expect(await push.pushDuePrices(fake.client)).toMatchObject({
        failed: 0,
      });
      const [after] =
        await sql`select last_error from model_price_push where model = ${name}`;
      expect(after?.last_error).toBeNull();
    });

    it("starts a future price only when its time has come", async () => {
      const name = model("future");
      const now = new Date();
      await create({
        model: name,
        now,
        rates: rates({ inputUsd: "0.000001" }),
      });
      const later = await create({
        model: name,
        now,
        effectiveFrom: new Date(now.getTime() + DAY),
        rates: rates({ inputUsd: "0.000003" }),
      });
      await push.pushDuePrices(fake.client, now);
      const carried = () =>
        [...fake.overrides.values()].find((item) => item.pattern === name)
          ?.patch.input_cost_per_token;
      expect(carried()).toBe(0.000001);
      await push.pushDuePrices(fake.client, new Date(now.getTime() + DAY));
      expect(carried()).toBe(0.000003);
      expect(
        [...fake.overrides.values()].find((item) => item.pattern === name)
          ?.name,
      ).toBe(`lemru-price:${later.version.id}`);
    });
  });

  describe("reconciliation with the gateway", () => {
    it("finds the gateway's price is ours after a push, then each kind of drift, and the job mends ours", async () => {
      // a clean slate: drift is reported for every target in force
      await push.pushDuePrices(fake.client);
      fake.overrides.clear();
      await sql`delete from model_price_push`;
      const name = model("drift");
      await create({
        model: name,
        rates: rates({ cacheReadUsd: "0.000000075" }),
      });
      await push.pushDuePrices(fake.client);
      // the catalog charges what the overrides say
      fake.models = (
        await prices.getVersionsInForce(new Date(), { scope: "provider" })
      ).map((version) => ({
        name: version.model,
        provider: version.provider,
        input_cost_per_token: 9,
        output_cost_per_token: 9,
        overridden_pricing: {
          input_cost_per_token:
            version.inputUsd === null ? undefined : Number(version.inputUsd),
          output_cost_per_token:
            version.outputUsd === null ? undefined : Number(version.outputUsd),
          cache_read_input_token_cost:
            version.cacheReadUsd === null
              ? undefined
              : Number(version.cacheReadUsd),
        },
      }));
      expect((await push.checkModelPrices(fake.client)).drift).toEqual([]);

      const ours = [...fake.overrides.values()].find(
        (item) => item.pattern === name,
      );
      if (!ours) throw new Error("no override pushed");
      // someone changed a rate in the gateway, and added an override of their own
      fake.overrides.set(ours.id, {
        ...ours,
        patch: { ...ours.patch, output_cost_per_token: 0.000001 },
      });
      fake.overrides.set("foreign", {
        ...ours,
        id: "foreign",
        name: "someone's test",
        matchType: "wildcard",
        pattern: "drift-*",
      });
      fake.overrides.set("orphan", {
        ...ours,
        id: "orphan",
        name: "lemru-price:999999",
        pattern: "gone",
      });
      const entry = fake.models.find((item) => item.name === name);
      if (entry?.overridden_pricing)
        entry.overridden_pricing.output_cost_per_token = 0.000001;

      const check = await push.checkModelPrices(fake.client);
      expect(
        check.drift.map((item) => `${item.kind} ${item.model}`).sort(byText),
      ).toEqual(
        [
          `effective_price ${name}`,
          `foreign_override ${name}`,
          "orphan gone",
          `wrong_rates ${name}`,
        ].sort(byText),
      );
      expect(
        check.drift.find((item) => item.kind === "wrong_rates")?.detail,
      ).toBe("output_cost_per_token");

      const synced = await push.syncModelPrices(fake.client);
      expect(synced.repaired).toMatchObject({ pushed: 1 });
      expect(synced.orphansDeleted).toBe(1);
      expect(fake.overrides.get(ours.id)?.patch.output_cost_per_token).toBe(
        6e-7,
      );
      expect(fake.overrides.has("orphan")).toBe(false);
      // someone else's override is reported, never deleted
      expect(fake.overrides.has("foreign")).toBe(true);
    });

    it("reports a model the gateway does not list: it cannot be charged", async () => {
      const name = model("absent");
      await create({ model: name });
      fake.models = [];
      const check = await push.checkModelPrices(fake.client);
      expect(check.drift).toContainEqual(
        expect.objectContaining({ kind: "not_in_catalog", model: name }),
      );
    });
  });

  describe("suggestions", () => {
    const offer = (...rows: ReturnType<typeof datasheetRow>[]) => {
      fake.datasheet = rows;
      fake.models = rows.map((row) => ({
        name: row.model,
        provider: row.provider,
      }));
    };
    const pendingFor = async (name: string) =>
      (await suggestions.listSuggestions({ status: "pending" })).filter(
        (item) => item.model === name,
      );
    const priceOf = async (name: string) =>
      (
        await prices.getVersionAt(
          { provider: "anthropic", model: name, providerKeyId: null },
          new Date(),
        )
      )?.inputUsd;

    it("proposes what differs and never writes a price; the same proposal is not made twice", async () => {
      const name = model("sug");
      await create({
        provider: "anthropic",
        model: name,
        rates: rates({ inputUsd: "0.000003" }),
      });
      const before = await count("model_price");
      offer(datasheetRow(name, 0.000004));

      expect(await suggestions.syncSuggestions(fake.client)).toMatchObject({
        created: 1,
      });
      expect(await suggestions.syncSuggestions(fake.client)).toMatchObject({
        created: 0,
        unchanged: 1,
      });
      expect(await count("model_price")).toBe(before);
      expect(await priceOf(name)).toBe("0.000003");
      const [pending] = await pendingFor(name);
      expect(pending).toMatchObject({
        inputUsd: "0.000004",
        base: { inputUsd: "0.000003" },
      });

      // the datasheet moves again: the open suggestion is superseded by the new one
      offer(datasheetRow(name, 0.000005));
      expect(await suggestions.syncSuggestions(fake.client)).toMatchObject({
        created: 1,
        superseded: 1,
      });
      expect((await pendingFor(name)).map((item) => item.inputUsd)).toEqual([
        "0.000005",
      ]);
      // and back to our price: nothing is left to propose
      offer(datasheetRow(name, 0.000003));
      expect(await suggestions.syncSuggestions(fake.client)).toMatchObject({
        superseded: 1,
      });
      expect(await pendingFor(name)).toEqual([]);
      // one open suggestion per model, in the database too
      await sql`insert into model_price_suggestion (provider, model, source, input_usd) values ('anthropic', ${name}, 'datasheet', '0.1')`;
      await expect(
        sql`insert into model_price_suggestion (provider, model, source, input_usd) values ('anthropic', ${name}, 'datasheet', '0.2')`,
      ).rejects.toMatchObject({
        constraint_name: "model_price_suggestion_pending_unique",
      });
    });

    it("approving makes a new version and pushes it", async () => {
      const name = model("approve");
      offer(datasheetRow(name, 0.000004));
      await suggestions.syncSuggestions(fake.client);
      const [pending] = await pendingFor(name);
      if (!pending) throw new Error("no suggestion");
      expect(pending.basePriceId).toBeNull();

      const app = await staffApp("finance");
      const response = await app.request(
        `/api/admin/model-prices/suggestions/${pending.id}/approve`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({}),
        },
      );
      expect(response.status).toBe(200);
      const [version] =
        await sql`select source, suggestion_id from model_price where model = ${name}`;
      expect(version?.source).toBe("suggestion");
      expect(Number(version?.suggestion_id)).toBe(pending.id);
      expect(await priceOf(name)).toBe("0.000004");
      expect(
        [...fake.overrides.values()].some((item) => item.pattern === name),
      ).toBe(true);
      // decided once
      await expect(
        suggestions.approveSuggestion({ id: pending.id, staffId: "s" }),
      ).rejects.toMatchObject({ status: 409 });
    });

    it("a price staff set wins: a suggestion made before it can no longer be approved", async () => {
      const name = model("manual");
      await create({
        provider: "anthropic",
        model: name,
        rates: rates({ inputUsd: "0.000003" }),
      });
      offer(datasheetRow(name, 0.000004));
      await suggestions.syncSuggestions(fake.client);
      const [pending] = await pendingFor(name);
      if (!pending) throw new Error("no suggestion");
      await create({
        provider: "anthropic",
        model: name,
        rates: rates({ inputUsd: "0.0000035" }),
      });

      await expect(
        suggestions.approveSuggestion({ id: pending.id, staffId: "s" }),
      ).rejects.toMatchObject({ status: 409 });
      expect(await priceOf(name)).toBe("0.0000035");
      // and the sync never overwrites it either
      await suggestions.syncSuggestions(fake.client);
      expect(await priceOf(name)).toBe("0.0000035");
    });

    it("rejecting leaves the price as it is, needs a reason, and is not proposed again", async () => {
      const name = model("reject");
      await create({
        provider: "anthropic",
        model: name,
        rates: rates({ inputUsd: "0.000003" }),
      });
      offer(datasheetRow(name, 0.000004));
      await suggestions.syncSuggestions(fake.client);
      const [pending] = await pendingFor(name);
      if (!pending) throw new Error("no suggestion");

      const app = await staffApp("admin");
      const noReason = await app.request(
        `/api/admin/model-prices/suggestions/${pending.id}/reject`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ reason: " " }),
        },
      );
      expect(noReason.status).toBe(422);
      const rejected = await app.request(
        `/api/admin/model-prices/suggestions/${pending.id}/reject`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ reason: "The provider's page still says 3" }),
        },
      );
      expect(rejected.status).toBe(200);
      expect(await priceOf(name)).toBe("0.000003");
      expect(await suggestions.syncSuggestions(fake.client)).toMatchObject({
        created: 0,
      });
      await expect(
        sql`update model_price_suggestion set status = 'rejected', reason = '', decided_by = 's', decided_at = now() where id = ${pending.id}`,
      ).rejects.toMatchObject({
        constraint_name: "model_price_suggestion_decision",
      });
    });
  });

  describe("the seed from the gateway's datasheet", () => {
    it("imports what the gateway charges today, once", async () => {
      const name = model("seed");
      fake.models = [
        { name, provider: "groq" },
        { name: `${name}-unpriced`, provider: "groq" },
      ];
      fake.datasheet = [
        {
          provider: "groq",
          model: name,
          mode: "chat",
          isCustom: false,
          upstream: { input_cost_per_token: 1 },
          effective: {
            input_cost_per_token: 5.0000000000000004e-8,
            output_cost_per_token: 8e-8,
            output_cost_per_image: 0.04,
          },
        },
      ];
      const first = await importer.importFromGateway({
        gateway: fake.client,
        staffId: "staff_1",
      });
      expect(first).toMatchObject({ imported: 1, unpriced: 1 });
      const [row] =
        await sql`select input_usd, output_usd, other_rates, source from model_price where model = ${name}`;
      expect(row).toMatchObject({
        input_usd: "0.000000050000000000000004000000",
        output_usd: "0.000000080000000000000000000000",
        other_rates: { output_cost_per_image: "0.04" },
        source: "import",
      });
      // the float the gateway is sent is the one it charges today
      await push.pushDuePrices(fake.client);
      expect(
        [...fake.overrides.values()].find((item) => item.pattern === name)
          ?.patch,
      ).toEqual({
        input_cost_per_token: 5.0000000000000004e-8,
        output_cost_per_token: 8e-8,
        output_cost_per_image: 0.04,
      });
      expect(
        await importer.importFromGateway({
          gateway: fake.client,
          staffId: "staff_1",
        }),
      ).toMatchObject({
        imported: 0,
        existing: 1,
      });
    });
  });

  describe("what customers see", () => {
    it("is our price in force × the effective rate, exactly", async () => {
      const name = model("catalog");
      await create({
        provider: "mistral",
        model: name,
        rates: rates({
          inputUsd: "0.00000000286102294921875",
          outputUsd: "0.0000006",
        }),
      });
      await sql`update pricing_settings set fx_spread_bps = 250 where id = 1`;
      try {
        const v1 = await import("../../v1/models");
        v1.clearModelCache();
        const { data } = await v1.listPublicModels({
          ...fake.client,
          listModels: async () => [
            // the datasheet's numbers: not what is shown
            {
              name,
              provider: "mistral",
              input_cost_per_token: 1,
              output_cost_per_token: 1,
            },
          ],
        });
        expect(data[0]).toMatchObject({
          pricing: {
            prompt: "0.00000000286102294921875",
            completion: "0.0000006",
          },
          // × 3.84375, the peg with a 2.5% spread
          pricing_sar: {
            prompt: "0.0000000109970569610595703125",
            completion: "0.00000230625",
          },
          usd_to_sar_rate: "3.84375",
        });
      } finally {
        await sql`update pricing_settings set fx_spread_bps = 0 where id = 1`;
      }
    });
  });

  describe("access and audit", () => {
    it("lets every staff role read and only admin and finance change", async () => {
      for (const role of ["admin", "finance", "support"]) {
        const app = await staffApp(role);
        expect((await app.request("/api/admin/model-prices")).status).toBe(200);
        expect(
          (await app.request("/api/admin/model-prices/suggestions")).status,
        ).toBe(200);
      }
      for (const role of ["support", "user"]) {
        const app = await staffApp(role);
        const response = await app.request("/api/admin/model-prices", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: body(model("denied")),
        });
        expect(response.status).toBe(403);
      }
      const user = await staffApp("user");
      expect((await user.request("/api/admin/model-prices")).status).toBe(403);
      for (const role of ["admin", "finance"]) {
        const app = await staffApp(role);
        const response = await app.request("/api/admin/model-prices", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: body(model(role)),
        });
        expect(response.status).toBe(200);
      }
    });

    it("records each change with the price before and after", async () => {
      const name = model("audit");
      const app = await staffApp("finance");
      await app.request("/api/admin/model-prices", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: body(name),
      });
      const changed = await app.request("/api/admin/model-prices", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          provider: "openai",
          model: name,
          rates: {
            inputUsd: "0.000005",
            outputUsd: "0.000002",
            cacheReadUsd: null,
            cacheWriteUsd: null,
            cacheWrite1hUsd: null,
          },
          note: "provider raised the price",
        }),
      });
      expect(changed.status).toBe(200);
      const last = audited.at(-1);
      expect(last).toMatchObject({
        action: "pricing.model-create",
        targetType: "model-price",
        actorId: "staff_finance",
        details: {
          before: { model: name, rates: { input_cost_per_token: "0.000001" } },
          after: { model: name, rates: { input_cost_per_token: "0.000005" } },
        },
      });
      // a refused change is not recorded
      const count = audited.length;
      await app.request("/api/admin/model-prices", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          provider: "openai",
          model: name,
          rates: { inputUsd: "1e-7" },
        }),
      });
      expect(audited.length).toBe(count);
    });
  });
});
