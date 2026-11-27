import { and, desc, eq, isNull, sql } from "@workspace/db";
import {
  creditAccount,
  creditLedgerEntry,
  creditReservation,
} from "@workspace/db/schema";
import { db } from "@workspace/db/server";

import type { CreditEntryKind, TopupFeeSource } from "@workspace/db/schema";

/** All amounts are micro-SAR (1 SAR = 1,000,000; docs/platform/adr/0017-sar-ledger.md). */
export { MICRO_SAR } from "@workspace/shared/utils";

export interface ApplyEntryInput {
  organizationId: string;
  /** Signed micro-SAR: credits are positive, debits negative. */
  amount: number;
  kind: CreditEntryKind;
  /** The same key applied again changes nothing. */
  idempotencyKey: string;
  reference?: string;
  metadata?: Record<string, unknown>;
  /** Top-up only: the fee rate the payment was quoted at and its source (ADMIN-24). */
  topupFeeBps?: number;
  topupFeeMinMicroSar?: number;
  topupFeeSource?: TopupFeeSource;
  /** Promo entries, and top-ups paid with a code: the code (BILL-08). */
  promoCodeId?: string;
}

export interface ApplyEntryResult {
  /** False when the key had already been applied. */
  isApplied: boolean;
  entryId: string;
  balance: number;
}

export interface Balance {
  balance: number;
  held: number;
  available: number;
}

/**
 * Appends one ledger entry and moves the cached balance by the same amount,
 * in one transaction. Replaying a key is a no-op; reusing a key for a
 * different organization, amount or kind is an error, because it means two
 * different events were given the same identity.
 */
export const applyEntry = async (
  input: ApplyEntryInput,
): Promise<ApplyEntryResult> =>
  db.transaction(async (tx) => applyEntryIn(tx, input));

/** A transaction of the control plane's database. */
export type LedgerTransaction = Parameters<
  Parameters<typeof db.transaction>[0]
>[0];

/**
 * applyEntry inside a transaction the caller holds, so the entry and what it
 * stands for (a promo code's redemption, BILL-08) commit or fail together.
 */
export const applyEntryIn = async (
  tx: LedgerTransaction,
  input: ApplyEntryInput,
): Promise<ApplyEntryResult> => {
  const [inserted] = await tx
    .insert(creditLedgerEntry)
    .values(input)
    .onConflictDoNothing({ target: creditLedgerEntry.idempotencyKey })
    .returning({ id: creditLedgerEntry.id });

  if (!inserted) {
    const [existing] = await tx
      .select()
      .from(creditLedgerEntry)
      .where(eq(creditLedgerEntry.idempotencyKey, input.idempotencyKey));

    if (
      !existing ||
      existing.organizationId !== input.organizationId ||
      existing.amount !== input.amount ||
      existing.kind !== input.kind
    ) {
      throw new Error(
        `Idempotency key "${input.idempotencyKey}" was already used for a different ledger entry`,
      );
    }

    const [account] = await tx
      .select({ balance: creditAccount.balance })
      .from(creditAccount)
      .where(eq(creditAccount.organizationId, input.organizationId));

    return {
      isApplied: false,
      entryId: existing.id,
      balance: account?.balance ?? 0,
    };
  }

  const [account] = await tx
    .insert(creditAccount)
    .values({ organizationId: input.organizationId, balance: input.amount })
    .onConflictDoUpdate({
      target: creditAccount.organizationId,
      set: {
        balance: sql`${creditAccount.balance} + ${input.amount}`,
        updatedAt: new Date(),
      },
    })
    .returning({ balance: creditAccount.balance });

  if (!account) {
    throw new Error("credit_account upsert returned no row");
  }

  return { isApplied: true, entryId: inserted.id, balance: account.balance };
};

export const getBalance = async (organizationId: string): Promise<Balance> => {
  const [account] = await db
    .select({ balance: creditAccount.balance, held: creditAccount.held })
    .from(creditAccount)
    .where(eq(creditAccount.organizationId, organizationId));

  const balance = account?.balance ?? 0;
  const held = account?.held ?? 0;

  return { balance, held, available: balance - held };
};

/**
 * Recomputes the cached account from the source of truth: balance from the
 * ledger entries, held from the open reservations. The account row is locked
 * before the sums are read, so an entry applied at the same time is either
 * already in the sum or lands on top of the rebuilt balance, never lost.
 */
export const rebuildBalance = async (
  organizationId: string,
): Promise<Balance> =>
  db.transaction(async (tx) => {
    await tx
      .insert(creditAccount)
      .values({ organizationId })
      .onConflictDoNothing({ target: creditAccount.organizationId });

    await tx
      .select({ organizationId: creditAccount.organizationId })
      .from(creditAccount)
      .where(eq(creditAccount.organizationId, organizationId))
      .for("update");

    const [entries] = await tx
      .select({
        total: sql<string>`coalesce(sum(${creditLedgerEntry.amount}), 0)`,
      })
      .from(creditLedgerEntry)
      .where(eq(creditLedgerEntry.organizationId, organizationId));

    const [reservations] = await tx
      .select({
        total: sql<string>`coalesce(sum(${creditReservation.amount}), 0)`,
      })
      .from(creditReservation)
      .where(
        and(
          eq(creditReservation.organizationId, organizationId),
          isNull(creditReservation.settledAt),
        ),
      );

    const balance = Number(entries?.total ?? 0);
    const held = Number(reservations?.total ?? 0);

    await tx
      .update(creditAccount)
      .set({ balance, held })
      .where(eq(creditAccount.organizationId, organizationId));

    return { balance, held, available: balance - held };
  });

export interface GetEntriesInput {
  organizationId: string;
  limit: number;
  /** Return entries created before this one (the id of the last entry of the previous page). */
  before?: string;
}

export const getEntries = async ({
  organizationId,
  limit,
  before,
}: GetEntriesInput) =>
  db
    .select({
      id: creditLedgerEntry.id,
      amount: creditLedgerEntry.amount,
      kind: creditLedgerEntry.kind,
      reference: creditLedgerEntry.reference,
      createdAt: creditLedgerEntry.createdAt,
    })
    .from(creditLedgerEntry)
    .where(
      and(
        eq(creditLedgerEntry.organizationId, organizationId),
        // the cursor is compared inside Postgres: a timestamp read into JS loses its microseconds
        before
          ? sql`(${creditLedgerEntry.createdAt}, ${creditLedgerEntry.id}) < (
              select c.created_at, c.id from ${creditLedgerEntry} c
              where c.id = ${before} and c.organization_id = ${organizationId}
            )`
          : undefined,
      ),
    )
    .orderBy(desc(creditLedgerEntry.createdAt), desc(creditLedgerEntry.id))
    .limit(limit);
