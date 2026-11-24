import { defineEnv } from "envin";
import * as z from "zod";

import { envConfig } from "@workspace/shared/constants";

import type { Preset } from "envin/types";

/**
 * Settings of the scheduled jobs (/api/internal), on their own so that
 * loading the jobs validates only what they need.
 */
export const preset = {
  id: "internal",
  server: {
    /** Bearer secret for /api/internal jobs. Unset refuses them all. */
    CRON_SECRET: z.string().min(32).optional(),
    /** Who is emailed when the daily reconciliation finds a difference. */
    RECONCILIATION_ALERT_EMAIL: z.email().optional(),
  },
} as const satisfies Preset;

export const env = defineEnv({
  ...envConfig,
  ...preset,
});
