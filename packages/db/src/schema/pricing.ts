import {
  bigserial,
  check,
  integer,
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
 * Prices staff set in the admin console (docs/platform/adr/0010-admin-controlled-pricing.md).
 * Every rate is in basis points: 100 bps = 1%. The gateway's credits plugin reads
 * pricing_settings and provider_markup; it cannot write them.
 */
export const MAX_MARKUP_BPS = 100_000;
export const MAX_TOPUP_FEE_BPS = 10_000;

/** A single row (id = 1), created by the migration. */
export const pricingSettings = pgTable(
  "pricing_settings",
  {
    id: smallint().primaryKey().default(1),
    /** Added to provider cost when no provider_markup row matches. */
    defaultMarkupBps: integer().notNull(),
    /** Added on a top-up payment, never taken from credits (ADR 0003). */
    topupFeeBps: integer().notNull(),
    updatedAt: timestamp().notNull().defaultNow(),
    updatedBy: text(),
  },
  (t) => [
    check("pricing_settings_single_row", sql`${t.id} = 1`),
    check(
      "pricing_settings_default_markup_range",
      sql`${t.defaultMarkupBps} between 0 and ${sql.raw(String(MAX_MARKUP_BPS))}`,
    ),
    check(
      "pricing_settings_topup_fee_range",
      sql`${t.topupFeeBps} between 0 and ${sql.raw(String(MAX_TOPUP_FEE_BPS))}`,
    ),
  ],
);

/** The markup for one provider, by its gateway name (openai, anthropic, ...). */
export const providerMarkup = pgTable(
  "provider_markup",
  {
    provider: text().primaryKey(),
    markupBps: integer().notNull(),
    updatedAt: timestamp().notNull().defaultNow(),
    updatedBy: text(),
  },
  (t) => [
    check(
      "provider_markup_provider_format",
      sql`${t.provider} ~ '^[a-z0-9][a-z0-9_.-]{0,63}$'`,
    ),
    check(
      "provider_markup_range",
      sql`${t.markupBps} between 0 and ${sql.raw(String(MAX_MARKUP_BPS))}`,
    ),
  ],
);

/** Append-only: a trigger rejects UPDATE and DELETE, as for the ledger. */
export const pricingChange = pgTable("pricing_change", {
  id: bigserial({ mode: "number" }).primaryKey(),
  /** What changed: "settings" or "provider:<name>". */
  target: text().notNull(),
  before: jsonb().$type<Record<string, unknown> | null>(),
  after: jsonb().$type<Record<string, unknown> | null>(),
  changedBy: text().notNull(),
  changedAt: timestamp().notNull().defaultNow(),
});

export const selectPricingSettingsSchema = createSelectSchema(pricingSettings);
export const selectProviderMarkupSchema = createSelectSchema(providerMarkup);
export const selectPricingChangeSchema = createSelectSchema(pricingChange);

export type SelectPricingSettings = z.infer<typeof selectPricingSettingsSchema>;
export type SelectProviderMarkup = z.infer<typeof selectProviderMarkupSchema>;
export type SelectPricingChange = z.infer<typeof selectPricingChangeSchema>;
