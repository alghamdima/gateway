"use client";

import mixpanel from "mixpanel-browser";
import { useEffect } from "react";

import { NodeEnv } from "@turbostarter/shared/constants";

import { env } from "./env";

import type { AnalyticsProviderClientStrategy } from "../types";

const init = () => {
  mixpanel.init(env.NEXT_PUBLIC_MIXPANEL_TOKEN, {
    debug: env.NODE_ENV === NodeEnv.DEVELOPMENT,
    autocapture: true,
    persistence: "localStorage",
  });
};

const Provider: AnalyticsProviderClientStrategy["Provider"] = ({
  children,
}) => {
  useEffect(() => {
    init();
  }, []);
  return children;
};

const track: AnalyticsProviderClientStrategy["track"] = (event, properties) => {
  if (typeof window === "undefined") {
    return;
  }

  mixpanel.track(event, properties);
};

const identify: AnalyticsProviderClientStrategy["identify"] = (
  userId,
  traits,
) => {
  if (typeof window === "undefined") {
    return;
  }

  mixpanel.identify(userId);
  if (traits) {
    mixpanel.people.set(traits);
  }
};

const reset: AnalyticsProviderClientStrategy["reset"] = () => {
  if (typeof window === "undefined") {
    return;
  }

  mixpanel.reset();
};

export { Provider, track, identify, reset };
