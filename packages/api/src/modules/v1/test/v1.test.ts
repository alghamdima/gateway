import { afterAll, beforeAll, describe, expect, it } from "vitest";

import type { TestDatabase } from "@workspace/db/testing";
import type {
  GatewayClient,
  GatewayModel,
} from "@workspace/gateway-client/server";
import type postgres from "postgres";

// Needs real Postgres: the answers are sums over the ledger. Skips without DATABASE_URL.
const adminUrl = process.env.DATABASE_URL;

const SAR = 1_000_000;

/** A gateway that only knows its model catalog. */
const gatewayWith = (models: GatewayModel[]): GatewayClient => ({
  createCustomer: async () => ({ id: "c" }),
  deleteCustomer: async () => {},
  createVirtualKey: async () => ({ id: "vk", value: "v" }),
  updateVirtualKey: async () => {},
  deleteVirtualKey: async () => {},
  listModels: async () => models,
  searchRequests: async () => ({ requests: [], total: 0 }),
  syncPrices: async () => {},
  listPricingOverrides: async () => [],
  createPricingOverride: async () => {
    throw new Error("not used");
  },
  updatePricingOverride: async () => {
    throw new Error("not used");
  },
  deletePricingOverride: async () => {},
  listDatasheetEntries: async () => [],
  listProviders: async () => [],
  createProvider: async () => {},
  deleteProvider: async () => {},
  createProviderKey: async () => ({
    id: "pk",
    name: "",
    maskedValue: "",
    models: [],
    weight: 1,
    isEnabled: true,
  }),
  updateProviderKey: async () => ({
    id: "pk",
    name: "",
    maskedValue: "",
    models: [],
    weight: 1,
    isEnabled: true,
  }),
  deleteProviderKey: async () => {},
  refreshProviderModels: async () => {},
  listProviderKeys: async () => [],
  setVirtualKeyAccess: async () => {},
  getCustomerCosts: async () => [],
  getGenerationSummary: async () => {
    throw new Error("not used");
  },
  getAttempts: async () => [],
  getLog: async () => undefined,
  setCustomerRateLimit: async () => {},
});

describe.skipIf(!adminUrl)("OpenRouter-compatible /api/v1", () => {
  let database: TestDatabase;
  let sql: postgres.Sql;
  let v1Router: typeof import("../router").v1Router;
  let hashKey: typeof import("../../keys/keys").hashKey;
  let models: typeof import("../models");
  let seq = 0;

  const createOrganization = async () => {
    const id = `org_v1_${++seq}`;
    await sql`insert into organization (id, name, slug, created_at) values (${id}, ${id}, ${id}, now())`;
    return id;
  };

  const createKey = async (
    organizationId: string,
    options: {
      limit?: number;
      reset?: string;
      disabled?: boolean;
      expiresAt?: Date;
    } = {},
  ) => {
    const n = ++seq;
    const value = `sk-bf-v1-test-key-${n}-${"x".repeat(24)}`;
    const virtualKeyId = `vk_v1_${n}`;
    // a key expires after it was created (api_key_expires_after_created): an expired one is an hour old
    const createdAt = options.expiresAt
      ? new Date(options.expiresAt.getTime() - 3_600_000)
      : new Date();
    await sql`insert into api_key (id, organization_id, created_by, name, virtual_key_id, prefix, value_hash, limit_micro_sar, limit_reset, disabled_at, expires_at, created_at)
      values (${`key_${n}`}, ${organizationId}, 'user_1', ${`key ${n}`}, ${virtualKeyId}, 'sk-bf-v1-tes', ${hashKey(value)},
              ${options.limit ?? null}, ${options.reset ?? null}, ${options.disabled ? new Date() : null}, ${options.expiresAt ?? null}, ${createdAt})`;
    return { value, virtualKeyId };
  };

  let entrySeq = 0;
  const entry = async (
    organizationId: string,
    kind: string,
    amount: number,
    options: { virtualKeyId?: string; createdAt?: string } = {},
  ) => {
    const id = `entry_v1_${++entrySeq}`;
    await sql.unsafe(
      `insert into credit_ledger_entry (id, organization_id, amount, kind, idempotency_key, virtual_key_id, created_at,
         cost_micro_usd, usd_to_sar_rate_e6, fx_spread_bps, effective_usd_to_sar_rate_e6)
       values ($1, $2, $3, $4, $1, $5, ${options.createdAt ?? "now()"}, $6, $7, $8, $9)`,
      // a charge says its USD cost and rates, as the gateway writes it
      kind === "usage"
        ? [
            id,
            organizationId,
            amount,
            kind,
            options.virtualKeyId ?? null,
            Math.round(-amount / 3.75),
            3_750_000,
            0,
            3_750_000,
          ]
        : [
            id,
            organizationId,
            amount,
            kind,
            options.virtualKeyId ?? null,
            null,
            null,
            null,
            null,
          ],
    );
    await sql`insert into credit_account (organization_id, balance) values (${organizationId}, ${amount})
      on conflict (organization_id) do update set balance = credit_account.balance + ${amount}`;
  };

  const get = (path: string, headers: Record<string, string> = {}) =>
    v1Router.request(path, { headers });

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;
    sql = (await import("postgres")).default(database.url, { max: 4 });
    ({ v1Router } = await import("../router"));
    ({ hashKey } = await import("../../keys/keys"));
    models = await import("../models");
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  describe("authentication errors", () => {
    it.each([
      ["no credentials", {}, "No auth credentials found", "key_missing"],
      [
        "a malformed header",
        { Authorization: "Token abc" },
        "Malformed Authorization header: expected 'Bearer <key>'",
        "key_invalid",
      ],
      [
        "an empty bearer",
        { Authorization: "Bearer " },
        "Malformed Authorization header: expected 'Bearer <key>'",
        "key_invalid",
      ],
      [
        "an unknown key",
        { Authorization: "Bearer sk-bf-nobody" },
        "Invalid API key",
        "key_invalid",
      ],
    ])(
      "401 for %s, with its own message and the error contract's metadata",
      async (_label, headers, message, reason) => {
        const response = await get("/key", headers);
        expect(response.status).toBe(401);
        expect(await response.json()).toEqual({
          error: {
            code: 401,
            message,
            metadata: {
              limit_source: "key",
              reason,
              remedy_hint: expect.any(String),
            },
          },
        });
      },
    );

    it("401 for a disabled key and for an expired one", async () => {
      const org = await createOrganization();
      const disabled = await createKey(org, { disabled: true });
      const expired = await createKey(org, {
        expiresAt: new Date(Date.now() - 1000),
      });

      const a = await get("/key", {
        Authorization: `Bearer ${disabled.value}`,
      });
      const b = await get("/credits", {
        Authorization: `Bearer ${expired.value}`,
      });
      expect([a.status, b.status]).toEqual([401, 401]);
      expect(await a.json()).toMatchObject({
        error: {
          code: 401,
          message: "This API key is disabled",
          metadata: { limit_source: "key", reason: "key_disabled" },
        },
      });
      expect(await b.json()).toMatchObject({
        error: {
          code: 401,
          message: "This API key has expired",
          metadata: { limit_source: "key", reason: "key_expired" },
        },
      });
    });

    it("accepts the key in x-api-key, as Anthropic SDK clients send it", async () => {
      const org = await createOrganization();
      const { value } = await createKey(org);
      expect((await get("/key", { "x-api-key": value })).status).toBe(200);
    });
  });

  describe("GET /key", () => {
    it("reports usage per window and what the limit still allows, counting open holds", async () => {
      const org = await createOrganization();
      const { value, virtualKeyId } = await createKey(org, {
        limit: 10 * SAR,
        reset: "monthly",
      });
      const other = await createKey(org);

      await entry(org, "top_up", 100 * SAR);
      await entry(org, "usage", -2 * SAR, { virtualKeyId });
      // last month: counts in usage, not in usage_monthly or against a monthly limit
      await entry(org, "usage", -3 * SAR, {
        virtualKeyId,
        createdAt:
          "date_trunc('month', now() AT TIME ZONE 'UTC') - interval '1 day'",
      });
      // another key of the same organization: not this key's usage
      await entry(org, "usage", -7 * SAR, { virtualKeyId: other.virtualKeyId });
      await sql`insert into credit_reservation (id, organization_id, amount, expires_at, virtual_key_id, usd_to_sar_rate_e6, fx_spread_bps, effective_usd_to_sar_rate_e6)
        values ('res_v1', ${org}, ${SAR}, now() + interval '1 minute', ${virtualKeyId}, 3750000, 0, 3750000)`;

      const response = await get("/key", { Authorization: `Bearer ${value}` });
      expect(response.status).toBe(200);
      const { data } = (await response.json()) as {
        data: Record<string, unknown>;
      };
      expect(data).toMatchObject({
        label: "sk-bf-v1-tes…",
        limit: 10,
        limit_reset: "monthly",
        // the first of next month, 00:00 UTC
        limit_resets_at: new Date(
          Date.UTC(
            new Date().getUTCFullYear(),
            new Date().getUTCMonth() + 1,
            1,
          ),
        ).toISOString(),
        limit_remaining: 7, // 10 - 2 this month - 1 held
        usage: 5,
        usage_monthly: 2,
        // amounts are the account's currency
        currency: "SAR",
        is_management_key: false,
        disabled: false,
        expires_at: null,
      });
      expect(JSON.stringify(data)).not.toContain(virtualKeyId);
    });

    it("has no limit fields for a key without a limit", async () => {
      const org = await createOrganization();
      const { value } = await createKey(org);
      const { data } = (await (
        await get("/key", { Authorization: `Bearer ${value}` })
      ).json()) as { data: Record<string, unknown> };
      expect(data).toMatchObject({
        limit: null,
        limit_remaining: null,
        limit_reset: null,
        limit_resets_at: null,
        usage: 0,
      });
    });

    it("says when each limit window starts again, in UTC, with the functions the credits plugin counts by", async () => {
      // a Friday afternoon; the windows are the database's (migration 0040)
      const [ends] = await sql<
        { daily: string; weekly: string; monthly: string; monday: string }[]
      >`select key_limit_window_end('daily', '2026-11-27 15:00')::text as daily,
               key_limit_window_end('weekly', '2026-11-27 15:00')::text as weekly,
               key_limit_window_end('monthly', '2026-11-27 15:00')::text as monthly,
               key_limit_window_end('weekly', '2026-11-30 00:00')::text as monday`;
      expect(ends).toEqual({
        daily: "2026-11-28 00:00:00",
        weekly: "2026-11-30 00:00:00",
        monthly: "2026-12-01 00:00:00",
        // on a Monday the week ends a week later
        monday: "2026-12-07 00:00:00",
      });
    });
  });

  describe("GET /models", () => {
    // our price table (ADMIN-09, ADR 0018) is what the catalog shows, not the
    // gateway's datasheet: these prices differ from the catalog's on purpose
    const price = async (
      provider: string,
      model: string,
      input: string,
      output: string,
      cacheRead: string | null = null,
    ) => {
      await sql`insert into model_price (provider, model, effective_from, input_usd, output_usd, cache_read_usd, source, created_by)
        values (${provider}, ${model}, now() - interval '1 minute', ${input}, ${output}, ${cacheRead}, 'manual', 'staff')`;
    };

    beforeAll(async () => {
      await price(
        "openai",
        "gpt-4o-mini",
        "0.00000015",
        "0.0000006",
        "0.000000075",
      );
      await price("anthropic", "claude-x", "0.000003", "0.00001");
      await price("openai", "old", "0.0000001", "0.0000001");
      await price("humain", "allam-34b", "0.000001", "0.000002");
      await price("humain", "gpt-oss-120b", "0.000001", "0.000002");
      // a later version is not in force yet: the catalog shows the one before it
      await sql`insert into model_price (provider, model, effective_from, input_usd, output_usd, source, created_by)
        values ('anthropic', 'claude-x', now() + interval '1 day', '0.5', '0.5', 'manual', 'staff')`;
    });

    it("shows each servable model at our price in force, no markup, as decimal strings", async () => {
      models.clearModelCache();

      const { data } = await models.listPublicModels(
        gatewayWith([
          {
            name: "gpt-4o-mini",
            provider: "openai",
            context_length: 128_000,
            max_output_tokens: 16_384,
            input_cost_per_token: 0.00000015,
            output_cost_per_token: 0.0000006,
            cache_read_input_token_cost: 0.000000075,
            architecture: {
              modality: "text+image->text",
              input_modalities: ["text", "image"],
              output_modalities: ["text"],
            },
          },
          {
            name: "claude-x",
            provider: "anthropic",
            input_cost_per_token: 0.000003,
            // the datasheet's price; ours (0.00001) is what is charged and shown
            output_cost_per_token: 0.000015,
          },
          {
            name: "unpriced",
            provider: "anthropic",
            input_cost_per_token: 0.000001,
          },
          {
            // the datasheet prices it, we do not (yet): not offered
            name: "not-ours",
            provider: "openai",
            input_cost_per_token: 0.000001,
            output_cost_per_token: 0.000002,
          },
          {
            name: "old",
            provider: "openai",
            input_cost_per_token: 1e-7,
            output_cost_per_token: 1e-7,
            is_deprecated: true,
          },
        ]),
      );

      expect(data.map((model) => model.id)).toEqual([
        "anthropic/claude-x",
        "openai/gpt-4o-mini",
      ]);
      expect(data[1]).toEqual({
        id: "openai/gpt-4o-mini",
        name: "gpt-4o-mini",
        created: null,
        context_length: 128_000,
        architecture: {
          modality: "text+image->text",
          input_modalities: ["text", "image"],
          output_modalities: ["text"],
          tokenizer: null,
        },
        // the provider's price as it is: 0.15 per million
        pricing: {
          prompt: "0.00000015",
          completion: "0.0000006",
          input_cache_read: "0.000000075",
        },
        // the same × the seeded rate, 3.75, exactly: what a request is charged per token
        pricing_sar: {
          prompt: "0.0000005625",
          completion: "0.00000225",
          input_cache_read: "0.00000028125",
        },
        usd_to_sar_rate: "3.75",
        top_provider: {
          context_length: 128_000,
          max_completion_tokens: 16_384,
        },
      });
      // our price, not the datasheet's 0.000015, and not the version that starts tomorrow
      expect(data[0]?.pricing).toEqual({
        prompt: "0.000003",
        completion: "0.00001",
      });
      expect(data[0]?.pricing_sar).toEqual({
        prompt: "0.00001125",
        completion: "0.0000375",
      });

      // with the bank's 2.5% the SAR prices use the effective rate, 3.84375
      await sql`update pricing_settings set fx_spread_bps = 250 where id = 1`;
      models.clearModelCache();
      try {
        const { data: withSpread } = await models.listPublicModels(
          gatewayWith([
            {
              name: "gpt-4o-mini",
              provider: "openai",
              input_cost_per_token: 0.00000015,
              output_cost_per_token: 0.0000006,
            },
          ]),
        );
        // 0.15 per million × 3.84375
        expect(withSpread[0]).toMatchObject({
          pricing: { prompt: "0.00000015" },
          pricing_sar: { prompt: "0.0000005765625" },
          usd_to_sar_rate: "3.84375",
        });
      } finally {
        await sql`update pricing_settings set fx_spread_bps = 0 where id = 1`;
        models.clearModelCache();
      }
    });

    it("lists the newest models first, those without a creation time last", async () => {
      models.clearModelCache();
      const priced = {
        input_cost_per_token: 0.000001,
        output_cost_per_token: 0.000002,
      };

      const { data } = await models.listPublicModels(
        gatewayWith([
          { name: "old", provider: "openai", ...priced, created: 100 },
          { name: "gpt-4o-mini", provider: "openai", ...priced, created: 300 },
          { name: "allam-34b", provider: "humain", ...priced },
          { name: "claude-x", provider: "anthropic", ...priced, created: 200 },
          { name: "gpt-oss-120b", provider: "humain", ...priced },
        ]),
      );

      expect(data.map((model) => [model.id, model.created])).toEqual([
        ["openai/gpt-4o-mini", 300],
        ["anthropic/claude-x", 200],
        ["openai/old", 100],
        // no time: after the rest, by id
        ["humain/allam-34b", null],
        ["humain/gpt-oss-120b", null],
      ]);
      models.clearModelCache();
    });

    describe("for an organization with a data policy", () => {
      const catalog: GatewayModel[] = [
        {
          name: "allam-34b",
          provider: "humain",
          input_cost_per_token: 0.000001,
          output_cost_per_token: 0.000002,
          accessible_by_keys: ["k_riyadh"],
        },
        {
          name: "gpt-oss-120b",
          provider: "humain",
          input_cost_per_token: 0.000001,
          output_cost_per_token: 0.000002,
          // served only by a key the policy does not allow
          accessible_by_keys: ["k_other"],
        },
        {
          // the gateway returns a provider that has none of the keys unfiltered
          name: "gpt-4o-mini",
          provider: "openai",
          input_cost_per_token: 0.00000015,
          output_cost_per_token: 0.0000006,
        },
      ];
      const recording = (seen: (string[] | undefined)[]): GatewayClient => ({
        ...gatewayWith(catalog),
        listModels: async (keyIds) => {
          seen.push(keyIds);
          return catalog;
        },
      });

      it("lists only the models an allowed provider key serves", async () => {
        models.clearModelCache();
        const seen: (string[] | undefined)[] = [];
        const { data } = await models.listPublicModels(
          recording(seen),
          undefined,
          {
            allowAll: false,
            providers: [{ provider: "humain", keyIds: ["k_riyadh"] }],
          },
        );
        expect(seen).toEqual([["k_riyadh"]]);
        expect(data.map((model) => model.id)).toEqual(["humain/allam-34b"]);
      });

      it("lists nothing, without asking the gateway, when the policy allows no endpoint", async () => {
        const seen: (string[] | undefined)[] = [];
        const { data } = await models.listPublicModels(
          recording(seen),
          undefined,
          { allowAll: false, providers: [] },
        );
        expect(data).toEqual([]);
        expect(seen).toEqual([]);
      });

      it("keeps the public catalog and a policy's list apart", async () => {
        models.clearModelCache();
        const gateway = recording([]);
        const strict = {
          allowAll: false as const,
          providers: [{ provider: "humain", keyIds: ["k_riyadh"] }],
        };
        const open = await models.listPublicModels(gateway);
        expect(open.data).toHaveLength(3);
        expect(
          (await models.listPublicModels(gateway, undefined, strict)).data,
        ).toHaveLength(1);
        // the cached public catalog is still whole
        expect((await models.listPublicModels(gateway)).data).toHaveLength(3);
      });

      it("refuses a key that is not valid rather than showing it the whole catalog", async () => {
        for (const headers of [
          { authorization: "Bearer sk-bf-not-a-real-key" },
          { authorization: "Basic abc" },
        ]) {
          const response = await get("/models", headers);
          expect(response.status).toBe(401);
        }
      });
    });

    it("is public, and answers 503 in OpenRouter's shape when the gateway is not reachable", async () => {
      models.clearModelCache();
      // no GATEWAY_URL in tests: the gateway is not configured
      const response = await get("/models");
      expect(response.status).toBe(503);
      expect(response.headers.get("retry-after")).toBe("5");
      expect(await response.json()).toEqual({
        error: {
          code: 503,
          message: "The model catalog is unavailable. Try again shortly.",
          metadata: {
            limit_source: "service",
            reason: "catalog_unavailable",
            remedy_hint: expect.any(String),
            retry_after_seconds: 5,
          },
        },
      });
    });
  });

  describe("GET /credits", () => {
    it("returns totals whose difference is the organization's balance", async () => {
      const org = await createOrganization();
      const { value } = await createKey(org);
      await entry(org, "top_up", 25 * SAR);
      await entry(org, "usage", -1_500_000);
      await entry(org, "refund", -5 * SAR);
      await entry(org, "adjustment", 2 * SAR);

      const response = await get("/credits", {
        Authorization: `Bearer ${value}`,
      });
      expect(response.status).toBe(200);
      expect(await response.json()).toEqual({
        data: { total_credits: 22, total_usage: 1.5, currency: "SAR" },
      });
      // 22 - 1.5 = 20.5 = 25 - 1.5 - 5 + 2
    });

    it("answers only for the key's own organization", async () => {
      const mine = await createOrganization();
      const theirs = await createOrganization();
      const { value } = await createKey(mine);
      await entry(theirs, "top_up", 99 * SAR);
      expect(
        await (
          await get("/credits", { Authorization: `Bearer ${value}` })
        ).json(),
      ).toEqual({
        data: { total_credits: 0, total_usage: 0, currency: "SAR" },
      });
    });
  });
});
