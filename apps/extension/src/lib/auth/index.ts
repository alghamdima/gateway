import { createClient } from "@workspace/auth/client/web";

import { getBaseUrl } from "~/lib/api";

export const authClient = createClient({
  baseURL: getBaseUrl(),
  fetchOptions: {
    headers: {
      "x-client-platform": "extension",
    },
    throw: true,
  },
});
