import { spawn } from "node:child_process";
import { mkdtempSync, writeFileSync } from "node:fs";
import { createServer } from "node:http";
import { createServer as createNetServer } from "node:net";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import * as z from "zod";

import type { TestDatabase } from "@workspace/db/testing";
import type { ChildProcess } from "node:child_process";
import type { Server } from "node:http";
import type postgres from "postgres";

/**
 * Step D2 end to end: the API's policy code against the built gateway binary,
 * with one provider that has two keys of different data terms. Which key the
 * gateway used is read from what the mock provider received. Needs
 * DATABASE_URL and GATEWAY_E2E_BINARY (the gateway built from
 * apps/gateway/transports/bifrost-http); skips without them.
 */
const adminUrl = process.env.DATABASE_URL;
const binary = process.env.GATEWAY_E2E_BINARY;

const ADMIN_USER = "control-plane";
const ADMIN_PASSWORD = "e2e-gateway-admin-password";
const USD = 1_000_000;
const LOCAL = "sk-mock-in-kingdom";
const ABROAD = "sk-mock-abroad";

const modelListSchema = z.object({
  data: z.array(z.object({ id: z.string() })).optional(),
});

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
  "data policy through the real gateway",
  () => {
    let database: TestDatabase;
    let sql: postgres.Sql;
    let provider: Server;
    let gatewayProcess: ChildProcess;
    let gatewayUrl = "";
    /** Chat requests the provider received, by the upstream key that carried them. */
    let served: Record<string, number> = {};
    let keys: typeof import("../../keys/keys");
    let ledger: typeof import("../../credits/ledger");
    let policy: typeof import("../policy");
    let providers: typeof import("../../admin/providers/providers");
    let generations: typeof import("../../activity/generations");
    let models: typeof import("../../v1/models");
    let v1Router: typeof import("../../v1/router").v1Router;
    let localKeyId = "";

    /** GET /api/v1/models as a caller with this key, or with none. */
    const modelsRoute = async (key?: string) => {
      const response = await v1Router.request("/models", {
        headers: key ? { authorization: `Bearer ${key}` } : {},
      });
      const body = modelListSchema.parse(await response.json());
      return { status: response.status, ids: body.data?.map((m) => m.id) };
    };

    /** The model ids the organization is offered under its policy. */
    const offered = async (organizationId: string) =>
      (
        await models.listPublicModels(
          undefined,
          undefined,
          await policy.accessForOrganization(organizationId),
        )
      ).data.map((model) => model.id);

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

    /** Sends `count` requests and returns their statuses and which upstream keys served them. */
    const send = async (key: string, count: number) => {
      served = {};
      const statuses: number[] = [];
      for (let i = 0; i < count; i++) {
        statuses.push((await chat(key)).status);
      }
      return { statuses: [...new Set(statuses)], served: { ...served } };
    };

    const createOrganization = async (id: string) => {
      await sql`insert into organization (id, name, slug, created_at) values (${id}, ${id}, ${id}, now())`;
      await ledger.applyEntry({
        organizationId: id,
        amount: USD,
        kind: "adjustment",
        idempotencyKey: `credit-${id}`,
      });
    };

    const localTerms = (retentionDays: number) =>
      providers.setProviderTerms({
        keyId: localKeyId,
        staffId: "staff_e2e",
        region: "sa",
        retentionDays,
        trainsOnData: false,
        agreement: "direct",
        source: "contract",
        verifiedOn: "2026-11-24",
      });

    const IN_KINGDOM = {
      requireZeroRetention: true,
      forbidTraining: true,
      allowedRegions: ["sa" as const],
      allowedProviders: null,
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
        const upstreamKey = (req.headers.authorization ?? "").replace(
          /^Bearer /,
          "",
        );
        served[upstreamKey] = (served[upstreamKey] ?? 0) + 1;
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
              prompt_tokens: 10,
              completion_tokens: 5,
              total_tokens: 15,
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
      const appDir = mkdtempSync(join(tmpdir(), "policy-e2e-"));
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
                { name: "abroad", value: ABROAD, models: ["*"], weight: 1 },
                { name: "in-kingdom", value: LOCAL, models: ["*"], weight: 1 },
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

      // the API's modules read these when first imported
      process.env.DATABASE_URL = database.url;
      process.env.GATEWAY_URL = gatewayUrl;
      process.env.GATEWAY_ADMIN_USERNAME = ADMIN_USER;
      process.env.GATEWAY_ADMIN_PASSWORD = ADMIN_PASSWORD;
      process.env.SKIP_ENV_VALIDATION = "1";
      keys = await import("../../keys/keys");
      ledger = await import("../../credits/ledger");
      policy = await import("../policy");
      providers = await import("../../admin/providers/providers");
      generations = await import("../../activity/generations");
      models = await import("../../v1/models");
      ({ v1Router } = await import("../../v1/router"));

      // staff record the terms of the in-Kingdom key; the other stays unknown
      const catalog = await providers.getProviderCatalog();
      localKeyId = catalog.find((e) => e.name === "in-kingdom")?.keyId ?? "";
      await localTerms(0);
      // the catalog offers models we price (ADMIN-09): the local pricing file's price
      await sql`insert into model_price (provider, model, effective_from, input_usd, output_usd, source, created_by)
        values ('openai', 'gpt-4o-mini', now(), '0.00000015', '0.0000006', 'manual', 'staff_e2e')`;
    }, 120_000);

    afterAll(async () => {
      gatewayProcess?.kill();
      await new Promise<void>((done) => provider?.close(() => done()));
      const { db } = await import("@workspace/db/server");
      await db.$client.end();
      await sql.end();
      await database.drop();
    });

    it("an organization with no policy is served by any key", async () => {
      await createOrganization("org_open");
      const { key } = await keys.createKey({
        organizationId: "org_open",
        userId: "user_e2e",
        name: "open",
      });
      const result = await send(key, 24);
      expect(result.statuses).toEqual([200]);
      // both upstream keys carry traffic: the proof below is not an accident of selection
      expect(result.served[LOCAL]).toBeGreaterThan(0);
      expect(result.served[ABROAD]).toBeGreaterThan(0);
    }, 60_000);

    it("an in-Kingdom policy is served only by the in-Kingdom key, for existing and new keys", async () => {
      await createOrganization("org_strict");
      const before = await keys.createKey({
        organizationId: "org_strict",
        userId: "user_e2e",
        name: "created before the policy",
      });
      const saved = await policy.setDataPolicy({
        organizationId: "org_strict",
        userId: "user_e2e",
        ...IN_KINGDOM,
      });
      expect(saved).toMatchObject({ version: 1, isApplied: true });
      const after = await keys.createKey({
        organizationId: "org_strict",
        userId: "user_e2e",
        name: "created after the policy",
      });

      for (const { key } of [before, after]) {
        const result = await send(key, 24);
        expect(result.statuses).toEqual([200]);
        expect(result.served).toEqual({ [LOCAL]: 24 });
      }

      // the model list it is offered is what its endpoint serves
      expect(await offered("org_strict")).toContain("openai/gpt-4o-mini");

      // the record of one of those requests says where it went and under which policy
      const response = await chat(after.key);
      const requestId = response.headers.get("x-bifrost-request-id") ?? "";
      for (let i = 0; i < 100; i++) {
        if ((await ledger.getBalance("org_strict")).held === 0) break;
        await new Promise((r) => setTimeout(r, 100));
      }
      const record = await generations.getGeneration("org_strict", requestId);
      expect(record?.data_region).toBe("sa");
      expect(record?.data_policy).toMatchObject({
        endpoint: {
          provider: "openai",
          region: "sa",
          zero_retention: true,
          never_trained_on: true,
          terms_changed_since: false,
        },
        policy: { version: 1, allowed_regions: ["sa"] },
      });
    }, 60_000);

    it("when the endpoint no longer satisfies the policy, requests are refused and never rerouted", async () => {
      const { key } = await keys.createKey({
        organizationId: "org_strict",
        userId: "user_e2e",
        name: "third",
      });
      // staff learn that the in-Kingdom endpoint keeps data for 30 days
      await localTerms(30);
      expect(await policy.applyPendingPolicies()).toEqual({
        applied: 1,
        failed: [],
      });

      // no endpoint is left: no model is offered, though the gateway still has them
      expect(await offered("org_strict")).toEqual([]);
      expect(await offered("org_open")).toContain("openai/gpt-4o-mini");
      // the same through GET /api/v1/models: by this key nothing, with no key the public catalog
      expect(await modelsRoute(key)).toEqual({ status: 200, ids: [] });
      expect((await modelsRoute()).ids).toContain("openai/gpt-4o-mini");

      const refused = await send(key, 6);
      expect(refused.statuses).toEqual([403]);
      expect(refused.served).toEqual({});
      // the error contract (SPEC API-08): 403, limit_source data_policy, naming the rule and
      // nothing of another organization's
      const text = await (await chat(key)).text();
      const body = JSON.parse(text) as {
        error: {
          code: number;
          message: string;
          metadata: Record<string, unknown>;
        };
      };
      expect(body.error).toMatchObject({
        code: 403,
        type: "provider_blocked",
        message: expect.any(String),
        metadata: {
          limit_source: "data_policy",
          reason: "provider_not_allowed",
          remedy_hint: expect.any(String),
        },
      });
      expect(text).not.toContain("org_open");
      // nothing was charged for the refused requests, and no hold is left
      const balance = await ledger.getBalance("org_strict");
      expect(balance.held).toBe(0);

      // the terms are confirmed again: served again, still only in the Kingdom
      await localTerms(0);
      await policy.applyPendingPolicies();
      const again = await send(key, 12);
      expect(again.statuses).toEqual([200]);
      expect(again.served).toEqual({ [LOCAL]: 12 });
    }, 60_000);

    it("opening the policy again reaches every key", async () => {
      const [row] = await sql<
        { virtual_key_id: string }[]
      >`select virtual_key_id from api_key where organization_id = 'org_strict' limit 1`;
      expect(row).toBeDefined();
      await policy.setDataPolicy({
        organizationId: "org_strict",
        userId: "user_e2e",
        requireZeroRetention: false,
        forbidTraining: false,
        allowedRegions: null,
        allowedProviders: null,
      });
      const { key } = await keys.createKey({
        organizationId: "org_strict",
        userId: "user_e2e",
        name: "open again",
      });
      const result = await send(key, 24);
      expect(result.statuses).toEqual([200]);
      expect(result.served[ABROAD]).toBeGreaterThan(0);
    }, 60_000);
  },
);
