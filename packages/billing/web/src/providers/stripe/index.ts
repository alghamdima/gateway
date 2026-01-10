import { BillingProvider } from "../types";

import { checkout, getBillingPortal } from "./checkout";
import { webhookHandler } from "./webhook";

import type { BillingProviderStrategy } from "../types";

export const strategy = {
  webhookHandler,
  checkout,
  getBillingPortal,
  provider: BillingProvider.STRIPE,
} satisfies BillingProviderStrategy;
