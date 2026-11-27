import { BillingProvider } from "../types";
import { checkout } from "./checkout";
import { secrets } from "./config";
import { getBillingPortal } from "./customer";
import { updateSubscription } from "./subscription";
import { createTopUpCheckout } from "./top-up";
import { getUsage, recordUsage } from "./usage";
import { webhookHandler } from "./webhook";

import type { BillingProviderStrategy } from "../types";

export const strategy = {
  webhookHandler,
  checkout,
  getBillingPortal,
  updateSubscription,
  recordUsage,
  getUsage,
  topUp: { checkout: createTopUpCheckout, secrets },
  provider: BillingProvider.STRIPE,
} satisfies BillingProviderStrategy;
