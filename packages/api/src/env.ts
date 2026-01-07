/* eslint-disable turbo/no-undeclared-env-vars */
import { defineEnv } from "envin";
import * as z from "zod";

import { preset as auth } from "@workspace/auth/env";
import { preset as billing } from "@workspace/billing/env";
import { preset as db } from "@workspace/db/env";
import { preset as email } from "@workspace/email/env";
import { preset as monitoring } from "@workspace/monitoring-web/env";
import { envConfig } from "@workspace/shared/constants";
import { preset as storage } from "@workspace/storage/env";

import type { Preset } from "envin/types";

export const preset = {
  id: "api",
  server: {
    OPENAI_API_KEY: z.string().optional(), // change it to your provider API key (e.g. ANTHROPIC_API_KEY if you use Anthropic)
  },
  extends: [billing, auth, db, email, storage, monitoring],
} as const satisfies Preset;

export const env = defineEnv({
  ...envConfig,
  ...preset,
  env: {
    ...process.env,
    NEXT_PUBLIC_SENTRY_DSN: process.env.NEXT_PUBLIC_SENTRY_DSN,

    NEXT_PUBLIC_POSTHOG_KEY: process.env.NEXT_PUBLIC_POSTHOG_KEY,
    NEXT_PUBLIC_POSTHOG_HOST: process.env.NEXT_PUBLIC_POSTHOG_HOST,
  },
});
