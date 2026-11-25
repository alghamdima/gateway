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
// gpt-4o-mini in the local pricing file: 0.15 USD per million input, 0.60 per million output
const EXPECTED_CHARGE = Math.round(
  PROMPT_TOKENS * 0.15 + COMPLETION_TOKENS * 0.6,
); // micro-USD at 0% markup: 450
const USD = 1_000_000;

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

    const chat = (key: string) =>
      fetch(`${gatewayUrl}/v1/chat/completions`, {
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
        ["-app-dir", appDir, "-port", String(port), "-host", "127.0.0.1"],
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
        amount: USD,
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
      expect(balance.balance).toBe(USD - EXPECTED_CHARGE);

      // disabled: refused before the provider is called, nothing charged
      await keys.updateKey("org_e2e", apiKey.id, { disabled: true });
      const callsBefore = providerCalls;
      expect((await chat(key)).status).toBe(403);
      expect(providerCalls).toBe(callsBefore);
      expect((await settled("org_e2e")).balance).toBe(USD - EXPECTED_CHARGE);

      // enabled again: works again
      await keys.updateKey("org_e2e", apiKey.id, { disabled: false });
      expect((await chat(key)).status).toBe(200);
      expect((await settled("org_e2e")).balance).toBe(
        USD - 2 * EXPECTED_CHARGE,
      );
    }, 60_000);

    it("a key whose spending limit does not cover the request is refused by the credits plugin", async () => {
      // a cap below one request's hold: refused before the provider is called (ADR 0013)
      const { key } = await keys.createKey({
        organizationId: "org_e2e",
        userId: "user_e2e",
        name: "capped",
        limit: { limitMicroUsd: 100, reset: "daily" },
      });
      const refused = await chat(key);
      expect(refused.status).toBe(403);
      expect(JSON.stringify(await refused.json())).toContain(
        "credits_key_limit",
      );
    }, 60_000);

    it("an organization over its rate limit gets 429, keeps no hold, and pays only for what ran", async () => {
      const rateLimit = await import("../../admin/organizations/rate-limit");
      await sql`insert into organization (id, name, slug, created_at) values ('org_rl', 'RL', 'rl', now())`;
      await sql`insert into credit_account (organization_id, balance) values ('org_rl', ${USD})`;
      await sql`insert into credit_ledger_entry (id, organization_id, amount, kind, idempotency_key) values ('seed_rl', 'org_rl', ${USD}, 'top_up', 'seed_rl')`;
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
      let refusal = "";
      for (let i = 0; i < 3; i++) {
        const response = await chat(key);
        statuses.push(response.status);
        if (response.status !== 200) {
          refusal = JSON.stringify(await response.json());
        } else {
          await response.text();
        }
        await settled("org_rl");
      }
      expect(statuses).toEqual([200, 200, 429]);
      expect(refusal).toContain("rate");

      const balance = await settled("org_rl");
      expect(balance.held).toBe(0);
      expect(balance.balance).toBe(USD - 2 * EXPECTED_CHARGE);
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
        total_cost: EXPECTED_CHARGE / USD,
        finish_reason: "stop",
        streamed: false,
      });
      expect(record?.latency).toBeGreaterThan(0);
    }, 60_000);

    it("lists the gateway's models with their catalog price on /api/v1/models", async () => {
      const models = await import("../../v1/models");
      models.clearModelCache();
      const { data } = await models.listPublicModels();
      const mini = data.find((model) => model.id === "openai/gpt-4o-mini");
      // the local pricing file, at the migrations' default 0% markup
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

    it("a deleted key and the keys of a deleted organization stop working", async () => {
      const { key, apiKey } = await keys.createKey({
        organizationId: "org_e2e",
        userId: "user_e2e",
        name: "to delete",
      });
      expect((await chat(key)).status).toBe(200);
      await settled("org_e2e");
      await keys.deleteKey("org_e2e", apiKey.id);
      expect((await chat(key)).status).toBe(401);

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
