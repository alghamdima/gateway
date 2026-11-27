import { eq } from "@workspace/db";
import {
  creditLedgerEntry,
  organization,
  TOPUP_FEE_SOURCES,
} from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { logger } from "@workspace/shared/logger";
import { formatMicroSar, HttpException } from "@workspace/shared/utils";

import {
  acceptsTestPayments,
  isTopUpAvailable,
  resolveTopUpGateway,
} from "../payments/gateway";
import { computeTopupFee } from "../pricing/fee";
import { getEffectiveTopupFee } from "../pricing/organization-fee";
import { getPricingSettings } from "../pricing/pricing";
import { applyEntry } from "./ledger";

import type { EffectiveTopupFee } from "../pricing/fee";
import type { TopUpEvent } from "@workspace/billing-web/server";
import type { WebhookCallbacks } from "@workspace/billing-web/server";
import type { TopupFeeSource } from "@workspace/db/schema";

/**
 * Buying credits (docs/platform/adr/0012-top-up-checkout.md). The checkout is
 * started here; the credits are written only when the verified payment webhook
 * arrives, never on the redirect back.
 */

export { MICRO_SAR_PER_HALALA } from "@workspace/shared/utils";

const LINE_LABELS = { credits: "Credits", fee: "Payment processing fee" };

export interface TopUpQuote {
  creditMicroSar: number;
  feeMicroSar: number;
  totalMicroSar: number;
}

/**
 * What the top-up form needs, all set by staff in the admin: the limits and
 * fee in the pricing settings (ADMIN-04, ADMIN-05), whether top-ups are
 * available in the payment settings (ADMIN-03).
 */
export interface TopUpOptions {
  /** False when top-ups are off or no payment provider can take them now. */
  isAvailable: boolean;
  /** The least and most credit one payment may buy, in micro-SAR. */
  minMicroSar: number;
  maxMicroSar: number;
  /** The quick-pick amounts, smallest first, in micro-SAR. */
  presetsMicroSar: number[];
  /**
   * The fee rates this organization pays now, so the form can say how the
   * fee is worked out: its own fee while valid, else the default (ADMIN-24).
   */
  feeBps: number;
  feeMinMicroSar: number;
}

export const getTopUpOptions = async (
  organizationId: string,
): Promise<TopUpOptions> => {
  const [settings, fee, isAvailable] = await Promise.all([
    getPricingSettings(),
    getEffectiveTopupFee(organizationId),
    isTopUpAvailable(),
  ]);
  return {
    isAvailable,
    minMicroSar: settings.topupMinMicroSar,
    maxMicroSar: settings.topupMaxMicroSar,
    presetsMicroSar: settings.topupPresetsMicroSar,
    feeBps: fee.topupFeeBps,
    feeMinMicroSar: fee.topupFeeMinMicroSar,
  };
};

/**
 * The fee as it stands now: what the checkout started next would charge.
 * Refuses an amount outside the limits staff set, read at the time of the
 * request, and refuses everything while top-ups are unavailable.
 */
export const quoteTopUp = async (
  organizationId: string,
  creditMicroSar: number,
): Promise<TopUpQuote> => {
  await resolveTopUpGateway();
  // the customer sees the amounts; the rate behind them is for the payment record
  const priced = await quote(organizationId, creditMicroSar);
  return {
    creditMicroSar: priced.creditMicroSar,
    feeMicroSar: priced.feeMicroSar,
    totalMicroSar: priced.totalMicroSar,
  };
};

/** The quote, and the fee rate it used, which only the payment record needs. */
const quote = async (
  organizationId: string,
  creditMicroSar: number,
): Promise<TopUpQuote & { fee: EffectiveTopupFee }> => {
  const [settings, fee] = await Promise.all([
    getPricingSettings(),
    getEffectiveTopupFee(organizationId),
  ]);
  if (
    creditMicroSar < settings.topupMinMicroSar ||
    creditMicroSar > settings.topupMaxMicroSar
  ) {
    throw new HttpException(HttpStatusCode.BAD_REQUEST, {
      code: "billing:error.topUpAmount",
      message: `A top-up must be between ${formatMicroSar(settings.topupMinMicroSar)} and ${formatMicroSar(settings.topupMaxMicroSar)}`,
    });
  }
  const feeMicroSar = computeTopupFee(creditMicroSar, fee);
  return {
    creditMicroSar,
    feeMicroSar,
    totalMicroSar: creditMicroSar + feeMicroSar,
    fee,
  };
};

export const startTopUp = async ({
  user,
  organizationId,
  creditMicroSar,
  origin,
}: {
  user: { id: string; email: string };
  organizationId: string;
  creditMicroSar: number;
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

  const { fee, ...priced } = await quote(organizationId, creditMicroSar);
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
      creditMicroSar: priced.creditMicroSar,
      feeMicroSar: priced.feeMicroSar,
      feeRate: {
        feeBps: fee.topupFeeBps,
        feeMinMicroSar: fee.topupFeeMinMicroSar,
        feeSource: fee.source,
      },
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
  if (event.collectedMicroSar !== event.creditMicroSar + event.feeMicroSar) {
    // only a bug or an edit in the provider's dashboard gets here: staff
    // resolve it with an adjustment rather than us guessing the credit
    logger.error(
      `Top-up ${event.paymentId} collected ${event.collectedMicroSar} micro-SAR, expected ${event.creditMicroSar + event.feeMicroSar}: not credited`,
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
    amount: event.creditMicroSar,
    kind: "top_up",
    idempotencyKey: topUpKey(event.provider, event.paymentId),
    reference: event.paymentId,
    metadata: {
      provider: event.provider,
      mode: event.mode,
      feeMicroSar: event.feeMicroSar,
      collectedMicroSar: event.collectedMicroSar,
    },
    ...recordedFeeRate(event),
  });
  logger.info(
    `💳 Top-up ${event.paymentId}: ${result.isApplied ? "credited" : "already credited"} ${event.creditMicroSar} micro-SAR to ${event.organizationId}`,
  );
  return { isApplied: result.isApplied };
};

/**
 * The fee rate the payment was quoted at, as ledger columns (ADMIN-24). A
 * payment that does not carry a known rate is still credited, without it: the
 * money was taken.
 */
const recordedFeeRate = (event: Extract<TopUpEvent, { type: "paid" }>) => {
  const rate = event.feeRate;
  if (!rate) {
    return {};
  }
  if (!isTopupFeeSource(rate.feeSource)) {
    logger.error(
      `Top-up ${event.paymentId} names an unknown fee source "${rate.feeSource}": its fee rate is not recorded`,
    );
    return {};
  }
  return {
    topupFeeBps: rate.feeBps,
    topupFeeMinMicroSar: rate.feeMinMicroSar,
    topupFeeSource: rate.feeSource,
  };
};

const isTopupFeeSource = (value: string): value is TopupFeeSource =>
  TOPUP_FEE_SOURCES.some((source) => source === value);

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
    creditMicroSar: payment.amount,
    collectedMicroSar: Number(payment.metadata?.collectedMicroSar),
    refundedMicroSar: event.refundedMicroSar,
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
      refundedMicroSar: event.refundedMicroSar,
    },
  });
  logger.info(
    `↩️ Refund ${event.refundId}: ${result.isApplied ? "removed" : "already removed"} ${removed} micro-SAR from ${payment.organizationId}`,
  );
  return { isApplied: result.isApplied };
};

/**
 * The refunded share of the payment, taken from the credit it bought:
 * floor(credit × refunded / collected). A full refund removes exactly the
 * credit; the fee's share is never removed as credit.
 */
export const refundedCredit = ({
  creditMicroSar,
  collectedMicroSar,
  refundedMicroSar,
}: {
  creditMicroSar: number;
  collectedMicroSar: number;
  refundedMicroSar: number;
}) => {
  if (
    !Number.isSafeInteger(collectedMicroSar) ||
    collectedMicroSar <= 0 ||
    !Number.isSafeInteger(refundedMicroSar) ||
    refundedMicroSar <= 0
  ) {
    throw new Error("A refund needs a positive payment and refund amount");
  }
  const refunded = Math.min(refundedMicroSar, collectedMicroSar);
  return Number(
    (BigInt(creditMicroSar) * BigInt(refunded)) / BigInt(collectedMicroSar),
  );
};

const organizationExists = async (organizationId: string) => {
  const [row] = await db
    .select({ id: organization.id })
    .from(organization)
    .where(eq(organization.id, organizationId));
  return row !== undefined;
};
