import { getBillingProvider } from "@workspace/billing-web/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { logger } from "@workspace/shared/logger";
import { HttpException } from "@workspace/shared/utils";

import { getPaymentSettings } from "./settings";

import type { BillingProviderStrategy } from "@workspace/billing-web/server";
import type { PaymentMode, PaymentProviderName } from "@workspace/db/schema";

/**
 * What other modules ask the payments module (ADMIN-03): where a new top-up
 * goes, and whether a verified payment may be credited. Read from the
 * settings staff set, at most 30 seconds old.
 */

export interface TopUpGateway {
  provider: PaymentProviderName;
  mode: PaymentMode;
  checkout: NonNullable<BillingProviderStrategy["topUp"]>["checkout"];
}

type Resolved =
  | { isAvailable: true; gateway: TopUpGateway }
  | { isAvailable: false; reason: string };

const resolve = async (): Promise<Resolved> => {
  const settings = await getPaymentSettings();
  if (!settings.topupsEnabled) {
    return { isAvailable: false, reason: "top-ups are switched off" };
  }
  const active = settings.providers.find(
    (row) => row.provider === settings.activeProvider,
  );
  if (!active?.enabled) {
    return {
      isAvailable: false,
      reason: `the active provider ${settings.activeProvider} is disabled`,
    };
  }
  const topUp = getBillingProvider(active.provider)?.topUp;
  if (!topUp) {
    return {
      isAvailable: false,
      reason: `${active.provider} cannot take top-ups`,
    };
  }
  if (!topUp.secrets()[active.mode].isConfigured) {
    return {
      isAvailable: false,
      reason: `${active.provider} has no ${active.mode} secrets of that mode in env`,
    };
  }
  return {
    isAvailable: true,
    gateway: {
      provider: active.provider,
      mode: active.mode,
      checkout: topUp.checkout,
    },
  };
};

/** Whether the customer's top-up form should be offered. */
export const isTopUpAvailable = async () => (await resolve()).isAvailable;

/**
 * The provider and mode a new top-up goes to. Refuses with 503 when top-ups
 * are off, the active provider is disabled, or its mode's secrets are missing.
 */
export const resolveTopUpGateway = async (): Promise<TopUpGateway> => {
  const resolved = await resolve();
  if (!resolved.isAvailable) {
    logger.warn(`Top-up refused: ${resolved.reason}`);
    throw new HttpException(HttpStatusCode.SERVICE_UNAVAILABLE, {
      code: "billing:error.topUpsUnavailable",
    });
  }
  return resolved.gateway;
};

/**
 * Whether a paid test-mode event may add credits: only while staff have the
 * provider in test mode. Test money is not real, so a test checkout left open
 * when the provider went live must not become spendable credit. Live payments
 * are always credited, whatever the switches say: the money was taken.
 */
export const acceptsTestPayments = async (provider: string) => {
  const settings = await getPaymentSettings();
  return settings.providers.some(
    (row) => row.provider === provider && row.mode === "test",
  );
};
