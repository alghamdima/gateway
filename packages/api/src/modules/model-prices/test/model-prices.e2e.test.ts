import { spawn } from "node:child_process";
import { mkdtempSync, writeFileSync } from "node:fs";
import { createServer } from "node:http";
import { createServer as createNetServer } from "node:net";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { afterAll, beforeAll, describe, expect, it } from "vitest";

import type { TestDatabase } from "@workspace/db/testing";
import type { GatewayClient } from "@workspace/gateway-client/server";
import type { ChildProcess } from "node:child_process";
import type { Server } from "node:http";
import type postgres from "postgres";

/**
 * ADMIN-09 end to end (docs/platform/adr/0018-model-price-table.md): our price
 * table pushed to the built gateway binary through its pricing overrides,
 * checked by the reconciliation, and charged by the credits plugin, which
 * records the version it charged. One provider with two keys; one key has a
 * negotiated price. Needs DATABASE_URL and GATEWAY_E2E_BINARY (the gateway
 * built from apps/gateway/transports/bifrost-http); skips without them.
 */
const adminUrl = process.env.DATABASE_URL;
const binary = process.env.GATEWAY_E2E_BINARY;

const ADMIN_USER = "control-plane";
const ADMIN_PASSWORD = "e2e-gateway-admin-password";
const SAR = 1_000_000;
const PROMPT_TOKENS = 10;
const COMPLETION_TOKENS = 5;

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
  "model prices through the real gateway",
  () => {
    let database: TestDatabase;
    let sql: postgres.Sql;
    let provider: Server;
    let gatewayProcess: ChildProcess;
    let gatewayUrl = "";
    let gateway: GatewayClient;
    let keys: typeof import("../../keys/keys");
    let ledger: typeof import("../../credits/ledger");
    let prices: typeof import("../prices");
    let push: typeof import("../push");
    let importer: typeof import("../import");
    let negotiatedKeyId = "";
    let apiKey = "";

    const chat = () =>
      fetch(`${gatewayUrl}/v1/chat/completions`, {
        method: "POST",
        headers: {
          Authorization: `Bearer ${apiKey}`,
          "Content-Type": "application/json",
        },
        body: JSON.stringify({
          model: "openai/gpt-4o-mini",
          max_tokens: 100,
          messages: [{ role: "user", content: "hi" }],
        }),
      });

    const settled = async () => {
      for (let i = 0; i < 100; i++) {
        if ((await ledger.getBalance("org_prices")).held === 0) return;
        await new Promise((r) => setTimeout(r, 100));
      }
      throw new Error("a hold was never settled");
    };

    /** The usage entries charged after `after`, with what each records. */
    const entriesAfter = async (after: string) =>
      sql<
        {
          id: string;
          amount: string;
          cost_micro_usd: string;
          price_override: string | null;
          provider_key_id: string | null;
        }[]
      >`select id, amount, cost_micro_usd, metadata->>'price_override' as price_override,
          metadata->>'provider_key_id' as provider_key_id
        from credit_ledger_entry
        where organization_id = 'org_prices' and kind = 'usage' and created_at > ${after}::timestamp
        order by created_at`;

    const manual = (
      providerKeyId: string | null,
      input: string,
      output: string,
    ) =>
      prices.createPriceVersion({
        provider: "openai",
        model: "gpt-4o-mini",
        providerKeyId,
        isRemoved: false,
        rates: {
          inputUsd: input,
          outputUsd: output,
          cacheReadUsd: null,
          cacheWriteUsd: null,
          cacheWrite1hUsd: null,
          otherRates: {},
        },
        note: "e2e",
        staffId: "staff_e2e",
      });

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
      const appDir = mkdtempSync(join(tmpdir(), "model-prices-e2e-"));
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
                  name: "standard",
                  value: "sk-mock-standard",
                  models: ["*"],
                  weight: 1,
                },
                {
                  name: "negotiated",
                  value: "sk-mock-negotiated",
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
      prices = await import("../prices");
      push = await import("../push");
      importer = await import("../import");
      gateway = (await import("@workspace/gateway-client/server")).getGateway();

      negotiatedKeyId =
        (await gateway.listProviderKeys()).find(
          (key) => key.name === "negotiated",
        )?.id ?? "";
      await sql`insert into organization (id, name, slug, created_at) values ('org_prices', 'org_prices', 'org_prices', now())`;
      await ledger.applyEntry({
        organizationId: "org_prices",
        amount: 10 * SAR,
        kind: "adjustment",
        idempotencyKey: "credit-org_prices",
      });
      ({ key: apiKey } = await keys.createKey({
        organizationId: "org_prices",
        userId: "user_e2e",
        name: "prices",
      }));
    }, 120_000);

    afterAll(async () => {
      gatewayProcess?.kill();
      await new Promise<void>((done) => provider?.close(() => done()));
      const { db } = await import("@workspace/db/server");
      await db.$client.end();
      await sql.end();
      await database.drop();
    });

    it("seeds our table from what the gateway charges, pushes it, and the gateway then charges exactly that", async () => {
      expect(negotiatedKeyId).not.toBe("");
      const summary = await importer.importFromGateway({
        gateway,
        staffId: "staff_e2e",
      });
      expect(summary.imported).toBeGreaterThan(0);
      const seeded = await prices.getVersionAt(
        { provider: "openai", model: "gpt-4o-mini", providerKeyId: null },
        new Date(),
      );
      // the local pricing file: 0.15 and 0.60 per million
      expect(seeded).toMatchObject({
        inputUsd: "0.00000015",
        outputUsd: "0.0000006",
      });

      expect(await push.pushDuePrices(gateway)).toMatchObject({ failed: 0 });
      expect((await push.checkModelPrices(gateway)).drift).toEqual([]);
    }, 60_000);

    it("charges a new version at once and records it on the usage entry", async () => {
      const since = (await sql`select now()::timestamp::text as t`)[0]?.t;
      const { version } = await manual(null, "0.000001", "0.000002");
      await push.pushNow(gateway, version);
      expect((await push.checkModelPrices(gateway)).drift).toEqual([]);
      const mini = (await gateway.listModels()).find(
        (model) => model.provider === "openai" && model.name === "gpt-4o-mini",
      );
      // the gateway's own view of what it charges is our price
      expect(mini?.overridden_pricing).toMatchObject({
        input_cost_per_token: 0.000001,
        output_cost_per_token: 0.000002,
      });

      expect((await chat()).status).toBe(200);
      await settled();
      const [entry] = await entriesAfter(String(since));
      // 10 × 0.000001 + 5 × 0.000002 = 0.00002 USD = 20 micro-USD, × 3.75 = 75 micro-SAR
      expect(entry).toMatchObject({
        cost_micro_usd: "20",
        amount: "-75",
        price_override: `lemru-price:${version.id}`,
      });
    }, 60_000);

    it("charges a key's negotiated price for what that key serves, the provider's for the rest", async () => {
      const since = (await sql`select now()::timestamp::text as t`)[0]?.t;
      const shared = await prices.getVersionAt(
        { provider: "openai", model: "gpt-4o-mini", providerKeyId: null },
        new Date(),
      );
      const { version } = await manual(
        negotiatedKeyId,
        "0.0000005",
        "0.000001",
      );
      await push.pushNow(gateway, version);
      expect((await push.checkModelPrices(gateway)).drift).toEqual([]);

      for (let i = 0; i < 16; i++) {
        expect((await chat()).status).toBe(200);
      }
      await settled();
      const entries = await entriesAfter(String(since));
      expect(entries).toHaveLength(16);
      const viaNegotiated = entries.filter(
        (entry) => entry.provider_key_id === negotiatedKeyId,
      );
      const viaStandard = entries.filter(
        (entry) => entry.provider_key_id !== negotiatedKeyId,
      );
      // both keys carried traffic, so the comparison is not an accident of selection
      expect(viaNegotiated.length).toBeGreaterThan(0);
      expect(viaStandard.length).toBeGreaterThan(0);
      for (const entry of viaNegotiated) {
        // 10 × 0.0000005 + 5 × 0.000001 = 10 micro-USD
        expect(entry).toMatchObject({
          cost_micro_usd: "10",
          price_override: `lemru-price:${version.id}`,
        });
      }
      for (const entry of viaStandard) {
        expect(entry).toMatchObject({
          cost_micro_usd: "20",
          price_override: `lemru-price:${shared?.id}`,
        });
      }
    }, 120_000);

    it("finds a price changed in the gateway behind our back, and puts ours back", async () => {
      const state = (await push.getPushStates()).find(
        (row) => row.model === "gpt-4o-mini" && row.providerKeyId === null,
      );
      const current = await prices.getVersionAt(
        { provider: "openai", model: "gpt-4o-mini", providerKeyId: null },
        new Date(),
      );
      if (!state?.gatewayOverrideId || !current)
        throw new Error("nothing pushed");
      await gateway.updatePricingOverride(state.gatewayOverrideId, {
        ...push.overrideFor(current),
        patch: {
          input_cost_per_token: 0.000009,
          output_cost_per_token: 0.000002,
        },
      });
      const check = await push.checkModelPrices(gateway);
      expect(check.drift.map((item) => item.kind).sort()).toEqual([
        "effective_price",
        "wrong_rates",
      ]);

      await push.syncModelPrices(gateway);
      expect((await push.checkModelPrices(gateway)).drift).toEqual([]);
    }, 60_000);
  },
);
