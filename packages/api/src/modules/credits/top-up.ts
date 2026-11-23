import { provider, topUp } from "@workspace/billing-web/server";
import { eq } from "@workspace/db";
import { creditLedgerEntry, organization } from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { logger } from "@workspace/shared/logger";
import { HttpException } from "@workspace/shared/utils";

import { computeTopupFee } from "../pricing/fee";
import { getPricingSettings } from "../pricing/pricing";
import { applyEntry } from "./ledger";

import type { TopUpEvent } from "@workspace/billing-web/server";
import type { WebhookCallbacks } from "@workspace/billing-web/server";

/**
 * Buying credits (docs/platform/adr/0012-top-up-checkout.md). The checkout is
 * started here; the credits are written only when the verified payment webhook
 * arrives, never on the redirect back.
 */

/** Limits per payment, in micro-USD (confirmed by the owner, ADR 0012). */
export const TOP_UP_MIN_MICRO_USD = 5_000_000;
export const TOP_UP_MAX_MICRO_USD = 1_000_000_000;
export const MICRO_USD_PER_CENT = 10_000;

const LINE_LABELS = { credits: "Credits", fee: "Payment processing fee" };

export interface TopUpQuote {
  creditMicroUsd: number;
  feeMicroUsd: number;
  totalMicroUsd: number;
}

/** The fee as it stands now: what the checkout started next would charge. */
export const quoteTopUp = async (
  creditMicroUsd: number,
): Promise<TopUpQuote> => {
  const feeMicroUsd = computeTopupFee(
    creditMicroUsd,
    await getPricingSettings(),
  );
  return {
    creditMicroUsd,
    feeMicroUsd,
    totalMicroUsd: creditMicroUsd + feeMicroUsd,
  };
};

export const startTopUp = async ({
  user,
  organizationId,
  creditMicroUsd,
  origin,
}: {
  user: { id: string; email: string };
  organizationId: string;
  creditMicroUsd: number;
  /** The site the customer is on; the payment page sends them back there. */
  origin: string;
}) => {
  if (!topUp) {
    throw new HttpException(HttpStatusCode.NOT_IMPLEMENTED, {
      code: "billing:error.checkout",
    });
  }

  const [org] = await db
    .select({ slug: organization.slug })
    .from(organization)
    .where(eq(organization.id, organizationId));
  if (!org) {
    throw new HttpException(HttpStatusCode.NOT_FOUND);
  }

  const quote = await quoteTopUp(creditMicroUsd);
  const page = new URL(`/dashboard/${org.slug}/credits`, origin);
  const back = (status: string) => {
    const url = new URL(page);
    url.searchParams.set("top_up", status);
    return url.toString();
  };

  try {
    const { url } = await topUp.checkout({
      user,
      organizationId,
      creditMicroUsd: quote.creditMicroUsd,
      feeMicroUsd: quote.feeMicroUsd,
      labels: LINE_LABELS,
      redirect: { success: back("success"), cancel: back("cancelled") },
    });
    return { url, ...quote };
  } catch (error) {
    logger.error(error);
    throw new HttpException(HttpStatusCode.BAD_GATEWAY, {
      code: "billing:error.checkout",
    });
  }
};

const topUpKey = (paymentId: string) => `top_up:${provider}:${paymentId}`;

/**
 * Writes what a verified webhook says to the ledger. Every delivery of the
 * same payment or refund lands on the same idempotency key, so it counts once.
 * Throwing makes the provider deliver the event again.
 */
export const applyTopUpEvent = async (event: TopUpEvent) => {
  if (event.type === "paid") {
    return applyPayment(event);
  }
  return applyRefund(event);
};

/** What the payment webhook route passes to the provider's handler. */
export const topUpWebhookCallbacks: WebhookCallbacks = {
  onTopUpEvent: async (event) => {
    await applyTopUpEvent(event);
  },
};

const applyPayment = async (event: Extract<TopUpEvent, { type: "paid" }>) => {
  if (event.collectedMicroUsd !== event.creditMicroUsd + event.feeMicroUsd) {
    // only a bug or an edit in the provider's dashboard gets here: staff
    // resolve it with an adjustment rather than us guessing the credit
    logger.error(
      `Top-up ${event.paymentId} collected ${event.collectedMicroUsd} micro-USD, expected ${event.creditMicroUsd + event.feeMicroUsd}: not credited`,
    );
    return { isApplied: false };
  }
  if (!(await organizationExists(event.organizationId))) {
    logger.error(
      `Top-up ${event.paymentId} is for organization ${event.organizationId}, which no longer exists: not credited`,
    );
    return { isApplied: false };
  }

  const result = await applyEntry({
    organizationId: event.organizationId,
    amount: event.creditMicroUsd,
    kind: "top_up",
    idempotencyKey: topUpKey(event.paymentId),
    reference: event.paymentId,
    metadata: {
      provider,
      feeMicroUsd: event.feeMicroUsd,
      collectedMicroUsd: event.collectedMicroUsd,
    },
  });
  logger.info(
    `💳 Top-up ${event.paymentId}: ${result.isApplied ? "credited" : "already credited"} ${event.creditMicroUsd} micro-USD to ${event.organizationId}`,
  );
  return { isApplied: result.isApplied };
};

const applyRefund = async (
  event: Extract<TopUpEvent, { type: "refunded" }>,
) => {
  const [payment] = await db
    .select()
    .from(creditLedgerEntry)
    .where(eq(creditLedgerEntry.idempotencyKey, topUpKey(event.paymentId)));

  if (!payment) {
    // a refund of a plan payment, or of a top-up that was never credited
    logger.info(
      `Refund ${event.refundId} is not for a credited top-up: ignored`,
    );
    return { isApplied: false };
  }

  const removed = refundedCredit({
    creditMicroUsd: payment.amount,
    collectedMicroUsd: Number(payment.metadata?.collectedMicroUsd),
    refundedMicroUsd: event.refundedMicroUsd,
  });
  if (removed === 0) {
    return { isApplied: false };
  }
  if (!(await organizationExists(payment.organizationId))) {
    logger.error(
      `Refund ${event.refundId} is for organization ${payment.organizationId}, which no longer exists: not applied`,
    );
    return { isApplied: false };
  }

  const result = await applyEntry({
    organizationId: payment.organizationId,
    amount: -removed,
    kind: "refund",
    idempotencyKey: `refund:${provider}:${event.refundId}`,
    reference: event.refundId,
    metadata: {
      provider,
      paymentId: event.paymentId,
      refundedMicroUsd: event.refundedMicroUsd,
    },
  });
  logger.info(
    `↩️ Refund ${event.refundId}: ${result.isApplied ? "removed" : "already removed"} ${removed} micro-USD from ${payment.organizationId}`,
  );
  return { isApplied: result.isApplied };
};

/**
 * The refunded share of the payment, taken from the credit it bought:
 * floor(credit × refunded / collected). A full refund removes exactly the
 * credit; the fee's share is never removed as credit.
 */
export const refundedCredit = ({
  creditMicroUsd,
  collectedMicroUsd,
  refundedMicroUsd,
}: {
  creditMicroUsd: number;
  collectedMicroUsd: number;
  refundedMicroUsd: number;
}) => {
  if (
    !Number.isSafeInteger(collectedMicroUsd) ||
    collectedMicroUsd <= 0 ||
    !Number.isSafeInteger(refundedMicroUsd) ||
    refundedMicroUsd <= 0
  ) {
    throw new Error("A refund needs a positive payment and refund amount");
  }
  const refunded = Math.min(refundedMicroUsd, collectedMicroUsd);
  return Number(
    (BigInt(creditMicroUsd) * BigInt(refunded)) / BigInt(collectedMicroUsd),
  );
};

const organizationExists = async (organizationId: string) => {
  const [row] = await db
    .select({ id: organization.id })
    .from(organization)
    .where(eq(organization.id, organizationId));
  return row !== undefined;
};
