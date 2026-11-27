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
  /** Which of the provider's secrets to use: staff choose it (ADMIN-03). */
  mode: PaymentMode;
  user: BillingUser;
  organizationId: string;
  /** micro-SAR, whole halalas. */
  creditMicroSar: number;
  /** micro-SAR, whole halalas; paid on top of the credits. */
  feeMicroSar: number;
  /** The rate the fee was worked out with, carried to the webhook so the payment records it. */
  feeRate: TopUpFeeRate;
  /** The promo code applied and what the server computed it gives (BILL-08), or null. */
  promo: TopUpPromo | null;
  /** The line names the customer sees on the payment page and the receipt. */
  labels: { credits: string; fee: string };
  redirect: { success: string; cancel: string };
}

/**
 * The fee rate a top-up was quoted at (ADMIN-24): 100 bps = 1%, the minimum in
 * micro-SAR, and where the rate came from ("default", "organization", "promo"), which
 * the API checks before it records it.
 */
export interface TopUpFeeRate {
  feeBps: number;
  feeMinMicroSar: number;
  feeSource: string;
}

/**
 * A promo code on a top-up (BILL-08): its id, and the fee discount and bonus
 * the server computed at checkout, in micro-SAR. The fee charged is already
 * net of the discount; the webhook grants the bonus.
 */
export interface TopUpPromo {
  codeId: string;
  feeDiscountMicroSar: number;
  bonusMicroSar: number;
}

/** What a verified payment webhook says about a top-up. Amounts are micro-SAR. */
export type TopUpEvent = TopUpEventSource &
  (
    | {
        type: "paid";
        /** The provider's payment id: one payment is credited once. */
        paymentId: string;
        organizationId: string;
        creditMicroSar: number;
        feeMicroSar: number;
        /** Null for a payment started before the rate was carried (ADMIN-24). */
        feeRate: TopUpFeeRate | null;
        /** The promo code the checkout carried, or null (BILL-08). */
        promo: TopUpPromo | null;
        /** What the provider actually collected. */
        collectedMicroSar: number;
      }
    | {
        type: "refunded";
        refundId: string;
        paymentId: string;
        refundedMicroSar: number;
      }
  );

/** Which provider and mode a verified event came from. */
export interface TopUpEventSource {
  provider: BillingProvider;
  mode: PaymentMode;
}

/**
 * Whether one secret is set, by its env name. Never the value: this is what
 * the admin shows (rule 6 of docs/platform/SPEC.md).
 */
export interface SecretState {
  name: string;
  /** wrongMode: set, but a key of the other mode (a test key named as live). */
  status: "present" | "missing" | "wrongMode";
}

export interface ModeSecrets {
  secretKey: SecretState;
  webhookSecret: SecretState;
  /** Both present and of this mode: checkouts and webhooks can work. */
  isConfigured: boolean;
}

export type ProviderSecrets = Record<PaymentMode, ModeSecrets>;

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
    /** Which secrets are set for each mode, computed from env on each call. */
    secrets: () => ProviderSecrets;
  };
}

export const BillingProvider = {
  STRIPE: "stripe",
} as const;

export type BillingProvider =
  (typeof BillingProvider)[keyof typeof BillingProvider];

/** Test takes no real money; live does. Each mode has its own secrets. */
export const PaymentMode = {
  TEST: "test",
  LIVE: "live",
} as const;

export type PaymentMode = (typeof PaymentMode)[keyof typeof PaymentMode];
