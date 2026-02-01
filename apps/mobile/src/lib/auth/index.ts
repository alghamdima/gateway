import * as SecureStore from "expo-secure-store";

import {
  adminClient,
  anonymousClient,
  emailOTPClient,
  magicLinkClient,
  twoFactorClient,
  organizationClient,
  inferAdditionalFields,
  lastLoginMethodClient,
  createAuthClient,
  expoClient,
} from "@workspace/auth/client/mobile";
import { config } from "@workspace/i18n";
import { Platform } from "@workspace/shared/constants";

import { getBaseUrl } from "~/lib/api/utils";
import { useI18nConfig } from "~/lib/providers/i18n";

import type { auth } from "@workspace/auth/server";

export const authClient = createAuthClient({
  baseURL: getBaseUrl(),
  disableDefaultFetchPlugins: true,
  fetchOptions: {
    headers: {
      Cookie: `${config.cookie}=${useI18nConfig.getState().config.locale}`,
      "x-client-platform": Platform.MOBILE,
    },
    throw: true,
  },
  plugins: [
    anonymousClient(),
    emailOTPClient(),
    magicLinkClient(),
    twoFactorClient(),
    adminClient(),
    organizationClient(),
    lastLoginMethodClient({
      storage: SecureStore,
    }),
    inferAdditionalFields<typeof auth>(),
    expoClient({
      storage: SecureStore,
      cookiePrefix: "turbostarter",
    }),
  ],
});
