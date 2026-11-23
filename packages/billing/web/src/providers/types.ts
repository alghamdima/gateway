import { GetUsagePayload } from "../schema/usage";

import type { CheckoutPayload, GetBillingPortalPayload } from "../schema";

export interface BillingUser {
  id: string;
  email: string;
}

export interface WebhookCallbacks {
  onCheckoutSessionCompleted?: (sessionId: string) => Promise<void> | void;
  onSubscriptionCreated?: (subscriptionId: string) => Promise<void> | void;
  onSubscriptionUpdated?: (subscriptionId: string) => Promise<void> | void;
  onSubscriptionDeleted?: (subscriptionId: string) => Promise<void> | void;
  onEvent?: (event: unknown) => Promise<void> | void;
  /** A top-up was paid or refunded. Throwing makes the provider deliver the event again. */
  onTopUpEvent?: (event: TopUpEvent) => Promise<void> | void;
}

/** Marks a payment as a credits top-up (docs/platform/adr/0012-top-up-checkout.md). */
export const TOP_UP_PURPOSE = "credits_top_up";

export interface TopUpCheckoutInput {
  user: BillingUser;
  organizationId: string;
  /** micro-USD, whole cents. */
  creditMicroUsd: number;
  /** micro-USD, whole cents; paid on top of the credits. */
  feeMicroUsd: number;
  /** The line names the customer sees on the payment page and the receipt. */
  labels: { credits: string; fee: string };
  redirect: { success: string; cancel: string };
}

/** What a verified payment webhook says about a top-up. Amounts are micro-USD. */
export type TopUpEvent =
  | {
      type: "paid";
      /** The provider's payment id: one payment is credited once. */
      paymentId: string;
      organizationId: string;
      creditMicroUsd: number;
      feeMicroUsd: number;
      /** What the provider actually collected. */
      collectedMicroUsd: number;
    }
  | {
      type: "refunded";
      refundId: string;
      paymentId: string;
      refundedMicroUsd: number;
    };

export interface BillingProviderStrategy {
  provider: BillingProvider;
  webhookHandler: (
    req: Request,
    callbacks?: WebhookCallbacks,
  ) => Promise<Response>;
  checkout: (
    input: CheckoutPayload & { user: BillingUser },
  ) => Promise<{ url: string | null }>;
  getBillingPortal: (
    input: GetBillingPortalPayload & { user: BillingUser },
  ) => Promise<{ url: string }>;
  updateSubscription: (
    id: string,
    data: Partial<{
      quantity: number;
    }>,
  ) => Promise<void>;
  recordUsage: (input: {
    externalId: string;
    quantity: number;
    event?: string;
    timestamp?: Date;
  }) => Promise<{ recorded: boolean }>;
  getUsage: (
    input: Omit<GetUsagePayload, "referenceId"> & { externalId: string },
  ) => Promise<{ usage: number; start: Date; end: Date }>;
  /** Only providers that support credit top-ups. */
  topUp?: {
    checkout: (input: TopUpCheckoutInput) => Promise<{ url: string | null }>;
  };
}

export const BillingProvider = {
  STRIPE: "stripe",
  LEMON_SQUEEZY: "lemon-squeezy",
  POLAR: "polar",
  DODO_PAYMENTS: "dodo-payments",
} as const;

export type BillingProvider =
  (typeof BillingProvider)[keyof typeof BillingProvider];
