import { eq } from "@workspace/db";
import { creditLedgerEntry, organization } from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { logger } from "@workspace/shared/logger";
import { HttpException } from "@workspace/shared/utils";

import {
  acceptsTestPayments,
  isTopUpAvailable,
  resolveTopUpGateway,
} from "../payments/gateway";
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

export const MICRO_USD_PER_CENT = 10_000;

const LINE_LABELS = { credits: "Credits", fee: "Payment processing fee" };

export interface TopUpQuote {
  creditMicroUsd: number;
  feeMicroUsd: number;
  totalMicroUsd: number;
}

/**
 * What the top-up form needs, all set by staff in the admin: the limits and
 * fee in the pricing settings (ADMIN-04, ADMIN-05), whether top-ups are
 * available in the payment settings (ADMIN-03).
 */
export interface TopUpOptions {
  /** False when top-ups are off or no payment provider can take them now. */
  isAvailable: boolean;
  /** The least and most credit one payment may buy, in micro-USD (SAR with ADMIN-06). */
  minMicroUsd: number;
  maxMicroUsd: number;
  /** The quick-pick amounts, smallest first, in micro-USD. */
  presetsMicroUsd: number[];
  /** The fee rates, so the form can say how the fee is worked out. */
  feeBps: number;
  feeMinMicroUsd: number;
}

export const getTopUpOptions = async (): Promise<TopUpOptions> => {
  const [settings, isAvailable] = await Promise.all([
    getPricingSettings(),
    isTopUpAvailable(),
  ]);
  return {
    isAvailable,
    minMicroUsd: settings.topupMinMicroUsd,
    maxMicroUsd: settings.topupMaxMicroUsd,
    presetsMicroUsd: settings.topupPresetsMicroUsd,
    feeBps: settings.topupFeeBps,
    feeMinMicroUsd: settings.topupFeeMinMicroUsd,
  };
};

/**
 * The fee as it stands now: what the checkout started next would charge.
 * Refuses an amount outside the limits staff set, read at the time of the
 * request, and refuses everything while top-ups are unavailable.
 */
export const quoteTopUp = async (
  creditMicroUsd: number,
): Promise<TopUpQuote> => {
  await resolveTopUpGateway();
  return quote(creditMicroUsd);
};

const quote = async (creditMicroUsd: number): Promise<TopUpQuote> => {
  const settings = await getPricingSettings();
  if (
    creditMicroUsd < settings.topupMinMicroUsd ||
    creditMicroUsd > settings.topupMaxMicroUsd
  ) {
    throw new HttpException(HttpStatusCode.BAD_REQUEST, {
      code: "billing:error.topUpAmount",
      message: `A top-up must be between ${settings.topupMinMicroUsd} and ${settings.topupMaxMicroUsd} micro-USD`,
    });
  }
  const feeMicroUsd = computeTopupFee(creditMicroUsd, settings);
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
  // the provider and mode staff chose, read now (ADMIN-03)
  const gateway = await resolveTopUpGateway();

  const [org] = await db
    .select({ slug: organization.slug })
    .from(organization)
    .where(eq(organization.id, organizationId));
  if (!org) {
    throw new HttpException(HttpStatusCode.NOT_FOUND);
  }

  const priced = await quote(creditMicroUsd);
  const page = new URL(`/dashboard/${org.slug}/credits`, origin);
  const back = (status: string) => {
    const url = new URL(page);
    url.searchParams.set("top_up", status);
    return url.toString();
  };

  try {
    const { url } = await gateway.checkout({
      mode: gateway.mode,
      user,
      organizationId,
      creditMicroUsd: priced.creditMicroUsd,
      feeMicroUsd: priced.feeMicroUsd,
      labels: LINE_LABELS,
      redirect: { success: back("success"), cancel: back("cancelled") },
    });
    return { url, ...priced };
  } catch (error) {
    logger.error(error);
    throw new HttpException(HttpStatusCode.BAD_GATEWAY, {
      code: "billing:error.checkout",
    });
  }
};

const topUpKey = (provider: string, paymentId: string) =>
  `top_up:${provider}:${paymentId}`;

/**
 * Writes what a verified webhook says to the ledger. Every delivery of the
 * same payment or refund lands on the same idempotency key, so it counts once.
 * Throwing makes the provider deliver the event again. The top-up switches do
 * not apply here: a payment already taken is always credited (ADMIN-03).
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
  if (event.mode === "test" && !(await acceptsTestPayments(event.provider))) {
    logger.warn(
      `Top-up ${event.paymentId} was paid in ${event.provider}'s test mode, which is not the mode in use: not credited`,
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
    idempotencyKey: topUpKey(event.provider, event.paymentId),
    reference: event.paymentId,
    metadata: {
      provider: event.provider,
      mode: event.mode,
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
    .where(
      eq(
        creditLedgerEntry.idempotencyKey,
        topUpKey(event.provider, event.paymentId),
      ),
    );

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
    idempotencyKey: `refund:${event.provider}:${event.refundId}`,
    reference: event.refundId,
    metadata: {
      provider: event.provider,
      mode: event.mode,
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
