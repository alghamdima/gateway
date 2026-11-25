import { afterAll, beforeAll, describe, expect, it } from "vitest";

import type { TestDatabase } from "@workspace/db/testing";
import type postgres from "postgres";

// Needs real Postgres: the organization behind each row. Skips without DATABASE_URL.
const adminUrl = process.env.DATABASE_URL;

/** A gateway log row as the gateway sends it, content included. */
const row = (id: string, customerId: string | null, extra: object = {}) => ({
  id,
  timestamp: "2026-11-25T10:00:00Z",
  status: "success",
  provider: "openai",
  model: "gpt-4o-mini",
  customer_id: customerId,
  selected_key_name: "main",
  stream: false,
  latency: 120.5,
  token_usage: { prompt_tokens: 10, completion_tokens: 5 },
  cost: 0.0004,
  stop_reason: "stop",
  input_history: [{ role: "user", content: "PRIVATE PROMPT" }],
  raw_request: "PRIVATE PROMPT",
  raw_response: "PRIVATE ANSWER",
  content_summary: "PRIVATE PROMPT",
  ...extra,
});

/**
 * The real client over a fetch that answers /api/logs and records the URL
 * asked. Loaded here: a static import would open the database before the test
 * points it at its own.
 */
const gatewayWith = async (rows: object[]) => {
  const { createGatewayClient } =
    await import("@workspace/gateway-client/server");
  const asked: string[] = [];
  const client = createGatewayClient({
    url: "http://gateway.test",
    fetch: async (input: RequestInfo | URL) => {
      asked.push(input instanceof Request ? input.url : input.toString());
      return Response.json({
        logs: rows,
        pagination: { total_count: rows.length },
      });
    },
  });
  return { client, asked };
};

describe.skipIf(!adminUrl)("staff request log", () => {
  let database: TestDatabase;
  let sql: postgres.Sql;
  let requests: typeof import("../requests");

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;
    sql = (await import("postgres")).default(database.url, { max: 4 });
    requests = await import("../requests");
    await sql`insert into organization (id, name, slug, created_at) values ('org_a', 'Acme', 'acme', now()), ('org_b', 'Beta', 'beta', now()), ('org_new', 'New', 'new', now())`;
    await sql`insert into gateway_customer (organization_id, customer_id) values ('org_a', 'cust_a'), ('org_b', 'cust_b')`;
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  it("names the organization behind each request", async () => {
    const { client } = await gatewayWith([
      row("r1", "cust_a"),
      row("r2", "cust_b"),
      row("r3", "cust_gone"),
      row("r4", null),
    ]);
    const page = await requests.getStaffRequests(
      { limit: 50, offset: 0 },
      client,
    );

    expect(page.total).toBe(4);
    expect(
      page.requests.map((request) => [request.id, request.organization?.slug]),
    ).toEqual([
      ["r1", "acme"],
      ["r2", "beta"],
      ["r3", undefined],
      ["r4", undefined],
    ]);
  });

  it("never passes on what was said", async () => {
    const { client } = await gatewayWith([row("r1", "cust_a")]);
    const page = await requests.getStaffRequests(
      { limit: 50, offset: 0 },
      client,
    );

    expect(JSON.stringify(page)).not.toContain("PRIVATE");
    expect(page.requests[0]).toMatchObject({
      model: "gpt-4o-mini",
      latencyMs: 120.5,
      promptTokens: 10,
      completionTokens: 5,
      costUsd: 0.0004,
    });
  });

  it("asks the gateway for one organization's customer and one outcome", async () => {
    const { client, asked } = await gatewayWith([]);
    await requests.getStaffRequests(
      { organizationId: "org_a", status: "error", limit: 20, offset: 40 },
      client,
    );

    const url = new URL(asked[0] ?? "");
    expect(url.pathname).toBe("/api/logs");
    expect(Object.fromEntries(url.searchParams)).toMatchObject({
      customer_ids: "cust_a",
      status: "error",
      limit: "20",
      offset: "40",
    });
  });

  it("lists nothing for an organization with no gateway customer, without asking for everybody's", async () => {
    const { client, asked } = await gatewayWith([row("r1", "cust_a")]);
    const page = await requests.getStaffRequests(
      { organizationId: "org_new", limit: 50, offset: 0 },
      client,
    );

    expect(page).toEqual({ requests: [], total: 0 });
    expect(asked).toEqual([]);
  });

  it("shows the error message of a failed request", async () => {
    const { client } = await gatewayWith([
      row("r1", "cust_a", {
        status: "error",
        error_details: { error: { message: "rate limit exceeded" } },
      }),
    ]);
    const page = await requests.getStaffRequests(
      { limit: 50, offset: 0 },
      client,
    );
    expect(page.requests[0]).toMatchObject({
      status: "error",
      error: "rate limit exceeded",
    });
  });

  it("answers 503 when the gateway cannot be reached", async () => {
    const { createGatewayClient } =
      await import("@workspace/gateway-client/server");
    const client = createGatewayClient({
      url: "http://gateway.test",
      fetch: async () => {
        throw new Error("connection refused");
      },
    });
    await expect(
      requests.getStaffRequests({ limit: 50, offset: 0 }, client),
    ).rejects.toMatchObject({ status: 503 });
  });
});
