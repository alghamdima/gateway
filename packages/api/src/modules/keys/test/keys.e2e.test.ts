import { spawn } from "node:child_process";
import { mkdtempSync, writeFileSync } from "node:fs";
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
 * Phase 5 end to end, without the browser: the API's own key code against the
 * built gateway binary, with the credits plugin on the real ledger and a mock
 * model provider. Needs DATABASE_URL and GATEWAY_E2E_BINARY (the gateway
 * built from apps/gateway/transports/bifrost-http); skips without them.
 */
const adminUrl = process.env.DATABASE_URL;
const binary = process.env.GATEWAY_E2E_BINARY;

const ADMIN_USER = "control-plane";
const ADMIN_PASSWORD = "e2e-gateway-admin-password";
const PROMPT_TOKENS = 1000;
const COMPLETION_TOKENS = 500;
// gpt-4o-mini in the local pricing file: 0.15 USD per million input, 0.60 per million output, so the
// provider's cost is 450 micro-USD. The ledger is in micro-SAR (ADR 0017): 450 x 3.75 (the seeded rate,
// no bank spread, no markup) = 1687.5, and the gateway's float cost sits a hair under 0.00045 USD, so the
// charge rounds to 1687 micro-SAR, the same figure the credits plugin's end-to-end test pins.
const EXPECTED_CHARGE = 1687;
/** The starting balance: 1 SAR in micro-SAR. */
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
  "API keys through the real gateway",
  () => {
    let database: TestDatabase;
    let sql: postgres.Sql;
    let provider: Server;
    let gatewayProcess: ChildProcess;
    let gatewayUrl = "";
    let providerCalls = 0;
    let keys: typeof import("../keys");
    let ledger: typeof import("../../credits/ledger");
    let removeOrganizationFromGateway: (typeof import("@workspace/gateway-client/server"))["removeOrganizationFromGateway"];

    const chat = (key: string, maxTokens?: number) =>
      fetch(`${gatewayUrl}/v1/chat/completions`, {
        method: "POST",
        headers: {
          Authorization: `Bearer ${key}`,
          "Content-Type": "application/json",
        },
        body: JSON.stringify({
          model: "openai/gpt-4o-mini",
          messages: [{ role: "user", content: "hi" }],
          ...(maxTokens ? { max_tokens: maxTokens } : {}),
        }),
      });

    /**
     * The product's error contract (SPEC API-08, docs/platform/api-errors.md),
     * read as OpenAI SDKs and OpenRouter clients read it: error.message and
     * error.type, error.code the HTTP status as a number, error.metadata.
     */
    interface RefusalBody {
      error: {
        code: number;
        message: string;
        type?: string;
        metadata?: Record<string, unknown>;
      };
    }

    const refusalOf = async (response: Response, status: number) => {
      const text = await response.text();
      // the body is shown when the status is not the one expected
      expect({ status: response.status, body: text }).toMatchObject({ status });
      const body = JSON.parse(text) as RefusalBody;
      expect(body.error.code).toBe(status);
      expect(typeof body.error.message).toBe("string");
      expect(body.error.message.length).toBeGreaterThan(0);
      expect(typeof body.error.metadata?.remedy_hint).toBe("string");
      // nothing secret: no upstream key, no gateway admin password
      expect(text).not.toContain("sk-mock-upstream");
      expect(text).not.toContain(ADMIN_PASSWORD);
      return { body, text, metadata: body.error.metadata ?? {} };
    };

    /** Settlement happens after the response: wait until nothing is held. */
    const settled = async (organizationId: string) => {
      for (let i = 0; i < 100; i++) {
        const balance = await ledger.getBalance(organizationId);
        if (balance.held === 0) {
          return balance;
        }
        await new Promise((r) => setTimeout(r, 100));
      }
      throw new Error("a hold was never settled");
    };

    const adminAuthorization = `Basic ${Buffer.from(`${ADMIN_USER}:${ADMIN_PASSWORD}`).toString("base64")}`;

    /** The virtual key as the gateway holds it; undefined when it is gone. */
    const gatewayKey = async (virtualKeyId: string) => {
      const response = await fetch(
        `${gatewayUrl}/api/governance/virtual-keys/${encodeURIComponent(virtualKeyId)}`,
        { headers: { Authorization: adminAuthorization } },
      );
      if (response.status === 404) {
        return undefined;
      }
      expect(response.status).toBe(200);
      const body = (await response.json()) as {
        virtual_key: { expires_at?: string | null; is_active?: boolean };
      };
      return body.virtual_key;
    };

    const stored = async (keyId: string) => {
      const [row] = await sql<
        { virtual_key_id: string; expires_at: Date | null; created_at: Date }[]
      >`select virtual_key_id, expires_at, created_at from api_key where id = ${keyId}`;
      if (!row) {
        throw new Error(`no api_key ${keyId}`);
      }
      return row;
    };

    /** The gateway's expiry and ours, as milliseconds; null for none. */
    const expiries = async (keyId: string) => {
      const row = await stored(keyId);
      const virtualKey = await gatewayKey(row.virtual_key_id);
      return {
        ours: row.expires_at?.getTime() ?? null,
        gateway: virtualKey?.expires_at
          ? new Date(virtualKey.expires_at).getTime()
          : null,
      };
    };

    beforeAll(async () => {
      const { createTestDatabase } = await import("@workspace/db/testing");
      database = await createTestDatabase(adminUrl ?? "");
      sql = (await import("postgres")).default(database.url, { max: 2 });

      provider = createServer((req, res) => {
        res.setHeader("Content-Type", "application/json");
        if (!req.url?.endsWith("/chat/completions")) {
          res.end(
            JSON.stringify({
              object: "list",
              data: [{ id: "gpt-4o-mini", object: "model" }],
            }),
          );
          return;
        }
        providerCalls++;
        req.resume();
        res.end(
          JSON.stringify({
            id: "chatcmpl-1",
            object: "chat.completion",
            created: 1,
            model: "gpt-4o-mini",
            choices: [
              {
                index: 0,
                message: { role: "assistant", content: "hello" },
                finish_reason: "stop",
              },
            ],
            usage: {
              prompt_tokens: PROMPT_TOKENS,
              completion_tokens: COMPLETION_TOKENS,
              total_tokens: PROMPT_TOKENS + COMPLETION_TOKENS,
            },
          }),
        );
      });
      await new Promise<void>((done) => provider.listen(0, "127.0.0.1", done));
      const providerAddress = provider.address();
      const providerPort =
        typeof providerAddress === "object" && providerAddress
          ? providerAddress.port
          : 0;

      const ledgerUrl = new URL(database.url);
      ledgerUrl.searchParams.set("options", "-c role=gateway_ledger");
      const pricing = resolve(
        __dirname,
        "../../../../../../apps/gateway/examples/configs/withlocalpricingfiles",
      );
      const appDir = mkdtempSync(join(tmpdir(), "keys-e2e-"));
      writeFileSync(
        join(appDir, "config.json"),
        JSON.stringify({
          client: { enforce_auth_on_inference: true },
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
          framework: {
            pricing: {
              pricing_url: `file://${join(pricing, "pricing.json")}`,
              model_parameters_url: `file://${join(pricing, "model-parameters.json")}`,
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
        {
          stdio: "ignore",
        },
      );
      for (let i = 0; i < 300; i++) {
        const ok = await fetch(`${gatewayUrl}/health`).then(
          (r) => r.ok,
          () => false,
        );
        if (ok) break;
        await new Promise((r) => setTimeout(r, 300));
      }

      // the API's modules read these when first imported
      process.env.DATABASE_URL = database.url;
      process.env.GATEWAY_URL = gatewayUrl;
      process.env.GATEWAY_ADMIN_USERNAME = ADMIN_USER;
      process.env.GATEWAY_ADMIN_PASSWORD = ADMIN_PASSWORD;
      process.env.SKIP_ENV_VALIDATION = "1";
      keys = await import("../keys");
      ledger = await import("../../credits/ledger");
      ({ removeOrganizationFromGateway } =
        await import("@workspace/gateway-client/server"));
    }, 120_000);

    afterAll(async () => {
      gatewayProcess?.kill();
      await new Promise<void>((done) => provider?.close(() => done()));
      const { db } = await import("@workspace/db/server");
      await db.$client.end();
      await sql.end();
      await database.drop();
    });

    it("a key created by the API calls a model and charges its organization", async () => {
      await sql`insert into organization (id, name, slug, created_at) values ('org_e2e', 'E2E', 'e2e', now())`;
      await ledger.applyEntry({
        organizationId: "org_e2e",
        amount: SAR,
        kind: "adjustment",
        idempotencyKey: "e2e-credit",
      });

      const { key, apiKey } = await keys.createKey({
        organizationId: "org_e2e",
        userId: "user_e2e",
        name: "e2e",
      });

      const response = await chat(key);
      expect(response.status).toBe(200);
      expect(JSON.stringify(await response.json())).toContain("hello");
      const balance = await settled("org_e2e");
      expect(balance.balance).toBe(SAR - EXPECTED_CHARGE);

      // disabled: 401 (SPEC API-08), refused before the provider is called, nothing charged
      await keys.updateKey("org_e2e", apiKey.id, { disabled: true });
      const callsBefore = providerCalls;
      const disabled = await refusalOf(await chat(key), 401);
      expect(disabled.metadata).toMatchObject({
        limit_source: "key",
        reason: "key_disabled",
      });
      expect(disabled.text).not.toContain(key);
      expect(providerCalls).toBe(callsBefore);
      expect((await settled("org_e2e")).balance).toBe(SAR - EXPECTED_CHARGE);

      // enabled again: works again
      await keys.updateKey("org_e2e", apiKey.id, { disabled: false });
      expect((await chat(key)).status).toBe(200);
      expect((await settled("org_e2e")).balance).toBe(
        SAR - 2 * EXPECTED_CHARGE,
      );
    }, 60_000);

    it("a key whose spending limit does not cover the request is refused by the credits plugin", async () => {
      // a cap below one request's hold: refused before the provider is called (ADR 0013)
      const { key } = await keys.createKey({
        organizationId: "org_e2e",
        userId: "user_e2e",
        name: "capped",
        limit: { limitMicroSar: 100, reset: "daily" },
      });
      // 402, as OpenRouter's, with the numbers in SAR (SPEC API-08)
      const { body, metadata } = await refusalOf(await chat(key), 402);
      expect(body.error.type).toBe("credits_key_limit");
      expect(metadata).toMatchObject({
        limit_source: "key_limit",
        // one request's hold is bigger than the whole limit: retrying will not help
        reason: "request_exceeds_key_limit",
        limit: 0.0001,
        spent: 0,
        remaining: 0.0001,
        limit_reset: "daily",
        currency: "SAR",
      });
      expect(metadata.resets_at).toBe(
        new Date(
          new Date().setUTCHours(24, 0, 0, 0), // the next UTC midnight
        )
          .toISOString()
          .replace(".000Z", "Z"),
      );
      expect(Number(metadata.required)).toBeGreaterThan(0.0001);
    }, 60_000);

    it("a key that spent its limit gets 402 key_limit_reached with what it spent and what is left", async () => {
      // room for one request (its hold), not two: the second is refused on what the first spent
      const { key } = await keys.createKey({
        organizationId: "org_e2e",
        userId: "user_e2e",
        name: "one request",
        limit: { limitMicroSar: 3_000, reset: "monthly" },
      });
      // max_tokens 1000 holds about 2,270 micro-SAR: one fits in 3,000, a second after 1,687 spent does not
      const ok = await chat(key, 1000);
      expect(ok.status).toBe(200);
      await ok.text();
      await settled("org_e2e");
      const { metadata } = await refusalOf(await chat(key, 1000), 402);
      expect(metadata).toMatchObject({
        limit_source: "key_limit",
        reason: "key_limit_reached",
        limit: 0.003,
        spent: EXPECTED_CHARGE / SAR,
        remaining: (3_000 - EXPECTED_CHARGE) / SAR,
        limit_reset: "monthly",
        currency: "SAR",
      });
    }, 60_000);

    it("an organization with no credit gets 402 credits, insufficient_balance, and the provider is never called", async () => {
      await sql`insert into organization (id, name, slug, created_at) values ('org_zero', 'Zero', 'zero', now())`;
      const { key } = await keys.createKey({
        organizationId: "org_zero",
        userId: "user_e2e",
        name: "broke",
      });
      const callsBefore = providerCalls;
      const { body, metadata, text } = await refusalOf(await chat(key), 402);
      expect(body.error.type).toBe("insufficient_credits");
      expect(metadata).toMatchObject({
        limit_source: "credits",
        reason: "insufficient_balance",
        balance: 0,
        available: 0,
        currency: "SAR",
      });
      expect(text).not.toContain("retry_after_seconds");
      // another organization's data is never in the answer
      expect(text).not.toContain("org_e2e");
      expect(providerCalls).toBe(callsBefore);
    }, 60_000);

    it("a balance smaller than one request's hold gets 402 request_exceeds_balance, with no Retry-After", async () => {
      await sql`insert into organization (id, name, slug, created_at) values ('org_small', 'Small', 'small', now())`;
      await ledger.applyEntry({
        organizationId: "org_small",
        amount: 100,
        kind: "adjustment",
        idempotencyKey: "e2e-small",
      });
      const { key } = await keys.createKey({
        organizationId: "org_small",
        userId: "user_e2e",
        name: "small",
      });
      const response = await chat(key);
      expect(response.headers.get("retry-after")).toBeNull();
      const { metadata } = await refusalOf(response, 402);
      expect(metadata).toMatchObject({
        limit_source: "credits",
        reason: "request_exceeds_balance",
        balance: 0.0001,
      });
    }, 60_000);

    it("an expired key gets 401 key_expired; a missing or unknown key 401 too", async () => {
      const { key } = await keys.createKey({
        organizationId: "org_e2e",
        userId: "user_e2e",
        name: "short-lived",
        expiresAt: new Date(Date.now() + 1500),
      });
      await new Promise((r) => setTimeout(r, 2000));
      const expired = await refusalOf(await chat(key), 401);
      expect(expired.metadata).toMatchObject({
        limit_source: "key",
        reason: "key_expired",
      });

      const unknown = await refusalOf(
        await chat("sk-bf-00000000-0000-0000-0000-000000000000"),
        401,
      );
      expect(unknown.metadata).toMatchObject({
        limit_source: "key",
        reason: "key_invalid",
      });

      const missing = await fetch(`${gatewayUrl}/v1/chat/completions`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model: "openai/gpt-4o-mini",
          messages: [{ role: "user", content: "hi" }],
        }),
      });
      const absent = await refusalOf(missing, 401);
      expect(absent.metadata).toMatchObject({
        limit_source: "key",
        reason: "key_missing",
      });
    }, 60_000);

    it("the OpenAI-compatible and Anthropic routes carry the same refusal, each in its own shape", async () => {
      const { key } = await keys.createKey({
        organizationId: "org_zero",
        userId: "user_e2e",
        name: "routes",
      });
      const openai = await fetch(`${gatewayUrl}/openai/v1/chat/completions`, {
        method: "POST",
        headers: {
          Authorization: `Bearer ${key}`,
          "Content-Type": "application/json",
        },
        body: JSON.stringify({
          model: "openai/gpt-4o-mini",
          messages: [{ role: "user", content: "hi" }],
        }),
      });
      const viaOpenAI = await refusalOf(openai, 402);
      expect(viaOpenAI.metadata).toMatchObject({
        limit_source: "credits",
        reason: "insufficient_balance",
      });

      const anthropic = await fetch(`${gatewayUrl}/anthropic/v1/messages`, {
        method: "POST",
        headers: {
          "x-api-key": key,
          "anthropic-version": "2023-06-01",
          "Content-Type": "application/json",
        },
        body: JSON.stringify({
          model: "openai/gpt-4o-mini",
          max_tokens: 100,
          messages: [{ role: "user", content: "hi" }],
        }),
      });
      const text = await anthropic.text();
      expect({ status: anthropic.status, body: text }).toMatchObject({
        status: 402,
      });
      // Anthropic's envelope: { type: "error", error: { type, message } }, plus our metadata
      expect(JSON.parse(text)).toMatchObject({
        type: "error",
        error: {
          type: "billing_error",
          message: expect.any(String),
          metadata: {
            limit_source: "credits",
            reason: "insufficient_balance",
            remedy_hint: expect.any(String),
          },
        },
      });
    }, 60_000);

    it("an organization over its rate limit gets 429, keeps no hold, and pays only for what ran", async () => {
      const rateLimit = await import("../../admin/organizations/rate-limit");
      await sql`insert into organization (id, name, slug, created_at) values ('org_rl', 'RL', 'rl', now())`;
      // through the ledger, so the grant has its lot (ADR 0019) and reconciliation stays clean
      await ledger.applyEntry({
        organizationId: "org_rl",
        amount: SAR,
        kind: "top_up",
        idempotencyKey: "seed_rl",
      });
      const { key } = await keys.createKey({
        organizationId: "org_rl",
        userId: "user_e2e",
        name: "limited",
      });
      await rateLimit.setOrganizationRateLimit({
        organizationId: "org_rl",
        staffId: "staff_e2e",
        requestsPerMinute: 2,
        tokensPerMinute: null,
      });

      const statuses: number[] = [];
      let refused: Response | undefined;
      for (let i = 0; i < 3; i++) {
        const response = await chat(key);
        statuses.push(response.status);
        if (response.status !== 200) {
          refused = response;
        } else {
          await response.text();
        }
        await settled("org_rl");
      }
      expect(statuses).toEqual([200, 200, 429]);
      // Retry-After: the seconds until the minute window resets (SPEC API-08)
      const retryAfter = Number(refused?.headers.get("retry-after"));
      expect(retryAfter).toBeGreaterThanOrEqual(1);
      expect(retryAfter).toBeLessThanOrEqual(60);
      const { metadata } = await refusalOf(refused as Response, 429);
      expect(metadata).toMatchObject({
        limit_source: "rate_limit",
        reason: "request_limit_reached",
        limit_kind: "requests",
        limit: 2,
        retry_after_seconds: retryAfter,
      });

      const balance = await settled("org_rl");
      expect(balance.held).toBe(0);
      expect(balance.balance).toBe(SAR - 2 * EXPECTED_CHARGE);
      const [open] = await sql<
        { n: string }[]
      >`select count(*) as n from credit_reservation where organization_id = 'org_rl' and settled_at is null`;
      expect(Number(open?.n)).toBe(0);

      // removing the limit lets the organization through again
      await rateLimit.setOrganizationRateLimit({
        organizationId: "org_rl",
        staffId: "staff_e2e",
        requestsPerMinute: null,
        tokensPerMinute: null,
      });
      const after = await chat(key);
      expect(after.status).toBe(200);
      await after.text();
      await settled("org_rl");
    }, 60_000);

    it("two keys with the same name both work: the gateway gets our ids as names", async () => {
      const one = await keys.createKey({
        organizationId: "org_e2e",
        userId: "user_e2e",
        name: "Same name",
      });
      const two = await keys.createKey({
        organizationId: "org_e2e",
        userId: "user_e2e",
        name: "Same name",
      });
      for (const { key } of [one, two]) {
        const response = await chat(key);
        expect(response.status).toBe(200);
        await response.text();
      }
      await settled("org_e2e");
    }, 60_000);

    it("a request's record joins the charge with the gateway's timing, by the id the gateway returned", async () => {
      const generations = await import("../../activity/generations");
      const { key } = await keys.createKey({
        organizationId: "org_e2e",
        userId: "user_e2e",
        name: "records",
      });
      const response = await chat(key);
      expect(response.status).toBe(200);
      await response.text();
      const id = response.headers.get("x-bifrost-request-id");
      expect(id).toBeTruthy();
      await settled("org_e2e");

      // the gateway writes its log after answering
      let record: Awaited<ReturnType<typeof generations.getGeneration>>;
      for (let i = 0; i < 50; i++) {
        record = await generations.getGeneration("org_e2e", id ?? "");
        if (record?.latency !== null && record?.latency !== undefined) break;
        await new Promise((r) => setTimeout(r, 200));
      }
      expect(record).toMatchObject({
        id,
        model: expect.stringContaining("gpt-4o-mini"),
        provider_name: "openai",
        tokens_prompt: PROMPT_TOKENS,
        tokens_completion: COMPLETION_TOKENS,
        total_cost: EXPECTED_CHARGE / SAR,
        finish_reason: "stop",
        streamed: false,
      });
      expect(record?.latency).toBeGreaterThan(0);
    }, 60_000);

    it("lists the gateway's models at our price, seeded from its datasheet, on /api/v1/models", async () => {
      // the one-time seed (ADMIN-09, ADR 0018): our table takes the price the gateway charges
      const { importFromGateway } = await import("../../model-prices/import");
      const { getGateway } = await import("@workspace/gateway-client/server");
      await importFromGateway({ gateway: getGateway(), staffId: "staff_e2e" });
      const models = await import("../../v1/models");
      models.clearModelCache();
      const { data } = await models.listPublicModels();
      const mini = data.find((model) => model.id === "openai/gpt-4o-mini");
      // the local pricing file, as it is: no markup
      expect(mini?.pricing).toMatchObject({
        prompt: "0.00000015",
        completion: "0.0000006",
      });
    }, 60_000);

    it("the gateway's cost and the ledger's agree for everything charged so far", async () => {
      const { reconcile } = await import("../../reconciliation/reconcile");
      const now = Date.now();
      const result = await reconcile({
        from: new Date(now - 60 * 60_000),
        to: new Date(now + 60_000),
      });
      expect(result.findings.error).toBeUndefined();
      expect(result.findings.organizations).toEqual([]);
      expect(result.findings.unattributed).toEqual([]);
      expect(result.findings.checkedOrganizations).toBeGreaterThan(0);
      expect(result.status).toBe("ok");
    }, 60_000);

    it("a key at its limit gets 402 key_limit until its window starts again: daily, weekly and monthly", async () => {
      const usage = await import("../usage");
      await sql`insert into organization (id, name, slug, created_at) values ('org_windows', 'Windows', 'windows', now())`;
      await ledger.applyEntry({
        organizationId: "org_windows",
        amount: SAR,
        kind: "top_up",
        idempotencyKey: "seed_windows",
      });
      const now = new Date();
      const midnight = Date.UTC(
        now.getUTCFullYear(),
        now.getUTCMonth(),
        now.getUTCDate(),
      );
      // when each window starts again, worked out here, apart from the database's functions
      const nextReset = {
        daily: new Date(midnight + 86_400_000),
        weekly: new Date(
          midnight + (7 - ((now.getUTCDay() + 6) % 7)) * 86_400_000,
        ),
        monthly: new Date(
          Date.UTC(now.getUTCFullYear(), now.getUTCMonth() + 1, 1),
        ),
      } as const;

      for (const reset of ["daily", "weekly", "monthly"] as const) {
        const { key, apiKey } = await keys.createKey({
          organizationId: "org_windows",
          userId: "user_e2e",
          name: `limit ${reset}`,
          limit: { limitMicroSar: 3_000, reset },
        });
        const { virtual_key_id: vk } = await stored(apiKey.id);
        const first = await chat(key, 1000);
        expect(first.status).toBe(200);
        await first.text();
        await settled("org_windows");

        // at its limit for this window
        const { metadata } = await refusalOf(await chat(key, 1000), 402);
        expect(metadata).toMatchObject({
          limit_source: "key_limit",
          reason: "key_limit_reached",
          limit_reset: reset,
          spent: EXPECTED_CHARGE / SAR,
          resets_at: nextReset[reset].toISOString().replace(".000Z", "Z"),
        });
        // the key's page counts the same window and the same reset
        const before = await usage.getKeyUsage(vk, reset);
        expect(before.spentAgainstLimit).toBe(EXPECTED_CHARGE);
        expect(before.resetsAt?.toISOString()).toBe(
          nextReset[reset].toISOString(),
        );

        // the window turns: the charge now lies one second before the current window started
        await sql`alter table credit_ledger_entry disable trigger credit_ledger_entry_append_only_trigger`;
        await sql`update credit_ledger_entry set created_at = key_limit_window_start(${reset}, now() at time zone 'UTC') - interval '1 second'
                  where virtual_key_id = ${vk} and kind = 'usage'`;
        await sql`alter table credit_ledger_entry enable trigger credit_ledger_entry_append_only_trigger`;

        const after = await chat(key, 1000);
        expect({ reset, status: after.status }).toEqual({ reset, status: 200 });
        await after.text();
        await settled("org_windows");
        const counted = await usage.getKeyUsage(vk, reset);
        expect(counted.spentAgainstLimit).toBe(EXPECTED_CHARGE);
        expect(counted.total).toBe(2 * EXPECTED_CHARGE);
      }
    }, 60_000);

    it("the expiry the gateway enforces is the one recorded, and a shorter maximum lifetime moves both", async () => {
      const settings = await import("../settings");
      const lifetime = await import("../lifetime");
      await sql`insert into organization (id, name, slug, created_at) values ('org_life', 'Life', 'life', now())`;
      await ledger.applyEntry({
        organizationId: "org_life",
        amount: SAR,
        kind: "top_up",
        idempotencyKey: "seed_life",
      });

      // the create form's choices, as the API turns them into a moment
      const { expiresAtFor } = await import("../expiry");
      const year = await keys.createKey({
        organizationId: "org_life",
        userId: "user_e2e",
        name: "a year",
        expiresAt: expiresAtFor("1y"),
      });
      const hour = await keys.createKey({
        organizationId: "org_life",
        userId: "user_e2e",
        name: "an hour",
        expiresAt: expiresAtFor("1h"),
      });
      const never = await keys.createKey({
        organizationId: "org_life",
        userId: "user_e2e",
        name: "never",
        expiresAt: null,
      });
      for (const { apiKey } of [year, hour]) {
        const both = await expiries(apiKey.id);
        expect(both.ours).not.toBeNull();
        expect(both.gateway).toBe(both.ours);
      }
      expect(await expiries(never.apiKey.id)).toEqual({
        ours: null,
        gateway: null,
      });

      // made two days ago, as far as the maximum is concerned: a one-day maximum has passed for it
      await sql`update api_key set created_at = created_at - interval '2 days' where id = ${never.apiKey.id}`;
      const change = await settings.updateKeySettings({
        staffId: "staff_e2e",
        defaultLimitMicroSar: null,
        defaultLimitReset: null,
        defaultExpiry: "1d",
        maxLifetime: "1d",
        limitPresetsMicroSar: [50 * SAR],
      });
      const result = await lifetime.applyMaxLifetime({
        maxLifetimeSeconds: change.settings.maxLifetimeSeconds,
      });
      expect(result.failed).toBe(0);

      // cut to created_at + 1 day, the same moment in the gateway
      const yearRow = await stored(year.apiKey.id);
      const cut = await expiries(year.apiKey.id);
      expect(cut.ours).toBe(yearRow.created_at.getTime() + 86_400_000);
      expect(cut.gateway).toBe(cut.ours);
      // an hour is within a day: untouched
      expect((await expiries(hour.apiKey.id)).ours).toBe(
        hour.apiKey.expiresAt?.getTime(),
      );
      // past its cut-off: expires within seconds, in both places, and the gateway refuses it
      const past = await expiries(never.apiKey.id);
      expect(past.gateway).toBe(past.ours);
      expect(past.ours ?? 0).toBeLessThanOrEqual(Date.now() + 5_000);
      const stillOn = await chat(never.key);
      expect(stillOn.status).toBe(200);
      await stillOn.text();
      await settled("org_life");
      await new Promise((r) =>
        setTimeout(r, (past.ours ?? 0) - Date.now() + 500),
      );
      const expired = await refusalOf(await chat(never.key), 401);
      expect(expired.metadata).toMatchObject({
        limit_source: "key",
        reason: "key_expired",
      });
      // the year key, cut to a day, still works
      const working = await chat(year.key);
      expect(working.status).toBe(200);
      await working.text();
      await settled("org_life");

      // no expiry change through the API at all: the gateway's stays as it was
      const { updateApiKeySchema } = await import("../../../schema");
      expect(
        updateApiKeySchema.safeParse({ expiresAt: new Date().toISOString() })
          .success,
      ).toBe(false);

      // back to no maximum, for the tests after this one
      await settings.updateKeySettings({
        staffId: "staff_e2e",
        defaultLimitMicroSar: null,
        defaultLimitReset: null,
        defaultExpiry: "90d",
        maxLifetime: null,
        limitPresetsMicroSar: [50 * SAR],
      });
    }, 60_000);

    it("a deleted key and the keys of a deleted organization stop working", async () => {
      const { key, apiKey } = await keys.createKey({
        organizationId: "org_e2e",
        userId: "user_e2e",
        name: "to delete",
      });
      expect((await chat(key)).status).toBe(200);
      await settled("org_e2e");
      const { virtual_key_id: deletedVk } = await stored(apiKey.id);
      await keys.deleteKey("org_e2e", apiKey.id);
      expect((await chat(key)).status).toBe(401);
      // gone from the gateway, not only refused
      expect(await gatewayKey(deletedVk)).toBeUndefined();

      const { key: survivor } = await keys.createKey({
        organizationId: "org_e2e",
        userId: "user_e2e",
        name: "survivor",
      });
      expect((await chat(survivor)).status).toBe(200);
      await settled("org_e2e");
      await removeOrganizationFromGateway("org_e2e");
      await sql`delete from organization where id = 'org_e2e'`;
      expect((await chat(survivor)).status).toBe(401);
    }, 60_000);
  },
);
