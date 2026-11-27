import { afterAll, beforeAll, describe, expect, it } from "vitest";

import type { TestDatabase } from "@workspace/db/testing";
import type {
  GatewayClient,
  GatewayGenerationSummary,
} from "@workspace/gateway-client/server";
import type postgres from "postgres";

/** A gateway that answers only the generation summary, for the customer it names. */
const gatewayWithSummary = (
  customerId: string,
  summary: GatewayGenerationSummary | "down",
  calls: { from: Date; to: Date }[] = [],
): GatewayClient =>
  ({
    getGenerationSummary: async (id: string, from: Date, to: Date) => {
      calls.push({ from, to });
      if (summary === "down") {
        const { GatewayError } =
          await import("@workspace/gateway-client/server");
        throw new GatewayError("gateway down", 502);
      }
      if (id !== customerId) {
        throw new Error(`asked for another customer: ${id}`);
      }
      return summary;
    },
  }) as Partial<GatewayClient> as GatewayClient;

const latency = (avg: number, p50: number, p90: number) => ({ avg, p50, p90 });

// Needs real Postgres: the answers are sums over the ledger. Skips without DATABASE_URL.
const adminUrl = process.env.DATABASE_URL;

describe.skipIf(!adminUrl)("activity", () => {
  let database: TestDatabase;
  let sql: postgres.Sql;
  let activity: typeof import("../queries");
  let seq = 0;

  const NOW = new Date("2026-11-24T15:00:00Z");

  const createOrganization = async () => {
    const id = `org_act_${++seq}`;
    await sql`insert into organization (id, name, slug, created_at) values (${id}, ${id}, ${id}, now())`;
    return id;
  };

  const usage = async (
    organizationId: string,
    entry: {
      amount: number;
      at: string;
      virtualKeyId?: string;
      model?: string;
      provider?: string;
      prompt?: number;
      completion?: number;
      kind?: string;
      /** Cached, cache-write and reasoning tokens: absent on entries before migration 0041. */
      tokens?: { cached: number; cacheWrite: number; reasoning: number };
    },
  ) => {
    const id = `act_${++seq}`;
    const isUsage = (entry.kind ?? "usage") === "usage";
    await sql`insert into credit_ledger_entry (id, organization_id, amount, kind, idempotency_key, virtual_key_id,
        cost_micro_usd, usd_to_sar_rate_e6, fx_spread_bps, effective_usd_to_sar_rate_e6, metadata, created_at,
        cached_tokens, cache_write_tokens, reasoning_tokens)
      values (${id}, ${organizationId}, ${entry.kind === "top_up" ? entry.amount : -entry.amount}, ${entry.kind ?? "usage"}, ${id},
              ${entry.virtualKeyId ?? null},
              ${isUsage ? Math.round(entry.amount / 3.75) : null}, ${isUsage ? 3_750_000 : null}, ${isUsage ? 0 : null}, ${isUsage ? 3_750_000 : null},
              ${sql.json({ model: entry.model ?? "gpt-4o-mini", provider: entry.provider ?? "openai", prompt_tokens: entry.prompt ?? 0, completion_tokens: entry.completion ?? 0 })},
              ${entry.at}, ${entry.tokens?.cached ?? null}, ${entry.tokens?.cacheWrite ?? null}, ${entry.tokens?.reasoning ?? null})`;
  };

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;
    sql = (await import("postgres")).default(database.url, { max: 4 });
    activity = await import("../queries");
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  it("sums the window's money and tokens from the ledger and its requests from the gateway's log", async () => {
    const org = await createOrganization();
    const other = await createOrganization();
    await sql`insert into gateway_customer (organization_id, customer_id) values (${org}, 'cus_act')`;
    await sql`insert into api_key (id, organization_id, created_by, name, virtual_key_id, prefix, value_hash)
      values ('key_prod', ${org}, 'u', 'Production', 'vk_prod', 'sk-bf-prod', 'hash_prod')`;
    await sql`insert into api_key (id, organization_id, created_by, name, virtual_key_id, prefix, value_hash)
      values ('key_free', ${org}, 'u', 'Free tier', 'vk_free', 'sk-bf-free', 'hash_free')`;

    // inside the 7-day window; the first two report their cached and reasoning tokens (0041)
    await usage(org, {
      amount: 300,
      at: "2026-11-24T10:00:00Z",
      virtualKeyId: "vk_prod",
      prompt: 1000,
      completion: 500,
      tokens: { cached: 400, cacheWrite: 30, reasoning: 120 },
    });
    await usage(org, {
      amount: 200,
      at: "2026-11-24T11:00:00Z",
      virtualKeyId: "vk_prod",
      model: "claude-x",
      provider: "anthropic",
      prompt: 400,
      completion: 100,
      tokens: { cached: 0, cacheWrite: 0, reasoning: 0 },
    });
    await usage(org, {
      amount: 100,
      at: "2026-11-20T23:59:00Z",
      virtualKeyId: "vk_deleted",
      prompt: 50,
      completion: 50,
    });
    await usage(org, {
      amount: 50,
      at: "2026-11-18T00:00:00Z",
      virtualKeyId: "vk_prod",
      prompt: 10,
      completion: 5,
    });
    // outside it, not usage, or not this organization
    await usage(org, {
      amount: 999,
      at: "2026-11-17T23:59:59Z",
      virtualKeyId: "vk_prod",
    });
    await usage(org, {
      amount: 5000,
      at: "2026-11-24T09:00:00Z",
      kind: "top_up",
    });
    await usage(other, { amount: 777, at: "2026-11-24T09:00:00Z" });

    // what the gateway's log says: 6 generations (one free, one that failed on every attempt) and 2 refusals
    const summary: GatewayGenerationSummary = {
      by_day: [
        { day: "2026-11-18", generations: 1, failed: 0, refused: 0 },
        { day: "2026-11-20", generations: 1, failed: 0, refused: 0 },
        { day: "2026-11-24", generations: 4, failed: 1, refused: 2 },
      ],
      by_model: [
        { provider: "openai", model: "gpt-4o-mini", generations: 3 },
        { provider: "anthropic", model: "claude-x", generations: 1 },
        { provider: "openai", model: "gpt-free", generations: 2 },
      ],
      by_virtual_key: [
        { virtual_key_id: "vk_prod", generations: 4 },
        { virtual_key_id: "vk_deleted", generations: 1 },
        { virtual_key_id: "vk_free", generations: 1 },
      ],
      refusals: [
        {
          limit_source: "credits",
          reason: "insufficient_balance",
          status_code: 402,
          requests: 1,
        },
        {
          limit_source: "rate_limit",
          reason: "requests_per_minute",
          status_code: 429,
          requests: 1,
        },
      ],
      timings: {
        measured: 5,
        streamed: 2,
        time_to_first_token_ms: latency(210, 200, 400),
        latency_ms: latency(900, 800, 1500),
        generation_time_ms_avg: 700,
        throughput_tokens_per_second_avg: 85.5,
      },
    };
    const calls: { from: Date; to: Date }[] = [];
    const result = await activity.getActivity({
      organizationId: org,
      days: 7,
      now: NOW,
      gateway: gatewayWithSummary("cus_act", summary, calls),
    });

    expect(result.from).toBe("2026-11-18T00:00:00.000Z");
    // the gateway is asked for the same window
    expect(calls).toEqual([
      { from: new Date("2026-11-18T00:00:00Z"), to: NOW },
    ]);
    expect(result.requestLogAvailable).toBe(true);
    expect(result.totals).toEqual({
      spend: 650,
      requests: 6,
      failed: 1,
      refused: 2,
      promptTokens: 1460,
      completionTokens: 655,
      reasoningTokens: 120,
      cachedTokens: 400,
      cacheWriteTokens: 30,
      // 400 cached of the 1,400 prompt tokens of the entries that report caching
      cacheHitRate: 400 / 1400,
    });

    // the same numbers straight from the ledger
    const [ledger] = await sql<
      {
        spend: string;
        prompt: string;
        completion: string;
        cached: string;
        reasoning: string;
      }[]
    >`select -sum(amount) as spend, sum((metadata->>'prompt_tokens')::bigint) as prompt,
        sum((metadata->>'completion_tokens')::bigint) as completion,
        coalesce(sum(cached_tokens), 0) as cached, coalesce(sum(reasoning_tokens), 0) as reasoning
      from credit_ledger_entry
      where organization_id = ${org} and kind = 'usage' and created_at >= '2026-11-18'`;
    expect(result.totals.spend).toBe(Number(ledger?.spend));
    expect(result.totals.promptTokens).toBe(Number(ledger?.prompt));
    expect(result.totals.completionTokens).toBe(Number(ledger?.completion));
    expect(result.totals.cachedTokens).toBe(Number(ledger?.cached));
    expect(result.totals.reasoningTokens).toBe(Number(ledger?.reasoning));
    // and the requests straight from the log's summary
    expect(result.totals.requests).toBe(
      summary.by_day.reduce((sum, d) => sum + d.generations, 0),
    );
    expect(result.byDay.reduce((sum, d) => sum + (d.requests ?? 0), 0)).toBe(6);
    expect(result.byModel.reduce((sum, m) => sum + (m.requests ?? 0), 0)).toBe(
      6,
    );
    expect(result.byKey.reduce((sum, k) => sum + (k.requests ?? 0), 0)).toBe(6);

    expect(result.byDay).toHaveLength(7);
    expect(
      result.byDay.map((d) => [d.day, d.spend, d.requests, d.refused]),
    ).toEqual([
      ["2026-11-18", 50, 1, 0],
      ["2026-11-19", 0, 0, 0],
      ["2026-11-20", 100, 1, 0],
      ["2026-11-21", 0, 0, 0],
      ["2026-11-22", 0, 0, 0],
      ["2026-11-23", 0, 0, 0],
      ["2026-11-24", 500, 4, 2],
    ]);

    expect(result.byModel).toEqual([
      {
        model: "gpt-4o-mini",
        provider: "openai",
        spend: 450,
        requests: 3,
        promptTokens: 1060,
        completionTokens: 555,
        reasoningTokens: 120,
        cachedTokens: 400,
        cacheWriteTokens: 30,
      },
      {
        model: "claude-x",
        provider: "anthropic",
        spend: 200,
        requests: 1,
        promptTokens: 400,
        completionTokens: 100,
        reasoningTokens: 0,
        cachedTokens: 0,
        cacheWriteTokens: 0,
      },
      // free: in the log, never in the ledger
      {
        model: "gpt-free",
        provider: "openai",
        spend: 0,
        requests: 2,
        promptTokens: 0,
        completionTokens: 0,
        reasoningTokens: 0,
        cachedTokens: 0,
        cacheWriteTokens: 0,
      },
    ]);

    expect(result.byKey).toEqual([
      {
        keyId: "key_prod",
        name: "Production",
        prefix: "sk-bf-prod",
        spend: 550,
        requests: 4,
        promptTokens: 1410,
        completionTokens: 605,
        reasoningTokens: 120,
        cachedTokens: 400,
        cacheWriteTokens: 30,
      },
      // a key that no longer exists keeps its spend, without a name
      {
        keyId: null,
        name: null,
        prefix: null,
        spend: 100,
        requests: 1,
        promptTokens: 50,
        completionTokens: 50,
        reasoningTokens: 0,
        cachedTokens: 0,
        cacheWriteTokens: 0,
      },
      // a key that made only free requests
      {
        keyId: "key_free",
        name: "Free tier",
        prefix: "sk-bf-free",
        spend: 0,
        requests: 1,
        promptTokens: 0,
        completionTokens: 0,
        reasoningTokens: 0,
        cachedTokens: 0,
        cacheWriteTokens: 0,
      },
    ]);

    expect(result.refusals).toEqual([
      {
        limitSource: "credits",
        reason: "insufficient_balance",
        statusCode: 402,
        requests: 1,
      },
      {
        limitSource: "rate_limit",
        reason: "requests_per_minute",
        statusCode: 429,
        requests: 1,
      },
    ]);
    expect(result.timings).toEqual({
      measured: 5,
      streamed: 2,
      timeToFirstToken: latency(210, 200, 400),
      latency: latency(900, 800, 1500),
      generationTimeAvg: 700,
      throughputAvg: 85.5,
    });
  });

  it("still shows money and tokens when the gateway's log cannot be read, with no request counts", async () => {
    const org = await createOrganization();
    await sql`insert into gateway_customer (organization_id, customer_id) values (${org}, 'cus_down')`;
    await usage(org, {
      amount: 300,
      at: "2026-11-24T10:00:00Z",
      prompt: 10,
      completion: 5,
    });
    const result = await activity.getActivity({
      organizationId: org,
      days: 7,
      now: NOW,
      gateway: gatewayWithSummary("cus_down", "down"),
    });
    expect(result.requestLogAvailable).toBe(false);
    expect(result.totals).toMatchObject({
      spend: 300,
      requests: null,
      refused: null,
      failed: null,
    });
    expect(result.byDay.every((d) => d.requests === null)).toBe(true);
    expect(result.byModel).toMatchObject([
      { model: "gpt-4o-mini", spend: 300, requests: null },
    ]);
    expect(result.timings).toBeNull();
    // entries before 0041 say nothing about caching: no rate rather than 0%
    expect(result.totals.cacheHitRate).toBeNull();
  });

  it("is all zeros, with every day present, for an organization that has not used anything", async () => {
    const org = await createOrganization();
    const result = await activity.getActivity({
      organizationId: org,
      days: 30,
      now: NOW,
    });
    expect(result.totals).toEqual({
      spend: 0,
      requests: 0,
      failed: 0,
      refused: 0,
      promptTokens: 0,
      completionTokens: 0,
      reasoningTokens: 0,
      cachedTokens: 0,
      cacheWriteTokens: 0,
      cacheHitRate: null,
    });
    expect(result.byDay).toHaveLength(30);
    expect(result.byDay.every((d) => d.spend === 0 && d.requests === 0)).toBe(
      true,
    );
    expect(result.byModel).toEqual([]);
    expect(result.byKey).toEqual([]);
    expect(result.refusals).toEqual([]);
  });
});
