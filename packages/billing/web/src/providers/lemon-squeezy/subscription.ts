import { getSubscription } from "@lemonsqueezy/lemonsqueezy.js";
import dayjs from "dayjs";

import {
  getCustomerByExternalId,
  upsertSubscription,
} from "@workspace/billing/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { logger } from "@workspace/shared/logger";
import { HttpException } from "@workspace/shared/utils";

import { BillingProvider } from "../types";

import { toSubscriptionStatus } from "./mappers/to-billing-status";

export const subscriptionStatusChangeHandler = async ({
  id,
}: {
  id: string;
}) => {
  const { data } = await getSubscription(id);

  const subscription = data?.data;

  if (!subscription) {
    throw new HttpException(HttpStatusCode.NOT_FOUND, {
      code: "billing:error.subscriptionNotFound",
    });
  }

  const customer = await getCustomerByExternalId(
    subscription.attributes.customer_id.toString(),
  );

  if (!customer) {
    throw new HttpException(HttpStatusCode.NOT_FOUND, {
      code: "billing:error.customerNotFound",
    });
  }

  const status = toSubscriptionStatus(subscription.attributes.status);
  const periodStartsAt = dayjs(subscription.attributes.created_at).toDate();
  const periodEndsAt = dayjs(
    subscription.attributes.ends_at ?? subscription.attributes.renews_at,
  ).toDate();
  const trialEndsAt = subscription.attributes.trial_ends_at
    ? dayjs(subscription.attributes.trial_ends_at).toDate()
    : null;
  const trialStartsAt = trialEndsAt ? periodStartsAt : null;

  await upsertSubscription({
    customerId: customer.id,
    externalId: subscription.id,
    variantId: subscription.attributes.variant_id.toString(),
    status,
    store: BillingProvider.LEMON_SQUEEZY,
    periodStartsAt,
    periodEndsAt,
    trialStartsAt,
    trialEndsAt,
  });

  logger.info(
    `✅ Subscription ${subscription.id} status changed for customer ${customer.id} to ${status}`,
  );
};
