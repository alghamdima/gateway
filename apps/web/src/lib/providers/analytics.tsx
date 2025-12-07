"use client";

import { useEffect } from "react";

import { identify, Provider, reset } from "@turbostarter/analytics-web";

import { authClient } from "~/lib/auth/client";

export const AnalyticsProvider = ({
  children,
}: {
  children: React.ReactNode;
}) => {
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

  return <Provider>{children}</Provider>;
};
