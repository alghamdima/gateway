import {
  bigint,
  check,
  index,
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

import type * as z from "zod";

/**
 * Prepaid credits (docs/platform/adr/0001-credits-ledger.md).
 *
 * Every amount is micro-USD (1 USD = 1,000,000) in a Postgres bigint. It is
 * read as a JS number, which is exact up to about 9 billion USD.
 */
const microUsd = () => bigint({ mode: "number" });

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
    /** Signed: credits are positive, debits negative. */
    amount: microUsd().notNull(),
    kind: creditEntryKindEnum().notNull(),
    /** Payment id, refund id or request attempt: the same event never counts twice. */
    idempotencyKey: text().notNull().unique(),
    reference: text(),
    metadata: jsonb().$type<Record<string, unknown>>(),
    /** The gateway virtual key that spent it (usage only): per-key limits and reports (ADR 0013). */
    virtualKeyId: text(),
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
    balance: microUsd().notNull().default(0),
    held: microUsd().notNull().default(0),
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
    amount: microUsd().notNull(),
    /** The gateway virtual key the hold is for, when the request came with one (ADR 0013). */
    virtualKeyId: text(),
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
