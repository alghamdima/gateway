import { logger } from "@workspace/shared/logger";

import { TOP_UP_PURPOSE } from "../types";
import { createOrRetrieveCustomer } from "./customer";
import { stripe } from "./sdk";

import type { TopUpCheckoutInput, TopUpEvent } from "../types";
import type { Stripe } from "stripe";

const MICRO_USD_PER_CENT = 10_000;

/**
 * A one-time payment for credits plus the fee (docs/platform/adr/0012-top-up-checkout.md).
 * Both lines are inline prices, because the fee is a setting staff change at any time.
 */
export const createTopUpCheckout = async ({
  user,
  organizationId,
  creditMicroUsd,
  feeMicroUsd,
  labels,
  redirect,
}: TopUpCheckoutInput) => {
  const { stripeCustomer } = await createOrRetrieveCustomer({
    referenceId: organizationId,
    email: user.email,
  });

  const metadata = {
    purpose: TOP_UP_PURPOSE,
    organizationId,
    creditMicroUsd: String(creditMicroUsd),
    feeMicroUsd: String(feeMicroUsd),
  };
  const line = (name: string, microUsd: number) => ({
    quantity: 1,
    price_data: {
      currency: "usd",
      unit_amount: toCents(microUsd),
      product_data: { name },
    },
  });

  const session = await stripe().checkout.sessions.create({
    mode: "payment",
    customer: stripeCustomer.id,
    client_reference_id: organizationId,
    line_items: [
      line(labels.credits, creditMicroUsd),
      ...(feeMicroUsd > 0 ? [line(labels.fee, feeMicroUsd)] : []),
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
 * The top-up fact a verified webhook event carries, or null when the event is
 * not about a top-up payment or a finished refund.
 */
export const toTopUpEvent = (event: Stripe.Event): TopUpEvent | null => {
  switch (event.type) {
    case "checkout.session.completed":
    case "checkout.session.async_payment_succeeded":
      return toPaid(event.data.object);
    case "refund.created":
    case "refund.updated":
      return toRefunded(event.data.object);
    default:
      return null;
  }
};

export const isTopUpSession = (session: Stripe.Checkout.Session) =>
  session.metadata?.purpose === TOP_UP_PURPOSE;

const toPaid = (session: Stripe.Checkout.Session): TopUpEvent | null => {
  // with delayed payment methods the session completes unpaid, and
  // async_payment_succeeded follows once the money arrives
  if (!isTopUpSession(session) || session.payment_status !== "paid") {
    return null;
  }

  const paymentId = idOf(session.payment_intent);
  const organizationId = session.metadata?.organizationId;
  const creditMicroUsd = toWholeNumber(session.metadata?.creditMicroUsd);
  const feeMicroUsd = toWholeNumber(session.metadata?.feeMicroUsd);

  if (
    !paymentId ||
    !organizationId ||
    creditMicroUsd === null ||
    feeMicroUsd === null ||
    session.amount_total === null ||
    session.currency !== "usd"
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
    creditMicroUsd,
    feeMicroUsd,
    collectedMicroUsd: session.amount_total * MICRO_USD_PER_CENT,
  };
};

const toRefunded = (refund: Stripe.Refund): TopUpEvent | null => {
  const paymentId = idOf(refund.payment_intent);

  if (
    refund.status !== "succeeded" ||
    !paymentId ||
    refund.currency !== "usd"
  ) {
    return null;
  }

  return {
    type: "refunded",
    refundId: refund.id,
    paymentId,
    refundedMicroUsd: refund.amount * MICRO_USD_PER_CENT,
  };
};

const toCents = (microUsd: number) => {
  if (!Number.isSafeInteger(microUsd) || microUsd % MICRO_USD_PER_CENT !== 0) {
    throw new Error(`${microUsd} micro-USD is not a whole number of cents`);
  }
  return microUsd / MICRO_USD_PER_CENT;
};

const toWholeNumber = (value: string | undefined) =>
  value !== undefined &&
  /^\d+$/.test(value) &&
  Number.isSafeInteger(Number(value))
    ? Number(value)
    : null;

const idOf = (value: string | { id: string } | null) =>
  typeof value === "string" ? value : (value?.id ?? null);
