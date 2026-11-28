import {
  bigint,
  check,
  index,
  integer,
  jsonb,
  pgEnum,
  pgTable,
  text,
  timestamp,
} from "drizzle-orm/pg-core";
import { sql } from "drizzle-orm/sql";

import { generateId } from "@workspace/shared/utils";

import { createInsertSchema, createSelectSchema } from "../lib/zod";
import { organization } from "./auth";
import { TOPUP_FEE_SOURCES } from "./pricing";
import { promoCode } from "./promo-codes";

import type { TopupFeeSource } from "./pricing";
import type { AnyPgColumn } from "drizzle-orm/pg-core";
import type * as z from "zod";

/**
 * Prepaid credits (docs/platform/adr/0001-credits-ledger.md).
 *
 * Every amount is micro-SAR (1 SAR = 1,000,000) in a Postgres bigint: the
 * customer's currency (docs/platform/adr/0017-sar-ledger.md). It is read as a
 * JS number, which is exact up to about 9 billion SAR. Provider costs are USD:
 * a usage entry and a hold store the official USD to SAR rate, the bank's
 * spread and the effective rate they were converted at.
 */

/**
 * effective = ceil(official × (10,000 + spread) / 10,000), in bigint so the
 * product cannot overflow: the same rule as effectiveUsdToSarRateE6.
 */
const effectiveRateRule = (
  official: AnyPgColumn,
  spread: AnyPgColumn,
  effective: AnyPgColumn,
) =>
  sql`${effective}::bigint = (${official}::bigint * (10000 + ${spread}) + 9999) / 10000`;
const microSar = () => bigint({ mode: "number" });

export const creditEntryKindEnum = pgEnum("credit_entry_kind", [
  "top_up",
  "usage",
  "refund",
  "adjustment",
  "promo",
  // what was left of a credit lot when it expired (BILL-17, ADR 0019)
  "expiry",
  // the unused part of a top-up's promo bonus, taken back when the top-up is refunded (ADR 0019)
  "promo_clawback",
  // credit moved from a personal account to an organization (ADR 0020): the
  // debit names the lot it leaves, the credit opens a lot with the same expiry
  "transfer_out",
  "transfer_in",
]);

/**
 * The kinds that name the credit lot they take from (ADR 0019): a refund of
 * a top-up, the expiry of a lot, and the clawback of a refunded top-up's
 * promo bonus. Compared as text in the checks, because migrations 0038 and
 * 0039 add "expiry" and "promo_clawback" to the enum in the same transaction.
 */
export const LOT_DEBIT_KINDS = [
  "refund",
  "expiry",
  "promo_clawback",
  "transfer_out",
] as const;
/** Lot debits that must name their lot and remove credit (a refund may predate lots). */
const NAMED_LOT_DEBIT_KINDS = [
  "expiry",
  "promo_clawback",
  "transfer_out",
] as const;
const textList = (values: readonly string[]) =>
  sql.raw(values.map((value) => `'${value}'`).join(", "));

/**
 * Append-only: a trigger rejects UPDATE and DELETE for every role. There is
 * no foreign key to organization on purpose, so the record of what was paid
 * and spent outlives the organization.
 */
export const creditLedgerEntry = pgTable(
  "credit_ledger_entry",
  {
    id: text().primaryKey().$defaultFn(generateId),
    organizationId: text().notNull(),
    /** Signed micro-SAR: credits are positive, debits negative. */
    amount: microSar().notNull(),
    kind: creditEntryKindEnum().notNull(),
    /** Payment id, refund id or request attempt: the same event never counts twice. */
    idempotencyKey: text().notNull().unique(),
    reference: text(),
    metadata: jsonb().$type<Record<string, unknown>>(),
    /** The gateway virtual key that spent it (usage only): per-key limits and reports (ADR 0013). */
    virtualKeyId: text(),
    /**
     * Usage only: the provider's cost in micro-USD, rounded
     * half up. Reconciliation compares it with the gateway's log (ADR 0002).
     */
    costMicroUsd: bigint({ mode: "number" }),
    /** Usage only: the official USD to SAR rate × 1,000,000 at the hold. */
    usdToSarRateE6: integer("usd_to_sar_rate_e6"),
    /** Usage only: the bank's spread in bps at the hold. */
    fxSpreadBps: integer(),
    /** Usage only: the rate × 1,000,000 the cost was converted at: official plus spread. */
    effectiveUsdToSarRateE6: integer("effective_usd_to_sar_rate_e6"),
    /*
     * Usage only (ACT-04, migration 0041): the parts of the attempt's tokens the
     * provider reported apart, summed by the activity page for as long as the
     * ledger is kept. Cached is the prompt read from the provider's cache, cache
     * write the prompt written to it, reasoning the completion spent thinking.
     * Null on entries before 0041 and on every other kind. Prompt and completion
     * tokens stay in metadata, where every earlier entry has them: the ledger is
     * append-only, so they cannot be moved.
     */
    cachedTokens: bigint({ mode: "number" }),
    cacheWriteTokens: bigint({ mode: "number" }),
    reasoningTokens: bigint({ mode: "number" }),
    /*
     * Top-up only: the fee rate the payment was quoted at and where it came
     * from (ADMIN-24), so a later change of the default or the organization's
     * fee does not rewrite what this payment paid. Null on top-ups started
     * before migration 0035. The fee amount is in metadata.feeMicroSar.
     */
    topupFeeBps: integer(),
    topupFeeMinMicroSar: bigint({ mode: "number" }),
    topupFeeSource: text().$type<TopupFeeSource>(),
    /**
     * The promo code behind the entry (BILL-08): every promo entry (bonus or
     * free credit) and every promo clawback names one; a top-up names the
     * code it was paid with.
     */
    promoCodeId: text().references(() => promoCode.id, {
      onDelete: "restrict",
    }),
    /**
     * Refund, expiry and promo clawback entries: the credit lot they take from (ADR 0019). A
     * grant is tied to its lot the other way, by credit_lot.entry_id.
     */
    creditLotId: text().references((): AnyPgColumn => creditLot.id, {
      onDelete: "restrict",
    }),
    createdAt: timestamp().notNull().defaultNow(),
  },
  (t) => [
    index("credit_ledger_entry_creditLotId_idx")
      .on(t.creditLotId)
      .where(sql`${t.creditLotId} is not null`),
    check(
      "credit_ledger_entry_lot_kind",
      sql`(${t.creditLotId} is null or ${t.kind}::text in (${textList(LOT_DEBIT_KINDS)})) and (${t.kind}::text not in (${textList(NAMED_LOT_DEBIT_KINDS)}) or (${t.creditLotId} is not null and ${t.amount} < 0))`,
    ),
    // a transfer adds to the organization; what leaves the personal account is a named lot debit
    check(
      "credit_ledger_entry_transfer_in_is_credit",
      sql`${t.kind}::text <> 'transfer_in' or ${t.amount} > 0`,
    ),
    check(
      "credit_ledger_entry_refund_is_debit",
      sql`${t.kind}::text <> 'refund' or ${t.amount} < 0`,
    ),
    index("credit_ledger_entry_organizationId_createdAt_idx").on(
      t.organizationId,
      t.createdAt,
    ),
    index("credit_ledger_entry_usage_virtualKeyId_createdAt_idx")
      .on(t.virtualKeyId, t.createdAt)
      .where(sql`${t.kind} = 'usage' and ${t.virtualKeyId} is not null`),
    check("credit_ledger_entry_amount_not_zero", sql`${t.amount} <> 0`),
    check(
      "credit_ledger_entry_cost_micro_usd_not_negative",
      sql`${t.costMicroUsd} is null or ${t.costMicroUsd} >= 0`,
    ),
    check(
      "credit_ledger_entry_usd_to_sar_rate_positive",
      sql`${t.usdToSarRateE6} is null or ${t.usdToSarRateE6} > 0`,
    ),
    check(
      "credit_ledger_entry_effective_rate",
      sql`${t.effectiveUsdToSarRateE6} is null or (${t.usdToSarRateE6} is not null and ${t.fxSpreadBps} between 0 and 10000 and ${effectiveRateRule(t.usdToSarRateE6, t.fxSpreadBps, t.effectiveUsdToSarRateE6)})`,
    ),
    // a charge always says what it cost in USD and the rates it was converted at
    check(
      "credit_ledger_entry_usage_has_cost_and_rate",
      sql`${t.kind} <> 'usage' or (${t.costMicroUsd} is not null and ${t.usdToSarRateE6} is not null and ${t.fxSpreadBps} is not null and ${t.effectiveUsdToSarRateE6} is not null)`,
    ),
    check(
      "credit_ledger_entry_tokens_usage_only",
      sql`(${t.cachedTokens} is null and ${t.cacheWriteTokens} is null and ${t.reasoningTokens} is null) or ${t.kind} = 'usage'`,
    ),
    check(
      "credit_ledger_entry_tokens_not_negative",
      sql`coalesce(${t.cachedTokens}, 0) >= 0 and coalesce(${t.cacheWriteTokens}, 0) >= 0 and coalesce(${t.reasoningTokens}, 0) >= 0`,
    ),
    check(
      "credit_ledger_entry_promo_code",
      sql`(${t.kind}::text not in ('promo', 'promo_clawback') or ${t.promoCodeId} is not null) and (${t.promoCodeId} is null or ${t.kind}::text in ('promo', 'top_up', 'promo_clawback'))`,
    ),
    index("credit_ledger_entry_promoCodeId_idx")
      .on(t.promoCodeId)
      .where(sql`${t.promoCodeId} is not null`),
    check(
      "credit_ledger_entry_topup_fee",
      sql`(${t.topupFeeBps} is null and ${t.topupFeeMinMicroSar} is null and ${t.topupFeeSource} is null) or (${t.kind} = 'top_up' and ${t.topupFeeBps} >= 0 and ${t.topupFeeMinMicroSar} >= 0 and ${t.topupFeeSource} in (${sql.raw(TOPUP_FEE_SOURCES.map((source) => `'${source}'`).join(", "))}))`,
    ),
  ],
);

/**
 * A cache of the ledger: balance is the sum of the organization's entries and
 * held is the sum of its open reservations. Both can be rebuilt.
 */
export const creditAccount = pgTable(
  "credit_account",
  {
    organizationId: text()
      .primaryKey()
      .references(() => organization.id, { onDelete: "cascade" }),
    balance: microSar().notNull().default(0),
    held: microSar().notNull().default(0),
    createdAt: timestamp().notNull().defaultNow(),
    updatedAt: timestamp()
      .notNull()
      .defaultNow()
      .$onUpdate(() => new Date()),
  },
  (t) => [check("credit_account_held_not_negative", sql`${t.held} >= 0`)],
);

/**
 * Where a credit lot came from (ADR 0019): a top-up, a promo code's credit, a
 * positive staff adjustment, or a transfer from a personal account (ADR 0020).
 */
export const CREDIT_LOT_SOURCES = [
  "top_up",
  "promo",
  "adjustment",
  // bought credit moved in from a member's personal account (ADR 0020): it
  // keeps the expiry of the lot it left and is never refundable
  "transfer",
] as const;
export type CreditLotSource = (typeof CREDIT_LOT_SOURCES)[number];

/**
 * One grant of credit and the terms it was granted on (BILL-17, BILL-10, ADR
 * 0019): written in the same transaction as its ledger entry, with the expiry
 * and refund window in force at that moment, so a later change of the
 * settings does not touch it. What is left of a lot is not stored: it is
 * worked out from the ledger (credits/lots.ts), so the gateway's settlement
 * stays one statement that knows nothing of lots. Append-only, except that
 * the expiry job sets expired_at once (trigger).
 */
export const creditLot = pgTable(
  "credit_lot",
  {
    id: text().primaryKey().$defaultFn(generateId),
    /** No foreign key, as for the ledger: the record outlives the organization. */
    organizationId: text().notNull(),
    /** The grant: a top_up, promo or positive adjustment entry. */
    entryId: text()
      .notNull()
      .unique()
      .references((): AnyPgColumn => creditLedgerEntry.id, {
        onDelete: "restrict",
      }),
    source: text().$type<CreditLotSource>().notNull(),
    grantedMicroSar: microSar().notNull(),
    /** From this moment what is left may be expired; null never expires. */
    expiresAt: timestamp(),
    /** Top-ups only: the self-serve refund is offered until then; null offers none. */
    refundableUntil: timestamp(),
    /** Set once, by the expiry job, when it closed the lot (with an expiry entry if anything was left). */
    expiredAt: timestamp(),
    createdAt: timestamp().notNull().defaultNow(),
  },
  (t) => [
    index("credit_lot_organizationId_createdAt_idx").on(
      t.organizationId,
      t.createdAt,
    ),
    // what the expiry job looks for every hour
    index("credit_lot_due_expiresAt_idx")
      .on(t.expiresAt)
      .where(sql`${t.expiredAt} is null and ${t.expiresAt} is not null`),
    check(
      "credit_lot_source",
      sql`${t.source} in (${sql.raw(CREDIT_LOT_SOURCES.map((source) => `'${source}'`).join(", "))})`,
    ),
    check("credit_lot_granted_positive", sql`${t.grantedMicroSar} > 0`),
    check(
      "credit_lot_refund_window",
      sql`${t.refundableUntil} is null or (${t.source} = 'top_up' and ${t.refundableUntil} > ${t.createdAt})`,
    ),
    check(
      "credit_lot_expiry",
      sql`(${t.expiresAt} is null or ${t.expiresAt} > ${t.createdAt}) and (${t.expiredAt} is null or ${t.expiresAt} is not null)`,
    ),
  ],
);

export const CREDIT_REFUND_STATUSES = [
  "pending",
  "succeeded",
  "failed",
] as const;
export type CreditRefundStatus = (typeof CREDIT_REFUND_STATUSES)[number];

/**
 * A self-serve refund of a top-up (BILL-10, ADR 0019): at most one per lot.
 * The money is in the ledger (a refund entry naming the lot, keyed by the
 * provider's refund id); this row is the request, so a double click or a
 * retry finds it and the provider gets the same idempotency key. While
 * pending, a hold (credit_reservation `refund:<id>`) keeps the credit from
 * being spent.
 */
export const creditRefund = pgTable(
  "credit_refund",
  {
    id: text().primaryKey().$defaultFn(generateId),
    organizationId: text().notNull(),
    creditLotId: text()
      .notNull()
      .unique()
      .references(() => creditLot.id, { onDelete: "restrict" }),
    /** The credit returned, in micro-SAR: whole halalas, never the fee. */
    amountMicroSar: microSar().notNull(),
    status: text().$type<CreditRefundStatus>().notNull().default("pending"),
    provider: text().notNull(),
    paymentId: text().notNull(),
    providerRefundId: text().unique(),
    /** Why the provider refused it, for staff. */
    failure: text(),
    requestedBy: text().notNull(),
    createdAt: timestamp().notNull().defaultNow(),
    updatedAt: timestamp()
      .notNull()
      .defaultNow()
      .$onUpdate(() => new Date()),
  },
  (t) => [
    index("credit_refund_organizationId_idx").on(t.organizationId),
    check(
      "credit_refund_amount_halalas",
      sql`${t.amountMicroSar} > 0 and ${t.amountMicroSar} % 10000 = 0`,
    ),
    check(
      "credit_refund_status",
      sql`${t.status} in (${sql.raw(CREDIT_REFUND_STATUSES.map((status) => `'${status}'`).join(", "))}) and (${t.status} <> 'succeeded' or ${t.providerRefundId} is not null)`,
    ),
  ],
);

/**
 * One move of credit from a user's personal account to an organization they
 * belong to (BILL-12, ADR 0020). The money is in the ledger: for each lot the
 * credit leaves, a `transfer_out` entry in the personal account and a
 * `transfer_in` entry of the same amount in the organization, both with this
 * row's id as their reference, all in one transaction. This row is the
 * request: the customer's idempotency key finds it again, so a retry moves
 * nothing twice. Append-only (trigger). No foreign keys, as for the ledger:
 * the record outlives both accounts.
 */
export const creditTransfer = pgTable(
  "credit_transfer",
  {
    id: text().primaryKey().$defaultFn(generateId),
    fromOrganizationId: text().notNull(),
    toOrganizationId: text().notNull(),
    /** micro-SAR, whole halalas. */
    amountMicroSar: microSar().notNull(),
    /** The user whose personal account it left. */
    requestedBy: text().notNull(),
    idempotencyKey: text().notNull().unique(),
    createdAt: timestamp().notNull().defaultNow(),
  },
  (t) => [
    index("credit_transfer_fromOrganizationId_idx").on(t.fromOrganizationId),
    // the cooldown reads an organization's latest transfer
    index("credit_transfer_toOrganizationId_createdAt_idx").on(
      t.toOrganizationId,
      t.createdAt,
    ),
    check(
      "credit_transfer_amount_halalas",
      sql`${t.amountMicroSar} > 0 and ${t.amountMicroSar} % 10000 = 0`,
    ),
    check(
      "credit_transfer_two_accounts",
      sql`${t.fromOrganizationId} <> ${t.toOrganizationId}`,
    ),
  ],
);

/** One row per in-flight request; released when settledAt is set. */
export const creditReservation = pgTable(
  "credit_reservation",
  {
    id: text().primaryKey().$defaultFn(generateId),
    organizationId: text()
      .notNull()
      .references(() => organization.id, { onDelete: "cascade" }),
    amount: microSar().notNull(),
    /** The gateway virtual key the hold is for, when the request came with one (ADR 0013). */
    virtualKeyId: text(),
    /**
     * The official USD to SAR rate × 1,000,000 and the bank's spread in bps
     * when the hold was taken, and the effective rate it was sized at; its
     * settlement converts the real cost at the same rate, whatever staff set
     * meanwhile.
     */
    usdToSarRateE6: integer("usd_to_sar_rate_e6").notNull(),
    fxSpreadBps: integer().notNull(),
    effectiveUsdToSarRateE6: integer("effective_usd_to_sar_rate_e6").notNull(),
    expiresAt: timestamp().notNull(),
    settledAt: timestamp(),
    createdAt: timestamp().notNull().defaultNow(),
  },
  (t) => [
    index("credit_reservation_open_expiresAt_idx")
      .on(t.expiresAt)
      .where(sql`${t.settledAt} is null`),
    index("credit_reservation_open_virtualKeyId_idx")
      .on(t.virtualKeyId)
      .where(sql`${t.settledAt} is null and ${t.virtualKeyId} is not null`),
    check("credit_reservation_amount_positive", sql`${t.amount} > 0`),
    check(
      "credit_reservation_usd_to_sar_rate_positive",
      sql`${t.usdToSarRateE6} > 0`,
    ),
    check(
      "credit_reservation_effective_rate",
      sql`${t.fxSpreadBps} between 0 and 10000 and ${effectiveRateRule(t.usdToSarRateE6, t.fxSpreadBps, t.effectiveUsdToSarRateE6)}`,
    ),
  ],
);

export const insertCreditLedgerEntrySchema =
  createInsertSchema(creditLedgerEntry);
export const selectCreditLedgerEntrySchema =
  createSelectSchema(creditLedgerEntry);
export const selectCreditAccountSchema = createSelectSchema(creditAccount);
export const selectCreditLotSchema = createSelectSchema(creditLot);
export const selectCreditRefundSchema = createSelectSchema(creditRefund);

export type InsertCreditLedgerEntry = z.infer<
  typeof insertCreditLedgerEntrySchema
>;
export type SelectCreditLedgerEntry = z.infer<
  typeof selectCreditLedgerEntrySchema
>;
export type SelectCreditAccount = z.infer<typeof selectCreditAccountSchema>;
export type SelectCreditLot = z.infer<typeof selectCreditLotSchema>;
export type SelectCreditRefund = z.infer<typeof selectCreditRefundSchema>;
export type CreditEntryKind = (typeof creditEntryKindEnum.enumValues)[number];

/**
 * When to email an organization that its credits are running out (step 10.2).
 * No row: the default threshold, alerts on. notifiedAt is set when the email
 * goes out and cleared once the balance is back above the threshold, so one
 * dip sends one email.
 */
export const lowBalanceAlert = pgTable(
  "low_balance_alert",
  {
    organizationId: text()
      .primaryKey()
      .references(() => organization.id, { onDelete: "cascade" }),
    /** micro-SAR; null turns the alert off. */
    thresholdMicroSar: microSar(),
    notifiedAt: timestamp(),
    updatedAt: timestamp()
      .notNull()
      .defaultNow()
      .$onUpdate(() => new Date()),
  },
  (t) => [
    check(
      "low_balance_alert_threshold_positive",
      sql`${t.thresholdMicroSar} is null or ${t.thresholdMicroSar} > 0`,
    ),
  ],
);
