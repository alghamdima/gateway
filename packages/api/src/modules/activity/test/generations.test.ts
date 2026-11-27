import { afterAll, beforeAll, describe, expect, it } from "vitest";

import type { TestDatabase } from "@workspace/db/testing";
import type {
  GatewayAttempt,
  GatewayClient,
  GatewayLog,
} from "@workspace/gateway-client/server";
import type postgres from "postgres";

// Needs real Postgres: the record is built from ledger entries. Skips without DATABASE_URL.
const adminUrl = process.env.DATABASE_URL;

/** A gateway that only answers log and attempt lookups, from what it is given. */
const gatewayWithLogs = (
  logs: GatewayLog[],
  options: { isDown?: boolean; attempts?: GatewayAttempt[] } = {},
): GatewayClient => ({
  getGenerationSummary: async () => {
    throw new Error("not used");
  },
  getAttempts: async (id) =>
    (options.attempts ?? []).filter(
      (a) => a.id === id || a.parent_request_id === id,
    ),
  createCustomer: async () => ({ id: "c" }),
  deleteCustomer: async () => {},
  createVirtualKey: async () => ({ id: "vk", value: "v" }),
  updateVirtualKey: async () => {},
  deleteVirtualKey: async () => {},
  listModels: async () => [],
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
  setCustomerRateLimit: async () => {},
  getLog: async (id) => {
    if (options.isDown) {
      // loaded here: a static import would open the database before the test points it at its own
      const { GatewayError } = await import("@workspace/gateway-client/server");
      throw new GatewayError("gateway down");
    }
    return logs.find((log) => log.id === id);
  },
});

describe.skipIf(!adminUrl)("generation records", () => {
  let database: TestDatabase;
  let sql: postgres.Sql;
  let generations: typeof import("../generations");
  let v1Router: typeof import("../../v1/router").v1Router;
  let hashKey: typeof import("../../keys/keys").hashKey;
  let seq = 0;

  const createOrganization = async () => {
    const id = `org_gen_${++seq}`;
    await sql`insert into organization (id, name, slug, created_at) values (${id}, ${id}, ${id}, now())`;
    await sql`insert into gateway_customer (organization_id, customer_id) values (${id}, ${`cust_${id}`})`;
    return id;
  };

  const attempt = async (
    organizationId: string,
    entry: {
      reference: string;
      amount: number;
      at: string;
      provider?: string;
      failed?: boolean;
      prompt?: number;
      completion?: number;
      virtualKeyId?: string;
      providerKeyId?: string;
      /** The provider's USD cost; by default the amount at the peg. */
      costMicroUsd?: number;
      fallbackIndex?: number;
      reasoning?: number;
      cached?: number;
    },
  ) => {
    const id = `gen_${++seq}`;
    await sql`insert into credit_ledger_entry (id, organization_id, amount, kind, idempotency_key, reference, virtual_key_id,
        cost_micro_usd, usd_to_sar_rate_e6, fx_spread_bps, effective_usd_to_sar_rate_e6, metadata, created_at,
        reasoning_tokens, cached_tokens, cache_write_tokens)
      values (${id}, ${organizationId}, ${-entry.amount}, 'usage', ${id}, ${entry.reference}, ${entry.virtualKeyId ?? null},
              ${entry.costMicroUsd ?? Math.round(entry.amount / 3.75)}, 3750000, 0, 3750000,
              ${sql.json({ model: "gpt-4o-mini", provider: entry.provider ?? "openai", failed: entry.failed ?? false, prompt_tokens: entry.prompt ?? 0, completion_tokens: entry.completion ?? 0, ...(entry.fallbackIndex === undefined ? {} : { fallback_index: entry.fallbackIndex }), ...(entry.providerKeyId ? { provider_key_id: entry.providerKeyId } : {}) })},
              ${entry.at}, ${entry.reasoning ?? null}, ${entry.cached ?? null}, ${entry.cached === undefined ? null : 0})`;
  };

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;
    sql = (await import("postgres")).default(database.url, { max: 4 });
    generations = await import("../generations");
    ({ v1Router } = await import("../../v1/router"));
    ({ hashKey } = await import("../../keys/keys"));
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  it("joins every attempt of a request with the gateway's timing, in SAR with the USD cost", async () => {
    const org = await createOrganization();
    // the first provider failed after billing some tokens; the fallback answered.
    // Each is charged in micro-SAR at 3.75 and keeps its USD cost
    await attempt(org, {
      reference: "req_1",
      amount: 450,
      costMicroUsd: 120,
      at: "2026-11-24T10:00:00Z",
      provider: "openai",
      failed: true,
      prompt: 100,
      completion: 10,
    });
    await attempt(org, {
      reference: "req_1",
      amount: 1688,
      costMicroUsd: 450,
      at: "2026-11-24T10:00:01Z",
      provider: "anthropic",
      prompt: 1000,
      completion: 500,
    });

    const record = await generations.getGeneration(
      org,
      "req_1",
      gatewayWithLogs([
        {
          id: "req_1",
          customer_id: `cust_${org}`,
          provider: "anthropic",
          model: "claude-x",
          latency: 842,
          stop_reason: "stop",
          stream: true,
        },
      ]),
    );

    expect(record).toEqual({
      id: "req_1",
      request_id: "req_1",
      created_at: "2026-11-24T10:00:00.000Z",
      model: "claude-x",
      provider_name: "anthropic",
      tokens_prompt: 1100,
      tokens_completion: 510,
      native_tokens_prompt: 1100,
      native_tokens_completion: 510,
      native_tokens_reasoning: 0,
      native_tokens_cached: 0,
      native_tokens_cache_write: 0,
      total_cost: 0.002138,
      upstream_cost_usd: 0.00057,
      currency: "SAR",
      // a stream whose first chunk was not timed (a log from before ACT-02): the whole latency
      latency: 842,
      generation_time: null,
      total_latency: 842,
      tokens_per_second: null,
      cancelled: false,
      finish_reason: "stop",
      streamed: true,
      // the gateway listed no attempts: the charged ones, from the ledger
      provider_responses: [
        {
          provider: "openai",
          model: "gpt-4o-mini",
          status: "error",
          status_code: null,
          latency: null,
          fallback_index: 0,
          is_byok: false,
          cost: 0.00045,
          upstream_cost_usd: 0.00012,
          usd_to_sar_rate: 3.75,
          created_at: "2026-11-24T10:00:00.000Z",
        },
        {
          provider: "anthropic",
          model: "gpt-4o-mini",
          status: "success",
          status_code: null,
          latency: null,
          fallback_index: 0,
          is_byok: false,
          cost: 0.001688,
          upstream_cost_usd: 0.00045,
          usd_to_sar_rate: 3.75,
          created_at: "2026-11-24T10:00:01.000Z",
        },
      ],
      // entries written before provider keys were recorded name no endpoint
      data_region: null,
      data_policy: {
        endpoint: null,
        policy: {
          version: 0,
          require_zero_retention: false,
          forbid_training: false,
          allowed_regions: null,
          allowed_providers: null,
        },
      },
    });
  });

  it("lists every attempt from the gateway, a failed one at 0, and times the one that answered (ACT-02, ROUTE-04)", async () => {
    const org = await createOrganization();
    // the failed first attempt wrote no ledger entry (CHARGE-06); the fallback was charged once
    await attempt(org, {
      reference: "req_fb",
      amount: 1688,
      costMicroUsd: 450,
      at: "2026-11-24T10:00:01Z",
      provider: "openai",
      prompt: 1000,
      completion: 500,
      fallbackIndex: 1,
      reasoning: 120,
      cached: 400,
    });
    const customer = `cust_${org}`;
    const record = await generations.getGeneration(
      org,
      "req_fb",
      gatewayWithLogs(
        [
          {
            id: "req_fb",
            customer_id: customer,
            provider: "openai",
            model: "gpt-4o-mini",
            status: "error",
            latency: 12,
            stream: true,
          },
          {
            id: "req_fb_1",
            customer_id: customer,
            provider: "openai",
            model: "gpt-4o",
            status: "success",
            latency: 1000,
            stream: true,
            stop_reason: "stop",
          },
        ],
        {
          attempts: [
            {
              id: "req_fb",
              fallback_index: 0,
              timestamp: "2026-11-24T10:00:00Z",
              provider: "openai",
              model: "gpt-4o-mini",
              status: "error",
              status_code: 500,
              error_type: "server_error",
              stream: true,
              latency: 12,
              upstream_latency: 10,
              customer_id: customer,
            },
            {
              id: "req_fb_1",
              parent_request_id: "req_fb",
              fallback_index: 1,
              timestamp: "2026-11-24T10:00:01Z",
              provider: "openai",
              model: "gpt-4o",
              status: "success",
              status_code: 200,
              stream: true,
              latency: 1000,
              upstream_latency: 800,
              time_to_first_token: 250,
              customer_id: customer,
            },
          ],
        },
      ),
    );
    expect(record).toMatchObject({
      model: "gpt-4o",
      // OpenRouter's latency: a stream's time to its first chunk
      latency: 250,
      generation_time: 800,
      total_latency: 1000,
      tokens_per_second: 500 / 0.8,
      cancelled: false,
      finish_reason: "stop",
      native_tokens_reasoning: 120,
      native_tokens_cached: 400,
      total_cost: 0.001688,
      provider_responses: [
        {
          model: "gpt-4o-mini",
          status: "error",
          status_code: 500,
          latency: 12,
          fallback_index: 0,
          cost: 0,
          upstream_cost_usd: 0,
        },
        {
          model: "gpt-4o",
          status: "success",
          status_code: 200,
          latency: 1000,
          fallback_index: 1,
          cost: 0.001688,
          upstream_cost_usd: 0.00045,
          usd_to_sar_rate: 3.75,
        },
      ],
    });
  });

  it("answers a free request from the gateway's log alone, with its token counts", async () => {
    const org = await createOrganization();
    const customer = `cust_${org}`;
    const record = await generations.getGeneration(
      org,
      "req_free",
      gatewayWithLogs(
        [
          {
            id: "req_free",
            customer_id: customer,
            provider: "openai",
            model: "gpt-free",
            status: "success",
            latency: 300,
            upstream_latency: 200,
            stream: false,
            token_usage: {
              prompt_tokens: 180,
              completion_tokens: 700,
              prompt_tokens_details: { cached_read_tokens: 0 },
              completion_tokens_details: { reasoning_tokens: 629 },
            },
          },
        ],
        {
          attempts: [
            {
              id: "req_free",
              fallback_index: 0,
              timestamp: "2026-11-24T10:00:00Z",
              provider: "openai",
              model: "gpt-free",
              status: "success",
              status_code: 200,
              stream: false,
              latency: 300,
              upstream_latency: 200,
              customer_id: customer,
            },
          ],
        },
      ),
    );
    expect(record).toMatchObject({
      total_cost: 0,
      native_tokens_prompt: 180,
      native_tokens_completion: 700,
      native_tokens_reasoning: 629,
      // not streamed: the whole latency, as OpenRouter's
      latency: 300,
      generation_time: 200,
      provider_responses: [{ status: "success", status_code: 200, cost: 0 }],
    });
  });

  describe("evidence of where a request went", () => {
    /** One entry in a key's history of terms, as staff recorded it at `at`. */
    const terms = (
      keyId: string,
      provider: string,
      region: string,
      retentionDays: number | null,
      trainsOnData: boolean | null,
      at: string,
    ) =>
      sql`insert into provider_endpoint_change (key_id, provider, region, retention_days, trains_on_data, agreement, source, verified_on, changed_by, changed_at)
          values (${keyId}, ${provider}, ${region}, ${retentionDays}, ${trainsOnData}, 'direct', 'contract', '2026-11-01', 'staff', ${at})`;

    const termsRemoved = (keyId: string, provider: string, at: string) =>
      sql`insert into provider_endpoint_change (key_id, provider, is_removed, changed_by, changed_at)
          values (${keyId}, ${provider}, true, 'staff', ${at})`;

    const policyChange = (
      organizationId: string,
      version: number,
      at: string,
      regions: string[] | null,
    ) =>
      sql`insert into organization_data_policy_change (organization_id, version, require_zero_retention, forbid_training, allowed_regions, changed_by, changed_at)
          values (${organizationId}, ${version}, ${regions !== null}, false, ${regions}, 'user', ${at})`;

    it("shows the endpoint that served the request, its terms, and the policy in force then", async () => {
      const org = await createOrganization();
      await terms("k_riyadh", "humain", "sa", 0, false, "2026-11-01T00:00:00Z");
      await policyChange(org, 1, "2026-11-10T00:00:00Z", null);
      await policyChange(org, 2, "2026-11-20T00:00:00Z", ["sa"]);
      // set after the request: not what was in force
      await policyChange(org, 3, "2026-11-25T00:00:00Z", ["sa", "eu"]);
      await attempt(org, {
        reference: "req_sa",
        amount: 450,
        at: "2026-11-24T10:00:00Z",
        provider: "humain",
        providerKeyId: "k_riyadh",
      });

      const record = await generations.getGeneration(
        org,
        "req_sa",
        gatewayWithLogs([]),
      );
      expect(record?.data_region).toBe("sa");
      expect(record?.data_policy).toEqual({
        endpoint: {
          provider: "humain",
          region: "sa",
          zero_retention: true,
          never_trained_on: true,
          verified_on: "2026-11-01",
          terms_changed_since: false,
        },
        policy: {
          version: 2,
          require_zero_retention: true,
          forbid_training: false,
          allowed_regions: ["sa"],
          allowed_providers: null,
        },
      });
    });

    it("names the endpoint that answered, not the one that failed before it", async () => {
      const org = await createOrganization();
      await terms(
        "k_virginia",
        "openai",
        "us",
        30,
        true,
        "2026-11-01T00:00:00Z",
      );
      await attempt(org, {
        reference: "req_fb",
        amount: 10,
        at: "2026-11-24T11:00:00Z",
        provider: "openai",
        failed: true,
        providerKeyId: "k_virginia",
      });
      await attempt(org, {
        reference: "req_fb",
        amount: 450,
        at: "2026-11-24T11:00:01Z",
        provider: "humain",
        providerKeyId: "k_riyadh",
      });
      const record = await generations.getGeneration(
        org,
        "req_fb",
        gatewayWithLogs([]),
      );
      expect(record?.data_policy.endpoint).toMatchObject({
        provider: "humain",
        region: "sa",
      });
    });

    it("claims nothing for a key with no recorded terms", async () => {
      const org = await createOrganization();
      await attempt(org, {
        reference: "req_unknown",
        amount: 450,
        at: "2026-11-24T12:00:00Z",
        provider: "groq",
        providerKeyId: "k_never_described",
      });
      const record = await generations.getGeneration(
        org,
        "req_unknown",
        gatewayWithLogs([]),
      );
      expect(record?.data_region).toBeNull();
      expect(record?.data_policy.endpoint).toEqual({
        provider: "groq",
        region: null,
        zero_retention: false,
        never_trained_on: false,
        verified_on: null,
        terms_changed_since: false,
      });
    });

    it("shows the terms recorded when the request was made, not a later edit, and says they changed", async () => {
      const org = await createOrganization();
      // zero retention when the request was served; staff learned of 30 days that evening
      await terms("k_edited", "azure", "eu", 0, false, "2026-11-20T00:00:00Z");
      await terms("k_edited", "azure", "eu", 30, true, "2026-11-24T18:00:00Z");
      await attempt(org, {
        reference: "req_edited",
        amount: 450,
        at: "2026-11-24T13:00:00Z",
        provider: "azure",
        providerKeyId: "k_edited",
      });
      const record = await generations.getGeneration(
        org,
        "req_edited",
        gatewayWithLogs([]),
      );
      expect(record?.data_policy.endpoint).toEqual({
        provider: "azure",
        region: "eu",
        zero_retention: true,
        never_trained_on: true,
        verified_on: "2026-11-01",
        terms_changed_since: true,
      });
    });

    it("claims nothing for terms that were only recorded after the request", async () => {
      const org = await createOrganization();
      await terms("k_late", "groq", "us", 0, false, "2026-11-24T18:00:00Z");
      await attempt(org, {
        reference: "req_late",
        amount: 450,
        at: "2026-11-24T13:00:00Z",
        provider: "groq",
        providerKeyId: "k_late",
      });
      const record = await generations.getGeneration(
        org,
        "req_late",
        gatewayWithLogs([]),
      );
      expect(record?.data_region).toBeNull();
      expect(record?.data_policy.endpoint).toEqual({
        provider: "groq",
        region: null,
        zero_retention: false,
        never_trained_on: false,
        verified_on: null,
        terms_changed_since: true,
      });
    });

    it("claims nothing for terms that had been removed by then", async () => {
      const org = await createOrganization();
      await terms(
        "k_removed",
        "cohere",
        "eu",
        0,
        false,
        "2026-11-10T00:00:00Z",
      );
      await termsRemoved("k_removed", "cohere", "2026-11-20T00:00:00Z");
      await attempt(org, {
        reference: "req_removed",
        amount: 450,
        at: "2026-11-24T13:00:00Z",
        provider: "cohere",
        providerKeyId: "k_removed",
      });
      const record = await generations.getGeneration(
        org,
        "req_removed",
        gatewayWithLogs([]),
      );
      expect(record?.data_policy.endpoint).toMatchObject({
        provider: "cohere",
        region: null,
        zero_retention: false,
        never_trained_on: false,
        terms_changed_since: false,
      });
    });

    it("keeps the policy history: no update, no delete", async () => {
      const org = await createOrganization();
      await policyChange(org, 1, "2026-11-10T00:00:00Z", ["sa"]);
      await expect(
        sql`update organization_data_policy_change set allowed_regions = null where organization_id = ${org}`,
      ).rejects.toThrow("append-only");
      await expect(
        sql`delete from organization_data_policy_change where organization_id = ${org}`,
      ).rejects.toThrow("append-only");
    });
  });

  it("never shows another organization's request, even under the same id", async () => {
    const mine = await createOrganization();
    const theirs = await createOrganization();
    // a caller can choose the request id, so two organizations can share one
    await attempt(theirs, {
      reference: "shared",
      amount: 999,
      at: "2026-11-24T10:00:00Z",
    });
    const theirLog = {
      id: "shared",
      customer_id: `cust_${theirs}`,
      latency: 1,
      stop_reason: "length",
    };

    expect(
      await generations.getGeneration(
        mine,
        "shared",
        gatewayWithLogs([theirLog]),
      ),
    ).toBeUndefined();

    await attempt(mine, {
      reference: "shared",
      amount: 5,
      at: "2026-11-24T11:00:00Z",
    });
    const record = await generations.getGeneration(
      mine,
      "shared",
      gatewayWithLogs([theirLog]),
    );
    expect(record).toMatchObject({
      total_cost: 0.000005,
      latency: null,
      finish_reason: null,
    });
  });

  it("still answers from the ledger when the log has expired or the gateway is down", async () => {
    const org = await createOrganization();
    await attempt(org, {
      reference: "old",
      amount: 300,
      at: "2026-06-01T00:00:00Z",
      prompt: 10,
      completion: 5,
    });

    for (const gateway of [
      gatewayWithLogs([]),
      gatewayWithLogs([], { isDown: true }),
    ]) {
      expect(
        await generations.getGeneration(org, "old", gateway),
      ).toMatchObject({
        total_cost: 0.0003,
        tokens_prompt: 10,
        latency: null,
        streamed: null,
      });
    }
    expect(
      await generations.getGeneration(
        org,
        "never-happened",
        gatewayWithLogs([]),
      ),
    ).toBeUndefined();
  });

  it("lists the latest charged requests, newest first, with the key, page by page, one row per request", async () => {
    const org = await createOrganization();
    const other = await createOrganization();
    await sql`insert into api_key (id, organization_id, created_by, name, virtual_key_id, prefix, value_hash)
      values ('key_gen', ${org}, 'u', 'Production', 'vk_gen', 'sk-bf-gen', 'hash_gen')`;
    await attempt(org, {
      reference: "a",
      amount: 1,
      at: "2026-11-24T10:00:00Z",
      virtualKeyId: "vk_gen",
    });
    await attempt(org, {
      reference: "b",
      amount: 2,
      at: "2026-11-24T11:00:00Z",
      virtualKeyId: "vk_gen",
    });
    await attempt(org, {
      reference: "c",
      amount: 3,
      at: "2026-11-24T12:00:00Z",
      virtualKeyId: "vk_gone",
    });
    await attempt(other, {
      reference: "x",
      amount: 9,
      at: "2026-11-24T13:00:00Z",
    });
    // one request, two charged attempts (billed before failing, then the fallback): one row
    await attempt(org, {
      reference: "d",
      amount: 4,
      at: "2026-11-24T14:00:00Z",
      provider: "openai",
      failed: true,
      prompt: 10,
      virtualKeyId: "vk_gen",
    });
    await attempt(org, {
      reference: "d",
      amount: 5,
      at: "2026-11-24T14:00:01Z",
      provider: "anthropic",
      prompt: 20,
      virtualKeyId: "vk_gen",
    });
    const [request] = await generations.listRecentRequests({
      organizationId: org,
      limit: 1,
    });
    expect(request).toMatchObject({
      id: "d",
      cost: 9,
      promptTokens: 30,
      provider: "anthropic",
      failed: false,
      createdAt: "2026-11-24T14:00:01.000Z",
    });

    const first = await generations.listRecentRequests({
      before: new Date("2026-11-24T14:00:00Z"),
      organizationId: org,
      limit: 2,
    });
    expect(first.map((r) => [r.id, r.cost, r.keyName])).toEqual([
      ["c", 3, null],
      ["b", 2, "Production"],
    ]);
    const next = await generations.listRecentRequests({
      organizationId: org,
      limit: 2,
      before: new Date(first.at(-1)?.createdAt ?? ""),
    });
    expect(next.map((r) => r.id)).toEqual(["a"]);
  });

  it("answers GET /api/v1/generation to any key of the organization, and only for its requests", async () => {
    const org = await createOrganization();
    const other = await createOrganization();
    const value = `sk-bf-gen-test-${"y".repeat(30)}`;
    await sql`insert into api_key (id, organization_id, created_by, name, virtual_key_id, prefix, value_hash)
      values ('key_v1gen', ${org}, 'u', 'k', 'vk_v1gen', 'sk-bf-gen-t', ${hashKey(value)})`;
    await attempt(org, {
      reference: "mine",
      amount: 450,
      at: "2026-11-24T10:00:00Z",
      prompt: 1000,
      completion: 500,
    });
    await attempt(other, {
      reference: "theirs",
      amount: 450,
      at: "2026-11-24T10:00:00Z",
    });
    const get = (query: string) =>
      v1Router.request(`/generation${query}`, {
        headers: { Authorization: `Bearer ${value}` },
      });

    const found = await get("?id=mine");
    expect(found.status).toBe(200);
    expect(await found.json()).toMatchObject({
      data: {
        id: "mine",
        total_cost: 0.00045,
        tokens_prompt: 1000,
        tokens_completion: 500,
      },
    });
    expect((await get("?id=theirs")).status).toBe(404);
    expect(await (await get("")).json()).toEqual({
      error: { code: 400, message: "Missing the id query parameter" },
    });
    expect((await v1Router.request("/generation?id=mine")).status).toBe(401);
  });
});
