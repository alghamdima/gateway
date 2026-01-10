import {
  createCheckout,
  getCustomer,
  getOrder,
} from "@lemonsqueezy/lemonsqueezy.js";

import { config } from "@workspace/billing";
import {
  getCustomerByExternalId,
  getCustomersByReferenceId,
  upsertOrder,
} from "@workspace/billing/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { logger } from "@workspace/shared/logger";
import { HttpException } from "@workspace/shared/utils";

import { BillingProvider } from "../types";

import { createOrRetrieveCustomer } from "./customer";
import { env } from "./env";
import { toPaymentStatus } from "./mappers/to-billing-status";

import type { CheckoutPayload } from "../../schema";
import type { User } from "@workspace/auth";

export const checkout = async ({
  user,
  variant: { id },
  discount,
  redirect,
}: CheckoutPayload & { user: User }) => {
  try {
    const variant = config.plans
      .find((plan) => plan.variants.some((v) => v.id === id))
      ?.variants.find((v) => v.id === id);

    if (!variant) {
      throw new HttpException(HttpStatusCode.NOT_FOUND, {
        code: "billing:error.variantNotFound",
      });
    }

    const { customer, lemonSqueezyCustomer } = await createOrRetrieveCustomer({
      userId: user.id,
      email: user.email,
    });

    const session = await createCheckout(env.LEMON_SQUEEZY_STORE_ID, id, {
      checkoutData: {
        email: lemonSqueezyCustomer.attributes.email,
        name: lemonSqueezyCustomer.attributes.name,
        custom: {
          customer_id: customer?.id,
          user_id: user.id,
        },
        ...(discount && { discountCode: discount.code }),
      },
      productOptions: {
        enabledVariants: [Number(id)],
        redirectUrl: redirect.success,
      },
    });

    return { url: session.data?.data.attributes.url ?? null };
  } catch (e) {
    logger.error(e);
    throw new HttpException(HttpStatusCode.INTERNAL_SERVER_ERROR, {
      code: "billing:error.checkout",
    });
  }
};

export const getBillingPortal = async ({ user }: { user: User }) => {
  const defaultUrl = `https://${env.LEMON_SQUEEZY_STORE_ID}.lemonsqueezy.com/billing`;

  try {
    const [customer] = await getCustomersByReferenceId(user.id, {
      provider: BillingProvider.LEMON_SQUEEZY,
    });

    if (!customer) {
      return {
        url: defaultUrl,
      };
    }

    const lemonCustomer = await getCustomer(customer.externalId);

    const url = lemonCustomer.data?.data.attributes.urls.customer_portal;

    return { url: url ?? defaultUrl };
  } catch (e) {
    logger.error(e);
    throw new HttpException(HttpStatusCode.INTERNAL_SERVER_ERROR, {
      code: "billing:error.portal",
    });
  }
};

export const checkoutStatusChangeHandler = async ({ id }: { id: string }) => {
  const { data } = await getOrder(id);

  const order = data?.data;

  if (!order) {
    throw new HttpException(HttpStatusCode.NOT_FOUND, {
      code: "billing:error.orderNotFound",
    });
  }

  const customer = await getCustomerByExternalId(
    order.attributes.customer_id.toString(),
  );

  if (!customer) {
    throw new HttpException(HttpStatusCode.NOT_FOUND, {
      code: "billing:error.customerNotFound",
    });
  }

  const status = toPaymentStatus(order.attributes.status);

  await upsertOrder({
    customerId: customer.id,
    externalId: order.id,
    variantId: order.attributes.first_order_item.variant_id.toString(),
    status,
    store: BillingProvider.LEMON_SQUEEZY,
  });

  logger.info(
    `✅ Order ${order.id} status changed for customer ${customer.id} to ${status}`,
  );
};
