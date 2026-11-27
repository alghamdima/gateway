import {
  bigint,
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
/** 100 USD in micro-USD: a ceiling against a typo, not a business rule. */
export const MAX_TOPUP_FEE_MIN_MICRO_USD = 100_000_000;
/** 1,000,000 USD in micro-USD: a ceiling against a typo, not a business rule. */
export const MAX_TOPUP_MICRO_USD = 1_000_000_000_000;
/** How many quick-pick amounts the top-up form shows at most. */
export const MAX_TOPUP_PRESETS = 8;

/** A single row (id = 1), created by the migration. */
export const pricingSettings = pgTable(
  "pricing_settings",
  {
    id: smallint().primaryKey().default(1),
    /** Added to provider cost when no provider_markup row matches. */
    defaultMarkupBps: integer().notNull(),
    /** Added on a top-up payment, never taken from credits (ADR 0003). */
    topupFeeBps: integer().notNull(),
    /**
     * The least fee a top-up pays, in micro-USD: the fee is the larger of the
     * percentage and this amount, as OpenRouter's 5.5% with a $0.80 minimum.
     */
    topupFeeMinMicroUsd: bigint({ mode: "number" }).notNull().default(800_000),
    /*
     * The limits of one top-up payment and the quick-pick amounts the form
     * offers (ADMIN-05). In micro-USD for now, like the fee minimum: the move
     * to SAR is ADMIN-06, which converts every money setting together.
     */
    /** The least credit one top-up may buy. */
    topupMinMicroUsd: bigint({ mode: "number" }).notNull().default(5_000_000),
    /** The most credit one top-up may buy. */
    topupMaxMicroUsd: bigint({ mode: "number" })
      .notNull()
      .default(1_000_000_000),
    /** 1 to 8 amounts, each within the limits. */
    topupPresetsMicroUsd: bigint({ mode: "number" })
      .array()
      .notNull()
      .default(sql`'{10000000,25000000,50000000,100000000}'::bigint[]`),
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
    check(
      "pricing_settings_topup_fee_min_range",
      sql`${t.topupFeeMinMicroUsd} between 0 and ${sql.raw(String(MAX_TOPUP_FEE_MIN_MICRO_USD))}`,
    ),
    check(
      "pricing_settings_topup_limits_range",
      sql`${t.topupMinMicroUsd} > 0 and ${t.topupMaxMicroUsd} >= ${t.topupMinMicroUsd} and ${t.topupMaxMicroUsd} <= ${sql.raw(String(MAX_TOPUP_MICRO_USD))}`,
    ),
    check(
      "pricing_settings_topup_presets_range",
      sql`cardinality(${t.topupPresetsMicroUsd}) between 1 and ${sql.raw(String(MAX_TOPUP_PRESETS))} and array_position(${t.topupPresetsMicroUsd}, null) is null and ${t.topupMinMicroUsd} <= all(${t.topupPresetsMicroUsd}) and ${t.topupMaxMicroUsd} >= all(${t.topupPresetsMicroUsd})`,
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
