import { and, eq, inArray, sql } from "@workspace/db";
import { creditLedgerEntry, creditLot } from "@workspace/db/schema";
import { logger } from "@workspace/shared/logger";

import { topUpBonusKey } from "../promo-codes/redeem";
import { applyEntryIn } from "./ledger";
import { floorToHalala, getLotRemainders } from "./lots";

import type { LedgerTransaction } from "./ledger";
import type { db } from "@workspace/db/server";

/**
 * Taking back a refunded top-up's promo bonus (owner, 2026-11-27; ADR 0019,
 * "Refunds take back the unused promo bonus").
 *
 * A top-up paid with a bonus code has two lots: its credit (`top_up`) and its
 * bonus (`promo`, the entry keyed `promo:<provider>:<payment>`). When the
 * top-up is refunded, self-serve or in the provider's dashboard, the bonus
 * follows in the same transaction as the refund entry: a `promo_clawback`
 * entry naming the bonus lot and its code, keyed by the refund
 * (`promo_clawback:<provider>:<refund id>`), so one refund takes back once.
 *
 * How much: the bonus in proportion to the purchase's credit refunded so far,
 * rounded down to the halala (in the customer's favour), less what earlier
 * refunds of it took back, and never more than what is left of the bonus,
 * itself rounded down to the halala. Bonus already spent is not taken back.
 * A free-credit code has no top-up, so it is never touched.
 */

/** What a purchase's bonus is: its lot, size and code. */
export interface TopUpBonus {
  purchaseLotId: string;
  purchaseGrantedMicroSar: number;
  bonusLotId: string;
  bonusGrantedMicroSar: number;
  promoCodeId: string;
}

const clawbackKey = (provider: string, refundId: string) =>
  `promo_clawback:${provider}:${refundId}`;

/**
 * What a refund takes back of a bonus. `refundedCreditMicroSar` is all the
 * purchase's credit refunded so far, this refund included.
 */
export const bonusClawbackDue = ({
  purchaseGrantedMicroSar,
  bonusGrantedMicroSar,
  refundedCreditMicroSar,
  alreadyClawedBackMicroSar,
  bonusRemainingMicroSar,
}: {
  purchaseGrantedMicroSar: number;
  bonusGrantedMicroSar: number;
  refundedCreditMicroSar: number;
  alreadyClawedBackMicroSar: number;
  bonusRemainingMicroSar: number;
}) => {
  if (purchaseGrantedMicroSar <= 0 || refundedCreditMicroSar <= 0) {
    return 0;
  }
  const refunded = Math.min(refundedCreditMicroSar, purchaseGrantedMicroSar);
  // in bigint: bonus × refunded can pass 2^53 for large top-ups
  const share = Number(
    (BigInt(bonusGrantedMicroSar) * BigInt(refunded)) /
      BigInt(purchaseGrantedMicroSar),
  );
  const due = floorToHalala(share) - alreadyClawedBackMicroSar;
  return Math.max(
    0,
    Math.min(due, floorToHalala(Math.max(bonusRemainingMicroSar, 0))),
  );
};

/** The bonus of each of these purchase lots that came with one, by purchase lot id. */
export const bonusesOfPurchases = async (
  tx: LedgerTransaction | typeof db,
  purchaseLotIds: string[],
): Promise<Map<string, TopUpBonus>> => {
  const bonuses = new Map<string, TopUpBonus>();
  if (purchaseLotIds.length === 0) {
    return bonuses;
  }
  const purchases = await tx
    .select({
      lotId: creditLot.id,
      grantedMicroSar: creditLot.grantedMicroSar,
      reference: creditLedgerEntry.reference,
      metadata: creditLedgerEntry.metadata,
    })
    .from(creditLot)
    .innerJoin(creditLedgerEntry, eq(creditLedgerEntry.id, creditLot.entryId))
    .where(
      and(
        inArray(creditLot.id, purchaseLotIds),
        eq(creditLot.source, "top_up"),
        eq(creditLedgerEntry.kind, "top_up"),
      ),
    );
  const purchaseOfKey = new Map<
    string,
    { lotId: string; grantedMicroSar: number }
  >();
  for (const purchase of purchases) {
    const provider = purchase.metadata?.provider;
    if (typeof provider === "string" && purchase.reference) {
      purchaseOfKey.set(topUpBonusKey(provider, purchase.reference), purchase);
    }
  }
  if (purchaseOfKey.size === 0) {
    return bonuses;
  }
  const rows = await tx
    .select({
      key: creditLedgerEntry.idempotencyKey,
      promoCodeId: creditLedgerEntry.promoCodeId,
      lotId: creditLot.id,
      grantedMicroSar: creditLot.grantedMicroSar,
    })
    .from(creditLedgerEntry)
    .innerJoin(creditLot, eq(creditLot.entryId, creditLedgerEntry.id))
    .where(
      and(
        inArray(creditLedgerEntry.idempotencyKey, [...purchaseOfKey.keys()]),
        eq(creditLedgerEntry.kind, "promo"),
      ),
    );
  for (const row of rows) {
    const purchase = purchaseOfKey.get(row.key);
    if (purchase && row.promoCodeId) {
      bonuses.set(purchase.lotId, {
        purchaseLotId: purchase.lotId,
        purchaseGrantedMicroSar: purchase.grantedMicroSar,
        bonusLotId: row.lotId,
        bonusGrantedMicroSar: row.grantedMicroSar,
        promoCodeId: row.promoCodeId,
      });
    }
  }
  return bonuses;
};

/** Minus the sum of the entries of a kind that name a lot: what they removed from it. */
const removedFrom = async (
  tx: LedgerTransaction,
  lotId: string,
  kind: "refund" | "promo_clawback",
) => {
  const [row] = await tx
    .select({
      total: sql<string>`coalesce(-sum(${creditLedgerEntry.amount}), 0)`,
    })
    .from(creditLedgerEntry)
    .where(
      and(
        eq(creditLedgerEntry.creditLotId, lotId),
        eq(creditLedgerEntry.kind, kind),
      ),
    );
  return Number(row?.total ?? 0);
};

/**
 * What a refund of the purchase lot would take back of its bonus now, in the
 * caller's transaction (under the account's lock). `pendingRefundMicroSar` is
 * credit about to be refunded and not yet in the ledger (0 once it is).
 */
export const bonusClawbackIn = async (
  tx: LedgerTransaction,
  organizationId: string,
  bonus: TopUpBonus,
  pendingRefundMicroSar: number,
) => {
  const [refunded, clawedBack, { remaining }] = await Promise.all([
    removedFrom(tx, bonus.purchaseLotId, "refund"),
    removedFrom(tx, bonus.bonusLotId, "promo_clawback"),
    getLotRemainders(tx, organizationId),
  ]);
  return bonusClawbackDue({
    purchaseGrantedMicroSar: bonus.purchaseGrantedMicroSar,
    bonusGrantedMicroSar: bonus.bonusGrantedMicroSar,
    refundedCreditMicroSar: refunded + pendingRefundMicroSar,
    alreadyClawedBackMicroSar: clawedBack,
    bonusRemainingMicroSar: remaining.get(bonus.bonusLotId) ?? 0,
  });
};

/** What a refund of this purchase lot would take back of its bonus; 0 when it came with none. */
export const estimateBonusClawbackIn = async (
  tx: LedgerTransaction,
  organizationId: string,
  purchaseLotId: string,
  refundMicroSar: number,
) => {
  const bonus = (await bonusesOfPurchases(tx, [purchaseLotId])).get(
    purchaseLotId,
  );
  return bonus ? bonusClawbackIn(tx, organizationId, bonus, refundMicroSar) : 0;
};

/**
 * Takes back the unused bonus of a purchase whose refund entry this
 * transaction has just written, under the account's lock. Keyed by the
 * refund, so a redelivered webhook or a retry takes back nothing more.
 * Returns what was taken back.
 */
export const clawBackBonusIn = async (
  tx: LedgerTransaction,
  refund: {
    organizationId: string;
    purchaseLotId: string;
    provider: string;
    mode: string;
    paymentId: string;
    refundId: string;
  },
) => {
  const bonus = (await bonusesOfPurchases(tx, [refund.purchaseLotId])).get(
    refund.purchaseLotId,
  );
  if (!bonus) {
    return 0;
  }
  const key = clawbackKey(refund.provider, refund.refundId);
  const [seen] = await tx
    .select({ id: creditLedgerEntry.id })
    .from(creditLedgerEntry)
    .where(eq(creditLedgerEntry.idempotencyKey, key));
  if (seen) {
    return 0;
  }
  const amount = await bonusClawbackIn(tx, refund.organizationId, bonus, 0);
  if (amount === 0) {
    logger.info(
      `Refund ${refund.refundId}: the bonus of ${refund.paymentId} is spent or already taken back: nothing to take back`,
    );
    return 0;
  }
  await applyEntryIn(tx, {
    organizationId: refund.organizationId,
    amount: -amount,
    kind: "promo_clawback",
    idempotencyKey: key,
    reference: refund.refundId,
    promoCodeId: bonus.promoCodeId,
    creditLotId: bonus.bonusLotId,
    metadata: {
      provider: refund.provider,
      mode: refund.mode,
      paymentId: refund.paymentId,
      purchaseLotId: refund.purchaseLotId,
    },
  });
  logger.info(
    `↩️ Refund ${refund.refundId}: took back ${amount} micro-SAR of the promo bonus of ${refund.paymentId} from ${refund.organizationId}`,
  );
  return amount;
};
