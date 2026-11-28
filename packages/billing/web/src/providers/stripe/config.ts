import { PaymentMode } from "../types";
import { env } from "./env";

import type { ModeSecrets, ProviderSecrets, SecretState } from "../types";

/**
 * Which Stripe secret serves which mode (ADMIN-03). A key of the wrong mode
 * counts as not set, so a test key named as the live one can never take a
 * live checkout, and the admin says why.
 */

/** The env names of each mode's secrets: the admin shows these, never the values. */
export const SECRET_NAMES = {
  [PaymentMode.LIVE]: {
    secretKey: "STRIPE_SECRET_KEY",
    webhookSecret: "STRIPE_WEBHOOK_SECRET",
  },
  [PaymentMode.TEST]: {
    secretKey: "STRIPE_TEST_SECRET_KEY",
    webhookSecret: "STRIPE_TEST_WEBHOOK_SECRET",
  },
} as const;

/** Secret and restricted keys carry their mode in the prefix. */
const KEY_PREFIX: Record<PaymentMode, RegExp> = {
  [PaymentMode.LIVE]: /^(sk|rk)_live_/,
  [PaymentMode.TEST]: /^(sk|rk)_test_/,
};

type SecretName =
  (typeof SECRET_NAMES)[PaymentMode][keyof (typeof SECRET_NAMES)[PaymentMode]];

const valueOf = (name: SecretName) => {
  const value = env[name]?.trim();
  return value ? value : null;
};

const keyState = (mode: PaymentMode): SecretState => {
  const name = SECRET_NAMES[mode].secretKey;
  const value = valueOf(name);
  if (value === null) {
    return { name, status: "missing" };
  }
  return {
    name,
    status: KEY_PREFIX[mode].test(value) ? "present" : "wrongMode",
  };
};

const webhookState = (mode: PaymentMode): SecretState => {
  const name = SECRET_NAMES[mode].webhookSecret;
  return { name, status: valueOf(name) === null ? "missing" : "present" };
};

const modeSecrets = (mode: PaymentMode): ModeSecrets => {
  const secretKey = keyState(mode);
  const webhookSecret = webhookState(mode);
  return {
    secretKey,
    webhookSecret,
    isConfigured:
      secretKey.status === "present" && webhookSecret.status === "present",
  };
};

export const secrets = (): ProviderSecrets => ({
  [PaymentMode.TEST]: modeSecrets(PaymentMode.TEST),
  [PaymentMode.LIVE]: modeSecrets(PaymentMode.LIVE),
});

/** The API key of a mode, or null when it is not set or belongs to the other mode. */
export const secretKeyFor = (mode: PaymentMode) =>
  keyState(mode).status === "present"
    ? valueOf(SECRET_NAMES[mode].secretKey)
    : null;

/** The webhook signing secret of a mode, or null when it is not set. */
export const webhookSecretFor = (mode: PaymentMode) =>
  valueOf(SECRET_NAMES[mode].webhookSecret);
