"use client";

import { useEffect, useState } from "react";

/**
 * The current time, refreshed every `intervalMs`, for what depends on the
 * clock: an expiry preview, an expiry about to come.
 */
export const useNow = (intervalMs = 30_000) => {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(timer);
  }, [intervalMs]);
  return now;
};
