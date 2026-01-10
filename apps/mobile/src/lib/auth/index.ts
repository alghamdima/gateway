import * as SecureStore from "expo-secure-store";

import { createClient } from "@workspace/auth/client/mobile";
import { config } from "@workspace/i18n";
import { Platform } from "@workspace/shared/constants";

import { getBaseUrl } from "~/lib/api/utils";
import { useI18nConfig } from "~/lib/providers/i18n";

export const authClient = createClient({
  baseURL: getBaseUrl(),
  disableDefaultFetchPlugins: true,
  mobile: {
    storage: SecureStore,
    cookiePrefix: "turbostarter",
  },
  lastLoginMethod: {
    storage: SecureStore,
  },
  fetchOptions: {
    headers: {
      Cookie: `${config.cookie}=${useI18nConfig.getState().config.locale}`,
      "x-client-platform": Platform.MOBILE,
    },
    throw: true,
  },
});
