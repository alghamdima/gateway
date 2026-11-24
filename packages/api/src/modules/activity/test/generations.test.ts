import { afterAll, beforeAll, describe, expect, it } from "vitest";

import type { TestDatabase } from "@workspace/db/testing";
import type { GatewayClient, GatewayLog } from "@workspace/gateway/server";
import type postgres from "postgres";

// Needs real Postgres: the record is built from ledger entries. Skips without DATABASE_URL.
const adminUrl = process.env.DATABASE_URL;

/** A gateway that only answers log lookups, from the logs it is given. */
const gatewayWithLogs = (
  logs: GatewayLog[],
  options: { isDown?: boolean } = {},
): GatewayClient => ({
  createCustomer: async () => ({ id: "c" }),
  deleteCustomer: async () => {},
  createVirtualKey: async () => ({ id: "vk", value: "v" }),
  updateVirtualKey: async () => {},
  deleteVirtualKey: async () => {},
  listModels: async () => [],
  getCustomerCosts: async () => [],
  setCustomerRateLimit: async () => {},
  getLog: async (id) => {
    if (options.isDown) {
      // loaded here: a static import would open the database before the test points it at its own
      const { GatewayError } = await import("@workspace/gateway/server");
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
    },
  ) => {
    const id = `gen_${++seq}`;
    await sql`insert into credit_ledger_entry (id, organization_id, amount, kind, idempotency_key, reference, virtual_key_id, metadata, created_at)
      values (${id}, ${organizationId}, ${-entry.amount}, 'usage', ${id}, ${entry.reference}, ${entry.virtualKeyId ?? null},
              ${sql.json({ model: "gpt-4o-mini", provider: entry.provider ?? "openai", failed: entry.failed ?? false, prompt_tokens: entry.prompt ?? 0, completion_tokens: entry.completion ?? 0 })},
              ${entry.at})`;
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

  it("joins every attempt of a request with the gateway's timing", async () => {
    const org = await createOrganization();
    // the first provider failed after billing some tokens; the fallback answered
    await attempt(org, {
      reference: "req_1",
      amount: 120,
      at: "2026-11-24T10:00:00Z",
      provider: "openai",
      failed: true,
      prompt: 100,
      completion: 10,
    });
    await attempt(org, {
      reference: "req_1",
      amount: 450,
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
      created_at: "2026-11-24T10:00:00.000Z",
      model: "claude-x",
      provider_name: "anthropic",
      tokens_prompt: 1100,
      tokens_completion: 510,
      total_cost: 0.00057,
      latency: 842,
      finish_reason: "stop",
      streamed: true,
      provider_responses: [
        {
          provider: "openai",
          model: "gpt-4o-mini",
          status: "error",
          cost: 0.00012,
          created_at: "2026-11-24T10:00:00.000Z",
        },
        {
          provider: "anthropic",
          model: "gpt-4o-mini",
          status: "success",
          cost: 0.00045,
          created_at: "2026-11-24T10:00:01.000Z",
        },
      ],
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

  it("lists the latest charged attempts, newest first, with the key, page by page", async () => {
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

    const first = await generations.listRecentRequests({
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
