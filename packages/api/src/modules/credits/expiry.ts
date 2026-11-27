import { and, asc, eq, isNull, lte, sql } from "@workspace/db";
import { creditAccount, creditLot } from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { logger } from "@workspace/shared/logger";

import { applyEntryIn } from "./ledger";
import { compareLots, getLotRemainders } from "./lots";

/**
 * Credit expiry (BILL-17, docs/platform/adr/0019-credit-lots-expiry-refunds.md).
 * Run every hour by /api/internal/credit-expiry. For each organization with a
 * lot past its expiry, in one short transaction: lock the account row (as the
 * gateway's settlement does, so no settlement runs in between and neither
 * update is lost), work out what is left of each lot from the ledger, write
 * one `expiry` entry per lot with something left and lower the balance by it,
 * and mark the lot expired. A lot can be spent until its expiry is written:
 * up to an hour past its date.
 *
 * Idempotent: a lot marked expired is not looked at again, and its entry's
 * key `expiry:<lot>` cannot be written twice. Credit held by requests in
 * flight is not expired from under them: a lot whose remainder is more than
 * the available balance waits for the next run.
 */

export interface CreditExpiryResult {
  organizations: number;
  /** Lots closed this run, with or without something left. */
  lotsClosed: number;
  /** Expiry entries written, and what they removed in micro-SAR. */
  entries: number;
  expiredMicroSar: number;
  /** Lots left for the next run, because requests in flight hold their credit. */
  deferred: number;
  /** Organizations whose expiry failed; the next run tries them again. */
  failed: number;
}

export const runCreditExpiryJob = async (): Promise<CreditExpiryResult> => {
  const due = await db
    .selectDistinct({ organizationId: creditLot.organizationId })
    .from(creditLot)
    .where(
      and(isNull(creditLot.expiredAt), lte(creditLot.expiresAt, sql`now()`)),
    );

  const result: CreditExpiryResult = {
    organizations: due.length,
    lotsClosed: 0,
    entries: 0,
    expiredMicroSar: 0,
    deferred: 0,
    failed: 0,
  };
  for (const { organizationId } of due) {
    try {
      const done = await expireOrganizationLots(organizationId);
      result.lotsClosed += done.lotsClosed;
      result.entries += done.entries;
      result.expiredMicroSar += done.expiredMicroSar;
      result.deferred += done.deferred;
    } catch (error) {
      result.failed += 1;
      logger.error(error, `Credit expiry failed for ${organizationId}`);
    }
  }
  if (result.entries > 0 || result.deferred > 0 || result.failed > 0) {
    logger.info(
      `⌛ Credit expiry: ${result.entries} lots expired (${result.expiredMicroSar} micro-SAR), ${result.lotsClosed} closed, ${result.deferred} deferred, ${result.failed} organizations failed`,
    );
  }
  return result;
};

/** Expires one organization's due lots, under its account's lock. */
export const expireOrganizationLots = async (organizationId: string) =>
  db.transaction(async (tx) => {
    // the account first: the gateway's settlement and the refunds take it too
    const [account] = await tx
      .select({ balance: creditAccount.balance, held: creditAccount.held })
      .from(creditAccount)
      .where(eq(creditAccount.organizationId, organizationId))
      .for("update");
    const lots = await tx
      .select()
      .from(creditLot)
      .where(
        and(
          eq(creditLot.organizationId, organizationId),
          isNull(creditLot.expiredAt),
          lte(creditLot.expiresAt, sql`now()`),
        ),
      )
      .orderBy(asc(creditLot.expiresAt), asc(creditLot.id));
    const done = { lotsClosed: 0, entries: 0, expiredMicroSar: 0, deferred: 0 };
    if (!account || lots.length === 0) {
      return done;
    }

    const { remaining } = await getLotRemainders(tx, organizationId);
    let available = account.balance - account.held;
    for (const lot of lots.sort(compareLots)) {
      const left = remaining.get(lot.id) ?? 0;
      if (left > available) {
        done.deferred += 1;
        continue;
      }
      if (left > 0) {
        const entry = await applyEntryIn(tx, {
          organizationId,
          amount: -left,
          kind: "expiry",
          idempotencyKey: `expiry:${lot.id}`,
          reference: lot.id,
          creditLotId: lot.id,
          metadata: {
            source: lot.source,
            grantedMicroSar: lot.grantedMicroSar,
            expiresAt: lot.expiresAt?.toISOString() ?? null,
          },
        });
        if (entry.isApplied) {
          available -= left;
          done.entries += 1;
          done.expiredMicroSar += left;
        }
      }
      await tx
        .update(creditLot)
        .set({ expiredAt: sql`now()` })
        .where(eq(creditLot.id, lot.id));
      done.lotsClosed += 1;
    }
    return done;
  });
