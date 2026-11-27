import { strategy as stripe } from "./stripe";
import { BillingProvider } from "./types";

import type { BillingProviderStrategy } from "./types";

/**
 * Every payment provider the product can use. Which one takes top-ups, and in
 * which mode, is chosen by staff in the admin (ADMIN-03), not here. A new
 * provider (the Saudi gateway, BILL-04) is a folder beside ./stripe, added to
 * this map and to PAYMENT_PROVIDERS in packages/db/src/schema/payments.ts.
 */
export const providers = {
  [BillingProvider.STRIPE]: stripe,
} as const satisfies Record<BillingProvider, BillingProviderStrategy>;

/** The template's subscription code still has one provider. */
export const strategy: BillingProviderStrategy = stripe;

/** The provider by its name, or null when the product has none of that name. */
export const getBillingProvider = (
  name: string,
): BillingProviderStrategy | null =>
  Object.entries(providers).find(([key]) => key === name)?.[1] ?? null;
