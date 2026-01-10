import { createClient } from "@workspace/auth/client/web";
import { Platform } from "@workspace/shared/constants";

import { getBaseUrl } from "~/lib/api";

export const authClient = createClient({
  baseURL: getBaseUrl(),
  fetchOptions: {
    headers: {
      "x-client-platform": Platform.EXTENSION,
    },
    throw: true,
  },
});
