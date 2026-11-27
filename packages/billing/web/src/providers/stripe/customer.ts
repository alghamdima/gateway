import {
  upsertCustomer,
  updateCustomer,
  getCustomersByReferenceId,
} from "@workspace/billing/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { logger } from "@workspace/shared/logger";
import { HttpException } from "@workspace/shared/utils";

import { GetBillingPortalPayload } from "../../schema";
import { BillingProvider, BillingUser, PaymentMode } from "../types";
import { stripe } from "./sdk";

import type { Stripe } from "stripe";

/**
 * Test and live mode have separate customers, so each mode keeps its own row
 * in `customer`. Live keeps the provider name rows already use.
 */
const customerProvider = (mode?: PaymentMode) =>
  mode === PaymentMode.TEST
    ? `${BillingProvider.STRIPE}_test`
    : BillingProvider.STRIPE;

const isMissingResource = (error: unknown) =>
  typeof error === "object" &&
  error !== null &&
  "code" in error &&
  error.code === "resource_missing";

const getStripeCustomerById = async (id: string, mode?: PaymentMode) => {
  try {
    return await stripe(mode).customers.retrieve(id);
  } catch (error) {
    // an id from another Stripe account or mode: a new customer replaces it
    if (isMissingResource(error)) {
      return null;
    }
    throw error;
  }
};

const createStripeCustomer = async (
  referenceId: string,
  email: string,
  mode?: PaymentMode,
) => {
  const customerData = { metadata: { referenceId }, email };
  const newCustomer = await stripe(mode).customers.create(customerData);

  return newCustomer;
};

export const createOrRetrieveCustomer = async ({
  referenceId,
  email,
  mode,
}: {
  referenceId: string;
  email: string;
  /** Top-ups name the mode staff chose; the template's plan code does not. */
  mode?: PaymentMode;
}) => {
  const provider = customerProvider(mode);
  const [existingCustomer] = await getCustomersByReferenceId(referenceId, {
    provider,
  });

  const stripeCustomer = existingCustomer?.externalId
    ? await getStripeCustomerById(existingCustomer.externalId, mode)
    : null;

  const stripeCustomerToProcess =
    stripeCustomer ?? (await createStripeCustomer(referenceId, email, mode));

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
    referenceId,
    externalId: stripeCustomerToProcess.id,
    provider,
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

export const getBillingPortal = async ({
  redirectUrl,
  referenceId,
  user,
}: GetBillingPortalPayload & { user: BillingUser }) => {
  try {
    const { stripeCustomer } = await createOrRetrieveCustomer({
      referenceId,
      email: user.email,
    });

    const { url } = await createBillingPortalSession({
      customer: stripeCustomer.id,
      return_url: redirectUrl,
    });

    return { url };
  } catch (e) {
    logger.error(e);
    throw new HttpException(HttpStatusCode.INTERNAL_SERVER_ERROR);
  }
};
