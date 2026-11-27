import { HttpStatusCode } from "@workspace/shared/constants";
import { logger } from "@workspace/shared/logger";
import { HttpException } from "@workspace/shared/utils";

import { checkoutStatusChangeHandler } from "../checkout";
import { webhookSecretFor } from "../config";
import { subscriptionStatusChangeHandler } from "../subscription";
import { isTopUpSession, modeOfEvent, toTopUpEvent } from "../top-up";
import { STRIPE_SIGNATURE_HEADER } from "./constants";
import { constructEvent } from "./event";

import type { WebhookCallbacks } from "../../types";
import type { Stripe } from "stripe";

export const webhookHandler = async (
  req: Request,
  callbacks?: WebhookCallbacks,
): Promise<Response> => {
  const body = await req.text();
  const sig = req.headers.get(STRIPE_SIGNATURE_HEADER);

  if (!sig) {
    throw new HttpException(HttpStatusCode.BAD_REQUEST, {
      code: "billing:error.webhook.signatureNotFound",
    });
  }

  // Test and live events arrive at the same URL, each signed with its own
  // mode's secret. The unverified livemode flag only picks the secret: a
  // forged flag fails the signature check, which covers the whole payload.
  const claimed = claimedMode(body);
  const secret = claimed ? webhookSecretFor(claimed) : null;
  if (!secret) {
    throw new HttpException(HttpStatusCode.BAD_REQUEST, {
      code: "billing:error.webhook.signatureInvalid",
      message: claimed
        ? `No webhook secret is set for Stripe's ${claimed} mode`
        : undefined,
    });
  }

  let event: Stripe.Event;
  try {
    event = constructEvent({ payload: body, sig, secret });
  } catch {
    throw new HttpException(HttpStatusCode.BAD_REQUEST, {
      code: "billing:error.webhook.signatureInvalid",
    });
  }
  if (modeOfEvent(event) !== claimed) {
    throw new HttpException(HttpStatusCode.BAD_REQUEST, {
      code: "billing:error.webhook.signatureInvalid",
    });
  }

  logger.info(`🔔  Webhook received: ${event.type}`);
  await callbacks?.onEvent?.(event);

  const topUpEvent = toTopUpEvent(event);
  if (topUpEvent) {
    await callbacks?.onTopUpEvent?.(topUpEvent);
  }

  switch (event.type) {
    case "customer.subscription.created":
      await callbacks?.onSubscriptionCreated?.(event.data.object.id);
      await subscriptionStatusChangeHandler({
        id: event.data.object.id,
        customerId: event.data.object.customer as string,
      });
      break;
    case "customer.subscription.updated":
      await callbacks?.onSubscriptionUpdated?.(event.data.object.id);
      await subscriptionStatusChangeHandler({
        id: event.data.object.id,
        customerId: event.data.object.customer as string,
      });
      break;
    case "customer.subscription.deleted":
      await callbacks?.onSubscriptionDeleted?.(event.data.object.id);
      await subscriptionStatusChangeHandler({
        id: event.data.object.id,
        customerId: event.data.object.customer as string,
      });
      break;
    case "checkout.session.completed":
      await callbacks?.onCheckoutSessionCompleted?.(event.data.object.id);
      // a top-up is credited by onEvent; it is not an order of a plan
      if (!isTopUpSession(event.data.object)) {
        await checkoutStatusChangeHandler(event.data.object);
      }
      break;
  }

  return new Response(JSON.stringify({ received: true }), {
    status: 200,
    headers: {
      "Content-Type": "application/json",
    },
  });
};

/** The mode the payload says it is from, before the signature is checked. */
const claimedMode = (body: string) => {
  try {
    const parsed: unknown = JSON.parse(body);
    return typeof parsed === "object" &&
      parsed !== null &&
      "livemode" in parsed &&
      typeof parsed.livemode === "boolean"
      ? modeOfEvent({ livemode: parsed.livemode })
      : null;
  } catch {
    return null;
  }
};
