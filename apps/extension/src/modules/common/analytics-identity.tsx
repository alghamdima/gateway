import { useEffect } from "react";

import { identify, reset } from "@turbostarter/analytics-extension";

import { authClient } from "~/lib/auth";

export const AnalyticsIdentity = () => {
  const session = authClient.useSession();

  useEffect(() => {
    if (session.isPending) {
      return;
    }

    if (session.data?.user) {
      identify(session.data.user.id, {
        email: session.data.user.email,
        name: session.data.user.name,
      });
    } else {
      reset();
    }
  }, [session]);

  return null;
};
