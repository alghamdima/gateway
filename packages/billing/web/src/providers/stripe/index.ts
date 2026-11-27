import { BillingProvider } from "../types";
import { checkout } from "./checkout";
import { secrets } from "./config";
import { getBillingPortal } from "./customer";
import { updateSubscription } from "./subscription";
import { createTopUpCheckout, refundTopUp } from "./top-up";
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
  topUp: { checkout: createTopUpCheckout, secrets, refund: refundTopUp },
  provider: BillingProvider.STRIPE,
} satisfies BillingProviderStrategy;
