import { getBillingProvider } from "@workspace/billing-web/server";
import { and, eq, isNull, sql } from "@workspace/db";
import {
  creditAccount,
  creditLedgerEntry,
  creditLot,
  creditRefund,
  creditReservation,
  pricingSettings,
} from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { logger } from "@workspace/shared/logger";
import { HttpException } from "@workspace/shared/utils";

import { clawBackBonusIn, estimateBonusClawbackIn } from "./clawback";
import { applyEntryIn } from "./ledger";
import { floorToHalala, getLotRemainders } from "./lots";

import type { LedgerTransaction } from "./ledger";
import type {
  PaymentMode,
  TopUpCreditRefund,
  TopUpEvent,
} from "@workspace/billing-web/server";

/**
 * Refunds (BILL-10, docs/platform/adr/0019-credit-lots-expiry-refunds.md).
 *
 * Self-serve: an organization admin refunds a top-up from the credits page
 * while its window is open. It returns the unused part of that purchase's
 * credit (its lot's remainder, down to the halala), never the fee and never a
 * promo bonus (a separate lot). The unused part of the bonus that came with
 * the top-up is taken back in the same transaction as the refund entry
 * (clawback.ts), in proportion to the credit refunded. The request is written and the credit held
 * first; the provider is called outside any transaction, with an idempotency
 * key per request; the refund entry is written when the provider says it
 * succeeded, by whichever of our call and its webhook gets there first, under
 * the same key (`refund:<provider>:<refund id>`), so it counts once.
 *
 * In the provider's dashboard: staff may refund any part of a payment. The
 * ledger removes the money refunded as credit, up to the purchase's credit
 * less what earlier refunds of it removed, matched by the payment id: the fee
 * is the last part of a payment to be refunded, and a refund of it removes
 * nothing. Each refund counts once, keyed by its id, and takes back its share
 * of the purchase's unused bonus with it.
 */

/** How long a request's hold lasts before the reaper frees it, if we stop half way. */
const HOLD_MINUTES = 15;
/** A refund the provider reports pending keeps its hold until its webhook says how it ended. */
const PENDING_HOLD_DAYS = 14;

const holdId = (creditRefundId: string) => `refund:${creditRefundId}`;
const refundKey = (provider: string, refundId: string) =>
  `refund:${provider}:${refundId}`;
const providerIdempotencyKey = (creditRefundId: string) =>
  `credit-refund:${creditRefundId}`;

/** Why a refund is refused; each is billing:error.refund.<reason>. */
export const REFUND_REFUSALS = [
  "notFound",
  "notRefundable",
  "refundsOff",
  "windowClosed",
  "alreadyRefunded",
  "failed",
  "nothingLeft",
  "creditInUse",
  "providerUnavailable",
] as const;
export type RefundRefusal = (typeof REFUND_REFUSALS)[number];

const STATUS_OF: Record<RefundRefusal, HttpStatusCode> = {
  notFound: HttpStatusCode.NOT_FOUND,
  notRefundable: HttpStatusCode.CONFLICT,
  refundsOff: HttpStatusCode.CONFLICT,
  windowClosed: HttpStatusCode.CONFLICT,
  alreadyRefunded: HttpStatusCode.CONFLICT,
  failed: HttpStatusCode.CONFLICT,
  nothingLeft: HttpStatusCode.CONFLICT,
  creditInUse: HttpStatusCode.CONFLICT,
  providerUnavailable: HttpStatusCode.BAD_GATEWAY,
};

export const refundRefusal = (reason: RefundRefusal) =>
  new HttpException(STATUS_OF[reason], {
    code: `billing:error.refund.${reason}`,
  });

export interface RefundResult {
  /** pending: the provider accepted it and will confirm; the credit stays held until then. */
  status: "succeeded" | "pending";
  amountMicroSar: number;
}

/** What the top-up entry says about where its payment was taken. */
interface PaymentOf {
  provider: string;
  mode: PaymentMode;
  paymentId: string;
}

/**
 * Refunds the unused credit of one top-up (BILL-10). Idempotent: a second
 * click while the first is in flight, or a retry after the provider could not
 * be reached, finds the same request and sends the provider the same key.
 */
export const requestRefund = async ({
  organizationId,
  lotId,
  userId,
}: {
  organizationId: string;
  lotId: string;
  userId: string;
}): Promise<RefundResult> => {
  const prepared = await db.transaction((tx) =>
    prepareRefund(tx, { organizationId, lotId, userId }),
  );
  const refund = getBillingProvider(prepared.payment.provider)?.topUp?.refund;
  if (!refund) {
    throw refundRefusal("notRefundable");
  }

  const creditRefund: TopUpCreditRefund = {
    creditRefundId: prepared.requestId,
    creditMicroSar: prepared.amountMicroSar,
  };
  let result: Awaited<ReturnType<typeof refund>>;
  try {
    result = await refund({
      mode: prepared.payment.mode,
      paymentId: prepared.payment.paymentId,
      creditRefund,
      idempotencyKey: providerIdempotencyKey(prepared.requestId),
    });
  } catch (error) {
    // the request stays pending: a retry sends the same key, and if the
    // provider did refund, its webhook completes it
    logger.error(
      error,
      `Refund ${prepared.requestId} could not reach the provider`,
    );
    throw refundRefusal("providerUnavailable");
  }

  const event = {
    provider: prepared.payment.provider,
    mode: prepared.payment.mode,
    paymentId: prepared.payment.paymentId,
    creditRefund,
  };
  switch (result.outcome) {
    case "succeeded":
      await completeSelfServeRefund({
        ...event,
        refundId: result.refundId,
        refundedMicroSar: prepared.amountMicroSar,
      });
      return { status: "succeeded", amountMicroSar: prepared.amountMicroSar };
    case "pending":
      await markPending(prepared.requestId, result.refundId);
      return { status: "pending", amountMicroSar: prepared.amountMicroSar };
    case "failed":
    case "refused":
      await failRefund(prepared.requestId, result.reason);
      throw refundRefusal("failed");
  }
};

/** Checks every rule under the account's lock, then writes the request and holds its credit. */
const prepareRefund = async (
  tx: LedgerTransaction,
  {
    organizationId,
    lotId,
    userId,
  }: { organizationId: string; lotId: string; userId: string },
) => {
  // the account first, as the gateway's settlement and the expiry job take it
  const account = await lockAccount(tx, organizationId);
  const [lot] = await tx
    .select({
      id: creditLot.id,
      source: creditLot.source,
      entryId: creditLot.entryId,
      isInWindow: sql<boolean>`${creditLot.refundableUntil} is not null and ${creditLot.refundableUntil} > now()`,
    })
    .from(creditLot)
    .where(
      and(
        eq(creditLot.id, lotId),
        eq(creditLot.organizationId, organizationId),
      ),
    );
  if (!lot || !account) {
    throw refundRefusal("notFound");
  }
  if (lot.source !== "top_up") {
    throw refundRefusal("notRefundable");
  }
  const payment = await paymentOfEntry(tx, lot.entryId);
  if (!payment) {
    throw refundRefusal("notRefundable");
  }

  const [existing] = await tx
    .select()
    .from(creditRefund)
    .where(eq(creditRefund.creditLotId, lot.id));
  if (existing?.status === "succeeded") {
    throw refundRefusal("alreadyRefunded");
  }
  if (existing?.status === "failed") {
    throw refundRefusal("failed");
  }
  if (existing) {
    // a retry: the same amount and key; the hold again if the reaper freed it and the credit is there
    const clawback = await estimateBonusClawbackIn(
      tx,
      organizationId,
      lot.id,
      existing.amountMicroSar,
    );
    await holdCredit(
      tx,
      existing.id,
      organizationId,
      existing.amountMicroSar + clawback,
      { ifAvailable: true },
    );
    return {
      requestId: existing.id,
      amountMicroSar: existing.amountMicroSar,
      payment,
    };
  }

  const [dashboardRefund] = await tx
    .select({ id: creditLedgerEntry.id })
    .from(creditLedgerEntry)
    .where(
      and(
        eq(creditLedgerEntry.creditLotId, lot.id),
        eq(creditLedgerEntry.kind, "refund"),
      ),
    )
    .limit(1);
  if (dashboardRefund) {
    throw refundRefusal("alreadyRefunded");
  }

  const [settings] = await tx
    .select({ refundWindowHours: pricingSettings.refundWindowHours })
    .from(pricingSettings)
    .where(eq(pricingSettings.id, 1));
  // 0 switches every self-serve refund off at once (ADMIN-23); a window
  // already given stays as it was when it was bought
  if (!settings || settings.refundWindowHours === 0) {
    throw refundRefusal("refundsOff");
  }
  if (!lot.isInWindow) {
    throw refundRefusal("windowClosed");
  }

  const { remaining } = await getLotRemainders(tx, organizationId);
  const amountMicroSar = floorToHalala(remaining.get(lot.id) ?? 0);
  if (amountMicroSar <= 0) {
    throw refundRefusal("nothingLeft");
  }
  // the unused bonus that goes with it is held too, so requests cannot spend it meanwhile
  const clawbackMicroSar = await estimateBonusClawbackIn(
    tx,
    organizationId,
    lot.id,
    amountMicroSar,
  );
  if (amountMicroSar + clawbackMicroSar > account.balance - account.held) {
    // part of it is held by requests in flight; they settle within minutes
    throw refundRefusal("creditInUse");
  }

  const [request] = await tx
    .insert(creditRefund)
    .values({
      organizationId,
      creditLotId: lot.id,
      amountMicroSar,
      provider: payment.provider,
      paymentId: payment.paymentId,
      requestedBy: userId,
    })
    .returning({ id: creditRefund.id });
  if (!request) {
    throw new Error("credit_refund insert returned no row");
  }
  await holdCredit(
    tx,
    request.id,
    organizationId,
    amountMicroSar + clawbackMicroSar,
    { ifAvailable: false },
  );
  return { requestId: request.id, amountMicroSar, payment };
};

const lockAccount = async (tx: LedgerTransaction, organizationId: string) => {
  const [account] = await tx
    .select({ balance: creditAccount.balance, held: creditAccount.held })
    .from(creditAccount)
    .where(eq(creditAccount.organizationId, organizationId))
    .for("update");
  return account;
};

/** The provider, mode and payment id a top-up entry recorded (top-up.ts). */
const paymentOfEntry = async (
  tx: LedgerTransaction,
  entryId: string,
): Promise<PaymentOf | null> => {
  const [entry] = await tx
    .select({
      kind: creditLedgerEntry.kind,
      reference: creditLedgerEntry.reference,
      metadata: creditLedgerEntry.metadata,
    })
    .from(creditLedgerEntry)
    .where(eq(creditLedgerEntry.id, entryId));
  const provider = entry?.metadata?.provider;
  const mode = entry?.metadata?.mode;
  if (
    entry?.kind !== "top_up" ||
    !entry.reference ||
    typeof provider !== "string" ||
    (mode !== "test" && mode !== "live")
  ) {
    return null;
  }
  return { provider, mode, paymentId: entry.reference };
};

/**
 * Holds the credit being refunded, and the bonus it takes back, the way a request's hold does
 * (credit_reservation, id `refund:<request>`), so it cannot be spent while the
 * provider works, and the gateway's reaper frees it if we never finish. The
 * rate columns, which a refund does not use, carry the rate in force.
 */
const holdCredit = async (
  tx: LedgerTransaction,
  creditRefundId: string,
  organizationId: string,
  amountMicroSar: number,
  { ifAvailable }: { ifAvailable: boolean },
) => {
  const id = holdId(creditRefundId);
  const [open] = await tx
    .select({ id: creditReservation.id })
    .from(creditReservation)
    .where(
      and(eq(creditReservation.id, id), isNull(creditReservation.settledAt)),
    );
  if (open) {
    return;
  }
  const held = await tx
    .update(creditAccount)
    .set({ held: sql`${creditAccount.held} + ${amountMicroSar}` })
    .where(
      and(
        eq(creditAccount.organizationId, organizationId),
        sql`${creditAccount.balance} - ${creditAccount.held} >= ${amountMicroSar}`,
      ),
    )
    .returning({ held: creditAccount.held });
  if (held.length === 0) {
    if (ifAvailable) {
      return;
    }
    throw refundRefusal("creditInUse");
  }
  // a retry finds its earlier hold reaped: it takes a new one under the same id
  await tx.execute(sql`
    INSERT INTO ${creditReservation} (id, organization_id, amount, expires_at,
                                      usd_to_sar_rate_e6, fx_spread_bps, effective_usd_to_sar_rate_e6)
    SELECT ${id}, ${organizationId}, ${amountMicroSar}, now() + make_interval(mins => ${HOLD_MINUTES}),
           s.usd_to_sar_rate_e6, s.fx_spread_bps,
           (s.usd_to_sar_rate_e6::bigint * (10000 + s.fx_spread_bps) + 9999) / 10000
      FROM ${pricingSettings} s WHERE s.id = 1
    ON CONFLICT (id) DO UPDATE
       SET amount = excluded.amount, settled_at = NULL, expires_at = excluded.expires_at`);
};

/** Frees a request's hold if it is still open, in the caller's transaction; returns what was freed. */
const releaseHold = async (tx: LedgerTransaction, creditRefundId: string) => {
  // the reservation first, then the account: the gateway's order
  const [released] = await tx
    .update(creditReservation)
    .set({ settledAt: sql`now()` })
    .where(
      and(
        eq(creditReservation.id, holdId(creditRefundId)),
        isNull(creditReservation.settledAt),
      ),
    )
    .returning({
      organizationId: creditReservation.organizationId,
      amount: creditReservation.amount,
    });
  if (released) {
    await tx
      .update(creditAccount)
      .set({ held: sql`${creditAccount.held} - ${released.amount}` })
      .where(eq(creditAccount.organizationId, released.organizationId));
  }
  return released?.amount ?? 0;
};

const markPending = async (creditRefundId: string, providerRefundId: string) =>
  db.transaction(async (tx) => {
    await tx
      .update(creditRefund)
      .set({ providerRefundId })
      .where(
        and(
          eq(creditRefund.id, creditRefundId),
          eq(creditRefund.status, "pending"),
        ),
      );
    await tx
      .update(creditReservation)
      .set({
        expiresAt: sql`now() + make_interval(days => ${PENDING_HOLD_DAYS})`,
      })
      .where(
        and(
          eq(creditReservation.id, holdId(creditRefundId)),
          isNull(creditReservation.settledAt),
        ),
      );
  });

/** The provider refused or the refund failed: the request ends and its credit is free again. */
const failRefund = async (creditRefundId: string, reason: string) =>
  db.transaction(async (tx) => {
    const [request] = await tx
      .update(creditRefund)
      .set({ status: "failed", failure: reason.slice(0, 500) })
      .where(
        and(
          eq(creditRefund.id, creditRefundId),
          eq(creditRefund.status, "pending"),
        ),
      )
      .returning({ id: creditRefund.id });
    if (request) {
      await releaseHold(tx, creditRefundId);
      logger.warn(`↩️ Refund ${creditRefundId} failed: ${reason}`);
    }
  });

interface SelfServeRefundEvent extends PaymentOf {
  refundId: string;
  refundedMicroSar: number;
  creditRefund: TopUpCreditRefund;
}

/**
 * Writes a self-serve refund that succeeded, once: frees its hold, removes
 * exactly the credit it carried from its lot, takes back the unused part of
 * the top-up's bonus, and marks the request. Called by
 * the request itself and by the refund's webhook, whichever comes first.
 */
const completeSelfServeRefund = async (event: SelfServeRefundEvent) =>
  db.transaction(async (tx) => {
    const [request] = await tx
      .select()
      .from(creditRefund)
      .where(eq(creditRefund.id, event.creditRefund.creditRefundId));
    if (!request) {
      return false;
    }
    if (request.amountMicroSar !== event.creditRefund.creditMicroSar) {
      // only a forged or edited refund gets here: staff look at it
      logger.error(
        `Refund ${event.refundId} says ${event.creditRefund.creditMicroSar} micro-SAR, its request ${request.id} ${request.amountMicroSar}: not applied`,
      );
      return true;
    }
    const released = await releaseHold(tx, request.id);
    await lockAccount(tx, request.organizationId);
    const applied = await applyEntryIn(tx, {
      organizationId: request.organizationId,
      amount: -request.amountMicroSar,
      kind: "refund",
      idempotencyKey: refundKey(event.provider, event.refundId),
      reference: event.refundId,
      creditLotId: request.creditLotId,
      metadata: {
        provider: event.provider,
        mode: event.mode,
        paymentId: event.paymentId,
        refundedMicroSar: event.refundedMicroSar,
        creditRefundId: request.id,
        selfServe: true,
      },
    });
    const clawedBack = applied.isApplied
      ? await clawBackBonusIn(tx, {
          organizationId: request.organizationId,
          purchaseLotId: request.creditLotId,
          provider: event.provider,
          mode: event.mode,
          paymentId: event.paymentId,
          refundId: event.refundId,
        })
      : 0;
    await tx
      .update(creditRefund)
      .set({
        status: "succeeded",
        providerRefundId: event.refundId,
        failure: null,
      })
      .where(eq(creditRefund.id, request.id));
    logger.info(
      `↩️ Refund ${event.refundId}: ${applied.isApplied ? "removed" : "already removed"} ${request.amountMicroSar} micro-SAR from ${request.organizationId}${clawedBack ? ` and ${clawedBack} of its bonus` : ""}${released ? ", freed its hold" : ""}`,
    );
    return true;
  });

/** Where a payment's credit landed: its top-up entry and lot. */
const topUpOfPayment = async (
  tx: LedgerTransaction,
  provider: string,
  paymentId: string,
) => {
  const [row] = await tx
    .select({
      organizationId: creditLedgerEntry.organizationId,
      lotId: creditLot.id,
      grantedMicroSar: creditLot.grantedMicroSar,
    })
    .from(creditLedgerEntry)
    .innerJoin(creditLot, eq(creditLot.entryId, creditLedgerEntry.id))
    .where(
      eq(creditLedgerEntry.idempotencyKey, `top_up:${provider}:${paymentId}`),
    );
  return row;
};

/**
 * What the ledger removes for a refund made in the provider's dashboard: the
 * money refunded, up to what is left of the purchase's credit after earlier
 * refunds of it. The fee is refunded last and removes nothing.
 */
export const dashboardRefundCredit = ({
  grantedMicroSar,
  alreadyRefundedMicroSar,
  refundedMicroSar,
}: {
  grantedMicroSar: number;
  alreadyRefundedMicroSar: number;
  refundedMicroSar: number;
}) => {
  if (!Number.isSafeInteger(refundedMicroSar) || refundedMicroSar <= 0) {
    throw new Error("A refund needs a positive amount");
  }
  return Math.max(
    0,
    Math.min(refundedMicroSar, grantedMicroSar - alreadyRefundedMicroSar),
  );
};

/**
 * Applies what a verified refund webhook says (top-up.ts). A self-serve
 * refund is completed or failed by its request; a refund made in the
 * provider's dashboard removes the money refunded, matched by the payment id,
 * once per refund id. Every write happens under the account's lock, so two
 * refunds of one payment are counted one after the other.
 */
export const applyProviderRefund = async (
  event: Extract<TopUpEvent, { type: "refunded" }>,
): Promise<{ isApplied: boolean }> => {
  if (event.creditRefund) {
    if (event.status === "failed") {
      await failRefund(
        event.creditRefund.creditRefundId,
        `refund ${event.refundId} ${event.status}`,
      );
      return { isApplied: false };
    }
    const isOurs = await completeSelfServeRefund({
      provider: event.provider,
      mode: event.mode,
      paymentId: event.paymentId,
      refundId: event.refundId,
      refundedMicroSar: event.refundedMicroSar,
      creditRefund: event.creditRefund,
    });
    if (isOurs) {
      return { isApplied: true };
    }
    logger.error(
      `Refund ${event.refundId} names request ${event.creditRefund.creditRefundId}, which does not exist: counted as a dashboard refund`,
    );
  }
  if (event.status !== "succeeded") {
    return { isApplied: false };
  }

  return db.transaction(async (tx) => {
    const topUp = await topUpOfPayment(tx, event.provider, event.paymentId);
    if (!topUp) {
      // a refund of a plan payment, or of a top-up that was never credited
      logger.info(
        `Refund ${event.refundId} is not for a credited top-up: ignored`,
      );
      return { isApplied: false };
    }
    const account = await lockAccount(tx, topUp.organizationId);
    if (!account) {
      logger.error(
        `Refund ${event.refundId} is for organization ${topUp.organizationId}, which has no account: not applied`,
      );
      return { isApplied: false };
    }
    const key = refundKey(event.provider, event.refundId);
    const [seen] = await tx
      .select({ id: creditLedgerEntry.id })
      .from(creditLedgerEntry)
      .where(eq(creditLedgerEntry.idempotencyKey, key));
    if (seen) {
      return { isApplied: false };
    }
    const [earlier] = await tx
      .select({
        total: sql<string>`coalesce(-sum(${creditLedgerEntry.amount}), 0)`,
      })
      .from(creditLedgerEntry)
      .where(
        and(
          eq(creditLedgerEntry.creditLotId, topUp.lotId),
          eq(creditLedgerEntry.kind, "refund"),
        ),
      );
    const removed = dashboardRefundCredit({
      grantedMicroSar: topUp.grantedMicroSar,
      alreadyRefundedMicroSar: Number(earlier?.total ?? 0),
      refundedMicroSar: event.refundedMicroSar,
    });
    if (removed === 0) {
      logger.info(
        `Refund ${event.refundId} returns only the fee: no credit removed`,
      );
      return { isApplied: false };
    }
    await applyEntryIn(tx, {
      organizationId: topUp.organizationId,
      amount: -removed,
      kind: "refund",
      idempotencyKey: key,
      reference: event.refundId,
      creditLotId: topUp.lotId,
      metadata: {
        provider: event.provider,
        mode: event.mode,
        paymentId: event.paymentId,
        refundedMicroSar: event.refundedMicroSar,
        selfServe: false,
      },
    });
    await clawBackBonusIn(tx, {
      organizationId: topUp.organizationId,
      purchaseLotId: topUp.lotId,
      provider: event.provider,
      mode: event.mode,
      paymentId: event.paymentId,
      refundId: event.refundId,
    });
    logger.info(
      `↩️ Refund ${event.refundId}: removed ${removed} micro-SAR from ${topUp.organizationId}`,
    );
    return { isApplied: true };
  });
};
