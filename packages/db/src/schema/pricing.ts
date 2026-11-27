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
 * Every rate is in basis points: 100 bps = 1%. Money is micro-SAR (1 SAR =
 * 1,000,000), the customer's currency (ADR 0017). The gateway's credits plugin
 * reads pricing_settings and provider_markup; it cannot write them.
 */
export const MAX_MARKUP_BPS = 100_000;
export const MAX_TOPUP_FEE_BPS = 10_000;
/** 400 SAR in micro-SAR: a ceiling against a typo, not a business rule. */
export const MAX_TOPUP_FEE_MIN_MICRO_SAR = 400_000_000;
/** 4,000,000 SAR in micro-SAR: a ceiling against a typo, not a business rule. */
export const MAX_TOPUP_MICRO_SAR = 4_000_000_000_000;
/** How many quick-pick amounts the top-up form shows at most. */
export const MAX_TOPUP_PRESETS = 8;

/**
 * The USD to SAR rate is stored × 1,000,000 as an exact integer (3.75 is
 * 3,750,000). The default is the peg. The range is a ceiling against a typo,
 * not a business rule: a rate of 37.5 or 0.375 is refused. Holds and charges
 * convert at the effective rate, official plus spread
 * (effectiveUsdToSarRateE6 in @workspace/shared/utils).
 */
export const DEFAULT_USD_TO_SAR_RATE_E6 = 3_750_000;
export const MIN_USD_TO_SAR_RATE_E6 = 3_000_000;
export const MAX_USD_TO_SAR_RATE_E6 = 5_000_000;
/**
 * The bank's commission on converting USD to SAR, in basis points, added to the
 * official rate: 1,000 bps (10%) is a ceiling against a typo, not a business rule.
 */
export const MAX_FX_SPREAD_BPS = 1_000;

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
     * The least fee a top-up pays, in micro-SAR: the fee is the larger of the
     * percentage and this amount, as OpenRouter's 5.5% with a $0.80 minimum.
     */
    topupFeeMinMicroSar: bigint({ mode: "number" })
      .notNull()
      .default(3_000_000),
    /*
     * The limits of one top-up payment and the quick-pick amounts the form
     * offers (ADMIN-05), in micro-SAR.
     */
    /** The least credit one top-up may buy. */
    topupMinMicroSar: bigint({ mode: "number" }).notNull().default(20_000_000),
    /** The most credit one top-up may buy. */
    topupMaxMicroSar: bigint({ mode: "number" })
      .notNull()
      .default(3_750_000_000),
    /** 1 to 8 amounts, each within the limits. */
    topupPresetsMicroSar: bigint({ mode: "number" })
      .array()
      .notNull()
      .default(sql`'{40000000,100000000,200000000,400000000}'::bigint[]`),
    /**
     * USD to SAR × 1,000,000 (ADMIN-06). The credits plugin converts each
     * request's USD cost with it when it holds and settles, and stores the
     * rate on the hold and the entry, so a change applies to new requests only.
     */
    usdToSarRateE6: integer("usd_to_sar_rate_e6")
      .notNull()
      .default(DEFAULT_USD_TO_SAR_RATE_E6),
    /** The bank's conversion commission in bps, added to the official rate (ADMIN-06). */
    fxSpreadBps: integer().notNull().default(0),
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
      sql`${t.topupFeeMinMicroSar} between 0 and ${sql.raw(String(MAX_TOPUP_FEE_MIN_MICRO_SAR))}`,
    ),
    check(
      "pricing_settings_topup_limits_range",
      sql`${t.topupMinMicroSar} > 0 and ${t.topupMaxMicroSar} >= ${t.topupMinMicroSar} and ${t.topupMaxMicroSar} <= ${sql.raw(String(MAX_TOPUP_MICRO_SAR))}`,
    ),
    check(
      "pricing_settings_topup_presets_range",
      sql`cardinality(${t.topupPresetsMicroSar}) between 1 and ${sql.raw(String(MAX_TOPUP_PRESETS))} and array_position(${t.topupPresetsMicroSar}, null) is null and ${t.topupMinMicroSar} <= all(${t.topupPresetsMicroSar}) and ${t.topupMaxMicroSar} >= all(${t.topupPresetsMicroSar})`,
    ),
    check(
      "pricing_settings_usd_to_sar_rate_range",
      sql`${t.usdToSarRateE6} between ${sql.raw(String(MIN_USD_TO_SAR_RATE_E6))} and ${sql.raw(String(MAX_USD_TO_SAR_RATE_E6))}`,
    ),
    check(
      "pricing_settings_fx_spread_range",
      sql`${t.fxSpreadBps} between 0 and ${sql.raw(String(MAX_FX_SPREAD_BPS))}`,
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
