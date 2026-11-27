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
import { organization } from "./auth";

import type * as z from "zod";

/**
 * Prices staff set in the admin console (docs/platform/adr/0010-admin-controlled-pricing.md).
 * Every rate is in basis points: 100 bps = 1%. Money is micro-SAR (1 SAR =
 * 1,000,000), the customer's currency (ADR 0017). Requests are charged at the
 * provider's cost converted at the effective USD to SAR rate, with no markup:
 * the platform's revenue is the top-up fee (ADR 0010, amended). The gateway's
 * credits plugin reads pricing_settings; it cannot write it.
 */
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

/**
 * Where the fee a top-up paid came from (ADMIN-24): the pricing settings, or
 * the organization's own fee below. BILL-08 adds promo codes here.
 */
export const TOPUP_FEE_SOURCES = ["default", "organization"] as const;
export type TopupFeeSource = (typeof TOPUP_FEE_SOURCES)[number];

/** The longest note staff may keep on an organization's fee. */
export const MAX_TOPUP_FEE_NOTE_LENGTH = 500;

/**
 * A top-up fee for one organization, higher or lower than the default
 * (ADMIN-24). A null field uses the default's value; no row, or a row past
 * `expiresAt`, is the default. The top-up reads it at the time of the quote
 * (packages/api/src/modules/pricing/organization-fee.ts), so an expired fee
 * falls back without anyone acting. The bounds are the default's.
 */
export const organizationTopupFee = pgTable(
  "organization_topup_fee",
  {
    organizationId: text()
      .primaryKey()
      .references(() => organization.id, { onDelete: "cascade" }),
    /** 100 bps = 1%; null uses pricing_settings.topup_fee_bps. */
    feeBps: integer(),
    /** micro-SAR; null uses pricing_settings.topup_fee_min_micro_sar. */
    feeMinMicroSar: bigint({ mode: "number" }),
    /** From this moment the default applies again; null never expires. */
    expiresAt: timestamp(),
    /** Why staff set it, for the next person who reads it. */
    note: text().notNull().default(""),
    updatedAt: timestamp().notNull().defaultNow(),
    updatedBy: text().notNull(),
  },
  (t) => [
    check(
      "organization_topup_fee_bps_range",
      sql`${t.feeBps} is null or ${t.feeBps} between 0 and ${sql.raw(String(MAX_TOPUP_FEE_BPS))}`,
    ),
    check(
      "organization_topup_fee_min_range",
      sql`${t.feeMinMicroSar} is null or ${t.feeMinMicroSar} between 0 and ${sql.raw(String(MAX_TOPUP_FEE_MIN_MICRO_SAR))}`,
    ),
    // a row that changes nothing is a cleared fee: it is deleted instead
    check(
      "organization_topup_fee_sets_something",
      sql`${t.feeBps} is not null or ${t.feeMinMicroSar} is not null`,
    ),
    check(
      "organization_topup_fee_note_length",
      sql`char_length(${t.note}) <= ${sql.raw(String(MAX_TOPUP_FEE_NOTE_LENGTH))}`,
    ),
  ],
);

/** Append-only: a trigger rejects UPDATE and DELETE, as for the ledger. */
export const pricingChange = pgTable("pricing_change", {
  id: bigserial({ mode: "number" }).primaryKey(),
  /**
   * What changed: "settings", or "organization:<id>" for one organization's
   * top-up fee (ADMIN-24; the row outlives the organization). Rows from before the markup was removed may also
   * name "provider:<name>" (a per-provider markup); they stay as history.
   */
  target: text().notNull(),
  before: jsonb().$type<Record<string, unknown> | null>(),
  after: jsonb().$type<Record<string, unknown> | null>(),
  changedBy: text().notNull(),
  changedAt: timestamp().notNull().defaultNow(),
});

export const selectPricingSettingsSchema = createSelectSchema(pricingSettings);
export const selectPricingChangeSchema = createSelectSchema(pricingChange);
export const selectOrganizationTopupFeeSchema =
  createSelectSchema(organizationTopupFee);

export type SelectPricingSettings = z.infer<typeof selectPricingSettingsSchema>;
export type SelectPricingChange = z.infer<typeof selectPricingChangeSchema>;
export type SelectOrganizationTopupFee = z.infer<
  typeof selectOrganizationTopupFeeSchema
>;
