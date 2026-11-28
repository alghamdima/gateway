import {
  bigint,
  bigserial,
  boolean,
  check,
  index,
  integer,
  jsonb,
  pgTable,
  primaryKey,
  text,
  timestamp,
  uniqueIndex,
} from "drizzle-orm/pg-core";
import { sql } from "drizzle-orm/sql";

import { generateId } from "@workspace/shared/utils";

import { createSelectSchema } from "../lib/zod";
import { organization } from "./auth";

import type { AnyPgColumn } from "drizzle-orm/pg-core";

/**
 * Promo codes (BILL-08, docs/platform/SPEC.md). Staff create them in the admin;
 * a customer applies one to a top-up (a discount on the fee, or bonus credit on
 * top of it) or redeems one for free credit. Money is micro-SAR (ADR 0017).
 * What a code granted is in the ledger: bonus and free credit are `promo`
 * entries carrying the code's id, a fee discount is on the top-up entry
 * (credit_ledger_entry.promo_code_id, metadata.promoFeeDiscountMicroSar). The
 * gateway reads none of these tables: a promo entry is a credit like any other
 * and reaches it through credit_account.balance.
 */

/** fee_discount and topup_bonus apply to a top-up; free_credit needs none. */
export const PROMO_CODE_KINDS = [
  "fee_discount",
  "topup_bonus",
  "free_credit",
] as const;
export type PromoCodeKind = (typeof PROMO_CODE_KINDS)[number];

/** The kinds a top-up can carry. */
export const TOPUP_PROMO_CODE_KINDS = ["fee_discount", "topup_bonus"] as const;

/** Upper case letters, digits, hyphen and underscore, 3 to 32 long, after normalizing. */
export const PROMO_CODE_PATTERN = /^[A-Z0-9_-]{3,32}$/;
/** 10,000 SAR in micro-SAR: a ceiling against a typo, not a business rule. */
export const MAX_PROMO_MICRO_SAR = 10_000_000_000;
/** The longest note staff may keep on a code. */
export const MAX_PROMO_NOTE_LENGTH = 500;
/** How many organizations a code may name at most. */
export const MAX_PROMO_ORGANIZATIONS = 500;

const microSar = () => bigint({ mode: "number" });
const halalas = (column: AnyPgColumn) =>
  sql`${column} is null or (${column} > 0 and ${column} <= ${sql.raw(String(MAX_PROMO_MICRO_SAR))} and ${column} % 10000 = 0)`;

export const promoCode = pgTable(
  "promo_code",
  {
    id: text().primaryKey().$defaultFn(generateId),
    /** Normalized: trimmed and upper case, so the unique index is case-insensitive. */
    code: text().notNull(),
    kind: text().$type<PromoCodeKind>().notNull(),
    /** fee_discount: percent off the fee, 100 bps = 1%. */
    feeDiscountBps: integer(),
    /** fee_discount: a fixed amount off the fee, never below zero. */
    feeDiscountMicroSar: microSar(),
    /** topup_bonus: percent of the credit bought, added as promo credit. */
    bonusBps: integer(),
    /** topup_bonus: the most bonus one top-up gets. */
    bonusCapMicroSar: microSar(),
    /** free_credit: what one redemption grants. */
    creditMicroSar: microSar(),
    /** Null: no limit. */
    maxUses: integer(),
    /**
     * Redemptions so far, moved in the same transaction as each redemption,
     * under a row lock. A top-up that was quoted before the code ran out is
     * still honoured when paid, so this can pass maxUses by the checkouts open
     * at the time (SPEC BILL-08, decisions).
     */
    uses: integer().notNull().default(0),
    onePerOrganization: boolean().notNull().default(true),
    /** Null: valid from creation. */
    startsAt: timestamp(),
    /** Null: never expires. The code is refused from this moment. */
    endsAt: timestamp(),
    /** Top-up kinds only: the least credit the top-up must buy. */
    minTopupMicroSar: microSar(),
    /**
     * When the credit the code grants expires (BILL-17, ADR 0019): its own
     * date, separate from the purchase rule. Null: the purchase rule
     * (pricing_settings.credit_expiry_days from the moment it is granted). No
     * earlier than the code's end, so a use never grants credit that expires
     * before the code does. Not for a fee discount, which grants no credit.
     */
    creditExpiresAt: timestamp(),
    /** False: only the organizations in promo_code_organization. */
    allOrganizations: boolean().notNull().default(true),
    isPaused: boolean().notNull().default(false),
    note: text().notNull().default(""),
    createdAt: timestamp().notNull().defaultNow(),
    createdBy: text().notNull(),
    updatedAt: timestamp().notNull().defaultNow(),
    updatedBy: text().notNull(),
  },
  (t) => [
    uniqueIndex("promo_code_code_unique").on(t.code),
    check("promo_code_code_format", sql`${t.code} ~ '^[A-Z0-9_-]{3,32}$'`),
    check(
      "promo_code_kind",
      sql`${t.kind} in (${sql.raw(PROMO_CODE_KINDS.map((kind) => `'${kind}'`).join(", "))})`,
    ),
    // each kind carries its own amounts and nothing else
    check(
      "promo_code_amounts_match_kind",
      sql`(${t.kind} = 'fee_discount' and ((${t.feeDiscountBps} is null) <> (${t.feeDiscountMicroSar} is null)) and ${t.bonusBps} is null and ${t.bonusCapMicroSar} is null and ${t.creditMicroSar} is null)
        or (${t.kind} = 'topup_bonus' and ${t.bonusBps} is not null and ${t.bonusCapMicroSar} is not null and ${t.feeDiscountBps} is null and ${t.feeDiscountMicroSar} is null and ${t.creditMicroSar} is null)
        or (${t.kind} = 'free_credit' and ${t.creditMicroSar} is not null and ${t.feeDiscountBps} is null and ${t.feeDiscountMicroSar} is null and ${t.bonusBps} is null and ${t.bonusCapMicroSar} is null and ${t.minTopupMicroSar} is null)`,
    ),
    check(
      "promo_code_bps_range",
      sql`(${t.feeDiscountBps} is null or ${t.feeDiscountBps} between 1 and 10000) and (${t.bonusBps} is null or ${t.bonusBps} between 1 and 10000)`,
    ),
    check(
      "promo_code_amounts_halalas",
      sql`(${halalas(t.feeDiscountMicroSar)}) and (${halalas(t.bonusCapMicroSar)}) and (${halalas(t.creditMicroSar)}) and (${halalas(t.minTopupMicroSar)})`,
    ),
    check(
      "promo_code_uses",
      sql`${t.uses} >= 0 and (${t.maxUses} is null or ${t.maxUses} > 0)`,
    ),
    check(
      "promo_code_dates",
      sql`${t.startsAt} is null or ${t.endsAt} is null or ${t.endsAt} > ${t.startsAt}`,
    ),
    check(
      "promo_code_credit_expiry",
      sql`${t.creditExpiresAt} is null or (${t.kind} <> 'fee_discount' and (${t.endsAt} is null or ${t.creditExpiresAt} >= ${t.endsAt}) and (${t.startsAt} is null or ${t.creditExpiresAt} > ${t.startsAt}))`,
    ),
    check(
      "promo_code_note_length",
      sql`char_length(${t.note}) <= ${sql.raw(String(MAX_PROMO_NOTE_LENGTH))}`,
    ),
  ],
);

/** The organizations a code is for, when it is not for all of them. */
export const promoCodeOrganization = pgTable(
  "promo_code_organization",
  {
    promoCodeId: text()
      .notNull()
      .references(() => promoCode.id, { onDelete: "cascade" }),
    organizationId: text()
      .notNull()
      .references(() => organization.id, { onDelete: "cascade" }),
  },
  (t) => [primaryKey({ columns: [t.promoCodeId, t.organizationId] })],
);

/**
 * One use of a code by an organization. No foreign key to organization, as for
 * the ledger: the record outlives it. The partial unique index is the last
 * guard of one use per organization; the code's row lock is the first.
 */
export const promoCodeRedemption = pgTable(
  "promo_code_redemption",
  {
    id: text().primaryKey().$defaultFn(generateId),
    promoCodeId: text()
      .notNull()
      .references(() => promoCode.id, { onDelete: "restrict" }),
    organizationId: text().notNull(),
    kind: text().$type<PromoCodeKind>().notNull(),
    /** The code's setting when it was used. */
    onePerOrganization: boolean().notNull(),
    /** Top-up kinds: "<provider>:<payment id>". */
    paymentId: text(),
    /** Top-up kinds: the credit bought. */
    topupCreditMicroSar: microSar(),
    /** Taken off the fee; 0 when none. */
    feeDiscountMicroSar: microSar().notNull().default(0),
    /** Bonus or free credit granted; 0 when none. */
    creditMicroSar: microSar().notNull().default(0),
    /** The promo ledger entry of the credit granted, when there is one. */
    ledgerEntryId: text(),
    /** The top-up ledger entry, for the top-up kinds. */
    topupEntryId: text(),
    /** The same use is recorded once, however often it is retried. */
    idempotencyKey: text().notNull().unique(),
    /** Free credit: the member who redeemed it. */
    redeemedBy: text(),
    createdAt: timestamp().notNull().defaultNow(),
  },
  (t) => [
    uniqueIndex("promo_code_redemption_once_per_organization")
      .on(t.promoCodeId, t.organizationId)
      .where(sql`${t.onePerOrganization}`),
    index("promo_code_redemption_promoCodeId_createdAt_idx").on(
      t.promoCodeId,
      t.createdAt,
    ),
    check(
      "promo_code_redemption_amounts",
      sql`${t.feeDiscountMicroSar} >= 0 and ${t.creditMicroSar} >= 0 and (${t.topupCreditMicroSar} is null or ${t.topupCreditMicroSar} > 0)`,
    ),
  ],
);

/** Append-only: a trigger rejects UPDATE and DELETE, as for pricing_change. */
export const promoCodeChange = pgTable(
  "promo_code_change",
  {
    id: bigserial({ mode: "number" }).primaryKey(),
    promoCodeId: text().notNull(),
    before: jsonb().$type<Record<string, unknown> | null>(),
    after: jsonb().$type<Record<string, unknown> | null>(),
    changedBy: text().notNull(),
    changedAt: timestamp().notNull().defaultNow(),
  },
  (t) => [index("promo_code_change_promoCodeId_idx").on(t.promoCodeId)],
);

export const selectPromoCodeSchema = createSelectSchema(promoCode);
export const selectPromoCodeRedemptionSchema =
  createSelectSchema(promoCodeRedemption);

/** The row as Drizzle reads it, with `kind` typed as a PromoCodeKind. */
export type SelectPromoCode = typeof promoCode.$inferSelect;
export type SelectPromoCodeRedemption = typeof promoCodeRedemption.$inferSelect;
