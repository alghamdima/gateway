import { BillingModel } from "@workspace/billing";

import type { Stripe } from "stripe";

export const toCheckoutMode = (
  model: BillingModel,
): Stripe.Checkout.SessionCreateParams.Mode => {
  switch (model) {
    case BillingModel.ONE_TIME:
      return "payment";
    case BillingModel.RECURRING:
      return "subscription";
    default:
      throw new Error("Invalid billing model");
  }
};
