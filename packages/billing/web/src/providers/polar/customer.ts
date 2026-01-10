import {
  upsertCustomer,
  updateCustomer,
  getCustomersByReferenceId,
} from "@workspace/billing/server";
import { logger } from "@workspace/shared/logger";

import { BillingProvider } from "../types";

import { polar } from "./sdk";

const getPolarCustomerById = async (customerId: string) => {
  return polar().customers.get({ id: customerId });
};

const getPolarCustomerByEmail = async (email: string) => {
  const customers = await polar().customers.list({
    email,
  });

  return customers.result.items[0];
};

const createPolarCustomer = async (email: string) => {
  const newCustomer = await polar().customers.create({
    email,
  });

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
    provider: BillingProvider.POLAR,
  });

  const polarCustomer = existingCustomer?.externalId
    ? await getPolarCustomerById(existingCustomer.externalId)
    : await getPolarCustomerByEmail(email);

  const polarCustomerToProcess =
    polarCustomer ?? (await createPolarCustomer(email));

  if (existingCustomer) {
    if (existingCustomer.externalId !== polarCustomerToProcess.id) {
      await updateCustomer(existingCustomer.id, {
        externalId: polarCustomerToProcess.id,
      });
      logger.warn(
        `Customer ${existingCustomer.id} had a different externalId. Updated to ${polarCustomerToProcess.id}.`,
      );
    }

    return {
      customer: existingCustomer,
      polarCustomer: polarCustomerToProcess,
    };
  }

  const [newCustomer] = await upsertCustomer({
    referenceId: userId,
    externalId: polarCustomerToProcess.id,
    provider: BillingProvider.POLAR,
  });

  return {
    customer: newCustomer,
    polarCustomer: polarCustomerToProcess,
  };
};
