import {
  upsertCustomer,
  updateCustomer,
  getCustomersByReferenceId,
} from "@workspace/billing/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { logger } from "@workspace/shared/logger";
import { HttpException } from "@workspace/shared/utils";

import { BillingProvider } from "../types";
import { stripe } from "./sdk";

import type { Stripe } from "stripe";

const getStripeCustomerById = async (id: string) => {
  return stripe().customers.retrieve(id);
};

const getStripeCustomerByEmail = async (email: string) => {
  const customers = await stripe().customers.list({ email: email });

  return customers.data.length > 0 ? customers.data[0] : null;
};

const createStripeCustomer = async (userId: string, email: string) => {
  const customerData = { metadata: { userId }, email };
  const newCustomer = await stripe().customers.create(customerData);

  return newCustomer;
};

export const createOrRetrieveCustomer = async ({
  userId,
  email,
}: {
  userId: string;
  email: string;
}) => {
  const [existingCustomer] = await getCustomersByReferenceId(userId, {
    provider: BillingProvider.STRIPE,
  });

  const stripeCustomer = existingCustomer?.externalId
    ? await getStripeCustomerById(existingCustomer.externalId)
    : await getStripeCustomerByEmail(email);

  const stripeCustomerToProcess =
    stripeCustomer ?? (await createStripeCustomer(userId, email));

  if (existingCustomer) {
    if (existingCustomer.externalId !== stripeCustomerToProcess.id) {
      await updateCustomer(existingCustomer.id, {
        externalId: stripeCustomerToProcess.id,
      });
      logger.warn(
        `Customer ${existingCustomer.id} had a different externalId. Updated to ${stripeCustomerToProcess.id}.`,
      );
    }

    return {
      customer: existingCustomer,
      stripeCustomer: stripeCustomerToProcess,
    };
  }

  const [newCustomer] = await upsertCustomer({
    referenceId: userId,
    externalId: stripeCustomerToProcess.id,
    provider: BillingProvider.STRIPE,
  });

  return {
    customer: newCustomer,
    stripeCustomer: stripeCustomerToProcess,
  };
};

export const createBillingPortalSession = async (
  params: Stripe.BillingPortal.SessionCreateParams,
) => {
  try {
    return await stripe().billingPortal.sessions.create(params);
  } catch (e) {
    logger.error(e);
    throw new HttpException(HttpStatusCode.INTERNAL_SERVER_ERROR, {
      code: "billing:error.portal",
    });
  }
};
