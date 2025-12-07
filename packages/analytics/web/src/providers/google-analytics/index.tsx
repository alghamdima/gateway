import { env } from "./env";

import type { AnalyticsProviderClientStrategy } from "../types";

declare global {
  interface Window {
    dataLayer?: unknown[];
    gtag?: (...args: unknown[]) => void;
  }
}

const Provider: AnalyticsProviderClientStrategy["Provider"] = ({
  children,
}) => {
  return (
    <>
      {children}
      <script
        async
        src={`https://www.googletagmanager.com/gtag/js?id=${env.NEXT_PUBLIC_GOOGLE_ANALYTICS_MEASUREMENT_ID}`}
        onLoad={() => {
          if (typeof window === "undefined") {
            return;
          }

          window.dataLayer = window.dataLayer ?? [];

          function gtag(...args: unknown[]) {
            window.dataLayer?.push(args);
          }

          window.gtag = gtag;

          window.gtag("js", new Date());
          window.gtag(
            "config",
            env.NEXT_PUBLIC_GOOGLE_ANALYTICS_MEASUREMENT_ID,
          );
        }}
      />
    </>
  );
};

const track: AnalyticsProviderClientStrategy["track"] = (event, data) => {
  if (typeof window === "undefined" || !window.gtag) {
    return;
  }

  window.gtag("event", event, data);
};

const identify: AnalyticsProviderClientStrategy["identify"] = (
  userId,
  traits,
) => {
  if (typeof window === "undefined" || !window.gtag) {
    return;
  }

  window.gtag("config", env.NEXT_PUBLIC_GOOGLE_ANALYTICS_MEASUREMENT_ID, {
    user_id: userId,
    ...traits,
  });
};

const reset: AnalyticsProviderClientStrategy["reset"] = () => {
  if (typeof window === "undefined" || !window.gtag) {
    return;
  }

  window.gtag("config", env.NEXT_PUBLIC_GOOGLE_ANALYTICS_MEASUREMENT_ID, {
    user_id: null,
  });
};

export { Provider, track, identify, reset };
