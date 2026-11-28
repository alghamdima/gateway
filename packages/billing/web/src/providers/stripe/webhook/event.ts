import { Stripe } from "stripe";

/** Stripe's own signature check; it needs the signing secret, not an API key. */
export const constructEvent = (data: {
  payload: string;
  sig: string;
  secret: string;
}): Stripe.Event =>
  Stripe.webhooks.constructEvent(data.payload, data.sig, data.secret);
