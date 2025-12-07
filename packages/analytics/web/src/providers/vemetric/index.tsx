import { VemetricScript, vemetric } from "@vemetric/react";

import { env } from "./env";

import type { AnalyticsProviderClientStrategy } from "../types";

const Provider: AnalyticsProviderClientStrategy["Provider"] = ({
  children,
}) => {
  return (
    <>
      <VemetricScript
        token={env.NEXT_PUBLIC_VEMETRIC_PROJECT_TOKEN}
        trackPageViews
        trackOutboundLinks
        trackDataAttributes
      />
      {children}
    </>
  );
};

const track: AnalyticsProviderClientStrategy["track"] = (event, data) => {
  if (typeof window === "undefined") {
    return;
  }

  void vemetric.trackEvent(event, {
    eventData: data,
  });
};

const identify: AnalyticsProviderClientStrategy["identify"] = (
  userId,
  traits,
) => {
  if (typeof window === "undefined") {
    return;
  }

  void vemetric.identify({
    identifier: userId,
    data: {
      set: traits,
    },
  });
};

const reset: AnalyticsProviderClientStrategy["reset"] = () => {
  if (typeof window === "undefined") {
    return;
  }

  void vemetric.resetUser();
};

export { Provider, track, identify, reset };
