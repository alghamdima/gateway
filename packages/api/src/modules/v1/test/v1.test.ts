import { afterAll, beforeAll, describe, expect, it } from "vitest";

import type { TestDatabase } from "@workspace/db/testing";
import type { GatewayClient, GatewayModel } from "@workspace/gateway/server";
import type postgres from "postgres";

// Needs real Postgres: the answers are sums over the ledger. Skips without DATABASE_URL.
const adminUrl = process.env.DATABASE_URL;

const USD = 1_000_000;

/** A gateway that only knows its model catalog. */
const gatewayWith = (models: GatewayModel[]): GatewayClient => ({
  createCustomer: async () => ({ id: "c" }),
  deleteCustomer: async () => {},
  createVirtualKey: async () => ({ id: "vk", value: "v" }),
  updateVirtualKey: async () => {},
  deleteVirtualKey: async () => {},
  listModels: async () => models,
  listProviderKeys: async () => [],
  getCustomerCosts: async () => [],
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
    await sql`insert into api_key (id, organization_id, created_by, name, virtual_key_id, prefix, value_hash, limit_micro_usd, limit_reset, disabled_at, expires_at)
      values (${`key_${n}`}, ${organizationId}, 'user_1', ${`key ${n}`}, ${virtualKeyId}, 'sk-bf-v1-tes', ${hashKey(value)},
              ${options.limit ?? null}, ${options.reset ?? null}, ${options.disabled ? new Date() : null}, ${options.expiresAt ?? null})`;
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
      `insert into credit_ledger_entry (id, organization_id, amount, kind, idempotency_key, virtual_key_id, created_at)
       values ($1, $2, $3, $4, $1, $5, ${options.createdAt ?? "now()"})`,
      [id, organizationId, amount, kind, options.virtualKeyId ?? null],
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
      ["no credentials", {}, "No auth credentials found"],
      [
        "a malformed header",
        { Authorization: "Token abc" },
        "Malformed Authorization header: expected 'Bearer <key>'",
      ],
      [
        "an empty bearer",
        { Authorization: "Bearer " },
        "Malformed Authorization header: expected 'Bearer <key>'",
      ],
      [
        "an unknown key",
        { Authorization: "Bearer sk-bf-nobody" },
        "Invalid API key",
      ],
    ])("401 for %s, with its own message", async (_label, headers, message) => {
      const response = await get("/key", headers);
      expect(response.status).toBe(401);
      expect(await response.json()).toEqual({ error: { code: 401, message } });
    });

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
      expect(
        ((await a.json()) as { error: { message: string } }).error.message,
      ).toBe("This API key is disabled");
      expect(
        ((await b.json()) as { error: { message: string } }).error.message,
      ).toBe("This API key has expired");
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
        limit: 10 * USD,
        reset: "monthly",
      });
      const other = await createKey(org);

      await entry(org, "top_up", 100 * USD);
      await entry(org, "usage", -2 * USD, { virtualKeyId });
      // last month: counts in usage, not in usage_monthly or against a monthly limit
      await entry(org, "usage", -3 * USD, {
        virtualKeyId,
        createdAt:
          "date_trunc('month', now() AT TIME ZONE 'UTC') - interval '1 day'",
      });
      // another key of the same organization: not this key's usage
      await entry(org, "usage", -7 * USD, { virtualKeyId: other.virtualKeyId });
      await sql`insert into credit_reservation (id, organization_id, amount, expires_at, virtual_key_id)
        values ('res_v1', ${org}, ${USD}, now() + interval '1 minute', ${virtualKeyId})`;

      const response = await get("/key", { Authorization: `Bearer ${value}` });
      expect(response.status).toBe(200);
      const { data } = (await response.json()) as {
        data: Record<string, unknown>;
      };
      expect(data).toMatchObject({
        label: "sk-bf-v1-tes…",
        limit: 10,
        limit_reset: "monthly",
        limit_remaining: 7, // 10 - 2 this month - 1 held
        usage: 5,
        usage_monthly: 2,
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
        usage: 0,
      });
    });
  });

  describe("GET /models", () => {
    it("shows each servable model with our price for its provider, as decimal strings", async () => {
      await sql`update pricing_settings set default_markup_bps = 100 where id = 1`;
      await sql`insert into provider_markup (provider, markup_bps) values ('openai', 500)
        on conflict (provider) do update set markup_bps = 500`;
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
            output_cost_per_token: 0.000015,
            // a gateway override is what the request is charged, so it is what is shown
            overridden_pricing: { output_cost_per_token: 0.00001 },
          },
          {
            name: "unpriced",
            provider: "anthropic",
            input_cost_per_token: 0.000001,
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
        context_length: 128_000,
        architecture: {
          modality: "text+image->text",
          input_modalities: ["text", "image"],
          output_modalities: ["text"],
          tokenizer: null,
        },
        pricing: {
          prompt: "0.0000001575", // 0.15 per million + 5%
          completion: "0.00000063",
          input_cache_read: "0.00000007875",
        },
        top_provider: {
          context_length: 128_000,
          max_completion_tokens: 16_384,
        },
      });
      // anthropic has no provider rule: the 1% default
      expect(data[0]?.pricing).toEqual({
        prompt: "0.00000303",
        completion: "0.0000101",
      });
    });

    it("is public, and answers 503 in OpenRouter's shape when the gateway is not reachable", async () => {
      models.clearModelCache();
      // no GATEWAY_URL in tests: the gateway is not configured
      const response = await get("/models");
      expect(response.status).toBe(503);
      expect(await response.json()).toEqual({
        error: {
          code: 503,
          message: "The model catalog is unavailable. Try again shortly.",
        },
      });
    });
  });

  describe("GET /credits", () => {
    it("returns totals whose difference is the organization's balance", async () => {
      const org = await createOrganization();
      const { value } = await createKey(org);
      await entry(org, "top_up", 25 * USD);
      await entry(org, "usage", -1_500_000);
      await entry(org, "refund", -5 * USD);
      await entry(org, "adjustment", 2 * USD);

      const response = await get("/credits", {
        Authorization: `Bearer ${value}`,
      });
      expect(response.status).toBe(200);
      expect(await response.json()).toEqual({
        data: { total_credits: 22, total_usage: 1.5 },
      });
      // 22 - 1.5 = 20.5 = 25 - 1.5 - 5 + 2
    });

    it("answers only for the key's own organization", async () => {
      const mine = await createOrganization();
      const theirs = await createOrganization();
      const { value } = await createKey(mine);
      await entry(theirs, "top_up", 99 * USD);
      expect(
        await (
          await get("/credits", { Authorization: `Bearer ${value}` })
        ).json(),
      ).toEqual({
        data: { total_credits: 0, total_usage: 0 },
      });
    });
  });
});
