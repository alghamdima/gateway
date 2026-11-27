import { defineEnv } from "envin";
import * as z from "zod";

import { envConfig } from "@workspace/shared/constants";

import type { Preset } from "envin/types";

/**
 * Stripe's secrets, one pair per mode. Staff choose the mode in the admin
 * (ADMIN-03); a mode without both of its secrets cannot be chosen. The live
 * pair keeps the names deployments already use. Every one is optional here:
 * which mode is usable is checked when it is used (./config.ts), so a
 * deployment without Stripe still starts.
 */
export const preset = {
  id: "stripe",
  server: {
    /** Live mode: sk_live_ or rk_live_. */
    STRIPE_SECRET_KEY: z.string().optional(),
    STRIPE_WEBHOOK_SECRET: z.string().optional(),
    /** Test mode: sk_test_ or rk_test_. */
    STRIPE_TEST_SECRET_KEY: z.string().optional(),
    STRIPE_TEST_WEBHOOK_SECRET: z.string().optional(),
  },
} as const satisfies Preset;

export const env = defineEnv({
  ...envConfig,
  ...preset,
});
