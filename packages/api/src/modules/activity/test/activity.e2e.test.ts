import { spawn } from "node:child_process";
import { mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { createServer } from "node:http";
import { createServer as createNetServer } from "node:net";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { afterAll, beforeAll, describe, expect, it } from "vitest";

import type { TestDatabase } from "@workspace/db/testing";
import type { ChildProcess } from "node:child_process";
import type { Server } from "node:http";
import type postgres from "postgres";

/**
 * SPEC ACT-01, ACT-02, ACT-04, ROUTE-04, CHARGE-06, API-04 end to end: the
 * activity and the generation record against the built gateway binary, with
 * the credits plugin on the real ledger, the gateway's request log in Postgres
 * (as deployed) and a mock provider that fails on demand and streams with a
 * delay before its first chunk. Needs DATABASE_URL and GATEWAY_E2E_BINARY;
 * skips without them.
 */
const adminUrl = process.env.DATABASE_URL;
const binary = process.env.GATEWAY_E2E_BINARY;

const ADMIN_USER = "control-plane";
const ADMIN_PASSWORD = "e2e-gateway-admin-password";
/** The mock provider waits this long before a stream's first chunk. */
const FIRST_CHUNK_DELAY_MS = 700;
/** And this long between the first chunk and the end of the stream. */
const STREAM_TAIL_MS = 400;
const USAGE = {
  prompt_tokens: 1000,
  completion_tokens: 500,
  total_tokens: 1500,
  prompt_tokens_details: { cached_tokens: 400 },
  completion_tokens_details: { reasoning_tokens: 120 },
};
/** 1 SAR in micro-SAR. */
const SAR = 1_000_000;

const freePort = () =>
  new Promise<number>((done) => {
    const probe = createNetServer().listen(0, "127.0.0.1", () => {
      const address = probe.address();
      probe.close(() =>
        done(typeof address === "object" && address ? address.port : 0),
      );
    });
  });

describe.skipIf(!adminUrl || !binary)(
  "activity and generation records through the real gateway",
  () => {
    let database: TestDatabase;
    let logsDatabase: TestDatabase;
    let sql: postgres.Sql;
    let provider: Server;
    let gatewayProcess: ChildProcess;
    let gatewayUrl = "";
    let keys: typeof import("../../keys/keys");
    let ledger: typeof import("../../credits/ledger");
    let activity: typeof import("../queries");
    let generations: typeof import("../generations");
    let v1Router: typeof import("../../v1/router").v1Router;
    const ids: Record<string, string> = {};
    let sent: Record<string, { status: number; id: string }> = {};
    let key = "";

    const chat = async (apiKey: string, body: Record<string, unknown>) => {
      const response = await fetch(`${gatewayUrl}/v1/chat/completions`, {
        method: "POST",
        headers: {
          Authorization: `Bearer ${apiKey}`,
          "Content-Type": "application/json",
        },
        body: JSON.stringify({
          model: "openai/gpt-4o-mini",
          max_tokens: 600,
          messages: [{ role: "user", content: "hi" }],
          ...body,
        }),
      });
      await response.text();
      return {
        status: response.status,
        id: response.headers.get("x-bifrost-request-id") ?? "",
      };
    };

    /** Settlement is written after the response: wait until nothing is held. */
    const settled = async () => {
      for (let i = 0; i < 100; i++) {
        const balance = await ledger.getBalance("org_act");
        if (balance.held === 0) {
          return;
        }
        await new Promise((r) => setTimeout(r, 100));
      }
      throw new Error("a hold was never settled");
    };

    beforeAll(async () => {
      const { createTestDatabase } = await import("@workspace/db/testing");
      database = await createTestDatabase(adminUrl ?? "");
      logsDatabase = await createTestDatabase(adminUrl ?? "");
      sql = (await import("postgres")).default(database.url, { max: 2 });

      provider = createServer((req, res) => {
        if (!req.url?.endsWith("/chat/completions")) {
          res.setHeader("Content-Type", "application/json");
          res.end(JSON.stringify({ object: "list", data: [] }));
          return;
        }
        let raw = "";
        req.on("data", (chunk: Buffer) => (raw += chunk.toString()));
        req.on("end", () => {
          const body = JSON.parse(raw) as {
            model: string;
            stream?: boolean;
            messages: { content: string }[];
          };
          // "fail" fails on gpt-4o-mini, so a fallback to another model can answer
          if (
            body.messages[0]?.content === "fail" &&
            body.model === "gpt-4o-mini"
          ) {
            res.statusCode = 500;
            res.setHeader("Content-Type", "application/json");
            res.end(
              JSON.stringify({
                error: { message: "upstream down", type: "server_error" },
              }),
            );
            return;
          }
          const chunk = (
            delta: Record<string, unknown>,
            finish: string | null,
          ) => ({
            id: "c1",
            object: "chat.completion.chunk",
            created: 1,
            model: body.model,
            choices: [{ index: 0, delta, finish_reason: finish }],
          });
          if (!body.stream) {
            res.setHeader("Content-Type", "application/json");
            res.end(
              JSON.stringify({
                id: "c1",
                object: "chat.completion",
                created: 1,
                model: body.model,
                choices: [
                  {
                    index: 0,
                    message: { role: "assistant", content: "hello" },
                    finish_reason: "stop",
                  },
                ],
                usage: USAGE,
              }),
            );
            return;
          }
          res.setHeader("Content-Type", "text/event-stream");
          const send = (payload: unknown) =>
            res.write(`data: ${JSON.stringify(payload)}\n\n`);
          setTimeout(() => {
            send(chunk({ content: "hel" }, null));
            setTimeout(() => {
              send(chunk({ content: "lo" }, null));
              send(chunk({}, "stop"));
              send({ ...chunk({}, null), choices: [], usage: USAGE });
              res.end("data: [DONE]\n\n");
            }, STREAM_TAIL_MS);
          }, FIRST_CHUNK_DELAY_MS);
        });
      });
      await new Promise<void>((done) => provider.listen(0, "127.0.0.1", done));
      const address = provider.address();
      const providerPort =
        typeof address === "object" && address ? address.port : 0;

      const ledgerUrl = new URL(database.url);
      ledgerUrl.searchParams.set("options", "-c role=gateway_ledger");
      const logs = new URL(logsDatabase.url);
      const pricingDir = resolve(
        __dirname,
        "../../../../../../apps/gateway/examples/configs/withlocalpricingfiles",
      );
      const appDir = mkdtempSync(join(tmpdir(), "activity-e2e-"));
      // the sample datasheet, plus a model that costs nothing
      const pricing = JSON.parse(
        readFileSync(join(pricingDir, "pricing.json"), "utf8"),
      ) as Record<string, Record<string, unknown>>;
      pricing["gpt-free"] = {
        ...pricing["gpt-4o-mini"],
        input_cost_per_token: 0,
        output_cost_per_token: 0,
        cache_read_input_token_cost: 0,
      };
      writeFileSync(join(appDir, "pricing.json"), JSON.stringify(pricing));
      writeFileSync(
        join(appDir, "config.json"),
        JSON.stringify({
          client: {
            enforce_auth_on_inference: true,
            disable_content_logging: true,
          },
          auth_config: {
            admin_username: ADMIN_USER,
            admin_password: ADMIN_PASSWORD,
            is_enabled: true,
          },
          providers: {
            openai: {
              keys: [
                {
                  name: "mock",
                  value: "sk-mock-upstream",
                  models: ["*"],
                  weight: 1,
                },
              ],
              network_config: {
                base_url: `http://127.0.0.1:${providerPort}`,
                allow_private_network: true,
                max_retries: 0,
              },
            },
          },
          plugins: [
            {
              name: "credits",
              enabled: true,
              config: { database_url: ledgerUrl.toString() },
            },
          ],
          // the deployed log store: Postgres
          logs_store: {
            enabled: true,
            type: "postgres",
            config: {
              host: logs.hostname,
              port: logs.port || "5432",
              user: decodeURIComponent(logs.username),
              password: decodeURIComponent(logs.password),
              db_name: logs.pathname.slice(1),
              ssl_mode: "disable",
            },
          },
          framework: {
            pricing: {
              pricing_url: `file://${join(appDir, "pricing.json")}`,
              model_parameters_url: `file://${join(pricingDir, "model-parameters.json")}`,
            },
          },
        }),
      );
      const port = await freePort();
      gatewayUrl = `http://127.0.0.1:${port}`;
      gatewayProcess = spawn(
        binary ?? "",
        [
          "-allow-sqlite",
          "-app-dir",
          appDir,
          "-port",
          String(port),
          "-host",
          "127.0.0.1",
        ],
        { stdio: "ignore" },
      );
      for (let i = 0; i < 300; i++) {
        const ok = await fetch(`${gatewayUrl}/health`).then(
          (r) => r.ok,
          () => false,
        );
        if (ok) break;
        await new Promise((r) => setTimeout(r, 300));
      }

      process.env.DATABASE_URL = database.url;
      process.env.GATEWAY_URL = gatewayUrl;
      process.env.GATEWAY_ADMIN_USERNAME = ADMIN_USER;
      process.env.GATEWAY_ADMIN_PASSWORD = ADMIN_PASSWORD;
      process.env.SKIP_ENV_VALIDATION = "1";
      keys = await import("../../keys/keys");
      ledger = await import("../../credits/ledger");
      activity = await import("../queries");
      generations = await import("../generations");
      ({ v1Router } = await import("../../v1/router"));

      // one organization, one key with room to spend and one capped below a single request
      await sql`insert into organization (id, name, slug, created_at) values ('org_act', 'Act', 'act', now())`;
      await ledger.applyEntry({
        organizationId: "org_act",
        amount: SAR,
        kind: "adjustment",
        idempotencyKey: "act-credit",
      });
      ({ key } = await keys.createKey({
        organizationId: "org_act",
        userId: "user_act",
        name: "main",
      }));
      const { key: capped } = await keys.createKey({
        organizationId: "org_act",
        userId: "user_act",
        name: "capped",
        limit: { limitMicroSar: 100, reset: "daily" },
      });

      sent = {
        plain: await chat(key, {}),
        fallback: await chat(key, {
          messages: [{ role: "user", content: "fail" }],
          fallbacks: ["openai/gpt-4o"],
        }),
        failed: await chat(key, {
          messages: [{ role: "user", content: "fail" }],
        }),
        free: await chat(key, { model: "openai/gpt-free" }),
        stream: await chat(key, { stream: true }),
        refused: await chat(capped, {}),
      };
      for (const [name, { id }] of Object.entries(sent)) {
        ids[name] = id;
      }
      await settled();
      // the gateway writes its log after answering: wait until every request is there
      for (let i = 0; i < 100; i++) {
        const { totals } = await activity.getActivity({
          organizationId: "org_act",
          days: 1,
        });
        if (totals.requests === 5 && totals.refused === 1) break;
        await new Promise((r) => setTimeout(r, 200));
      }
    }, 180_000);

    afterAll(async () => {
      gatewayProcess?.kill();
      await new Promise<void>((done) => provider?.close(() => done()));
      const { db } = await import("@workspace/db/server");
      await db.$client.end();
      await sql.end();
      await database.drop();
      await logsDatabase?.drop();
    });

    it("counts each request once, free and failed included, a fallback once, and refusals apart (ACT-01)", async () => {
      expect(
        Object.fromEntries(
          Object.entries(sent).map(([name, request]) => [
            name,
            { status: request.status, hasId: request.id !== "" },
          ]),
        ),
      ).toEqual({
        plain: { status: 200, hasId: true },
        fallback: { status: 200, hasId: true },
        // the provider's 500 reaches the caller as the product's 502 (api-errors.md, "provider")
        failed: { status: 502, hasId: true },
        free: { status: 200, hasId: true },
        stream: { status: 200, hasId: true },
        refused: { status: 402, hasId: true },
      });
      const result = await activity.getActivity({
        organizationId: "org_act",
        days: 1,
      });
      expect(result.requestLogAvailable).toBe(true);
      // plain, fallback, failed, free, stream: five generations; the capped key's 402 is a refusal
      expect(result.totals).toMatchObject({
        requests: 5,
        failed: 1,
        refused: 1,
      });
      expect(result.refusals).toEqual([
        {
          limitSource: "key_limit",
          reason: "request_exceeds_key_limit",
          statusCode: 402,
          requests: 1,
        },
      ]);
      // by the model that answered: the fallback counts under gpt-4o, once
      const requestsBy = Object.fromEntries(
        result.byModel.map((m) => [m.model, m.requests]),
      );
      expect(requestsBy).toEqual({
        "gpt-4o-mini": 3,
        "gpt-4o": 1,
        "gpt-free": 1,
      });
      expect(result.byDay.at(-1)).toMatchObject({ requests: 5, refused: 1 });
      expect(
        result.byKey.reduce((sum, row) => sum + (row.requests ?? 0), 0),
      ).toBe(5);

      // the spend equals the ledger's, which holds charged attempts only
      const [row] = await sql<{ spend: string; entries: string }[]>`
        select -sum(amount) as spend, count(*) as entries from credit_ledger_entry
        where organization_id = 'org_act' and kind = 'usage'`;
      expect(result.totals.spend).toBe(Number(row?.spend));
      // plain, the fallback's answering attempt, stream: nothing for the free or failed ones
      expect(Number(row?.entries)).toBe(3);
    });

    it("never charges a failed attempt: a fallback request is charged once, for the attempt that answered (CHARGE-06, ROUTE-04)", async () => {
      const entries = await sql<
        { amount: string; fallback_index: string | null }[]
      >`select amount, metadata->>'fallback_index' as fallback_index from credit_ledger_entry
        where reference = ${ids.fallback ?? ""} and kind = 'usage'`;
      expect(entries).toHaveLength(1);
      expect(entries[0]?.fallback_index).toBe("1");
      for (const id of [ids.failed, ids.free, ids.refused]) {
        const [none] = await sql<{ n: string }[]>`
          select count(*) as n from credit_ledger_entry where reference = ${id ?? ""}`;
        expect(none?.n).toBe("0");
      }

      // the record lists both attempts, the failed one at 0 with its HTTP status
      const record = await generations.getGeneration(
        "org_act",
        ids.fallback ?? "",
      );
      expect(record?.provider_responses).toMatchObject([
        {
          model: "gpt-4o-mini",
          status: "error",
          status_code: 500,
          cost: 0,
          fallback_index: 0,
        },
        {
          model: "gpt-4o",
          status: "success",
          status_code: 200,
          fallback_index: 1,
        },
      ]);
      expect(record?.provider_responses[1]?.cost).toBeGreaterThan(0);
      expect(record?.total_cost).toBe(record?.provider_responses[1]?.cost);
      expect(record?.model).toBe("gpt-4o");
      expect(
        record?.provider_responses.every((attempt) => attempt.latency !== null),
      ).toBe(true);
    });

    it("records a stream's time to first token, apart from its total latency; a plain request's latency is its whole time (ACT-02)", async () => {
      const stream = await generations.getGeneration(
        "org_act",
        ids.stream ?? "",
      );
      expect(stream?.streamed).toBe(true);
      // the provider held its first chunk for 700 ms and finished 400 ms later
      expect(stream?.latency).toBeGreaterThanOrEqual(FIRST_CHUNK_DELAY_MS - 50);
      expect(stream?.total_latency).toBeGreaterThanOrEqual(
        FIRST_CHUNK_DELAY_MS + STREAM_TAIL_MS - 50,
      );
      expect(stream?.total_latency ?? 0).toBeGreaterThan(
        (stream?.latency ?? 0) + STREAM_TAIL_MS - 100,
      );
      expect(stream?.generation_time).toBeGreaterThan(0);
      expect(stream?.tokens_per_second).toBeGreaterThan(0);
      expect(stream?.cancelled).toBe(false);

      const plain = await generations.getGeneration("org_act", ids.plain ?? "");
      expect(plain?.streamed).toBe(false);
      expect(plain?.latency).toBeGreaterThan(0);
      expect(plain?.latency).toBe(plain?.total_latency);

      // the activity's timings include the stream's first token
      const { timings } = await activity.getActivity({
        organizationId: "org_act",
        days: 1,
      });
      expect(timings).toMatchObject({ measured: 4, streamed: 1 });
      expect(timings?.timeToFirstToken.p90).toBeGreaterThan(0);
      expect(timings?.latency.avg).toBeGreaterThanOrEqual(
        timings?.timeToFirstToken.avg ?? 0,
      );
    });

    it("stores cached and reasoning tokens on each charged entry and sums them (ACT-04)", async () => {
      const entries = await sql<
        {
          cached_tokens: string;
          reasoning_tokens: string;
          cache_write_tokens: string;
        }[]
      >`select cached_tokens, reasoning_tokens, cache_write_tokens from credit_ledger_entry
        where organization_id = 'org_act' and kind = 'usage'`;
      expect(entries).toHaveLength(3);
      for (const entry of entries) {
        expect(entry).toEqual({
          cached_tokens: "400",
          reasoning_tokens: "120",
          cache_write_tokens: "0",
        });
      }
      const { totals } = await activity.getActivity({
        organizationId: "org_act",
        days: 1,
      });
      expect(totals).toMatchObject({
        promptTokens: 3000,
        completionTokens: 1500,
        cachedTokens: 1200,
        reasoningTokens: 360,
        cacheHitRate: 0.4,
      });
    });

    it("answers GET /api/v1/generation with the new fields, OpenRouter's names where the concept is the same (API-04)", async () => {
      const response = await v1Router.request(
        `/generation?id=${ids.stream ?? ""}`,
        { headers: { Authorization: `Bearer ${key}` } },
      );
      expect(response.status).toBe(200);
      const { data } = (await response.json()) as {
        data: Record<string, unknown>;
      };
      expect(data).toMatchObject({
        id: ids.stream,
        request_id: ids.stream,
        streamed: true,
        cancelled: false,
        native_tokens_prompt: 1000,
        native_tokens_completion: 500,
        native_tokens_reasoning: 120,
        native_tokens_cached: 400,
        native_tokens_cache_write: 0,
      });
      expect({
        latency: typeof data.latency,
        generation_time: typeof data.generation_time,
        total_latency: typeof data.total_latency,
      }).toEqual({
        latency: "number",
        generation_time: "number",
        total_latency: "number",
      });
      expect(data.provider_responses).toMatchObject([
        { status: "success", status_code: 200, is_byok: false },
      ]);

      // a free request: no charge, its tokens from the log
      const free = await generations.getGeneration("org_act", ids.free ?? "");
      expect(free).toMatchObject({
        total_cost: 0,
        native_tokens_prompt: 1000,
        native_tokens_reasoning: 120,
        provider_responses: [{ status_code: 200, cost: 0 }],
      });
    });
  },
);
