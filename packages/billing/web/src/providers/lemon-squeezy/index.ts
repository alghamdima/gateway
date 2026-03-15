import { BillingProvider } from "../types";
import { checkout, getBillingPortal } from "./checkout";
import { initialize } from "./sdk";
import { webhookHandler } from "./webhook";

import type { BillingProviderStrategy } from "../types";

initialize();

export const strategy = {
  webhookHandler,
  checkout,
  getBillingPortal,
  provider: BillingProvider.LEMON_SQUEEZY,
} satisfies BillingProviderStrategy;
