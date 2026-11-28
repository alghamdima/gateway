import { and, eq, isNull, or, sql } from "@workspace/db";
import { apiKey } from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { getGateway } from "@workspace/gateway-client/server";
import { logger } from "@workspace/shared/logger";

import { latestExpiry } from "./expiry";
import { getKeySettings } from "./settings";

import type { GatewayClient } from "@workspace/gateway-client/server";

/**
 * The platform's maximum key lifetime applied to keys that already exist
 * (ADMIN-12). OpenRouter refuses a key that outlives its account's maximum;
 * here such a key stops at created_at + maximum, the moment it would have
 * expired had the maximum been in force when it was made.
 *
 * The gateway enforces a key's expiry itself, from the expiry stored on its
 * virtual key (governance IsExpiredAt: 401 key_expired), so the new expiry is
 * written there first and then here, the same value in both. A key whose
 * cut-off moment has already passed gets an expiry a few seconds ahead, since
 * the gateway only accepts a future expiry: it stops within seconds of the
 * change. Idempotent: a key already within the maximum is not touched, so a
 * retry (or the hourly job) only finishes what a failure left. Raising the
 * maximum later extends nothing: a key's expiry only ever gets shorter.
 */

/** How far ahead a key already past its cut-off is set to expire. */
const PAST_CUTOFF_GRACE_MS = 5_000;

export interface MaxLifetimeResult {
  /** Keys whose expiry was shortened, in the gateway and here. */
  shortened: number;
  /** Keys the gateway could not update; the next run retries them. */
  failed: number;
}

export const applyMaxLifetime = async ({
  maxLifetimeSeconds,
  gateway = getGateway(),
  now = () => new Date(),
}: {
  maxLifetimeSeconds: number | null;
  gateway?: GatewayClient;
  now?: () => Date;
}): Promise<MaxLifetimeResult> => {
  if (maxLifetimeSeconds === null) {
    return { shortened: 0, failed: 0 };
  }
  const beyondMaximum = or(
    isNull(apiKey.expiresAt),
    sql`${apiKey.expiresAt} > ${apiKey.createdAt} + make_interval(secs => ${maxLifetimeSeconds})`,
  );
  const candidates = await db
    .select({ id: apiKey.id })
    .from(apiKey)
    .where(beyondMaximum);

  let shortened = 0;
  let failed = 0;
  for (const { id } of candidates) {
    // one key at a time, locked while the gateway is told, so two runs at once
    // cannot leave the gateway and this table with different moments; the
    // credits plugin's plain reads of api_key do not wait on the lock
    const outcome = await db
      .transaction(async (tx) => {
        const [key] = await tx
          .select({
            virtualKeyId: apiKey.virtualKeyId,
            createdAt: apiKey.createdAt,
            expiresAt: apiKey.expiresAt,
          })
          .from(apiKey)
          .where(and(eq(apiKey.id, id), beyondMaximum))
          .for("update", { skipLocked: true });
        const cutoff = key && latestExpiry(key.createdAt, maxLifetimeSeconds);
        if (!key || !cutoff) {
          return "skipped";
        }
        const at = now().getTime();
        // already expired: nothing left to shorten
        if (key.expiresAt && key.expiresAt.getTime() <= at) {
          return "skipped";
        }
        const expiresAt = new Date(
          Math.max(cutoff.getTime(), at + PAST_CUTOFF_GRACE_MS),
        );
        // only ever shorter
        if (key.expiresAt && expiresAt.getTime() >= key.expiresAt.getTime()) {
          return "skipped";
        }
        await gateway.updateVirtualKey(key.virtualKeyId, { expiresAt });
        await tx.update(apiKey).set({ expiresAt }).where(eq(apiKey.id, id));
        return "shortened";
      })
      .catch((error: unknown) => {
        logger.error("could not apply the maximum key lifetime", {
          keyId: id,
          error,
        });
        return "failed";
      });
    if (outcome === "shortened") shortened++;
    if (outcome === "failed") failed++;
  }
  return { shortened, failed };
};

/** The hourly job: applies the maximum in force, finishing what a failed save left. */
export const runKeyLifetimeJob = async (gateway?: GatewayClient) => {
  const { maxLifetimeSeconds } = await getKeySettings();
  return applyMaxLifetime({ maxLifetimeSeconds, gateway });
};
