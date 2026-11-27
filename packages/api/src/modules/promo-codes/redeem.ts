import { and, eq, sql } from "@workspace/db";
import {
  promoCode,
  promoCodeOrganization,
  promoCodeRedemption,
} from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { logger } from "@workspace/shared/logger";
import { generateId, HttpException } from "@workspace/shared/utils";

import { applyEntryIn } from "../credits/ledger";
import {
  checkPromoCode,
  computeTopUpEffect,
  isPromoCodeFormat,
  normalizePromoCode,
} from "./math";

import type { ApplyEntryInput, LedgerTransaction } from "../credits/ledger";
import type { PromoCodeTerms, PromoRefusal } from "./math";
import type { PromoCodeKind, SelectPromoCode } from "@workspace/db/schema";

/**
 * Using a promo code (BILL-08). A top-up asks for a quote with the code and
 * puts what it computed in the payment's metadata; the payment webhook then
 * records the use through creditTopUpWithPromo. Free credit is redeemed
 * directly. Every use locks the code's row first (SELECT ... FOR UPDATE), so
 * the total-uses and one-per-organization limits are checked and moved by one
 * transaction at a time; a partial unique index on the redemptions backs the
 * second.
 */

type Database = typeof db | LedgerTransaction;

/** A code applied to a top-up quote: what it changes, worked out on the server. */
export interface AppliedTopUpPromo {
  status: "applied";
  codeId: string;
  code: string;
  kind: PromoCodeKind;
  feeDiscountMicroSar: number;
  bonusMicroSar: number;
}

export interface RefusedPromo {
  status: "refused";
  code: string;
  reason: PromoRefusal;
}

export type TopUpPromoQuote = AppliedTopUpPromo | RefusedPromo;

/** The code-dependent facts the rules need about one organization. */
const organizationFacts = async (
  database: Database,
  codeId: string,
  organizationId: string,
) => {
  const [listed, redeemed] = await Promise.all([
    database
      .select({ id: promoCodeOrganization.promoCodeId })
      .from(promoCodeOrganization)
      .where(
        and(
          eq(promoCodeOrganization.promoCodeId, codeId),
          eq(promoCodeOrganization.organizationId, organizationId),
        ),
      )
      .limit(1),
    database
      .select({ id: promoCodeRedemption.id })
      .from(promoCodeRedemption)
      .where(
        and(
          eq(promoCodeRedemption.promoCodeId, codeId),
          eq(promoCodeRedemption.organizationId, organizationId),
        ),
      )
      .limit(1),
  ]);
  return { isListed: listed.length > 0, hasRedeemed: redeemed.length > 0 };
};

const findCode = async (
  database: Database,
  normalized: string,
  { lock }: { lock: boolean },
): Promise<SelectPromoCode | null> => {
  const query = database
    .select()
    .from(promoCode)
    .where(eq(promoCode.code, normalized));
  const [row] = lock ? await query.for("update") : await query;
  return row ?? null;
};

/**
 * What a code does to a top-up of `creditMicroSar` whose fee before any
 * discount is `baseFeeMicroSar`, or why it is refused. Read without a lock:
 * the quote and the checkout promise nothing the webhook does not re-check.
 */
export const quoteTopUpPromo = async ({
  organizationId,
  code: raw,
  creditMicroSar,
  baseFeeMicroSar,
  at = new Date(),
}: {
  organizationId: string;
  code: string;
  creditMicroSar: number;
  baseFeeMicroSar: number;
  at?: Date;
}): Promise<TopUpPromoQuote> => {
  const normalized = normalizePromoCode(raw);
  const refuse = (reason: PromoRefusal): RefusedPromo => ({
    status: "refused",
    code: normalized,
    reason,
  });
  if (!isPromoCodeFormat(normalized)) {
    return refuse("unknown");
  }
  const row = await findCode(db, normalized, { lock: false });
  if (!row) {
    return refuse("unknown");
  }
  const refusal = checkPromoCode({
    code: row,
    at,
    use: { type: "top_up", creditMicroSar },
    ...(await organizationFacts(db, row.id, organizationId)),
  });
  if (refusal) {
    return refuse(refusal);
  }
  const effect = computeTopUpEffect(row, creditMicroSar, baseFeeMicroSar);
  if ("refusal" in effect) {
    return refuse(effect.refusal);
  }
  return {
    status: "applied",
    codeId: row.id,
    code: row.code,
    kind: row.kind,
    ...effect,
  };
};

/** The refusal as the API answers it: translated by its code (billing.json). */
export const promoRefusalError = (reason: PromoRefusal) =>
  new HttpException(HttpStatusCode.BAD_REQUEST, {
    code: `billing:promo.refused.${reason}`,
  });

/** What a paid top-up says about the code it carried, as the checkout computed it. */
export interface PaidTopUpPromo {
  codeId: string;
  feeDiscountMicroSar: number;
  bonusMicroSar: number;
}

export interface CreditTopUpWithPromoInput {
  promo: PaidTopUpPromo;
  /** "<provider>:<payment id>": the redemption and the bonus entry are keyed by it. */
  paymentKey: string;
  /** The top-up entry as the webhook would write it without a code. */
  topUp: ApplyEntryInput;
}

/**
 * Credits a top-up paid with a code, records the code's use and adds its
 * bonus, in one transaction (BILL-08). What the checkout showed is honoured:
 * the discount is already in what was paid and the customer saw the bonus,
 * so a code paused, expired or used up since then still applies, and its use
 * is counted. The one exception is one use per organization: a second payment
 * by the same organization (two checkouts opened before either was paid) is
 * credited without the bonus and without a redemption, and logged; its fee
 * discount was already in the price and stays on its top-up entry.
 * A redelivered webhook finds the top-up applied and changes nothing.
 */
export const creditTopUpWithPromo = async ({
  promo,
  paymentKey,
  topUp,
}: CreditTopUpWithPromoInput) =>
  db.transaction(async (tx) => {
    // the code first, then the account (in applyEntryIn): the order every use takes
    const [code] = await tx
      .select()
      .from(promoCode)
      .where(eq(promoCode.id, promo.codeId))
      .for("update");
    const fitsCode =
      code !== undefined &&
      code.kind !== "free_credit" &&
      (promo.feeDiscountMicroSar === 0 || code.kind === "fee_discount") &&
      (promo.bonusMicroSar === 0 || code.kind === "topup_bonus");
    if (!fitsCode) {
      logger.error(
        `Top-up ${paymentKey} names promo code ${promo.codeId}, which does not exist or cannot give what it says: credited without it`,
      );
      return applyEntryIn(tx, topUp);
    }

    const applied = await applyEntryIn(tx, {
      ...topUp,
      promoCodeId: code.id,
      metadata: {
        ...topUp.metadata,
        promoCode: code.code,
        promoFeeDiscountMicroSar: promo.feeDiscountMicroSar,
        promoBonusMicroSar: promo.bonusMicroSar,
      },
    });
    if (!applied.isApplied) {
      return applied;
    }

    const { hasRedeemed } = await organizationFacts(
      tx,
      code.id,
      topUp.organizationId,
    );
    if (code.onePerOrganization && hasRedeemed) {
      logger.error(
        `Top-up ${paymentKey} used promo code ${code.code} again for ${topUp.organizationId}, which may use it once: credited without the bonus`,
      );
      return applied;
    }

    const bonus =
      promo.bonusMicroSar > 0
        ? await applyEntryIn(tx, {
            organizationId: topUp.organizationId,
            amount: promo.bonusMicroSar,
            kind: "promo",
            idempotencyKey: `promo:${paymentKey}`,
            reference: code.code,
            promoCodeId: code.id,
            metadata: { paymentId: paymentKey, kind: code.kind },
          })
        : null;
    await tx.insert(promoCodeRedemption).values({
      promoCodeId: code.id,
      organizationId: topUp.organizationId,
      kind: code.kind,
      onePerOrganization: code.onePerOrganization,
      paymentId: paymentKey,
      topupCreditMicroSar: topUp.amount,
      feeDiscountMicroSar: promo.feeDiscountMicroSar,
      creditMicroSar: promo.bonusMicroSar,
      ledgerEntryId: bonus?.entryId ?? null,
      topupEntryId: applied.entryId,
      idempotencyKey: `promo:${paymentKey}`,
    });
    await countUse(tx, code.id);
    return { ...applied, balance: bonus?.balance ?? applied.balance };
  });

const countUse = async (tx: LedgerTransaction, codeId: string) => {
  await tx
    .update(promoCode)
    .set({ uses: sql`${promoCode.uses} + 1` })
    .where(eq(promoCode.id, codeId));
};

export interface FreeCreditRedemption {
  code: string;
  creditMicroSar: number;
  balance: number;
}

/**
 * Redeems a free-credit code for the organization: checks every rule under
 * the code's row lock, then writes the redemption, the promo entry and the
 * use count in one transaction. Two redemptions at once wait for each other
 * on the lock, so the second sees the first's use.
 */
export const redeemFreeCredit = async ({
  organizationId,
  code: raw,
  userId,
  at = new Date(),
}: {
  organizationId: string;
  code: string;
  userId: string;
  at?: Date;
}): Promise<FreeCreditRedemption> => {
  const normalized = normalizePromoCode(raw);
  if (!isPromoCodeFormat(normalized)) {
    throw promoRefusalError("unknown");
  }
  try {
    return await db.transaction(async (tx) => {
      const code = await findCode(tx, normalized, { lock: true });
      if (!code) {
        throw promoRefusalError("unknown");
      }
      const refusal = checkPromoCode({
        code,
        at,
        use: { type: "free_credit" },
        ...(await organizationFacts(tx, code.id, organizationId)),
      });
      if (refusal) {
        throw promoRefusalError(refusal);
      }
      const creditMicroSar = grantedCredit(code);

      const redemptionId = generateId();
      const entry = await applyEntryIn(tx, {
        organizationId,
        amount: creditMicroSar,
        kind: "promo",
        idempotencyKey: `promo:free:${redemptionId}`,
        reference: code.code,
        promoCodeId: code.id,
        metadata: { kind: code.kind, redeemedBy: userId },
      });
      await tx.insert(promoCodeRedemption).values({
        id: redemptionId,
        promoCodeId: code.id,
        organizationId,
        kind: code.kind,
        onePerOrganization: code.onePerOrganization,
        creditMicroSar,
        ledgerEntryId: entry.entryId,
        idempotencyKey: `promo:free:${redemptionId}`,
        redeemedBy: userId,
      });
      await countUse(tx, code.id);
      logger.info(
        `🎟️ Promo code ${code.code}: granted ${creditMicroSar} micro-SAR to ${organizationId}`,
      );
      return { code: code.code, creditMicroSar, balance: entry.balance };
    });
  } catch (error) {
    // the index behind the lock: only reachable if the lock were bypassed
    if (
      isUniqueViolation(error, "promo_code_redemption_once_per_organization")
    ) {
      throw promoRefusalError("already_used");
    }
    throw error;
  }
};

const grantedCredit = (code: PromoCodeTerms) => {
  if (code.creditMicroSar === null || code.creditMicroSar <= 0) {
    throw new Error("A free-credit code has no amount");
  }
  return code.creditMicroSar;
};

const isUniqueViolation = (error: unknown, constraint: string): boolean => {
  if (typeof error !== "object" || error === null) {
    return false;
  }
  if (
    "code" in error &&
    error.code === "23505" &&
    "constraint_name" in error &&
    error.constraint_name === constraint
  ) {
    return true;
  }
  return "cause" in error && isUniqueViolation(error.cause, constraint);
};
