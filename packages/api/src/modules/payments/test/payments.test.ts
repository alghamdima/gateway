import {
  afterAll,
  beforeAll,
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from "vitest";
import * as z from "zod";

import type { User } from "@workspace/auth";
import type { TestDatabase } from "@workspace/db/testing";
import type postgres from "postgres";

// Needs real Postgres: the single row, the known providers, the append-only
// history and the row locks are the database's. Skips without DATABASE_URL.
const adminUrl = process.env.DATABASE_URL;

// The router's middleware imports the auth server and the translations (JSX the
// test runner does not compile).
vi.mock("@workspace/auth/server", () => ({ auth: {} }));
vi.mock("@workspace/i18n", async () => {
  const z = await import("zod");
  return { getValidationLocale: () => z.locales.en };
});
vi.mock("@workspace/i18n/server", () => ({ getLocaleFromRequest: () => "en" }));

/**
 * Stripe's secrets as env would hold them, changed per test. Values are
 * recognisable so a test can prove none of them leaves the server.
 */
const stripeEnv = vi.hoisted((): Record<string, string | undefined> => ({
  STRIPE_SECRET_KEY: "sk_live_SECRETVALUE_live_key",
  STRIPE_WEBHOOK_SECRET: "whsec_SECRETVALUE_live_hook",
  STRIPE_TEST_SECRET_KEY: undefined,
  STRIPE_TEST_WEBHOOK_SECRET: undefined,
}));
const configured = {
  STRIPE_SECRET_KEY: "sk_live_SECRETVALUE_live_key",
  STRIPE_WEBHOOK_SECRET: "whsec_SECRETVALUE_live_hook",
  STRIPE_TEST_SECRET_KEY: "sk_test_SECRETVALUE_test_key",
  STRIPE_TEST_WEBHOOK_SECRET: "whsec_SECRETVALUE_test_hook",
};

vi.mock(
  "../../../../../billing/web/src/providers/stripe/env",
  async (importOriginal) => ({
    ...(await importOriginal<object>()),
    env: stripeEnv,
  }),
);

const setEnv = (values: Partial<typeof configured>) => {
  for (const name of Object.keys(configured)) {
    stripeEnv[name] = undefined;
  }
  Object.assign(stripeEnv, values);
};

const STAFF = "staff_payments";

describe.skipIf(!adminUrl)("admin payments (ADMIN-03)", () => {
  let database: TestDatabase;
  let sql: postgres.Sql;
  let payments: typeof import("../settings");
  let gateway: typeof import("../gateway");

  const changes = async () =>
    sql<
      {
        target: string;
        before: Record<string, unknown> | null;
        after: Record<string, unknown> | null;
        changed_by: string;
      }[]
    >`select target, before, after, changed_by from payment_change order by id`;

  /** Back to what the migration seeds: top-ups on, Stripe active, enabled, live. */
  const reset = async () => {
    await sql`update payment_provider set enabled = true, mode = 'live'`;
    await sql`update payment_settings set topups_enabled = true, active_provider = 'stripe'`;
    payments.clearPaymentSettingsCache();
  };

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;

    sql = (await import("postgres")).default(database.url, { max: 4 });
    payments = await import("../settings");
    gateway = await import("../gateway");
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  beforeEach(async () => {
    setEnv(configured);
    await reset();
  });

  describe("settings", () => {
    it("starts as today: top-ups on, Stripe active, enabled, in live mode", async () => {
      expect(await payments.getPaymentSettings()).toMatchObject({
        topupsEnabled: true,
        activeProvider: "stripe",
        providers: [{ provider: "stripe", enabled: true, mode: "live" }],
      });
    });

    it("has one provider row per provider the billing package implements", async () => {
      const { PAYMENT_PROVIDERS } = await import("@workspace/db/schema");
      const { providers } = await import("@workspace/billing-web/server");
      expect(Object.keys(providers).sort()).toEqual([...PAYMENT_PROVIDERS]);
      const rows = await sql`select provider from payment_provider`;
      expect(rows.map((row) => row.provider)).toEqual([...PAYMENT_PROVIDERS]);
    });

    it("records who switched top-ups off, and from what to what", async () => {
      const count = (await changes()).length;
      const after = await payments.updatePaymentSettings({
        topupsEnabled: false,
        activeProvider: "stripe",
        staffId: STAFF,
      });
      expect(after).toMatchObject({ topupsEnabled: false, updatedBy: STAFF });

      const recorded = await changes();
      expect(recorded).toHaveLength(count + 1);
      expect(recorded.at(-1)).toEqual({
        target: "settings",
        before: { topupsEnabled: true, activeProvider: "stripe" },
        after: { topupsEnabled: false, activeProvider: "stripe" },
        changed_by: STAFF,
      });
    });

    it("records a provider's mode change before and after", async () => {
      await payments.updatePaymentProvider({
        provider: "stripe",
        enabled: true,
        mode: "test",
        staffId: STAFF,
      });
      expect((await payments.getPaymentSettings()).providers[0]).toMatchObject({
        mode: "test",
        updatedBy: STAFF,
      });
      expect((await changes()).at(-1)).toEqual({
        target: "provider:stripe",
        before: { enabled: true, mode: "live" },
        after: { enabled: true, mode: "test" },
        changed_by: STAFF,
      });
    });

    it("applies a change at once in this process, without waiting for the cache", async () => {
      // read once so the settings are cached
      expect(await gateway.isTopUpAvailable()).toBe(true);
      await payments.updatePaymentSettings({
        topupsEnabled: false,
        activeProvider: "stripe",
        staffId: STAFF,
      });
      expect(await gateway.isTopUpAvailable()).toBe(false);
    });

    it("keeps the history append-only", async () => {
      await payments.updatePaymentSettings({
        topupsEnabled: true,
        activeProvider: "stripe",
        staffId: STAFF,
      });
      await expect(sql`delete from payment_change`).rejects.toThrow(
        /append-only/,
      );
      await expect(
        sql`update payment_change set changed_by = 'someone'`,
      ).rejects.toThrow(/append-only/);
    });

    it("refuses a provider or mode the product does not know", async () => {
      await expect(
        sql`insert into payment_provider (provider) values ('paypal')`,
      ).rejects.toMatchObject({ constraint_name: "payment_provider_known" });
      await expect(
        sql`update payment_provider set mode = 'sandbox'`,
      ).rejects.toMatchObject({ constraint_name: "payment_provider_mode" });
      await expect(
        sql`insert into payment_settings (id) values (2)`,
      ).rejects.toMatchObject({
        constraint_name: "payment_settings_single_row",
      });
    });
  });

  describe("refusals", () => {
    it("refuses to make a disabled provider active, and records nothing", async () => {
      // with one provider there is no other to switch from, so the row says
      // another is active, as it would once the Saudi gateway exists
      await sql`alter table payment_provider drop constraint payment_provider_known`;
      try {
        await sql`insert into payment_provider (provider, enabled) values ('other', true)`;
        await sql`update payment_settings set active_provider = 'other'`;
        await sql`update payment_provider set enabled = false where provider = 'stripe'`;
        payments.clearPaymentSettingsCache();
        const count = (await changes()).length;

        await expect(
          payments.updatePaymentSettings({
            topupsEnabled: true,
            activeProvider: "stripe",
            staffId: STAFF,
          }),
        ).rejects.toMatchObject({
          status: 409,
          code: "admin:payments.error.providerDisabled",
        });
        expect(await changes()).toHaveLength(count);
        expect(await payments.getPaymentSettings()).toMatchObject({
          activeProvider: "other",
        });

        // enabled, but its live secrets are missing: still refused
        await sql`update payment_provider set enabled = true where provider = 'stripe'`;
        setEnv({ STRIPE_SECRET_KEY: configured.STRIPE_SECRET_KEY });
        await expect(
          payments.updatePaymentSettings({
            topupsEnabled: true,
            activeProvider: "stripe",
            staffId: STAFF,
          }),
        ).rejects.toMatchObject({
          status: 409,
          code: "admin:payments.error.providerNotConfigured",
        });

        setEnv(configured);
        await payments.updatePaymentSettings({
          topupsEnabled: true,
          activeProvider: "stripe",
          staffId: STAFF,
        });
        expect(await payments.getPaymentSettings()).toMatchObject({
          activeProvider: "stripe",
        });
      } finally {
        await sql`update payment_settings set active_provider = 'stripe'`;
        await sql`delete from payment_provider where provider = 'other'`;
        await sql`alter table payment_provider add constraint payment_provider_known check (provider in ('stripe'))`;
      }
    });

    it("refuses to disable the active provider", async () => {
      const count = (await changes()).length;
      await expect(
        payments.updatePaymentProvider({
          provider: "stripe",
          enabled: false,
          mode: "live",
          staffId: STAFF,
        }),
      ).rejects.toMatchObject({
        status: 409,
        code: "admin:payments.error.activeProviderDisabled",
      });
      expect(await changes()).toHaveLength(count);
      expect((await payments.getPaymentSettings()).providers[0]?.enabled).toBe(
        true,
      );
    });

    it.each([
      [
        "no test secrets",
        {
          ...configured,
          STRIPE_TEST_SECRET_KEY: undefined,
          STRIPE_TEST_WEBHOOK_SECRET: undefined,
        },
      ],
      [
        "no test webhook secret",
        { ...configured, STRIPE_TEST_WEBHOOK_SECRET: undefined },
      ],
      [
        "a live key named as the test key",
        { ...configured, STRIPE_TEST_SECRET_KEY: "sk_live_wrong_mode" },
      ],
    ])("refuses to switch to test mode with %s", async (_, values) => {
      setEnv(values);
      const count = (await changes()).length;
      await expect(
        payments.updatePaymentProvider({
          provider: "stripe",
          enabled: true,
          mode: "test",
          staffId: STAFF,
        }),
      ).rejects.toMatchObject({
        status: 409,
        code: "admin:payments.error.secretsMissing",
      });
      expect(await changes()).toHaveLength(count);
      expect((await payments.getPaymentSettings()).providers[0]?.mode).toBe(
        "live",
      );
    });

    it("refuses to switch to live mode when the live key is a test key", async () => {
      await payments.updatePaymentProvider({
        provider: "stripe",
        enabled: true,
        mode: "test",
        staffId: STAFF,
      });
      setEnv({ ...configured, STRIPE_SECRET_KEY: "sk_test_named_as_live" });
      await expect(
        payments.updatePaymentProvider({
          provider: "stripe",
          enabled: true,
          mode: "live",
          staffId: STAFF,
        }),
      ).rejects.toMatchObject({
        status: 409,
        code: "admin:payments.error.secretsMissing",
      });
    });

    it("still lets staff save a provider whose current mode lost its secrets", async () => {
      setEnv({});
      await payments.updatePaymentProvider({
        provider: "stripe",
        enabled: true,
        mode: "live",
        staffId: STAFF,
      });
      expect(await gateway.isTopUpAvailable()).toBe(false);
    });
  });

  describe("secret states", () => {
    it.each([
      ["sk_live_abc", "present"],
      ["rk_live_abc", "present"],
      ["sk_test_abc", "wrongMode"],
      ["pk_live_abc", "wrongMode"],
      ["", "missing"],
      [undefined, "missing"],
    ])("live key %s is %s", async (key, status) => {
      setEnv({ ...configured, STRIPE_SECRET_KEY: key });
      const { providers } = await import("@workspace/billing-web/server");
      const secrets = providers.stripe.topUp.secrets();
      expect(secrets.live.secretKey).toEqual({
        name: "STRIPE_SECRET_KEY",
        status,
      });
      expect(secrets.live.isConfigured).toBe(status === "present");
    });

    it.each([
      ["sk_test_abc", "present"],
      ["rk_test_abc", "present"],
      ["sk_live_abc", "wrongMode"],
    ])("test key %s is %s", async (key, status) => {
      setEnv({ ...configured, STRIPE_TEST_SECRET_KEY: key });
      const { providers } = await import("@workspace/billing-web/server");
      expect(providers.stripe.topUp.secrets().test.secretKey.status).toBe(
        status,
      );
    });
  });

  describe("admin API", () => {
    const staff: User = {
      id: STAFF,
      name: "Staff",
      email: "staff@example.com",
      emailVerified: true,
      banned: false,
      isAnonymous: false,
      twoFactorEnabled: false,
      createdAt: new Date(),
      updatedAt: new Date(),
    };

    const app = async () => {
      const { Hono } = await import("hono");
      const { HttpException } = await import("@workspace/shared/utils");
      const { paymentsRouter } = await import("../router");
      // enforceAdmin puts the signed-in staff user here in the real app
      return new Hono<{ Variables: { user: User } }>()
        .use(async (c, next) => {
          c.set("user", staff);
          await next();
        })
        .route("/", paymentsRouter)
        .onError((error) =>
          error instanceof HttpException
            ? Response.json({ code: error.code }, { status: error.status })
            : Response.json({ message: error.message }, { status: 500 }),
        );
    };

    it("shows which secrets are set and the webhook URL, never a secret's value", async () => {
      setEnv({ ...configured, STRIPE_TEST_WEBHOOK_SECRET: undefined });
      const response = await (
        await app()
      ).request("https://admin.example.com/");
      expect(response.status).toBe(200);
      const text = await response.text();

      expect(text).not.toContain("SECRETVALUE");
      for (const value of Object.values(configured)) {
        expect(text).not.toContain(value);
      }
      expect(JSON.parse(text)).toMatchObject({
        topupsEnabled: true,
        activeProvider: "stripe",
        providers: [
          {
            provider: "stripe",
            enabled: true,
            mode: "live",
            isActive: true,
            webhookUrl: "https://admin.example.com/api/billing/webhook/stripe",
            secrets: {
              live: {
                secretKey: { name: "STRIPE_SECRET_KEY", status: "present" },
                webhookSecret: {
                  name: "STRIPE_WEBHOOK_SECRET",
                  status: "present",
                },
                isConfigured: true,
              },
              test: {
                secretKey: {
                  name: "STRIPE_TEST_SECRET_KEY",
                  status: "present",
                },
                webhookSecret: {
                  name: "STRIPE_TEST_WEBHOOK_SECRET",
                  status: "missing",
                },
                isConfigured: false,
              },
            },
          },
        ],
      });
    });

    it("answers a refusal with 409 and its reason", async () => {
      setEnv({
        STRIPE_SECRET_KEY: configured.STRIPE_SECRET_KEY,
        STRIPE_WEBHOOK_SECRET: configured.STRIPE_WEBHOOK_SECRET,
      });
      const response = await (
        await app()
      ).request("/providers/stripe", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ enabled: true, mode: "test" }),
      });
      expect(response.status).toBe(409);
      expect(await response.json()).toEqual({
        code: "admin:payments.error.secretsMissing",
      });
    });

    it("refuses a provider the product does not have", async () => {
      const response = await (
        await app()
      ).request("/providers/paypal", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ enabled: true, mode: "live" }),
      });
      expect(response.status).toBe(422);
    });

    it("saves top-ups off and lists the change", async () => {
      const server = await app();
      const saved = await server.request("/settings", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          topupsEnabled: false,
          activeProvider: "stripe",
        }),
      });
      expect(saved.status).toBe(200);
      expect(await saved.json()).toMatchObject({
        topupsEnabled: false,
        updatedBy: STAFF,
      });

      const listed = await server.request("/changes");
      const [latest] = z
        .array(z.record(z.string(), z.unknown()))
        .parse(await listed.json());
      expect(latest).toMatchObject({
        target: "settings",
        before: { topupsEnabled: true },
        after: { topupsEnabled: false },
        changedBy: STAFF,
      });
    });
  });
});
