import { defineEnv } from "envin";
import * as z from "zod";

import { envConfig } from "@workspace/shared/constants";

import type { Preset } from "envin/types";

/**
 * The gateway's management API: private network only (docs/platform/adr/0007),
 * authenticated with the admin user of the gateway's auth_config. Optional so
 * the web app starts without a gateway; key operations then fail with
 * GatewayNotConfiguredError.
 */
export const preset = {
  id: "gateway",
  server: {
    GATEWAY_URL: z.url().optional(),
    GATEWAY_ADMIN_USERNAME: z.string().min(1).optional(),
    GATEWAY_ADMIN_PASSWORD: z.string().min(1).optional(),
  },
} as const satisfies Preset;

export const env = defineEnv({
  ...envConfig,
  ...preset,
});
