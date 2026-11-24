import { createServer } from "node:http";
import { afterAll, beforeAll, beforeEach, describe, expect, it } from "vitest";

import { createGatewayClient, GatewayError } from "../client";

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

  it("creates a key for every provider under the customer, with no gateway budget", async () => {
    replies.push({
      status: 200,
      body: { virtual_key: { id: "vk_1", value: "sk-bf-abc" } },
    });
    const expiresAt = new Date("2027-01-01T00:00:00Z");
    expect(
      await client().createVirtualKey({
        name: "prod",
        customerId: "cust_1",
        expiresAt,
      }),
    ).toEqual({ id: "vk_1", value: "sk-bf-abc" });
    expect(seen[0]?.body).toEqual({
      name: "prod",
      customer_id: "cust_1",
      allow_all_providers: true,
      provider_configs: [],
      is_active: true,
      expires_at: "2027-01-01T00:00:00.000Z",
    });
  });

  it("creates a key that reaches only the provider keys it is given", async () => {
    replies.push({
      status: 200,
      body: { virtual_key: { id: "vk_2", value: "sk-bf-def" } },
    });
    await client().createVirtualKey({
      name: "strict",
      customerId: "cust_1",
      access: {
        allowAll: false,
        providers: [{ provider: "humain", keyIds: ["k_riyadh"] }],
      },
    });
    expect(seen[0]?.body).toEqual({
      name: "strict",
      customer_id: "cust_1",
      allow_all_providers: false,
      provider_configs: [
        { provider: "humain", allowed_models: ["*"], key_ids: ["k_riyadh"] },
      ],
      is_active: true,
    });
  });

  it("replaces what a key may reach: a list, nothing, or everything", async () => {
    await client().setVirtualKeyAccess("vk/1", {
      allowAll: false,
      providers: [
        { provider: "openai", keyIds: ["k1", "k2"] },
        { provider: "humain", keyIds: ["k3"] },
      ],
    });
    await client().setVirtualKeyAccess("vk_1", {
      allowAll: false,
      providers: [],
    });
    await client().setVirtualKeyAccess("vk_1", { allowAll: true });
    expect(seen[0]).toMatchObject({
      method: "PUT",
      path: "/api/governance/virtual-keys/vk%2F1",
    });
    expect(seen.map((request) => request.body)).toEqual([
      {
        allow_all_providers: false,
        provider_configs: [
          { provider: "openai", allowed_models: ["*"], key_ids: ["k1", "k2"] },
          { provider: "humain", allowed_models: ["*"], key_ids: ["k3"] },
        ],
      },
      { allow_all_providers: false, provider_configs: [] },
      { allow_all_providers: true, provider_configs: [] },
    ]);
  });

  it("refuses a created key that came back without a value", async () => {
    replies.push({ status: 200, body: { virtual_key: { id: "vk_1" } } });
    await expect(
      client().createVirtualKey({ name: "x", customerId: "c" }),
    ).rejects.toThrow(/without an id or value/);
  });

  it("updates only what it is given", async () => {
    await client().updateVirtualKey("vk_1", { isActive: false });
    await client().updateVirtualKey("vk_1", { name: "renamed" });
    expect(seen.map((s) => [s.method, s.path, s.body])).toEqual([
      ["PUT", "/api/governance/virtual-keys/vk_1", { is_active: false }],
      ["PUT", "/api/governance/virtual-keys/vk_1", { name: "renamed" }],
    ]);
  });

  it("sets a customer's rate limit per minute, and removes it with nothing set", async () => {
    await client().setCustomerRateLimit("cust_1", {
      requestsPerMinute: 600,
      tokensPerMinute: 1_000_000,
    });
    await client().setCustomerRateLimit("cust_1", {
      requestsPerMinute: 60,
      tokensPerMinute: null,
    });
    await client().setCustomerRateLimit("cust_1", null);
    expect(seen.map((s) => [s.method, s.path, s.body])).toEqual([
      [
        "PUT",
        "/api/governance/customers/cust_1",
        {
          rate_limit: {
            request_max_limit: 600,
            request_reset_duration: "1m",
            token_max_limit: 1_000_000,
            token_reset_duration: "1m",
          },
        },
      ],
      [
        "PUT",
        "/api/governance/customers/cust_1",
        { rate_limit: { request_max_limit: 60, request_reset_duration: "1m" } },
      ],
      ["PUT", "/api/governance/customers/cust_1", { rate_limit: {} }],
    ]);
  });

  it("lists provider keys with their id, name, provider and state, and nothing else", async () => {
    replies.push({
      status: 200,
      body: [
        {
          key_id: "k1",
          name: "main",
          provider: "openai",
          enabled: false,
          value: "sk-live-secret",
          azure_key_config: { client_secret: "also-secret" },
        },
        // enabled is absent on a key that was never turned off
        { key_id: "k2", name: "riyadh", provider: "humain" },
        // a row without an id cannot be put on an allowlist
        { name: "broken", provider: "openai" },
      ],
    });
    const keys = await client().listProviderKeys();
    expect(seen[0]).toMatchObject({ method: "GET", path: "/api/keys" });
    expect(keys).toEqual([
      { id: "k1", name: "main", provider: "openai", isEnabled: false },
      { id: "k2", name: "riyadh", provider: "humain", isEnabled: true },
    ]);
    expect(JSON.stringify(keys)).not.toContain("secret");
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
