"use client";

import dynamic from "next/dynamic";
import { posthog } from "posthog-js";
import { PostHogProvider } from "posthog-js/react";
import { useEffect } from "react";

import { initPostHog, schedulePostHogInit } from "./client";

import type { AnalyticsProviderClientStrategy } from "@workspace/analytics";

const PageView = dynamic(
  () => import("./page-view").then((mod) => mod.PageView),
  {
    ssr: false,
  },
);

export const strategy = {
  Provider: ({ children }) => {
    useEffect(() => {
      schedulePostHogInit();
    }, []);

    return (
      <PostHogProvider client={posthog}>
        {children}
        <PageView />
      </PostHogProvider>
    );
  },
  track: (event, properties) => {
    if (typeof window === "undefined") {
      return;
    }

    initPostHog();
    posthog.capture(event, properties);
  },
  identify: (userId, traits) => {
    if (typeof window === "undefined") {
      return;
    }

    initPostHog();
    posthog.identify(userId, traits);
  },
  reset: () => {
    if (typeof window === "undefined") {
      return;
    }

    initPostHog();
    posthog.reset();
  },
} satisfies AnalyticsProviderClientStrategy;
