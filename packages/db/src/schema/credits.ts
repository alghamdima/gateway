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
]);

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
     * Top-up only: the fee rate the payment was quoted at and where it came
     * from (ADMIN-24), so a later change of the default or the organization's
     * fee does not rewrite what this payment paid. Null on top-ups started
     * before migration 0035. The fee amount is in metadata.feeMicroSar.
     */
    topupFeeBps: integer(),
    topupFeeMinMicroSar: bigint({ mode: "number" }),
    topupFeeSource: text().$type<TopupFeeSource>(),
    createdAt: timestamp().notNull().defaultNow(),
  },
  (t) => [
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

export type InsertCreditLedgerEntry = z.infer<
  typeof insertCreditLedgerEntrySchema
>;
export type SelectCreditLedgerEntry = z.infer<
  typeof selectCreditLedgerEntrySchema
>;
export type SelectCreditAccount = z.infer<typeof selectCreditAccountSchema>;
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
