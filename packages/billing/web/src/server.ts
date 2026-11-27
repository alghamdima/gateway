import { strategy } from "./providers";

export const {
  webhookHandler,
  checkout,
  getBillingPortal,
  provider,
  updateSubscription,
  recordUsage,
  getUsage,
  topUp,
} = strategy;

export { getBillingProvider, providers } from "./providers";
export {
  BillingProvider,
  PaymentMode,
  TOP_UP_PURPOSE,
} from "./providers/types";
export type {
  BillingProviderStrategy,
  ModeSecrets,
  ProviderSecrets,
  SecretState,
  TopUpEvent,
  WebhookCallbacks,
} from "./providers/types";
