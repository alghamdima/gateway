import { createServer } from "node:http";
import { afterAll, beforeAll, beforeEach, describe, expect, it } from "vitest";

import { createGatewayClient, GatewayError, toBudgets } from "../client";

import type { IncomingMessage, Server } from "node:http";

interface Seen {
  method: string;
  path: string;
  authorization?: string;
  body: unknown;
}

// A stand-in for the gateway's governance API: records each request and
// answers with whatever the test queued.
let server: Server;
let url = "";
let seen: Seen[] = [];
let replies: { status: number; body?: unknown; delayMs?: number }[] = [];

const readBody = (req: IncomingMessage) =>
  new Promise<unknown>((resolve) => {
    let raw = "";
    req.on("data", (chunk: Buffer) => (raw += chunk.toString()));
    req.on("end", () => resolve(raw ? JSON.parse(raw) : undefined));
  });

beforeAll(async () => {
  server = createServer(async (req, res) => {
    seen.push({
      method: req.method ?? "",
      path: req.url ?? "",
      authorization: req.headers.authorization,
      body: await readBody(req),
    });
    const reply = replies.shift() ?? { status: 200, body: {} };
    if (reply.delayMs) {
      await new Promise((r) => setTimeout(r, reply.delayMs));
    }
    res.writeHead(reply.status, { "Content-Type": "application/json" });
    res.end(reply.body === undefined ? "" : JSON.stringify(reply.body));
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  url = `http://127.0.0.1:${typeof address === "object" && address ? address.port : 0}/`;
});

afterAll(() => new Promise<void>((resolve) => server.close(() => resolve())));

beforeEach(() => {
  seen = [];
  replies = [];
});

const client = () =>
  createGatewayClient({
    url,
    username: "cp-admin",
    password: "s3cret",
    timeoutMs: 300,
  });

describe("gateway client", () => {
  it("authenticates with the admin user and creates a customer", async () => {
    replies.push({ status: 200, body: { customer: { id: "cust_1" } } });
    expect(await client().createCustomer("org_1")).toEqual({ id: "cust_1" });
    expect(seen[0]).toEqual({
      method: "POST",
      path: "/api/governance/customers",
      authorization: `Basic ${Buffer.from("cp-admin:s3cret").toString("base64")}`,
      body: { name: "org_1" },
    });
  });

  it("sends no Authorization header when no admin user is configured", async () => {
    replies.push({ status: 200, body: { customer: { id: "cust_1" } } });
    await createGatewayClient({ url }).createCustomer("org_1");
    expect(seen[0]?.authorization).toBeUndefined();
  });

  it("creates a key for every provider under the customer, with its limit", async () => {
    replies.push({
      status: 200,
      body: { virtual_key: { id: "vk_1", value: "sk-bf-abc" } },
    });
    const expiresAt = new Date("2027-01-01T00:00:00Z");
    expect(
      await client().createVirtualKey({
        name: "prod",
        customerId: "cust_1",
        limit: { limitMicroUsd: 25_000_000, reset: "monthly" },
        expiresAt,
      }),
    ).toEqual({ id: "vk_1", value: "sk-bf-abc" });
    expect(seen[0]?.body).toEqual({
      name: "prod",
      customer_id: "cust_1",
      allow_all_providers: true,
      is_active: true,
      calendar_aligned: true,
      budgets: [{ max_limit: 25, reset_duration: "1M" }],
      expires_at: "2027-01-01T00:00:00.000Z",
    });
  });

  it("refuses a created key that came back without a value", async () => {
    replies.push({ status: 200, body: { virtual_key: { id: "vk_1" } } });
    await expect(
      client().createVirtualKey({ name: "x", customerId: "c" }),
    ).rejects.toThrow(/without an id or value/);
  });

  it("updates only what it is given, and null removes the limit", async () => {
    await client().updateVirtualKey("vk_1", { isActive: false });
    await client().updateVirtualKey("vk_1", { name: "renamed", limit: null });
    expect(seen.map((s) => [s.method, s.path, s.body])).toEqual([
      ["PUT", "/api/governance/virtual-keys/vk_1", { is_active: false }],
      [
        "PUT",
        "/api/governance/virtual-keys/vk_1",
        { name: "renamed", budgets: [], calendar_aligned: true },
      ],
    ]);
  });

  it("treats a key or customer that is already gone as deleted", async () => {
    replies.push({ status: 404, body: { error: "not found" } });
    replies.push({ status: 404, body: { error: "not found" } });
    await expect(client().deleteVirtualKey("vk_gone")).resolves.toBeUndefined();
    await expect(client().deleteCustomer("cust_gone")).resolves.toBeUndefined();
  });

  it("reports an error status with what the gateway said", async () => {
    replies.push({
      status: 400,
      body: { error: "expires_at must be a future timestamp" },
    });
    const error = await client()
      .createVirtualKey({ name: "x", customerId: "c" })
      .catch((e: unknown) => e);
    expect(error).toBeInstanceOf(GatewayError);
    expect(error).toMatchObject({ status: 400 });
    expect(String(error)).toMatch(/future timestamp/);
  });

  it("gives up on a gateway that does not answer in time", async () => {
    replies.push({ status: 200, body: {}, delayMs: 2000 });
    await expect(client().deleteVirtualKey("vk_slow")).rejects.toBeInstanceOf(
      GatewayError,
    );
  });

  it("escapes ids in paths", async () => {
    await client().deleteVirtualKey("../customers/x");
    expect(seen[0]?.path).toBe(
      "/api/governance/virtual-keys/..%2Fcustomers%2Fx",
    );
  });
});

describe("spending limits as gateway budgets", () => {
  it.each([
    [
      { limitMicroUsd: 1_000_000, reset: "daily" as const },
      [{ max_limit: 1, reset_duration: "1d" }],
    ],
    [
      { limitMicroUsd: 2_500_000, reset: "weekly" as const },
      [{ max_limit: 2.5, reset_duration: "1w" }],
    ],
    [
      { limitMicroUsd: 100, reset: null },
      [{ max_limit: 0.0001, reset_duration: "100Y" }],
    ],
    [null, []],
  ])("%j", (limit, budgets) => {
    expect(toBudgets(limit)).toEqual(budgets);
  });
});
