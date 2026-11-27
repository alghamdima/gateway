import { KEY_EXPIRIES, KEY_EXPIRY_SECONDS } from "@workspace/db/schema";

import type { KeyExpiry } from "@workspace/db/schema";

/**
 * A key's expiry (KEYS-03) and the platform's maximum key lifetime (ADMIN-12).
 * An expiry is chosen once, at creation, as a lifetime counted from that
 * moment, and is never changed by the customer. The maximum lifetime caps it:
 * a new key may not outlive created_at + maximum, and "no expiration" is
 * refused while a maximum is set. The gateway enforces the stored expiry
 * itself (governance IsExpiredAt: 401 key_expired), so the value written here
 * is the value sent to it.
 */

export type ExpiryChoice = KeyExpiry | "never";

/** The moment a key created at `now` with this choice expires; null: never. */
export const expiresAtFor = (choice: ExpiryChoice, now = new Date()) =>
  choice === "never"
    ? null
    : new Date(now.getTime() + KEY_EXPIRY_SECONDS[choice] * 1000);

/** The latest moment a key created at `createdAt` may live to; null: no maximum. */
export const latestExpiry = (
  createdAt: Date,
  maxLifetimeSeconds: number | null,
) =>
  maxLifetimeSeconds === null
    ? null
    : new Date(createdAt.getTime() + maxLifetimeSeconds * 1000);

/**
 * Whether a key created at `now` may expire at `expiresAt` (null: never) under
 * the maximum lifetime.
 */
export const isWithinMaxLifetime = (
  expiresAt: Date | null,
  maxLifetimeSeconds: number | null,
  now = new Date(),
) => {
  const latest = latestExpiry(now, maxLifetimeSeconds);
  if (latest === null) {
    return true;
  }
  return expiresAt !== null && expiresAt.getTime() <= latest.getTime();
};

/** The lifetime a number of seconds stands for. */
export const expiryFromSeconds = (seconds: number): KeyExpiry => {
  const found = KEY_EXPIRIES.find(
    (choice) => KEY_EXPIRY_SECONDS[choice] === seconds,
  );
  // the database only holds the listed values (key_settings checks)
  if (!found) {
    throw new Error(`no key expiry of ${seconds} seconds`);
  }
  return found;
};

/** The choice a number of seconds stands for: null is no expiration. */
export const choiceFromSeconds = (seconds: number | null): ExpiryChoice =>
  seconds === null ? "never" : expiryFromSeconds(seconds);

export const secondsFromChoice = (choice: ExpiryChoice) =>
  choice === "never" ? null : KEY_EXPIRY_SECONDS[choice];
