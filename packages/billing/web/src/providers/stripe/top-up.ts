import { logger } from "@workspace/shared/logger";
import { MICRO_SAR_PER_HALALA } from "@workspace/shared/utils";

import { BillingProvider, PaymentMode, TOP_UP_PURPOSE } from "../types";
import { createOrRetrieveCustomer } from "./customer";
import { stripe } from "./sdk";

import type { TopUpCheckoutInput, TopUpEvent } from "../types";
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

/** The mode an event belongs to: Stripe marks every event live or not. */
export const modeOfEvent = (event: { livemode: boolean }) =>
  event.livemode ? PaymentMode.LIVE : PaymentMode.TEST;

/**
 * The top-up fact a verified webhook event carries, or null when the event is
 * not about a top-up payment or a finished refund.
 */
export const toTopUpEvent = (event: Stripe.Event): TopUpEvent | null => {
  const source = { provider: BillingProvider.STRIPE, mode: modeOfEvent(event) };
  switch (event.type) {
    case "checkout.session.completed":
    case "checkout.session.async_payment_succeeded": {
      const paid = toPaid(event.data.object);
      return paid && { ...source, ...paid };
    }
    case "refund.created":
    case "refund.updated": {
      const refunded = toRefunded(event.data.object);
      return refunded && { ...source, ...refunded };
    }
    default:
      return null;
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
    collectedMicroSar: session.amount_total * MICRO_SAR_PER_HALALA,
  };
};

const toRefunded = (refund: Stripe.Refund): TopUpFact | null => {
  const paymentId = idOf(refund.payment_intent);

  if (
    refund.status !== "succeeded" ||
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
  };
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
