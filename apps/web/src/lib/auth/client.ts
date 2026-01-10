import { createClient } from "@workspace/auth/client/web";
import { ExecutionSide, Platform } from "@workspace/shared/constants";

export const authClient = createClient({
  fetchOptions: {
    headers: {
      "x-client-platform": `${Platform.WEB}-${ExecutionSide.CLIENT}`,
    },
    throw: true,
  },
});
