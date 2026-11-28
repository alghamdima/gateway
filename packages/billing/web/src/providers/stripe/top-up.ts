import { logger } from "@workspace/shared/logger";
import { MICRO_SAR_PER_HALALA } from "@workspace/shared/utils";

import {
  BillingProvider,
  CREDIT_REFUND_PURPOSE,
  PaymentMode,
  TOP_UP_PURPOSE,
} from "../types";
import { createOrRetrieveCustomer } from "./customer";
import { stripe } from "./sdk";

import type {
  TopUpCheckoutInput,
  TopUpCreditRefund,
  TopUpEvent,
  TopUpRefundInput,
  TopUpRefundResult,
} from "../types";
import type { Stripe } from "stripe";

/**
 * Top-ups are charged in SAR. Stripe takes SAR in its minor unit, the halala
 * (2 decimals: SAR 20.00 is 2000), and a micro-SAR amount is a whole number of
 * halalas when it divides by 10,000.
 */
const CURRENCY = "sar";

/**
 * A one-time payment for credits plus the fee (docs/platform/adr/0012-top-up-checkout.md).
 * Both lines are inline prices, because the fee is a setting staff change at any time.
 */
export const createTopUpCheckout = async ({
  mode,
  user,
  organizationId,
  creditMicroSar,
  feeMicroSar,
  feeRate,
  promo,
  labels,
  redirect,
}: TopUpCheckoutInput) => {
  const { stripeCustomer } = await createOrRetrieveCustomer({
    referenceId: organizationId,
    email: user.email,
    mode,
  });

  // the mode is also on the event (livemode); kept here so the payment shows it
  const metadata = {
    purpose: TOP_UP_PURPOSE,
    provider: BillingProvider.STRIPE,
    mode,
    organizationId,
    creditMicroSar: String(creditMicroSar),
    feeMicroSar: String(feeMicroSar),
    feeBps: String(feeRate.feeBps),
    feeMinMicroSar: String(feeRate.feeMinMicroSar),
    feeSource: feeRate.feeSource,
    ...(promo && {
      promoCodeId: promo.codeId,
      promoFeeDiscountMicroSar: String(promo.feeDiscountMicroSar),
      promoBonusMicroSar: String(promo.bonusMicroSar),
    }),
  };
  const line = (name: string, microSar: number) => ({
    quantity: 1,
    price_data: {
      currency: CURRENCY,
      unit_amount: toHalalas(microSar),
      product_data: { name },
    },
  });

  const session = await stripe(mode).checkout.sessions.create({
    mode: "payment",
    customer: stripeCustomer.id,
    client_reference_id: organizationId,
    line_items: [
      line(labels.credits, creditMicroSar),
      ...(feeMicroSar > 0 ? [line(labels.fee, feeMicroSar)] : []),
    ],
    metadata,
    payment_intent_data: { metadata },
    invoice_creation: { enabled: true, invoice_data: { metadata } },
    success_url: redirect.success,
    cancel_url: redirect.cancel,
  });

  return { url: session.url };
};

/**
 * Stripe's errors that mean it said no for good: the refund request was wrong
 * (the payment cannot be refunded, or not by that much) or the card refused.
 * Anything else (a connection error, Stripe's own 5xx, a rate limit) is
 * thrown, and the request is tried again with the same idempotency key.
 */
const REFUSED_ERRORS = new Set([
  "StripeInvalidRequestError",
  "StripeCardError",
]);

/**
 * A self-serve refund of part of a top-up (BILL-10, ADR 0019): the credit
 * only, in halalas, never the fee. The idempotency key makes a double click
 * or a retry return the same refund; the metadata lets the refund's webhook
 * remove exactly that credit, if it arrives before our own write.
 */
export const refundTopUp = async ({
  mode,
  paymentId,
  creditRefund,
  idempotencyKey,
}: TopUpRefundInput): Promise<TopUpRefundResult> => {
  try {
    const refund = await stripe(mode).refunds.create(
      {
        payment_intent: paymentId,
        amount: toHalalas(creditRefund.creditMicroSar),
        reason: "requested_by_customer",
        metadata: {
          purpose: CREDIT_REFUND_PURPOSE,
          creditRefundId: creditRefund.creditRefundId,
          creditMicroSar: String(creditRefund.creditMicroSar),
        },
      },
      { idempotencyKey },
    );
    switch (refund.status) {
      case "succeeded":
        return { outcome: "succeeded", refundId: refund.id };
      case "failed":
      case "canceled":
        return {
          outcome: "failed",
          refundId: refund.id,
          reason: refund.failure_reason ?? refund.status,
        };
      default:
        // pending or requires_action: its webhook says how it ends
        return { outcome: "pending", refundId: refund.id };
    }
  } catch (error) {
    if (isRefusal(error)) {
      return { outcome: "refused", reason: error.message };
    }
    throw error;
  }
};

const isRefusal = (
  error: unknown,
): error is { type: string; message: string } =>
  typeof error === "object" &&
  error !== null &&
  "type" in error &&
  typeof error.type === "string" &&
  REFUSED_ERRORS.has(error.type) &&
  "message" in error &&
  typeof error.message === "string";

/** The mode an event belongs to: Stripe marks every event live or not. */
export const modeOfEvent = (event: { livemode: boolean }) =>
  event.livemode ? PaymentMode.LIVE : PaymentMode.TEST;

/**
 * The top-up facts a verified webhook event carries: none when the event is
 * not about a top-up payment or a finished refund. A refund can arrive as
 * refund.created, refund.updated and charge.refunded; each names the refund,
 * and the ledger keys it by the refund's id, so it counts once whichever come.
 * charge.refunded does not carry its refunds unless expanded, so they are
 * listed from Stripe.
 */
export const toTopUpEvents = async (
  event: Stripe.Event,
): Promise<TopUpEvent[]> => {
  const mode = modeOfEvent(event);
  const source = { provider: BillingProvider.STRIPE, mode };
  switch (event.type) {
    case "checkout.session.completed":
    case "checkout.session.async_payment_succeeded": {
      const paid = toPaid(event.data.object);
      return paid ? [{ ...source, ...paid }] : [];
    }
    case "refund.created":
    case "refund.updated":
    case "refund.failed": {
      const refunded = toRefunded(event.data.object);
      return refunded ? [{ ...source, ...refunded }] : [];
    }
    case "charge.refunded": {
      const charge = event.data.object;
      const refunds =
        charge.refunds?.data ??
        (await stripe(mode).refunds.list({ charge: charge.id, limit: 100 }))
          .data;
      return refunds.flatMap((refund) => {
        const refunded = toRefunded(refund);
        return refunded ? [{ ...source, ...refunded }] : [];
      });
    }
    default:
      return [];
  }
};

type WithoutSource<T> = T extends unknown
  ? Omit<T, "provider" | "mode">
  : never;
type TopUpFact = WithoutSource<TopUpEvent>;

export const isTopUpSession = (session: Stripe.Checkout.Session) =>
  session.metadata?.purpose === TOP_UP_PURPOSE;

const toPaid = (session: Stripe.Checkout.Session): TopUpFact | null => {
  // with delayed payment methods the session completes unpaid, and
  // async_payment_succeeded follows once the money arrives
  if (!isTopUpSession(session) || session.payment_status !== "paid") {
    return null;
  }

  const paymentId = idOf(session.payment_intent);
  const organizationId = session.metadata?.organizationId;
  const creditMicroSar = toWholeNumber(session.metadata?.creditMicroSar);
  const feeMicroSar = toWholeNumber(session.metadata?.feeMicroSar);

  if (
    !paymentId ||
    !organizationId ||
    creditMicroSar === null ||
    feeMicroSar === null ||
    session.amount_total === null ||
    session.currency !== CURRENCY
  ) {
    logger.error(
      `Top-up session ${session.id} is missing what it needs to be credited`,
    );
    return null;
  }

  return {
    type: "paid",
    paymentId,
    organizationId,
    creditMicroSar,
    feeMicroSar,
    feeRate: toFeeRate(session.metadata),
    promo: toPromo(session.metadata),
    collectedMicroSar: session.amount_total * MICRO_SAR_PER_HALALA,
  };
};

/** The fee rate the checkout carried, or null when a session started before it did. */
const toFeeRate = (metadata: Stripe.Metadata | null) => {
  const feeBps = toWholeNumber(metadata?.feeBps);
  const feeMinMicroSar = toWholeNumber(metadata?.feeMinMicroSar);
  const feeSource = metadata?.feeSource;
  return feeBps === null || feeMinMicroSar === null || !feeSource
    ? null
    : { feeBps, feeMinMicroSar, feeSource };
};

/** The promo code the checkout carried (BILL-08), or null when it carried none or an incomplete one. */
const toPromo = (metadata: Stripe.Metadata | null) => {
  const codeId = metadata?.promoCodeId;
  const feeDiscountMicroSar = toWholeNumber(metadata?.promoFeeDiscountMicroSar);
  const bonusMicroSar = toWholeNumber(metadata?.promoBonusMicroSar);
  if (!codeId) {
    return null;
  }
  if (feeDiscountMicroSar === null || bonusMicroSar === null) {
    logger.error(`A top-up names promo code ${codeId} without its amounts`);
    return null;
  }
  return { codeId, feeDiscountMicroSar, bonusMicroSar };
};

/**
 * A refund that has ended: succeeded, or (for a self-serve refund, whose hold
 * must be released) failed or cancelled. A refund still pending is not a fact yet.
 */
const toRefunded = (refund: Stripe.Refund): TopUpFact | null => {
  const paymentId = idOf(refund.payment_intent);
  const creditRefund = toCreditRefund(refund.metadata);
  const status =
    refund.status === "succeeded"
      ? "succeeded"
      : refund.status === "failed" || refund.status === "canceled"
        ? "failed"
        : null;

  if (
    !status ||
    (status === "failed" && !creditRefund) ||
    !paymentId ||
    refund.currency !== CURRENCY
  ) {
    return null;
  }

  return {
    type: "refunded",
    refundId: refund.id,
    paymentId,
    refundedMicroSar: refund.amount * MICRO_SAR_PER_HALALA,
    status,
    creditRefund,
  };
};

/** What a self-serve refund carried (refundTopUp), or null for one made elsewhere. */
const toCreditRefund = (
  metadata: Stripe.Metadata | null,
): TopUpCreditRefund | null => {
  if (metadata?.purpose !== CREDIT_REFUND_PURPOSE) {
    return null;
  }
  const creditRefundId = metadata.creditRefundId;
  const creditMicroSar = toWholeNumber(metadata.creditMicroSar);
  if (!creditRefundId || creditMicroSar === null) {
    logger.error("A credits refund carries incomplete metadata");
    return null;
  }
  return { creditRefundId, creditMicroSar };
};

const toHalalas = (microSar: number) => {
  if (
    !Number.isSafeInteger(microSar) ||
    microSar % MICRO_SAR_PER_HALALA !== 0
  ) {
    throw new Error(`${microSar} micro-SAR is not a whole number of halalas`);
  }
  return microSar / MICRO_SAR_PER_HALALA;
};

const toWholeNumber = (value: string | undefined) =>
  value !== undefined &&
  /^\d+$/.test(value) &&
  Number.isSafeInteger(Number(value))
    ? Number(value)
    : null;

const idOf = (value: string | { id: string } | null) =>
  typeof value === "string" ? value : (value?.id ?? null);
