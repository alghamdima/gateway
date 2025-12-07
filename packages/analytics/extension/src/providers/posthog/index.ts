import { PostHog } from "posthog-js/dist/module.no-external";
import { v7 as uuidv7 } from "uuid";

import { env } from "./env";

import type { AnalyticsProviderStrategy } from "../types";

const posthog = new PostHog();

export async function getSharedDistinctId() {
  const stored = await chrome.storage.local.get(["posthog_distinct_id"]);
  if (stored.posthog_distinct_id) {
    return stored.posthog_distinct_id as string;
  }

  const distinctId = uuidv7();
  await chrome.storage.local.set({ posthog_distinct_id: distinctId });
  return distinctId;
}

const init = async () => {
  const distinctID = await getSharedDistinctId();
  posthog.init(env.VITE_POSTHOG_KEY, {
    bootstrap: {
      distinctID,
    },
    api_host: env.VITE_POSTHOG_HOST,
    disable_external_dependency_loading: true,
    persistence: "localStorage",
  });
};

const track: AnalyticsProviderStrategy["track"] = (name, params) => {
  void (async () => {
    await init();
    posthog.capture(name, params);
  })();
};

const identify: AnalyticsProviderStrategy["identify"] = (userId, traits) => {
  void (async () => {
    await init();
    posthog.identify(userId, traits);
  })();
};

const reset: AnalyticsProviderStrategy["reset"] = () => {
  void (async () => {
    await init();
    posthog.reset();
  })();
};

export { track, identify, reset };
