import {
  bigserial,
  boolean,
  check,
  jsonb,
  pgTable,
  smallint,
  text,
  timestamp,
} from "drizzle-orm/pg-core";
import { sql } from "drizzle-orm/sql";

import { createSelectSchema } from "../lib/zod";

import type * as z from "zod";

/**
 * The payment gateways staff manage in the admin (ADMIN-03): which provider
 * takes top-ups, in which mode, and whether top-ups are on at all. Secrets
 * stay in env (packages/billing/web/src/providers/<provider>/env.ts); these
 * tables only choose between them.
 */

/** The providers the billing package implements (packages/billing/web/src/providers). */
export const PAYMENT_PROVIDERS = ["stripe"] as const;
export type PaymentProviderName = (typeof PAYMENT_PROVIDERS)[number];

/** Test takes no real money; live does. Each mode has its own secrets. */
export const PAYMENT_MODES = ["test", "live"] as const;
export type PaymentMode = (typeof PAYMENT_MODES)[number];

const quoted = (values: readonly string[]) =>
  sql.raw(values.map((value) => `'${value}'`).join(", "));

/** One row per provider, created by the migration that adds the provider. */
export const paymentProvider = pgTable(
  "payment_provider",
  {
    provider: text().primaryKey().$type<PaymentProviderName>(),
    /** A disabled provider cannot be the active one. */
    enabled: boolean().notNull().default(true),
    /** Which of the provider's env secrets checkouts use. */
    mode: text().$type<PaymentMode>().notNull().default("live"),
    updatedAt: timestamp().notNull().defaultNow(),
    updatedBy: text(),
  },
  (t) => [
    check(
      "payment_provider_known",
      sql`${t.provider} in (${quoted(PAYMENT_PROVIDERS)})`,
    ),
    check(
      "payment_provider_mode",
      sql`${t.mode} in (${quoted(PAYMENT_MODES)})`,
    ),
  ],
);

/** A single row (id = 1), created by the migration. */
export const paymentSettings = pgTable(
  "payment_settings",
  {
    id: smallint().primaryKey().default(1),
    /** Off: new checkouts are refused; payments already started still credit. */
    topupsEnabled: boolean().notNull().default(true),
    /** The provider new checkouts go to. */
    activeProvider: text()
      .$type<PaymentProviderName>()
      .notNull()
      .default("stripe")
      .references(() => paymentProvider.provider),
    updatedAt: timestamp().notNull().defaultNow(),
    updatedBy: text(),
  },
  (t) => [check("payment_settings_single_row", sql`${t.id} = 1`)],
);

/** Append-only: a trigger rejects UPDATE and DELETE, as for pricing_change. */
export const paymentChange = pgTable("payment_change", {
  id: bigserial({ mode: "number" }).primaryKey(),
  /** What changed: "settings" or "provider:<name>". */
  target: text().notNull(),
  before: jsonb().$type<Record<string, unknown> | null>(),
  after: jsonb().$type<Record<string, unknown> | null>(),
  changedBy: text().notNull(),
  changedAt: timestamp().notNull().defaultNow(),
});

export const selectPaymentProviderSchema = createSelectSchema(paymentProvider);
export const selectPaymentSettingsSchema = createSelectSchema(paymentSettings);
export const selectPaymentChangeSchema = createSelectSchema(paymentChange);

export type SelectPaymentProvider = z.infer<typeof selectPaymentProviderSchema>;
export type SelectPaymentSettings = z.infer<typeof selectPaymentSettingsSchema>;
export type SelectPaymentChange = z.infer<typeof selectPaymentChangeSchema>;
