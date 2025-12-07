import { OpenPanelComponent } from "@openpanel/nextjs";

import { env } from "./env";

import type { AnalyticsProviderClientStrategy } from "../types";

const Provider: AnalyticsProviderClientStrategy["Provider"] = ({
  children,
}) => {
  return (
    <>
      {children}
      <OpenPanelComponent
        clientId={env.NEXT_PUBLIC_OPEN_PANEL_CLIENT_ID}
        trackScreenViews
        trackAttributes
        trackOutgoingLinks
      />
    </>
  );
};

const track: AnalyticsProviderClientStrategy["track"] = (event, data) => {
  if (typeof window === "undefined") {
    return;
  }

  window.op("track", event, data);
};

const identify: AnalyticsProviderClientStrategy["identify"] = (
  userId,
  traits,
) => {
  if (typeof window === "undefined") {
    return;
  }

  window.op("identify", {
    profileId: userId,
    ...traits,
  });
};

const reset: AnalyticsProviderClientStrategy["reset"] = () => {
  if (typeof window === "undefined") {
    return;
  }

  window.op("clear");
};

export { Provider, track, identify, reset };
